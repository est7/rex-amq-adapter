package crew_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
)

// The built-in table is the one the user chose (goal contract
// 2026-11-15-rex-amq-adapter-routing-toml, spec 1), written out here from
// that list rather than read back from the file.
func TestRoutingBuiltinMatchesTheChosenTable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	type h = struct {
		kind          core.Kind
		model, effort string
	}
	want := map[string][]h{
		"orchestrator": {{core.Claude, "opus", "high"}, {core.Agy, "gemini-4-carbon-high", "high"}, {core.Codex, "gpt-5.4", "high"}},
		"narrator":     {{core.Agy, "gemini-4-hydrogen-low", ""}, {core.Claude, "haiku", "low"}},
		"architect":    {{core.Claude, "opus", "max"}, {core.Codex, "gpt-5.4", "xhigh"}, {core.Agy, "gemini-4-carbon-high", "high"}},
		"implementer":  {{core.Claude, "opus", "high"}, {core.Codex, "gpt-5.4", "high"}, {core.Agy, "gemini-4-carbon-high", "high"}},
		"reviewer":     {{core.Codex, "gpt-5.4", "xhigh"}, {core.Agy, "gemini-4-carbon-high", "high"}, {core.Claude, "opus", "high"}},
		"qa":           {{core.Codex, "gpt-5.4", "high"}, {core.Agy, "gemini-4-carbon-medium", "medium"}, {core.Claude, "sonnet", "high"}},
		"editor":       {{core.Codex, "gpt-5.4", "high"}, {core.Agy, "gemini-4-carbon-high", "high"}, {core.Claude, "opus", "high"}},
		"*":            {{core.Claude, "opus", "high"}, {core.Agy, "gemini-4-carbon-medium", "medium"}, {core.Codex, "gpt-5.4", "medium"}},
	}
	lib := crew.Load("")
	for role, hs := range want {
		r, err := lib.Routing(role)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if r.Role != role || r.Layer != "built-in" || r.Path != "" {
			t.Errorf("%s routed by %q from %s %q", role, r.Role, r.Layer, r.Path)
		}
		var kinds []core.Kind
		for _, x := range hs {
			kinds = append(kinds, x.kind)
			if got := r.Launch(x.kind); got != (core.Launch{Model: x.model, Effort: x.effort}) {
				t.Errorf("%s on %s = %+v, want %s/%s", role, x.kind, got, x.model, x.effort)
			}
		}
		if !slices.Equal(r.Harnesses, kinds) {
			t.Errorf("%s harnesses %v, want %v", role, r.Harnesses, kinds)
		}
	}
	// A role with no table of its own, a built-in prompt or a custom one, takes "*".
	for _, role := range []string{"writer", "explorer"} {
		if r, err := lib.Routing(role); err != nil || r.Role != crew.AnyRole || r.Harnesses[0] != core.Claude {
			t.Errorf("%s: %+v %v", role, r, err)
		}
	}
}

func writeRouting(t *testing.T, dir, text string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, crew.RoutingFile)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A project's table beats the user's, which beats the built-in one, role by
// role and whole: a role's own table in a lower layer still beats a higher
// layer's "*".
func TestRoutingLayersOverrideRoleByRole(t *testing.T) {
	xdg, repo := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	user := writeRouting(t, filepath.Join(xdg, "rex-amq-adapter", "crews"), `
[role.reviewer]
harness = ["agy"]
agy = { model = "claude-opus-4", effort = "max" }

[role.implementer]
harness = ["codex"]
`)
	project := writeRouting(t, filepath.Join(repo, ".agents", "crews"), `
[role.implementer]
harness = ["pi", "claude"]
pi = { model = "openai/gpt-5.4", effort = "off" }

[role."*"]
harness = ["codex"]
codex = { model = "gpt-5.4-mini" }
`)
	lib := crew.Load(filepath.Join(repo, "sub"))
	impl, err := lib.Routing("implementer")
	if err != nil || impl.Path != project || impl.Layer != "project" || !slices.Equal(impl.Harnesses, []core.Kind{core.Pi, core.Claude}) || impl.Launch(core.Pi) != (core.Launch{Model: "openai/gpt-5.4", Effort: "off"}) || impl.Launch(core.Claude) != (core.Launch{}) {
		t.Fatalf("implementer: %+v %v", impl, err)
	}
	rev, err := lib.Routing("reviewer")
	if err != nil || rev.Path != user || rev.Layer != "user" || rev.Role != "reviewer" || rev.Launch(core.Agy).Model != "claude-opus-4" {
		t.Fatalf("reviewer: %+v %v", rev, err)
	}
	// Built-in narrator beats the project's "*".
	if narr, err := lib.Routing("narrator"); err != nil || narr.Layer != "built-in" || narr.Launch(core.Claude).Model != "haiku" {
		t.Fatalf("narrator: %+v %v", narr, err)
	}
	// A role nobody names takes the highest "*".
	if other, err := lib.Routing("explorer"); err != nil || other.Path != project || other.Role != crew.AnyRole || other.Harnesses[0] != core.Codex {
		t.Fatalf("explorer: %+v %v", other, err)
	}
}

// A model that could be read as a flag, an effort the CLI does not take, a
// misspelt key, or an unknown harness makes the file an error: every route
// refuses, naming the file, and no lower layer stands in for it.
func TestRoutingRejectsInvalidFiles(t *testing.T) {
	for name, text := range map[string]string{
		"flag model":        "[role.reviewer]\nharness = [\"claude\"]\nclaude = { model = \"--dangerously-skip-permissions\" }\n",
		"dash model":        "[role.reviewer]\nharness = [\"codex\"]\ncodex = { model = \"-c\" }\n",
		"spaced model":      "[role.reviewer]\nharness = [\"codex\"]\ncodex = { model = \"gpt 5\" }\n",
		"unknown effort":    "[role.reviewer]\nharness = [\"codex\"]\ncodex = { effort = \"ultra\" }\n",
		"effort claude off": "[role.reviewer]\nharness = [\"claude\"]\nclaude = { effort = \"off\" }\n",
		"effort agy min":    "[role.reviewer]\nharness = [\"agy\"]\nagy = { effort = \"minimal\" }\n",
		"misspelt key":      "[role.reviewer]\nharness = [\"codex\"]\ncodex = { modle = \"gpt-5.4\" }\n",
		"unknown harness":   "[role.reviewer]\nharness = [\"gpt\"]\n",
		"empty harness":     "[role.reviewer]\nharness = []\n",
		"twice":             "[role.reviewer]\nharness = [\"codex\", \"codex\"]\n",
		"bad role":          "[role.\"Re viewer\"]\nharness = [\"codex\"]\n",
		"not toml":          "[role.reviewer\n",
	} {
		t.Run(name, func(t *testing.T) {
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			p := writeRouting(t, filepath.Join(xdg, "rex-amq-adapter", "crews"), text)
			for _, role := range []string{"reviewer", "narrator"} {
				if r, err := crew.Load("").Routing(role); err == nil || !strings.Contains(err.Error(), p) {
					t.Errorf("%s: %+v, err %v", role, r, err)
				}
			}
		})
	}
}
