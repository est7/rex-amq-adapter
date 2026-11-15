package app

import (
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A personal agent's summon is refused before anything happens: no pane is
// launched or resumed, no reservation is taken, and no mail waits, whether
// the target is live, departed, or new. The reason names `epic`.
func TestSummonRefusesAPersonalCallerBeforeAnyPane(t *testing.T) {
	e, term, loc := summonWorld(t)
	me, _, _ := e.Store.Record(loc.Block)
	me.Role = core.PersonalRole
	add(t, e, me)
	add(t, e, departed(store.Record{Handle: "codex-07", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 7, AgentSessionID: "thread-7", AdoptedAt: e.Now().Add(-time.Hour), Cwd: t.TempDir()}, e.Now().Add(-time.Minute)))
	for _, target := range []string{"implementer", "reviewer", "codex-07"} {
		res := summon(t, e, loc, target, func(r *SummonRequest) { r.Asked = "开个 agent" })
		if res.Action != "refused" || !strings.Contains(res.Reason, "rex-amq-adapter epic") || res.Mailed || res.Block != "" {
			t.Fatalf("%s: %+v", target, res)
		}
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched: %+v", term.launches)
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
	if got := unread(t, e, "codex-07"); len(got) != 0 {
		t.Fatalf("mail waits for the departed agent: %v", got)
	}
}
