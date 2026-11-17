package core

import (
	"strings"
	"testing"
)

// A personal agent never summons: it opens an Epic. Every other role,
// the unadopted "" included, passes this guard to the others.
func TestGuardSummonerRefusesAPersonalAgent(t *testing.T) {
	g := GuardSummoner(PersonalRole)
	if !g.Refused || !strings.Contains(g.Reason, "rex-amq-adapter epic open <name> --cwd <dir>") {
		t.Fatalf("personal: %+v", g)
	}
	for _, role := range []string{"", OrchestratorRole, NarratorRole, "implementer", "reviewer"} {
		if g := GuardSummoner(role); g.Refused {
			t.Errorf("%q refused: %+v", role, g)
		}
	}
}
