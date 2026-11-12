package core

import (
	"testing"
	"time"
)

func flaggedSet(hs ...Handle) func(Handle) bool {
	m := set(hs...)
	return func(h Handle) bool { return m[h] }
}

// A flagged owner means its whole model family may be down, so the fallback
// must avoid every family skipped on the way, not only the first one.
func TestErrorRecipientFallbackAvoidsEverySkippedFamily(t *testing.T) {
	rec := Peer{Handle: "codex-01", Kind: Codex, Role: "implementer", Parent: "claude-01"}
	summoner := Peer{Handle: "claude-01", Kind: Claude}
	orch := Peer{Handle: "pi-01", Kind: Pi, Role: OrchestratorRole}
	orchSibling := Peer{Handle: "pi-02", Kind: Pi}
	other := Peer{Handle: "agy-01", Kind: Agy}
	isFlagged := flaggedSet("claude-01", "pi-01")

	to, why, ok := ErrorRecipient(rec, []Peer{rec, summoner, orch, orchSibling, other}, "", NoticeTurnError, isFlagged)
	if !ok || to.Handle != "agy-01" {
		t.Errorf("with claude and pi both skipped as flagged: got %s ok=%v, want agy-01", to.Handle, ok)
	}
	if want := "another model family because claude-01 is flagged"; ok && why != want {
		t.Errorf("why = %q, want %q (the first skipped owner)", why, want)
	}

	to, _, ok = ErrorRecipient(rec, []Peer{rec, summoner, orch, orchSibling}, "", NoticeTurnError, isFlagged)
	if ok {
		t.Errorf("only siblings of skipped families remain: got %s, want no recipient", to.Handle)
	}
}

func TestErrorRecipientFallbackAvoidsFamiliesOfSkippedTabOwners(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	rec := Peer{Handle: "codex-01", Kind: Codex, Window: "w1", AdoptedAt: t0.Add(2 * time.Minute)}
	firstOwner := Peer{Handle: "agy-01", Kind: Agy, Window: "w1", AdoptedAt: t0}
	secondOwner := Peer{Handle: "claude-01", Kind: Claude, Window: "w1", AdoptedAt: t0.Add(time.Minute)}
	claudeSibling := Peer{Handle: "claude-02", Kind: Claude, Window: "w2"}
	other := Peer{Handle: "pi-01", Kind: Pi, Window: "w2"}

	to, why, ok := ErrorRecipient(rec, []Peer{rec, secondOwner, firstOwner, claudeSibling, other}, "", NoticeTurnError, flaggedSet("agy-01", "claude-01"))
	if !ok || to.Handle != "pi-01" {
		t.Errorf("with both earlier tab owners flagged: got %s ok=%v, want pi-01", to.Handle, ok)
	}
	if want := "another model family because agy-01 is flagged"; ok && why != want {
		t.Errorf("why = %q, want %q (the earliest skipped owner)", why, want)
	}
}

// A turn limit is the agent's own budget, not a family outage: nobody outside
// the ownership chain is woken for it.
func TestErrorRecipientTurnLimitNeverFallsBackAcrossFamilies(t *testing.T) {
	rec := Peer{Handle: "codex-01", Kind: Codex, Parent: "claude-01"}
	summoner := Peer{Handle: "claude-01", Kind: Claude}
	orch := Peer{Handle: "pi-01", Kind: Pi, Role: OrchestratorRole}
	other := Peer{Handle: "agy-01", Kind: Agy}

	for name, tc := range map[string]struct {
		peers   []Peer
		flagged []Handle
	}{
		"one flagged owner":  {[]Peer{rec, summoner, other}, []Handle{"claude-01"}},
		"two flagged owners": {[]Peer{rec, summoner, orch, other}, []Handle{"claude-01", "pi-01"}},
	} {
		if to, _, ok := ErrorRecipient(rec, tc.peers, "", NoticeTurnLimit, flaggedSet(tc.flagged...)); ok {
			t.Errorf("%s, turn_limit: got %s, want no recipient", name, to.Handle)
		}
		if to, _, ok := ErrorRecipient(rec, tc.peers, "", NoticeTurnError, flaggedSet(tc.flagged...)); !ok || to.Handle != "agy-01" {
			t.Errorf("%s, turn_error: got %s ok=%v, want agy-01", name, to.Handle, ok)
		}
	}
}

// Each flagged owner hands the notice to the next one down the chain.
func TestErrorRecipientPrecedence(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	rec := Peer{Handle: "codex-05", Kind: Codex, Role: "implementer", Window: "w1", Parent: "claude-01", AdoptedAt: t0.Add(2 * time.Minute)}
	summoner := Peer{Handle: "claude-01", Kind: Claude, Window: "w2", AdoptedAt: t0}
	lead := Peer{Handle: "claude-02", Kind: Claude, Role: "architect", Window: "w1", AdoptedAt: t0.Add(3 * time.Minute)}
	tabOwner := Peer{Handle: "claude-03", Kind: Claude, Window: "w1", AdoptedAt: t0}
	orch := Peer{Handle: "claude-09", Kind: Claude, Role: OrchestratorRole, Window: "w2", AdoptedAt: t0}
	other := Peer{Handle: "agy-01", Kind: Agy, Window: "w2", AdoptedAt: t0}
	// Lowest precedence first, so slice order cannot stand in for the chain.
	peers := []Peer{other, orch, tabOwner, lead, summoner, rec}

	steps := []struct {
		want Handle
		why  string
	}{
		{"claude-01", "its summoner"},
		{"claude-02", "its crew lead"},
		{"claude-03", "its tab owner"},
		{"claude-09", "the session's orchestrator"},
		{"agy-01", "another model family because claude-01 is flagged"},
		{"", ""},
	}
	var flagged []Handle
	for _, s := range steps {
		to, why, ok := ErrorRecipient(rec, peers, "architect", NoticeTurnError, flaggedSet(flagged...))
		if to.Handle != s.want || why != s.why || ok != (s.want != "") {
			t.Errorf("flagged %v: got %q (%q) ok=%v, want %q (%q)", flagged, to.Handle, why, ok, s.want, s.why)
		}
		flagged = append(flagged, s.want)
	}
}
