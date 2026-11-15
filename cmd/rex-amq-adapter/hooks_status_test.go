package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
)

// status names each Codex hook still waiting for approval, the fix (/hooks),
// and what waits on it; with none waiting it says so.
func TestStatusHookApprovalNote(t *testing.T) {
	c := codexHooks{File: "/h/.codex/hooks.json", Unapproved: []install.CodexHookReview{{Key: "/h/.codex/hooks.json:pre_tool_use:1:0", Status: install.HookUntrusted}}}
	note := c.note()
	for _, want := range []string{"1 hook(s)", "unapproved_hooks", "/h/.codex/hooks.json:pre_tool_use:1:0 (untrusted)", "/hooks", "Hooks need review", "another family"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if ok := (codexHooks{File: "/h/.codex/hooks.json"}).note(); !strings.Contains(ok, "every hook") || strings.Contains(ok, "/hooks in") {
		t.Errorf("approved note: %s", ok)
	}
}

// summon's JSON line names the harness a start launched and each default it
// passed over (agy-29's s1 review: the CLI dropped both).
func TestSummonOutputNamesTheFallbackHarness(t *testing.T) {
	out := summonOutput(app.SummonResult{Action: "start", Harness: core.Agy, Passed: []string{"codex: unapproved_hooks: …"}})
	if out["harness"] != core.Agy || len(out["passed_over"].([]string)) != 1 {
		t.Fatalf("start: %v", out)
	}
	out = summonOutput(app.SummonResult{Action: "refused", Passed: []string{"codex: not installed", "agy: not installed"}})
	if _, ok := out["harness"]; ok || len(out["passed_over"].([]string)) != 2 {
		t.Fatalf("refusal: %v", out)
	}
	if out := summonOutput(app.SummonResult{Action: "deliver"}); out["harness"] != nil || out["passed_over"] != nil {
		t.Fatalf("a delivery: %v", out)
	}
}

// status reads each Codex agent's hooks (and no other harness's) into its
// codex_hooks section; runInstall itself refuses a test binary as the hook
// target, so this covers its read.
func TestStatusCodexHookApprovalsReadTheAgentsHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codex := filepath.Join(home, "codex")
	if err := os.MkdirAll(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codex)
	hooks := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`
	if err := os.WriteFile(filepath.Join(codex, "hooks.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := install.Agents([]core.Kind{core.Claude, core.Codex})
	if err != nil {
		t.Fatal(err)
	}
	got, err := codexHookApprovals(agents)
	if err != nil || len(got) != 1 || len(got[0].Unapproved) != 1 || !strings.HasSuffix(got[0].Unapproved[0].Key, "hooks.json:stop:0:0") {
		t.Fatalf("%+v %v", got, err)
	}
}
