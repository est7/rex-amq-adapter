package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// designLeadWorld is summonWorld with its caller, claude-01, the lead
// architect of a design Epic: the session's policy names architect as its
// lead role and claude-01 as its lead.
func designLeadWorld(t *testing.T) (Env, *fakeTerm, Location) {
	t.Helper()
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	rec, _, _ := e.agentAt(loc.Block)
	rec.Role = architectRole
	add(t, e, seedOf(rec))
	p := epicPolicy("session:s", "unix:///fake.sock", designCrew, "claude-09", e.Now())
	p.LeadHandle = rec.Handle
	if err := e.Store.PutSessionPolicy(p); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutSessionCrew("session:s", store.SessionCrew{Crew: designCrew, ClaimedBy: "claude-09", ClaimedAt: e.Now()}); err != nil {
		t.Fatal(err)
	}
	return e, term, loc
}

// A design Epic's lead architect (claude, depth 0) seats a council of
// architects in its own directory: it holds the claude seat itself (self,
// never mailed or launched), and only codex and agy start, at depth 1, so
// the council is exactly three architects. The dry run plans the same
// seats and launches nothing. The seats count against the cap with the
// lead; a seated architect may not summon another architect (a cycle, at
// depth 1); and a plain summon of an architect still starts on the role's
// first harness, claude, never on another family.
func TestCouncilSelfSeat(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	brainstorm := func(extra ...func(*SummonRequest)) func(*SummonRequest) {
		return func(r *SummonRequest) {
			r.Council, r.Kind, r.Cwd, r.Asked = true, "brainstorm", dir, "convene the design council"
			for _, f := range extra {
				f(r)
			}
		}
	}

	plan := summon(t, e, loc, architectRole, brainstorm(func(r *SummonRequest) { r.DryRun = true }))
	if plan.Action != "council" || !plan.DryRun || seatActions(plan) != "claude:self codex:start agy:start" || len(term.launches) != 0 {
		t.Fatalf("dry run %+v launches %d", plan, len(term.launches))
	}
	if plan.Seats[0].Handle != "claude-01" || plan.Seats[0].Mailed {
		t.Fatalf("the lead's own seat %+v", plan.Seats[0])
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("the dry run reserved %d", n)
	}

	res := summon(t, e, loc, architectRole, brainstorm())
	if res.Action != "council" || seatActions(res) != "claude:self codex:start agy:start" || res.Depth != 1 {
		t.Fatalf("council %+v", res)
	}
	if len(term.launches) != 2 || launchProgram(term.launches[0])[0] != "codex" || launchProgram(term.launches[1])[0] != "agy" {
		t.Fatalf("launches %+v", term.launches)
	}
	for _, l := range term.launches {
		if l.Dir != dir || !strings.HasPrefix(l.Label, "architect | ") {
			t.Fatalf("launch %+v", l)
		}
		if brief := l.Command[len(l.Command)-1]; !strings.Contains(brief, "claude-01, which sent it, holds the claude seat itself") {
			t.Fatalf("brief %q", brief)
		}
	}
	if len(unread(t, e, "claude-01")) != 0 {
		t.Fatal("the lead mailed itself")
	}
	var seated []agent
	for i, k := range []core.Kind{core.Codex, core.Agy} {
		rec := adoptLaunch(t, e, term, term.launches[i], k, fmt.Sprintf("arch-%d", i))
		if rec.Role != architectRole || rec.Parent != "claude-01" || rec.Depth != 1 {
			t.Fatalf("seat %+v", rec)
		}
		seated = append(seated, rec)
	}
	if err := e.AwaitCouncil(t.Context(), loc, SummonRequest{Kind: "brainstorm", Subject: "s"}, &res, 0); err != nil {
		t.Fatal(err)
	}
	if len(res.Handles) != 2 || len(unread(t, e, "claude-01")) != 0 {
		t.Fatalf("handles %v", res.Handles)
	}
	architects := 0
	recs, _ := e.agents()
	for _, r := range recs {
		if r.Live() && r.SessionID == "session:s" && r.Role == architectRole {
			architects++
		}
	}
	if architects != 3 {
		t.Fatalf("%d architects, want the lead and two seats", architects)
	}
	// The council's seats are not leads: a hand-back stays the lead's.
	for _, r := range seated {
		if lead, _ := e.sessionLead(r); lead {
			t.Errorf("%s leads", r.Handle)
		}
	}

	// A seated architect (depth 1) summoning an architect is a cycle.
	again := summon(t, e, Location{Block: seated[0].BlockID}, architectRole, brainstorm())
	if again.Action != "refused" || !strings.Contains(again.Reason, "cycle") {
		t.Fatalf("a seat's council: %+v", again)
	}
	single := summon(t, e, Location{Block: seated[0].BlockID}, architectRole, func(r *SummonRequest) { r.Asked, r.Cwd = "one more", workDir(t, e, "other") })
	if single.Action != "refused" || !strings.Contains(single.Reason, "cycle") {
		t.Fatalf("a seat's single summon: %+v", single)
	}
	if len(term.launches) != 2 {
		t.Fatalf("launches %d", len(term.launches))
	}
}

// The cap counts the lead and every seat: with four workers beside the
// lead (five live), the two seats a council would start make seven, over
// the cap of six, so it is refused whole and nothing launches.
func TestCouncilSelfSeatCap(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	for i := 0; i < 4; i++ {
		add(t, e, seed{Handle: core.Handle(fmt.Sprintf("pi-%02d", i+2)), Kind: core.Pi, Server: "unix:///fake.sock", BlockID: fmt.Sprintf("block:peer-%d", i), SessionID: "session:s", AgentPID: i + 10, AdoptedAt: e.Now()})
	}
	res := summon(t, e, loc, architectRole, func(r *SummonRequest) {
		r.Council, r.Kind, r.Cwd, r.Asked = true, "brainstorm", dir, "convene"
	})
	if res.Action != "refused" || !strings.Contains(res.Reason, "cap") || len(term.launches) != 0 {
		t.Fatalf("over the cap: %+v", res)
	}
}

// Outside a council the architect keeps its routing: a plain summon starts
// it on claude, its first harness, whatever the caller's family; and an
// architect that is not its Epic's lead seats no council of architects.
func TestCouncilSelfSeatArchitectRouting(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	res := summon(t, e, loc, architectRole, func(r *SummonRequest) { r.Asked = "design it" })
	if res.Action != "start" || res.Harness != core.Claude || launchProgram(term.launches[0])[0] != "claude" {
		t.Fatalf("a plain architect %+v", res)
	}

	e2, term2, loc2 := summonWorld(t)
	e2.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	rec, _, _ := e2.agentAt(loc2.Block)
	rec.Role = architectRole
	add(t, e2, seedOf(rec)) // an architect in a session no design Epic leads
	if err := e2.Store.PutSessionPolicy(epicPolicy("session:s", "unix:///fake.sock", "", "claude-09", e2.Now())); err != nil {
		t.Fatal(err)
	}
	got := summon(t, e2, loc2, architectRole, func(r *SummonRequest) {
		r.Council, r.Kind, r.Cwd, r.Asked = true, "brainstorm", workDir(t, e2, "app"), "convene"
	})
	if got.Action != "refused" || !strings.Contains(got.Reason, "cycle") || len(term2.launches) != 0 {
		t.Fatalf("a non-lead architect's council: %+v", got)
	}
}
