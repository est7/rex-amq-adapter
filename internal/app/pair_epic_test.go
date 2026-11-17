package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
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
		seed{Handle: "claude-10", Kind: core.Claude, Role: implementerRole, SessionID: "session:pair", AdoptedAt: now},
		seed{Handle: "codex-11", Kind: core.Codex, Role: implementerRole, SessionID: "session:pair", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		seed{Handle: "codex-12", Kind: core.Codex, Role: "reviewer", SessionID: "session:pair", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		seed{Handle: "agy-13", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:pair", AdoptedAt: now},
		seed{Handle: "claude-20", Kind: core.Claude, Role: implementerRole, SessionID: "session:had-orch", AdoptedAt: now},
		seed{Handle: "claude-21", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:had-orch", AdoptedAt: now, DepartedAt: &now},
		seed{Handle: "claude-30", Kind: core.Claude, Role: implementerRole, SessionID: "session:dev", AdoptedAt: now},
		seed{Handle: "claude-40", Kind: core.Claude, Role: implementerRole, SessionID: "session:none", AdoptedAt: now},
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
	// The narrator relays user control to the pair lead, which takes the
	// orchestrator's place as the Epic's control plane; every other
	// implementer stays a worker it never mails.
	control := core.Operation{Kind: core.OpControl, ControlRequestID: "req-1"}
	narr, err := e.endpointOf("agy-13")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who    core.Handle
		reason string
	}{
		{"claude-10", core.ReasonControl},
		{"codex-11", core.ReasonNarratorWorker},
		{"codex-12", core.ReasonNarratorWorker},
	} {
		to, err := e.endpointOf(c.who)
		if err != nil {
			t.Fatal(err)
		}
		if d := core.CanExchange(narr, to, control, nil); d.Allow != (c.reason == core.ReasonControl) || d.Reason != c.reason {
			t.Errorf("narrator control to %s (lead=%v): %+v", c.who, to.Lead, d)
		}
	}
}

// A narrator's --control mail reaches a pair Epic's lead and passes its
// arrival gate; a plain send to it, control to an implementer the lead
// summoned, and control to an implementer in an Epic that had an
// orchestrator are refused. The lead itself still never mails the personal
// agent, and the refusal names the path that does reach it.
func TestPairEpicNarratorControlReachesTheLead(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx := context.Background()
	now := time.Now()
	for session, c := range map[string]string{"session:pair": pairCrew, "session:had-orch": pairCrew} {
		if err := e.Store.PutSessionCrew(session, store.SessionCrew{Crew: c, ClaimedBy: "claude-01", ClaimedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	put(t, e,
		seed{Handle: "claude-01", Role: core.PersonalRole, SessionID: "session:personal", AdoptedAt: now},
		seed{Handle: "claude-10", Role: implementerRole, SessionID: "session:pair", AdoptedAt: now},
		seed{Handle: "codex-11", Role: implementerRole, SessionID: "session:pair", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		seed{Handle: "agy-13", Role: core.NarratorRole, SessionID: "session:pair", AdoptedAt: now},
		seed{Handle: "claude-20", Role: implementerRole, SessionID: "session:had-orch", AdoptedAt: now},
		seed{Handle: "claude-21", Role: core.OrchestratorRole, SessionID: "session:had-orch", AdoptedAt: now, DepartedAt: &now},
		seed{Handle: "agy-23", Role: core.NarratorRole, SessionID: "session:had-orch", AdoptedAt: now},
	)
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:personal"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:pair"}}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude-01", "claude-10", "codex-11", "agy-13", "claude-20", "agy-23"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		from core.Handle
		args []string
	}{
		{"agy-13", []string{"--to", "claude-10", "--subject", "s", "--body", "b"}},
		{"agy-13", []string{"--to", "codex-11", "--control", "req-1", "--body", "b"}},
		{"agy-23", []string{"--to", "claude-20", "--control", "req-2", "--body", "b"}},
	} {
		if _, _, err := run(t, e, ctx, c.from, "send", c.args...); !errors.Is(err, ErrRefused) {
			t.Errorf("%s send %q: %v", c.from, c.args, err)
		}
	}
	out, code, err := run(t, e, ctx, "agy-13", "send", "--to", "claude-10", "--control", "req-3", "--subject", "directive", "--body", "the user: also cover X", "--json")
	if err != nil || code != 0 {
		t.Fatalf("control to the pair lead: %d %v %s", code, err, out)
	}
	var sent amq.Sent
	if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.ID == "" {
		t.Fatalf("output %q", out)
	}
	lead := recordOf(t, e, "claude-10")
	if kept, err := e.admit(lead); err != nil || len(kept) != 1 || kept[0] != sent.ID {
		t.Fatalf("the pair lead's gate kept %v (%v), want %s", kept, err, sent.ID)
	}
	_, _, err = run(t, e, ctx, "claude-10", "send", "--to", "claude-01", "--subject", "done", "--body", "b")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), core.ReasonPersonalOnly) || !strings.Contains(err.Error(), `report with "rex-amq-adapter epic handback @<file>"`) {
		t.Errorf("the pair lead mailing the personal agent: %v", err)
	}
	if _, _, err := run(t, e, ctx, "claude-01", "send", "--to", "claude-10", "--subject", "s", "--body", "b"); err == nil || strings.Contains(err.Error(), "hand-back") {
		t.Errorf("the personal agent mailing the pair lead is refused without the members' hint: %v", err)
	}
}

// A pair Epic's lead starts its own reviewer, as the goal's cross-model
// review needs.
func TestPairEpicLeadSummonsItsReviewer(t *testing.T) {
	e, term, loc := summonWorld(t)
	rec, _, _ := e.agentAt(loc.Block)
	rec.Role = implementerRole
	add(t, e, seedOf(rec))
	if err := e.Store.PutSessionCrew(rec.SessionID, store.SessionCrew{Crew: pairCrew, ClaimedBy: rec.Handle, ClaimedAt: e.Now()}); err != nil {
		t.Fatal(err)
	}
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Kind = "todo"; r.Asked = "review the Slice" })
	if res.Action != "start" || res.Parent != rec.Handle || len(term.launches) != 1 {
		t.Fatalf("%+v %v", res, term.launches)
	}
}

// A pair Epic runs unattended like any Epic: its implementer lead and its
// narrator carry ME_UNATTENDED=1 in their own commands, and the reviewer the
// lead starts in the Epic's session does too.
func TestPairEpicRunsItsLeadAndItsReviewerUnattended(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	e.Crews = crew.Load
	adoptPane(t, e, "block:epic-1-1", "agy-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", implementerRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "one slice", Dir: filepath.Dir(goal), Goal: goal, Crew: pairCrew}); err != nil {
		t.Fatal(err)
	}
	for i, p := range term.sessions_[0].panes {
		if !unattendedPrefix(p.Command) {
			t.Errorf("pane %d (%s) %q: want %s=1 in its env prefix", i, p.Label, p.Command, UnattendedEnv)
		}
	}

	e2, term2, loc2 := summonWorld(t)
	lead, _, _ := e2.agentAt(loc2.Block)
	lead.Role = implementerRole
	add(t, e2, seedOf(lead))
	if err := e2.Store.PutSessionCrew(lead.SessionID, store.SessionCrew{Crew: pairCrew, ClaimedBy: lead.Handle, ClaimedAt: e2.Now()}); err != nil {
		t.Fatal(err)
	}
	putPolicy(t, e2, lead.SessionID)
	res := summon(t, e2, loc2, "reviewer", func(r *SummonRequest) { r.Kind = "todo"; r.Asked = "review the Slice"; r.Harness = "codex" })
	if res.Action != "start" || !unattendedPrefix(term2.launches[0].Command) {
		t.Fatalf("the pair lead's reviewer: %+v %q", res, term2.launches)
	}
}

// epic --crew pair --op-id (s1 x s3 x s4): a first run whose op record fails
// after Rex created the session, and that died before the Epic's policy was
// written, is reconciled by its retry: the retry finds the session by its
// narrator and its implementer lead (no orchestrator in a pair Epic), starts
// nothing, reports the lead, and records the unattended policy the first run
// left out.
func TestPairEpicOpIDReconcilesItsSessionWithItsLeadAndPolicy(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	e.Crews = crew.Load
	req := EpicRequest{Name: "one slice", Dir: filepath.Dir(goal), Goal: goal, Crew: pairCrew, OpID: "me:board:pair-1#open"}
	repair := breakOpRecord(t, e, term, req.OpID)
	first, err := openWithin(e, loc, req, time.Second)
	if err == nil || !strings.Contains(err.Error(), "retry with the same op id") || first.SessionID != "session:epic-1" || first.Lead != implementerRole {
		t.Fatalf("first run %+v: %v", first, err)
	}
	repair()
	policy := filepath.Join(e.Paths.SessionDir("session:epic-1"), "policy.json")
	if err := os.Remove(policy); err != nil {
		t.Fatalf("setup: the first run wrote no policy to lose: %v", err)
	}
	for i, role := range []string{core.NarratorRole, implementerRole} {
		put(t, e, seed{Handle: core.Handle(fmt.Sprintf("claude-%02d", i+2)), Kind: core.Claude, Role: role, BlockID: fmt.Sprintf("block:epic-1-%d", i+1), SessionID: "session:epic-1", AdoptedAt: time.Now()})
	}
	again, err := openWithin(e, loc, req, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(term.sessions_) != 1 {
		t.Fatalf("the retry created another session: %+v", term.sessions_)
	}
	if !again.Replayed || !again.Ready() || again.Lead != implementerRole || again.Orchestrator.Block != "block:epic-1-2" || again.LifecycleState != store.EpicOpened {
		t.Fatalf("retry %+v", again)
	}
	p, ok, err := e.Store.SessionPolicy("session:epic-1")
	if err != nil || !ok || !p.Unattended || p.CreatedBy != "claude-01" || p.Server != fakeServer {
		t.Fatalf("policy after the retry %+v %v %v", p, ok, err)
	}
	if c, ok, _ := e.Store.SessionCrew("session:epic-1"); !ok || c.Crew != pairCrew {
		t.Errorf("crew after the retry %+v %v", c, ok)
	}
}

// A pair lead's hand-backs (s1 x s4) are numbered and digested, and the
// report its first hand-back stored while the Epic's lifecycle could not be
// written is backfilled into the lifecycle beside the one replacing it.
func TestPairEpicLeadHandBackIsNumberedAndBackfilled(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	now := time.Now()
	if err := e.Store.PutSessionCrew("session:pair", store.SessionCrew{Crew: pairCrew, ClaimedBy: "claude-01", ClaimedAt: now}); err != nil {
		t.Fatal(err)
	}
	put(t, e, seed{Handle: "claude-10", Kind: core.Claude, Role: implementerRole, SessionID: "session:pair", AdoptedAt: now})
	ctx := context.Background()
	lead := Location{Block: "block:claude-10"}
	lifecycle := filepath.Join(e.Paths.SessionDir("session:pair"), "lifecycle.json")
	if err := os.MkdirAll(lifecycle, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := e.RecordHandBack(ctx, lead, "## Review queue\n- PR #1")
	if err == nil || first.Seq != 1 || first.Digest != store.HandBackDigest("## Review queue\n- PR #1") {
		t.Fatalf("first, its lifecycle unwritable: %+v %v", first, err)
	}
	if err := os.Remove(lifecycle); err != nil {
		t.Fatal(err)
	}
	second, err := e.RecordHandBack(ctx, lead, "## Review queue\n- PR #1 merged")
	if err != nil || second.Seq != 2 || second.By != "claude-10" {
		t.Fatalf("second: %+v %v", second, err)
	}
	lc, ok, err := e.Store.EpicLifecycle("session:pair")
	if err != nil || !ok || lc.State != store.EpicHandBack || len(lc.HandBacks) != 2 || lc.HandBacks[0].Digest != first.Digest || lc.HandBacks[1].Digest != second.Digest {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
}
