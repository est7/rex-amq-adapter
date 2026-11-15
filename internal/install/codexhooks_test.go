package install

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Golden pairs Codex itself wrote: each declaration and the trusted_hash its
// /hooks recorded for it (codex-cli 0.169.0). They cover a matcher, quotes,
// a `>` that Go's JSON would escape by default, a SessionEnd timeout, and an
// event that drops its matcher.
var codexGolden = []struct {
	event, group, hash string
}{
	{"PreToolUse", `{"hooks":[{"command":"rtk hook codex","type":"command"}],"matcher":"Bash"}`,
		"sha256:be6c6fcb2978ea348dd05101efed0afcf00efcbfb5d7e78949108142c875a77d"},
	{"SessionStart", `{"hooks":[{"type":"command","command":"/bin/sh -c 'rex __internal session-start --agent codex --feature-session-naming=false 2>/dev/null || true'","timeout":5}]}`,
		"sha256:3da181f6a6d78fa2e4446bb51881bea0b4d69562eb909961fa616df84970d643"},
	{"SessionEnd", `{"hooks":[{"type":"command","command":"'/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter' hook session-end --agent codex 2>/dev/null || true","timeout":3}]}`,
		"sha256:1fa999491a4ebc363d0180363296717887af75008a2de4f1cd5578659fecc388"},
	{"UserPromptSubmit", `{"hooks":[{"type":"command","command":"'/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter' hook user-prompt-submit --agent codex 2>/dev/null || true","timeout":3}]}`,
		"sha256:8e91d42f8897ea2a3153253f138bee638ea1818b57e65a5dc0b80b15803eff1c"},
	{"PostToolUse", `{"hooks":[{"type":"command","command":"'/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter' hook post-tool-use --agent codex 2>/dev/null || true","timeout":3}]}`,
		"sha256:dce4c87449a238be721982e84462e2c3dd2eb404d39f272201a5c88d28532117"},
}

func writeCodexHooks(t *testing.T, dir string, events map[string]string) string {
	t.Helper()
	body := `{"hooks":{`
	first := true
	for ev, groups := range events {
		if !first {
			body += ","
		}
		first = false
		body += `"` + ev + `":[` + groups + `]`
	}
	body += `}}`
	p := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHookHashMatchesCodex(t *testing.T) {
	for _, g := range codexGolden {
		dir := t.TempDir()
		hooks := writeCodexHooks(t, dir, map[string]string{g.event: g.group})
		key := filepath.Join(dir, "hooks.json")
		label := codexEvents[g.event].label
		config := filepath.Join(dir, "config.toml")
		writeFile(t, config, "[hooks.state.\""+key+":"+label+":0:0\"]\ntrusted_hash = \""+g.hash+"\"\n")
		got, err := CodexHooksNeedingReview(hooks, key, config)
		if err != nil || len(got) != 0 {
			t.Errorf("%s approved with Codex's own hash, yet: %+v %v", g.event, got, err)
		}
	}
}

func TestCodexHookReviewFindsUntrustedAndModified(t *testing.T) {
	dir := t.TempDir()
	pre, start := codexGolden[0], codexGolden[1]
	// A second PreToolUse group, as the host had: approved index 0 only.
	hooks := writeCodexHooks(t, dir, map[string]string{
		"PreToolUse":   pre.group + `,{"hooks":[{"command":"/usr/local/bin/other hook","type":"command"}],"matcher":"Bash"}`,
		"SessionStart": start.group,
	})
	key := hooks
	config := filepath.Join(dir, "config.toml")
	writeFile(t, config, `model = "x"

[hooks.state."`+key+`:pre_tool_use:0:0"]
trusted_hash = "`+pre.hash+`"

[hooks.state."`+key+`:session_start:0:0"]
trusted_hash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
enabled = false
`)
	got, err := CodexHooksNeedingReview(hooks, key, config)
	if err != nil {
		t.Fatal(err)
	}
	want := []CodexHookReview{
		{Key: key + ":pre_tool_use:1:0", Status: HookUntrusted},
		{Key: key + ":session_start:0:0", Status: HookModified},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("review:\n got %+v\nwant %+v", got, want)
	}
}

// A hook Codex never loads asks nothing: no hooks.json, one Codex cannot
// parse, an empty command, a prompt hook.
func TestCodexHookReviewIgnoresWhatCodexDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	hooks := filepath.Join(dir, "hooks.json")
	if got, err := CodexHooksNeedingReview(hooks, hooks, config); err != nil || got != nil {
		t.Fatalf("no hooks.json: %+v %v", got, err)
	}
	writeFile(t, hooks, "{not json")
	if got, err := CodexHooksNeedingReview(hooks, hooks, config); err != nil || got != nil {
		t.Fatalf("unparseable hooks.json: %+v %v", got, err)
	}
	writeCodexHooks(t, dir, map[string]string{"Stop": `{"hooks":[{"type":"command","command":"  "},{"type":"prompt"}]}`})
	if got, err := CodexHooksNeedingReview(hooks, hooks, config); err != nil || got != nil {
		t.Fatalf("hooks Codex skips: %+v %v", got, err)
	}
	writeCodexHooks(t, dir, map[string]string{"Stop": codexGolden[3].group})
	writeFile(t, config, "[broken")
	if _, err := CodexHooksNeedingReview(hooks, hooks, config); err == nil {
		t.Fatal("an unreadable config.toml was taken as no approvals")
	}
}

// Stop and UserPromptSubmit take no matcher: Codex hashes them without one,
// so a matcher written there does not change the approval.
func TestCodexHookHashDropsAMatcherTheEventIgnores(t *testing.T) {
	g := codexGolden[3] // UserPromptSubmit
	dir := t.TempDir()
	withMatcher := g.group[:len(g.group)-1] + `,"matcher":"anything"}`
	hooks := writeCodexHooks(t, dir, map[string]string{g.event: withMatcher})
	config := filepath.Join(dir, "config.toml")
	writeFile(t, config, "[hooks.state.\""+hooks+":user_prompt_submit:0:0\"]\ntrusted_hash = \""+g.hash+"\"\n")
	if got, err := CodexHooksNeedingReview(hooks, hooks, config); err != nil || len(got) != 0 {
		t.Fatalf("a matcher the event ignores changed the hash: %+v %v", got, err)
	}
}

func TestCodexHooksKeyResolvesAnOverrideOnly(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "codex-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got := CodexHooksKey(filepath.Join(link, "hooks.json"), true); got != filepath.Join(resolved, "hooks.json") {
		t.Errorf("CODEX_HOME override key %s, want %s", got, filepath.Join(resolved, "hooks.json"))
	}
	if got := CodexHooksKey(filepath.Join(link, "hooks.json"), false); got != filepath.Join(link, "hooks.json") {
		t.Errorf("default key %s, want the path as given", got)
	}
}

// After setup, Codex has not approved the adapter's hooks yet: the Codex
// agent reports each one untrusted, keyed under the resolved CODEX_HOME
// (a temp dir under /var is /private/var to Codex on macOS); Claude has no
// such review.
func TestCodexHookReviewAfterSetup(t *testing.T) {
	dir, agents := setup(t)
	if _, err := Setup(agents, Options{SelfBin: "/new/rex-amq-adapter"}); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		review, err := a.CodexHookReview()
		if err != nil {
			t.Fatal(err)
		}
		if a.Name != "Codex" {
			if review != nil {
				t.Errorf("%s reported a hook review: %+v", a.Name, review)
			}
			continue
		}
		if len(review) == 0 {
			t.Fatal("freshly installed Codex hooks reported approved")
		}
		for _, r := range review {
			if r.Status != HookUntrusted || !strings.HasPrefix(r.Key, filepath.Join(real, "hooks.json")+":") {
				t.Errorf("review %+v, want untrusted under %s", r, real)
			}
		}
	}
}
