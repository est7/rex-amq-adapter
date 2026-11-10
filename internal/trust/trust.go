// Package trust derives, for a linked git worktree, the folder trust the user
// already gave its main checkout in a harness's own trust store. It derives
// and never creates: a repository the user never trusted keeps its dialog.
//
// Only Antigravity needs it. Claude Code 2.1.319 and Codex 0.167 resolve a
// linked worktree to its main checkout themselves (Codex in
// codex-rs/git-utils/src/trust.rs); Antigravity keeps one exact directory per
// entry of trustedWorkspaces in ~/.gemini/antigravity-cli/settings.json (a
// trusted parent does not cover its children) and asks again in every
// worktree. ~/.gemini/trustedFolders.json is Gemini CLI's, not Antigravity's.
package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// Result says what Derive found and did for one harness.
type Result struct {
	Harness string `json:"harness"`
	Store   string `json:"store"`
	// Main is the main checkout the worktree belongs to; "" when dir is not
	// a linked worktree.
	Main string `json:"main,omitempty"`
	// Written is set when an entry for dir was added to Store.
	Written bool   `json:"written"`
	Note    string `json:"note"`
}

// Derive extends the trust home's Antigravity store gives dir's main
// checkout to dir, when dir is a linked worktree of a trusted repository.
// It writes nothing in every other case and says why in the note.
func Derive(home, dir string) (Result, error) {
	r := Result{Harness: "agy", Store: filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return r, err
	}
	main, ok, err := MainCheckout(dir)
	if err != nil {
		return r, err
	}
	if !ok {
		r.Note = "not a linked worktree; the dialog stays the user's"
		return r, nil
	}
	r.Main = main
	// Adapter writers serialize on a lock of their own: the store is
	// replaced by rename, so a lock on it would not outlive a write.
	// Antigravity's own writes do not take it; a read inside the lock keeps
	// that window to the write itself.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := fsutil.Flock(ctx, filepath.Join(home, ".local", "state", "rex-amq-adapter", "trust.lock"))
	if err != nil {
		return r, err
	}
	defer unlock()
	raw, err := os.ReadFile(r.Store)
	if errors.Is(err, os.ErrNotExist) {
		r.Note = "no trust store; the user has trusted nothing in Antigravity"
		return r, nil
	}
	if err != nil {
		return r, err
	}
	// A managed store is a symlink: update the file it names, keep the link.
	target, err := filepath.EvalSymlinks(r.Store)
	if err != nil {
		return r, err
	}
	var settings struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return r, fmt.Errorf("%s: %w", r.Store, err)
	}
	if key, ok := lookup(settings.Trusted, dir); ok {
		r.Note = fmt.Sprintf("already trusted as %s", key)
		return r, nil
	}
	if _, ok := lookup(settings.Trusted, main); !ok {
		r.Note = fmt.Sprintf("main checkout %s is not trusted in Antigravity; the dialog stays the user's", main)
		return r, nil
	}
	out, err := appendWorkspace(raw, dir)
	if err != nil {
		return r, fmt.Errorf("%s: %w", r.Store, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return r, err
	}
	if err := fsutil.WriteAtomic(target, out); err != nil {
		return r, err
	}
	if err := os.Chmod(target, info.Mode().Perm()); err != nil {
		return r, err
	}
	r.Written = true
	r.Note = fmt.Sprintf("trust of %s extended to its linked worktree %s", main, dir)
	return r, nil
}

// lookup finds the entry that names path's directory. Antigravity stores
// each directory as the user opened it, so entries compare by file identity:
// on a case-insensitive volume two spellings are one directory, on a
// case-sensitive one they are two, and an entry that no longer exists names
// nothing.
func lookup(trusted []string, path string) (string, bool) {
	want, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	for _, t := range trusted {
		if info, err := os.Stat(t); err == nil && os.SameFile(info, want) {
			return t, true
		}
	}
	return "", false
}

// appendWorkspace adds dir as the last element of "trustedWorkspaces" in the
// settings file, keeping every other byte, and proves the result parses to
// the old settings with that one element added.
func appendWorkspace(raw []byte, dir string) ([]byte, error) {
	keyAt := bytes.Index(raw, []byte(`"trustedWorkspaces"`))
	if keyAt < 0 {
		return nil, errors.New("no trustedWorkspaces key")
	}
	open := bytes.IndexByte(raw[keyAt:], '[')
	if open < 0 {
		return nil, errors.New("trustedWorkspaces is not an array")
	}
	open += keyAt
	depth, close := 0, -1
	for i := open; i < len(raw); i++ {
		switch raw[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				close = i
			}
		case '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' {
					i++
				}
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 {
		return nil, errors.New("trustedWorkspaces is not closed")
	}
	inner := bytes.TrimRight(raw[open+1:close], " \t\r\n")
	indent := "    "
	if nl := bytes.LastIndexByte(inner, '\n'); nl >= 0 {
		indent = string(inner[nl+1:])
		indent = indent[:len(indent)-len(strings.TrimLeft(indent, " \t"))]
	}
	closeIndent := ""
	if nl := bytes.LastIndexByte(raw[:close], '\n'); nl >= 0 && bytes.TrimSpace(raw[nl+1:close]) == nil {
		closeIndent = string(raw[nl+1 : close])
	}
	elem, _ := json.Marshal(dir)
	var out bytes.Buffer
	out.Write(raw[:open+1])
	out.Write(inner)
	if len(bytes.TrimSpace(inner)) > 0 {
		out.WriteByte(',')
	}
	out.WriteString("\n" + indent)
	out.Write(elem)
	out.WriteString("\n" + closeIndent)
	out.Write(raw[close:])
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(out.Bytes(), &after); err != nil {
		return nil, fmt.Errorf("the edited settings do not parse: %w", err)
	}
	if len(before) != len(after) {
		return nil, errors.New("the edited settings lost a key")
	}
	for k, v := range before {
		if k == "trustedWorkspaces" {
			continue
		}
		if !bytes.Equal(bytes.TrimSpace(v), bytes.TrimSpace(after[k])) {
			return nil, fmt.Errorf("the edited settings changed %q", k)
		}
	}
	var was, now []string
	json.Unmarshal(before["trustedWorkspaces"], &was)
	if err := json.Unmarshal(after["trustedWorkspaces"], &now); err != nil || len(now) != len(was)+1 || now[len(now)-1] != dir {
		return nil, errors.New("the edited trustedWorkspaces is not the old list plus the worktree")
	}
	for i := range was {
		if was[i] != now[i] {
			return nil, errors.New("the edited trustedWorkspaces reordered an entry")
		}
	}
	return out.Bytes(), nil
}

// MainCheckout resolves a linked worktree to its main checkout by reading
// git's own files, without running git: dir/.git is a file naming a gitdir
// under <common>/worktrees/<name>, whose commondir names <common>, and the
// main checkout is <common>'s parent. ok is false for a main checkout, a
// plain directory, or anything that does not fit that shape.
func MainCheckout(dir string) (main string, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && strings.Contains(err.Error(), "is a directory") {
			return "", false, nil
		}
		return "", false, err
	}
	target, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !found {
		return "", false, nil
	}
	gitdir := strings.TrimSpace(target)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir, err = filepath.EvalSymlinks(gitdir)
	if err != nil {
		return "", false, nil
	}
	if filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return "", false, nil
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", false, nil
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitdir, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil || commonDir != filepath.Dir(filepath.Dir(gitdir)) || filepath.Base(commonDir) != ".git" {
		return "", false, nil
	}
	if info, err := os.Stat(filepath.Join(commonDir, "HEAD")); err != nil || info.IsDir() {
		return "", false, nil
	}
	// The registration must point back at this very .git file: a copied
	// pointer in another directory names a gitdir that registers someone else.
	back, err := os.ReadFile(filepath.Join(gitdir, "gitdir"))
	if err != nil {
		return "", false, nil
	}
	registered := strings.TrimSpace(string(back))
	if !filepath.IsAbs(registered) {
		registered = filepath.Join(gitdir, registered)
	}
	regInfo, err := os.Stat(registered)
	if err != nil {
		return "", false, nil
	}
	ownInfo, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil || !os.SameFile(regInfo, ownInfo) {
		return "", false, nil
	}
	return filepath.Dir(commonDir), true, nil
}
