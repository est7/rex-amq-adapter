package core

import (
	"slices"
	"testing"
)

// A narrator and a personal agent are nobody's mates, never hear of a
// departure, and never stand in to report a stopped goal; their own
// departure is nobody's mail.
func TestNarratorStealthInSessionPolicy(t *testing.T) {
	windowOf := map[string]string{"b:o": "w", "b:i": "w", "b:n": "w2", "b:p": "w"}
	orch := Peer{Handle: "claude-01", Kind: Claude, Role: OrchestratorRole, Block: "b:o"}
	impl := Peer{Handle: "claude-02", Kind: Claude, Role: "implementer", Block: "b:i"}
	narr := Peer{Handle: "pi-03", Kind: Pi, Role: NarratorRole, Block: "b:n"}
	pers := Peer{Handle: "codex-04", Kind: Codex, Role: PersonalRole, Block: "b:p"}
	mates := SessionMates("b:i", windowOf, []Peer{orch, narr, pers})
	if len(mates) != 1 || mates[0].Handle != orch.Handle {
		t.Fatalf("mates of a worker: %+v", mates)
	}
	if !Stealth(NarratorRole) || !Stealth(PersonalRole) || Stealth(OrchestratorRole) || Stealth("") {
		t.Fatal("Stealth covers exactly the narrator and the personal agent")
	}

	// The narrator leaves: the orchestrator hears nothing.
	if to, _, _ := DepartureMail(narr, "sess", []Peer{orch, impl}); len(to) != 0 {
		t.Fatalf("a narrator's departure was mailed to %v", to)
	}
	// The orchestrator leaves with only a narrator (which notifies) and a
	// worker beside it: the worker reports, never the narrator.
	narr.Kind = Claude
	to, _, body := DepartureMail(orch, "", []Peer{narr, impl})
	if !slices.Equal(to, []Handle{impl.Handle}) || body == "" {
		t.Fatalf("reporting peer: %v", to)
	}
	// With only the narrator left, nobody is mailed.
	if to, _, _ := DepartureMail(orch, "", []Peer{narr, pers}); len(to) != 0 {
		t.Fatalf("the narrator was asked to report a stopped goal: %v", to)
	}
	// A worker leaves beside an orchestrator and a narrator: only the
	// orchestrator hears.
	if to, _, _ := DepartureMail(impl, "", []Peer{narr, orch}); !slices.Equal(to, []Handle{orch.Handle}) {
		t.Fatalf("departure mail went to %v", to)
	}
}
