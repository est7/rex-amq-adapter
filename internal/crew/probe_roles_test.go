package crew_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
)

func TestRoutingFirstHarnessMatchesBuiltinRolesOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lib := crew.Load("")
	first := func(role string) core.Kind {
		r, err := lib.Routing(role)
		if err != nil {
			t.Fatal(err)
		}
		return r.Harnesses[0]
	}
	entries, err := lib.List()
	if err != nil {
		t.Fatal(err)
	}
	var shared []string
	for _, e := range entries {
		if e.Crew == "" {
			shared = e.Roles
			break
		}
	}
	if len(shared) == 0 {
		t.Fatal("missing shared built-in roles")
	}

	codexRoles := []string{"reviewer", "editor", "qa"}
	for _, r := range codexRoles {
		if !slices.Contains(shared, r) {
			t.Errorf("Codex-default role %q has no built-in roles/%s.md prompt", r, r)
		}
		if got := first(r); got != core.Codex {
			t.Errorf("first harness of %q = %v, want %v", r, got, core.Codex)
		}
	}
	for _, r := range shared {
		want := core.Claude
		switch {
		case slices.Contains(codexRoles, r):
			want = core.Codex
		case r == "narrator":
			// The narrator only reads and relays: the cheap agy model.
			want = core.Agy
		}
		if got := first(r); got != want {
			t.Errorf("first harness of %q = %v, want %v", r, got, want)
		}
	}
	// Removed orphan role must not exist in built-in roles or carry a special-case harness.
	if slices.Contains(shared, "explorer") {
		t.Error("orphan role explorer should not be in shared built-in roles")
	}
	if got := first("explorer"); got != core.Claude {
		t.Errorf("first harness of explorer = %v, want default %v", got, core.Claude)
	}
}

func TestProbeBeforeAskingRulesPresent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lib := crew.Load("")

	arch, ok, err := lib.Role("", "architect")
	if err != nil || !ok {
		t.Fatalf("architect role: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(arch.Text, "safe, self-cleaning probe before proposing or asking") {
		t.Errorf("architect prompt missing probe-before-asking rule: %s", arch.Text)
	}

	orch, ok, err := lib.Role("", "orchestrator")
	if err != nil || !ok {
		t.Fatalf("orchestrator role: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(orch.Text, "Settle observable facts by experiment before escalating") {
		t.Errorf("orchestrator prompt missing probe-before-escalating rule: %s", orch.Text)
	}

	// Peer lifetime: one read-only reviewer or qa serves the whole run, and a
	// Slice writer outlives its Slice only until review and proposal finish.
	for _, want := range []string{
		"Keep one read-only `reviewer` or `qa` open across Slices",
		"close a Slice's `implementer` only after its review and proposal finish",
	} {
		if !strings.Contains(orch.Text, want) {
			t.Errorf("orchestrator prompt missing peer-lifetime rule %q: %s", want, orch.Text)
		}
	}
	if strings.Contains(orch.Text, "Close an agent when its work is done") {
		t.Errorf("orchestrator prompt still closes every agent as soon as its work is done: %s", orch.Text)
	}
	// F16: an orchestrator that may "do the work yourself" codes its Slices
	// itself (3/3 agy orchestrators did); it delegates every Slice instead,
	// and on agy never stops for a plan or feedback review.
	if strings.Contains(orch.Text, "or do the work yourself") {
		t.Errorf("orchestrator prompt still lets it do the work itself: %s", orch.Text)
	}
	for _, want := range []string{
		"You never edit a Slice's code or tests yourself",
		"its coding and rework go to an implementer (`rex-amq-adapter summon implementer --cwd <worktree>`)",
		"never call `EnterPlanMode` or set `RequestFeedback: true`",
	} {
		if !strings.Contains(orch.Text, want) {
			t.Errorf("orchestrator prompt missing delegation rule %q: %s", want, orch.Text)
		}
	}

	design, ok, err := lib.Conventions("design")
	if err != nil || !ok {
		t.Fatalf("design conventions: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(design.Text, "safe, self-cleaning probes outside the repository") {
		t.Errorf("design conventions missing disposable probe allowance: %s", design.Text)
	}
	// F15: a member's `nmem t` search during the seal surfaced the lead's
	// unsent proposal from a live session summary.
	if !strings.Contains(design.Text, "until then nobody searches session transcripts (`nmem t`, `rex-history`, transcript files)") {
		t.Errorf("design conventions do not seal session transcripts before the Ledger: %s", design.Text)
	}
}
