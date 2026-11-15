package trust

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// rtk's hook and the hash Codex 0.169.0 recorded when the user approved it.
const (
	rtkGroup = `{"hooks":[{"command":"rtk hook codex","type":"command"}],"matcher":"Bash"}`
	rtkHash  = "sha256:be6c6fcb2978ea348dd05101efed0afcf00efcbfb5d7e78949108142c875a77d"
)

// F8: a Codex hook the user has not approved stops every new Codex session
// on "Hooks need review". The preflight refuses Codex with
// unapproved_hooks before any pane, and before it grants folder trust
// under a root, so probing an unready Codex writes nothing; another harness
// is not affected, and an approval Codex recorded lets Codex through.
func TestTrustPreflightRefusesCodexWithUnapprovedHooks(t *testing.T) {
	g, home := world(t)
	ctx := context.Background()
	side := mkdir(t, home, "projects", "side")
	repoDir := mkdir(t, side, "app")
	gitInit(t, repoDir)
	if _, err := g.AddRoots(ctx, side); err != nil {
		t.Fatal(err)
	}
	writeFile(t, g.Stores.CodexHooks, `{"hooks":{"PreToolUse":[`+rtkGroup+`,`+strings.Replace(rtkGroup, "rtk hook codex", "me hook pretooluse --agent codex", 1)+`]}}`)
	writeFile(t, g.Stores.Codex, `[hooks.state."`+g.Stores.CodexHooks+`:pre_tool_use:0:0"]
trusted_hash = "`+rtkHash+`"
`)
	before, _ := os.ReadFile(g.Stores.Codex)

	err := g.Preflight(ctx, repoDir, core.Claude, core.Codex)
	var h HooksUnapproved
	if !errors.As(err, &h) || len(h.Hooks) != 1 || h.Hooks[0].Key != g.Stores.CodexHooks+":pre_tool_use:1:0" || h.Hooks[0].Status != "untrusted" {
		t.Fatalf("refusal %v %+v", err, h)
	}
	for _, want := range []string{"unapproved_hooks", "Hooks need review", "/hooks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if !Refusal(err) {
		t.Error("unapproved_hooks is not reported as a refusal")
	}
	if after, _ := os.ReadFile(g.Stores.Codex); string(after) != string(before) {
		t.Errorf("the preflight changed Codex's config:\n%s", after)
	}
	if _, err := os.Stat(g.logPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused Codex preflight granted folder trust first")
	}
	if err := g.Preflight(ctx, repoDir, core.Claude, core.Agy); err != nil {
		t.Fatalf("a preflight without Codex read Codex's hooks: %v", err)
	}

	// The user approves the second hook in /hooks; Codex writes its hash.
	approved := `
[hooks.state."` + g.Stores.CodexHooks + `:pre_tool_use:1:0"]
trusted_hash = "sha256:` + strings.Repeat("0", 64) + `"
`
	writeFile(t, g.Stores.Codex, string(before)+approved)
	if err := g.Preflight(ctx, repoDir, core.Codex); !errors.As(err, &h) || h.Hooks[0].Status != "modified" {
		t.Fatalf("a stale approval: %v", err)
	}
	review, err := g.CodexHooks()
	if err != nil || len(review) != 1 {
		t.Fatalf("CodexHooks %+v %v", review, err)
	}
	writeFile(t, g.Stores.Codex, `[hooks.state."`+g.Stores.CodexHooks+`:pre_tool_use:0:0"]
trusted_hash = "`+rtkHash+`"
`)
	writeFile(t, g.Stores.CodexHooks, `{"hooks":{"PreToolUse":[`+rtkGroup+`]}}`)
	if err := g.Preflight(ctx, repoDir, core.Claude, core.Codex); err != nil {
		t.Fatalf("approved hooks: %v", err)
	}
}

// CODEX_HOME moves the hooks and keys them by its resolved path.
func TestTrustHookStoresFollowCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	s := StoresFor(t.TempDir(), func(k string) string {
		if k == "CODEX_HOME" {
			return codexHome
		}
		return ""
	})
	if s.CodexHooks != codexHome+"/hooks.json" || !s.CodexHomeSet {
		t.Fatalf("stores %+v", s)
	}
}
