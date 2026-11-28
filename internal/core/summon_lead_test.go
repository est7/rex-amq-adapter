package core

import (
	"strings"
	"testing"
)

// An Epic's lead at depth 0 may seat peers of its own read-only role (a
// council, or one seat): it alone is not a cycle. Its own handle, a seat at
// depth 1, a lead of a role that writes, and an agent that does not lead
// still are.
func TestGuardSummonLeadSeatsItsOwnReadOnlyRole(t *testing.T) {
	lead := []Peer{{Handle: "claude-01", Role: "architect"}}
	seated := []Peer{{Handle: "claude-01", Role: "architect"}, {Handle: "codex-02", Role: "architect"}}
	underOrchestrator := []Peer{{Handle: "claude-01", Role: OrchestratorRole}, {Handle: "codex-02", Role: "architect"}}
	pairLead := []Peer{{Handle: "claude-01", Role: "implementer"}}
	for _, c := range []struct {
		name   string
		chain  []Peer
		depth  int
		target string
		lead   bool
		cycle  bool
	}{
		{"the lead seats an architect", lead, 0, "architect", true, false},
		{"the lead seats a reviewer of its own role", []Peer{{Handle: "claude-01", Role: "reviewer"}}, 0, "reviewer", true, false},
		{"an architect that does not lead", lead, 0, "architect", false, true},
		{"the lead by its own handle", lead, 0, "claude-01", true, true},
		{"a seat at depth 1", seated, 1, "architect", true, true},
		{"a depth-1 architect under an orchestrator", underOrchestrator, 1, "architect", true, true},
		{"a pair lead summons an implementer", pairLead, 0, "implementer", true, true},
	} {
		g := GuardSummon("brainstorm", c.chain, c.depth, c.target, 1, c.lead)
		if got := g.Refused && strings.HasPrefix(g.Reason, "cycle: "); got != c.cycle || !c.cycle && g.Refused {
			t.Errorf("%s: %+v, want cycle %v", c.name, g, c.cycle)
		}
	}
}
