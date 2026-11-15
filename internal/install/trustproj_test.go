package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

const claudeJSON = `{
  "numStartups": 2318,
  "userID": "abc",
  "bigNumber": 12345678901234567890,
  "projects": {
    "/work/old": {
      "allowedTools": [],
      "hasTrustDialogAccepted": false,
      "lastCost": 0.25
    },
    "/work/other": {
      "hasTrustDialogAccepted": true
    }
  },
  "zLast": "kept last"
}
`

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Trusting a project in Claude's config sets that one key: every other key
// keeps its value and its place, numbers keep their digits, and a second
// grant writes nothing.
func TestTrustClaudeProjectSetsOnlyThatKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(file, []byte(claudeJSON), 0o640); err != nil {
		t.Fatal(err)
	}
	before := readJSON(t, file)
	for _, root := range []string{"/work/old", "/work/new"} {
		changed, err := TrustClaudeProject(context.Background(), file, root)
		if err != nil || !changed {
			t.Fatalf("%s: changed %v err %v", root, changed, err)
		}
	}
	after := readJSON(t, file)
	want := before
	projects := want["projects"].(map[string]any)
	projects["/work/old"].(map[string]any)["hasTrustDialogAccepted"] = true
	projects["/work/new"] = map[string]any{"hasTrustDialogAccepted": true}
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("after\n%v\nwant\n%v", after, want)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "12345678901234567890") || strings.Index(string(b), `"numStartups"`) > strings.Index(string(b), `"zLast"`) {
		t.Fatalf("digits or order lost:\n%s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(file + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the lock was left behind: %v", err)
	}
	stamp, _ := os.Stat(file)
	time.Sleep(10 * time.Millisecond)
	if changed, err := TrustClaudeProject(context.Background(), file, "/work/new"); err != nil || changed {
		t.Fatalf("a second grant: changed %v err %v", changed, err)
	}
	if again, _ := os.Stat(file); !again.ModTime().Equal(stamp.ModTime()) {
		t.Fatal("a second grant rewrote the file")
	}
	trusted, err := ClaudeTrustedProjects(file)
	if err != nil || !trusted["/work/old"] || !trusted["/work/new"] || !trusted["/work/other"] || len(trusted) != 3 {
		t.Fatalf("trusted %v %v", trusted, err)
	}
}

// A missing config gets one with just that project; a config that is not a
// JSON object is left alone.
func TestTrustClaudeProjectCreatesOrRefuses(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cfg", ".claude.json")
	if changed, err := TrustClaudeProject(context.Background(), file, "/p"); err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	if got := readJSON(t, file); !reflect.DeepEqual(got, map[string]any{"projects": map[string]any{"/p": map[string]any{"hasTrustDialogAccepted": true}}}) {
		t.Fatalf("%v", got)
	}
	bad := filepath.Join(t.TempDir(), ".claude.json")
	os.WriteFile(bad, []byte(`{"projects": [1]}`), 0o600)
	if _, err := TrustClaudeProject(context.Background(), bad, "/p"); err == nil {
		t.Fatal("a projects array was overwritten")
	}
	if b, _ := os.ReadFile(bad); string(b) != `{"projects": [1]}` {
		t.Fatalf("rewritten: %s", b)
	}
}

// The <file>.lock directory is Claude's: a live one is waited for, a stale
// one is taken over, and a grant whose context ends first writes nothing.
func TestTrustClaudeProjectHonoursClaudesLock(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".claude.json")
	os.WriteFile(file, []byte("{}"), 0o600)
	if err := os.Mkdir(file+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := TrustClaudeProject(ctx, file, "/p"); err == nil {
		t.Fatal("wrote while Claude held its lock")
	}
	if b, _ := os.ReadFile(file); string(b) != "{}" {
		t.Fatalf("written under a live lock: %s", b)
	}
	old := time.Now().Add(-time.Minute)
	os.Chtimes(file+".lock", old, old)
	if changed, err := TrustClaudeProject(context.Background(), file, "/p"); err != nil || !changed {
		t.Fatalf("a stale lock: changed %v err %v", changed, err)
	}
}

// Concurrent grants of different projects each keep their key.
func TestTrustClaudeProjectConcurrentGrantsKeepEveryKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".claude.json")
	os.WriteFile(file, []byte(claudeJSON), 0o600)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := TrustClaudeProject(context.Background(), file, fmt.Sprintf("/c/%d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	trusted, _ := ClaudeTrustedProjects(file)
	for i := range 8 {
		if !trusted[fmt.Sprintf("/c/%d", i)] {
			t.Errorf("/c/%d lost: %v", i, trusted)
		}
	}
}

const codexTOML = `# my codex config
model = "gpt-6" # inline comment
approval_policy = "never"

[projects."/work/a"]
trust_level = "untrusted" # keep this comment

[projects."/work/b"]
hide = true

[tui]
theme = "dark"
`

func codexDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return doc
}

// Trusting a project in config.toml changes only its trust_level: an
// existing line is rewritten in place, a table without one gets it, a new
// project gets its table at the end, comments stay, and a second grant
// writes nothing.
func TestTrustCodexProjectSetsOnlyThatKey(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(config, []byte(codexTOML), 0o600)
	for _, root := range []string{"/work/a", "/work/b", "/work/new dir/x.y"} {
		if changed, err := TrustCodexProject(config, root); err != nil || !changed {
			t.Fatalf("%s: changed %v err %v", root, changed, err)
		}
	}
	got := codexDoc(t, config)
	want := map[string]any{
		"model": "gpt-6", "approval_policy": "never",
		"projects": map[string]any{
			"/work/a":           map[string]any{"trust_level": "trusted"},
			"/work/b":           map[string]any{"hide": true, "trust_level": "trusted"},
			"/work/new dir/x.y": map[string]any{"trust_level": "trusted"},
		},
		"tui": map[string]any{"theme": "dark"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	text, _ := os.ReadFile(config)
	for _, keep := range []string{"# my codex config", `model = "gpt-6" # inline comment`, `trust_level = "trusted" # keep this comment`} {
		if !strings.Contains(string(text), keep) {
			t.Errorf("lost %q:\n%s", keep, text)
		}
	}
	if !strings.HasSuffix(string(text), "[projects.\"/work/new dir/x.y\"]\ntrust_level = \"trusted\"\n") {
		t.Errorf("new table not appended:\n%s", text)
	}
	stamp, _ := os.ReadFile(config)
	if changed, err := TrustCodexProject(config, "/work/a"); err != nil || changed {
		t.Fatalf("a second grant: changed %v err %v", changed, err)
	}
	if again, _ := os.ReadFile(config); !bytes.Equal(again, stamp) {
		t.Fatal("a second grant rewrote the file")
	}
	for root, want := range map[string]bool{"/work/a": true, "/work/b": true, "/work/c": false} {
		if ok, err := CodexProjectTrusted(config, root); err != nil || ok != want {
			t.Errorf("%s: %v %v", root, ok, err)
		}
	}
}

// A missing config.toml gets just the table; one that does not parse is
// left alone.
func TestTrustCodexProjectCreatesOrRefuses(t *testing.T) {
	config := filepath.Join(t.TempDir(), "codex", "config.toml")
	if changed, err := TrustCodexProject(config, "/p"); err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	if b, _ := os.ReadFile(config); string(b) != "[projects.\"/p\"]\ntrust_level = \"trusted\"\n" {
		t.Fatalf("%q", b)
	}
	bad := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(bad, []byte("model = \n"), 0o600)
	if _, err := TrustCodexProject(bad, "/p"); err == nil {
		t.Fatal("a broken config was edited")
	}
}
