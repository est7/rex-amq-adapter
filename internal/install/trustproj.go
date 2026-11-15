package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// Folder trust lives in two stores the harnesses own and write themselves:
// Claude Code's global config (~/.claude.json, or .claude.json under
// CLAUDE_CONFIG_DIR) and Codex's config.toml. These functions read them and
// set one project's trust, changing nothing else; package trust decides
// when.

// ClaudeTrustedProjects lists the project keys file marks
// hasTrustDialogAccepted. A missing file trusts nothing.
func ClaudeTrustedProjects(file string) (map[string]bool, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Projects map[string]struct {
			Accepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	out := map[string]bool{}
	for k, p := range doc.Projects {
		if p.Accepted {
			out[k] = true
		}
	}
	return out, nil
}

// claudeLockStale is proper-lockfile's default: Claude Code refreshes a lock
// it holds well inside it, so an older one belongs to a process that died.
const claudeLockStale = 10 * time.Second

// TrustClaudeProject sets projects[root].hasTrustDialogAccepted = true in
// Claude Code's global config file, keeping every other key and their order.
// It follows Claude's own protocol: hold the <file>.lock directory, re-read
// the file under it, replace it atomically, and read it back. changed is
// false when the key was already set; nothing is written then.
func TrustClaudeProject(ctx context.Context, file, root string) (changed bool, err error) {
	if r, err := filepath.EvalSymlinks(file); err == nil {
		file = r // replace the target, never a symlink the user keeps
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return false, err
	}
	unlock, err := claudeLock(ctx, file+".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	mode := fs.FileMode(0o600)
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	default:
		if st, err := os.Stat(file); err == nil {
			mode = st.Mode().Perm()
		}
	}
	doc, err := parseObject(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", file, err)
	}
	projects, err := childObject(doc, "projects")
	if err != nil {
		return false, fmt.Errorf("%s: %w", file, err)
	}
	project, err := childObject(projects, root)
	if err != nil {
		return false, fmt.Errorf("%s: projects[%q]: %w", file, root, err)
	}
	if v, _ := project.get("hasTrustDialogAccepted"); v == true {
		return false, nil
	}
	project.set("hasTrustDialogAccepted", true)
	out, err := marshal(doc)
	if err != nil {
		return false, err
	}
	if err := fsutil.WriteAtomic(file, out); err != nil {
		return false, err
	}
	if err := os.Chmod(file, mode); err != nil {
		return true, err
	}
	trusted, err := ClaudeTrustedProjects(file)
	if err != nil {
		return true, err
	}
	if !trusted[root] {
		return true, fmt.Errorf("%s: projects[%q] was written but does not read back as trusted", file, root)
	}
	return true, nil
}

// childObject is o[k] as an object, set to an empty one when absent.
func childObject(o *object, k string) (*object, error) {
	v, ok := o.get(k)
	if !ok || v == nil {
		c := newObject()
		o.set(k, c)
		return c, nil
	}
	c, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("%q is not an object", k)
	}
	return c, nil
}

// claudeLock takes proper-lockfile's directory lock at path, waiting while
// a live holder keeps it and taking over a stale one.
func claudeLock(ctx context.Context, path string) (func(), error) {
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > claudeLockStale {
			_ = os.Remove(path)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// CodexProjectTrusted reports whether config marks any of keys
// trust_level = "trusted". A missing file trusts nothing.
func CodexProjectTrusted(config string, keys ...string) (bool, error) {
	c, err := readCodexConfig(config)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if codexTrustLevel(c.doc, k) == "trusted" {
			return true, nil
		}
	}
	return false, nil
}

func codexTrustLevel(doc map[string]any, root string) any {
	projects, _ := doc["projects"].(map[string]any)
	project, _ := projects[root].(map[string]any)
	return project["trust_level"]
}

// TrustCodexProject sets projects."<root>".trust_level = "trusted" in
// config by lines (writeCodexEdit), keeping the rest of the file and its
// comments; the edit is checked with go-toml before it replaces the file.
// Codex states no lock protocol for config.toml, so callers serialize their
// own writes. changed is false when it was already set.
func TrustCodexProject(config, root string) (changed bool, err error) {
	c, err := readCodexConfig(config)
	if err != nil {
		return false, err
	}
	if codexTrustLevel(c.doc, root) == "trusted" {
		return false, nil
	}
	edits := func(lines []string) [][]string { return codexTrustEdits(lines, root) }
	check := func(before map[string]any, text string) error { return checkCodexTrustEdit(before, text, root) }
	if err := writeCodexEdit(config, edits, check, fmt.Sprintf("add [projects.%s] trust_level = \"trusted\" by hand", strconv.Quote(root))); err != nil {
		return false, err
	}
	return true, nil
}

var (
	trustLevelLine = regexp.MustCompile(`^(\s*trust_level\s*=\s*)([^#]*?)(\s*(#.*)?)$`)
	projectsHeader = regexp.MustCompile(`^\s*\[\s*projects\s*\.`)
)

// codexTrustEdits are the ways to trust root by lines, most specific first:
// rewrite a trust_level line, insert one after a [projects.…] header, or
// append the table. A candidate in the wrong table changes another project,
// so checkCodexTrustEdit refuses it and the next is tried.
func codexTrustEdits(lines []string, root string) [][]string {
	var edits [][]string
	for i, line := range lines {
		if trustLevelLine.MatchString(line) {
			edit := slices.Clone(lines)
			edit[i] = trustLevelLine.ReplaceAllString(line, `${1}"trusted"${3}`)
			edits = append(edits, edit)
		}
	}
	for i, line := range lines {
		if projectsHeader.MatchString(line) {
			edits = append(edits, slices.Insert(slices.Clone(lines), i+1, `trust_level = "trusted"`))
		}
	}
	table := slices.Clone(lines)
	if len(table) > 0 && strings.TrimSpace(table[len(table)-1]) != "" {
		table = append(table, "")
	}
	return append(edits, append(table, "[projects."+strconv.Quote(root)+"]", `trust_level = "trusted"`))
}

// checkCodexTrustEdit reports whether text is before with only
// projects[root].trust_level set to "trusted".
func checkCodexTrustEdit(before map[string]any, text, root string) error {
	after := map[string]any{}
	if err := toml.Unmarshal([]byte(text), &after); err != nil {
		return fmt.Errorf("the edit would not parse (%v)", err)
	}
	if codexTrustLevel(after, root) != "trusted" {
		return errors.New("the edit did not trust the project")
	}
	strip := func(doc map[string]any) map[string]any {
		out := maps.Clone(doc)
		projects, ok := out["projects"].(map[string]any)
		if !ok {
			return out
		}
		projects = maps.Clone(projects)
		if project, ok := projects[root].(map[string]any); ok {
			project = maps.Clone(project)
			delete(project, "trust_level")
			if len(project) == 0 {
				delete(projects, root)
			} else {
				projects[root] = project
			}
		}
		if len(projects) == 0 {
			delete(out, "projects")
		} else {
			out["projects"] = projects
		}
		return out
	}
	if !reflect.DeepEqual(strip(before), strip(after)) {
		return errors.New("the edit would change other settings")
	}
	return nil
}
