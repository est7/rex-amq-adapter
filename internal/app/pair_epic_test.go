package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// epic --crew pair opens a one-Slice Epic: its narrator and, in the
// orchestrator's place, an implementer that leads, started on the
// implementer's routing (here a project table that sends implementers to
// Codex) and briefed with the goal contract. An Epic of no crew still opens
// an orchestrator on its own routing.
func TestPairEpicOpensItsImplementerAndNarrator(t *testing.T) {
	for _, tc := range []struct {
		crew, harness, lead string
		argv                []string
	}{
		{"pair", "", implementerRole, []string{"codex", "-m", "gpt-6.1-sol-mini", "-c", "model_reasoning_effort=low"}},
		{"pair", "claude", implementerRole, []string{"claude", "--"}},
		{"", "", core.OrchestratorRole, []string{"claude", "--model", "opus", "--effort", "high"}},
	} {
		t.Run("crew="+tc.crew+"/harness="+tc.harness, func(t *testing.T) {
			e, term, loc, goal := openEpicWorld(t)
			dir := filepath.Dir(goal)
			writeFile(t, filepath.Join(dir, ".agents", "crews", crew.RoutingFile), "[role.implementer]\nharness = [\"codex\"]\ncodex = { model = \"gpt-6.1-sol-mini\", effort = \"low\" }\n")
			e.Crews = crew.Load
			adoptPane(t, e, "block:epic-1-1", "agy-02", core.NarratorRole)
			adoptPane(t, e, "block:epic-1-2", "claude-03", tc.lead)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "one slice", Dir: dir, Goal: goal, Crew: tc.crew, Harness: tc.harness})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Ready() || res.Lead != tc.lead || res.Orchestrator.Handle != "claude-03" {
				t.Fatalf("result %+v", res)
			}
			panes := term.sessions_[0].panes
			if len(panes) != 2 || panes[0].Label != "narrator | one slice" || panes[1].Label != tc.lead+" | one slice" {
				t.Fatalf("panes %+v", panes)
			}
			lead := panes[1].Command
			if got := program(lead)[:len(tc.argv)]; !slices.Equal(got, tc.argv) {
				t.Errorf("lead argv %q", lead)
			}
			if brief := lead[len(lead)-1]; !strings.Contains(brief, "Execute the goal contract at "+goal) {
				t.Errorf("the lead is not briefed with the goal: %q", brief)
			}
			if c, ok, _ := e.Store.SessionCrew(res.SessionID); ok != (tc.crew != "") || c.Crew != tc.crew {
				t.Errorf("session crew %+v %v", c, ok)
			}
		})
	}
}

// A pair Epic's own implementer records the hand-back and reads its Epic's
// view, as an orchestrator does. Every other implementer stays a worker: one
// an orchestrator summoned, one in an Epic that has (or had) an orchestrator,
// one in a session of another crew or none; and the lead reads no other
// Epic's view.
func TestPairEpicLeadHandsBackAndReadsTheView(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	now := time.Now()
	for session, c := range map[string]string{"session:pair": pairCrew, "session:had-orch": pairCrew, "session:dev": "dev"} {
		if err := e.Store.PutSessionCrew(session, store.SessionCrew{Crew: c, ClaimedBy: "claude-01", ClaimedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	put(t, e,
		store.Record{Handle: "claude-10", Kind: core.Claude, Role: implementerRole, SessionID: "session:pair", AdoptedAt: now},
		store.Record{Handle: "codex-11", Kind: core.Codex, Role: implementerRole, SessionID: "session:pair", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		store.Record{Handle: "codex-12", Kind: core.Codex, Role: "reviewer", SessionID: "session:pair", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		store.Record{Handle: "agy-13", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:pair", AdoptedAt: now},
		store.Record{Handle: "claude-20", Kind: core.Claude, Role: implementerRole, SessionID: "session:had-orch", AdoptedAt: now},
		store.Record{Handle: "claude-21", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:had-orch", AdoptedAt: now, DepartedAt: &now},
		store.Record{Handle: "claude-30", Kind: core.Claude, Role: implementerRole, SessionID: "session:dev", AdoptedAt: now},
		store.Record{Handle: "claude-40", Kind: core.Claude, Role: implementerRole, SessionID: "session:none", AdoptedAt: now},
	)
	ctx := context.Background()
	for _, c := range []struct {
		who  core.Handle
		lead bool
	}{
		{"claude-10", true},
		{"codex-11", false}, // summoned by the lead
		{"codex-12", false}, // a reviewer
		{"claude-20", false},
		{"claude-30", false},
		{"claude-40", false},
	} {
		loc := Location{Block: "block:" + string(c.who)}
		_, err := e.RecordHandBack(ctx, loc, "## Review queue\n- the Slice")
		if c.lead != (err == nil) || !c.lead && !errors.Is(err, ErrRefused) {
			t.Errorf("%s recording a hand-back: %v", c.who, err)
		}
		id, err := e.viewReader(ctx, loc, "")
		if c.lead != (err == nil) || !c.lead && !errors.Is(err, ErrRefused) || c.lead && id != "session:pair" {
			t.Errorf("%s reading the view: %q %v", c.who, id, err)
		}
	}
	if hb, ok, _ := e.Store.HandBack("session:pair"); !ok || hb.By != "claude-10" {
		t.Errorf("the pair lead's hand-back: %+v %v", hb, ok)
	}
	if _, err := e.viewReader(ctx, Location{Block: "block:claude-10"}, "session:dev"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), core.ReasonCrossEpic) {
		t.Errorf("the pair lead read another Epic's view: %v", err)
	}
	// The narrator still relays no user control to a pair lead: the gateway
	// knows only an orchestrator as an Epic's control plane.
	narr, lead := recordOf(t, e, "agy-13").Endpoint(), recordOf(t, e, "claude-10").Endpoint()
	if d := core.CanExchange(narr, lead, core.Operation{Kind: core.OpControl, ControlRequestID: "req-1"}, nil); d.Allow || d.Reason != core.ReasonNarratorWorker {
		t.Errorf("narrator control to a pair lead: %+v", d)
	}
}

// A pair Epic's lead starts its own reviewer, as the goal's cross-model
// review needs.
func TestPairEpicLeadSummonsItsReviewer(t *testing.T) {
	e, term, loc := summonWorld(t)
	rec, _, _ := e.Store.Record(loc.Block)
	rec.Role = implementerRole
	add(t, e, rec)
	if err := e.Store.PutSessionCrew(rec.SessionID, store.SessionCrew{Crew: pairCrew, ClaimedBy: rec.Handle, ClaimedAt: e.Now()}); err != nil {
		t.Fatal(err)
	}
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Kind = "todo"; r.Asked = "review the Slice" })
	if res.Action != "start" || res.Parent != rec.Handle || len(term.launches) != 1 {
		t.Fatalf("%+v %v", res, term.launches)
	}
}
