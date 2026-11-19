package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// epicLead maps each crew to the role that leads its Epic: a pair Epic's
// implementer, a design Epic's architect, every other an orchestrator.
func TestDesignLeadRoleOfEachCrew(t *testing.T) {
	for c, want := range map[string]string{"pair": implementerRole, "design": architectRole, "dev": core.OrchestratorRole, "": core.OrchestratorRole} {
		if got := epicLead(c); got != want {
			t.Errorf("epicLead(%q) = %q, want %q", c, got, want)
		}
	}
}

// A design Epic opens like any other: its lead is an architect, recorded
// in the session's policy as lead_role when the Epic opens and reported as
// `lead` with its role in the result. The first architect nobody summoned
// adopted in the session (a real hook) is recorded as lead_handle; from
// then on that architect alone is the Epic's lead (sessionLead): it hears
// its grant, takes the narrator's control mail, reads its view and grant,
// records the hand-back, keeps the watcher, and hears departures. A council
// architect it summoned (Parent set) is refused all of it, and so is a
// second architect nobody summoned, adopted after the lead.
func TestDesignLead(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	e.Crews = crew.Load
	adoptPane(t, e, "block:epic-1-1", "agy-02", core.NarratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The lead's pane is adopted after the open: its record is written by
	// its first hook below. The open waits for it, so it times out on the
	// lead alone and still records the policy.
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	res, err := e.OpenEpic(short, loc, EpicRequest{Name: "design it", Dir: dir, Goal: goal, Crew: designCrew})
	cancelShort()
	if res.SessionID != "session:epic-1" || res.Lead.Role != architectRole || res.Lead.Block != "block:epic-1-2" {
		t.Fatalf("result %+v (%v)", res, err)
	}
	panes := term.sessions_[0].panes
	if panes[1].Label != "architect | design it" {
		t.Fatalf("the lead's pane %q", panes[1].Label)
	}
	p, ok, err := e.Store.SessionPolicy(res.SessionID)
	if err != nil || !ok || p.LeadRole != architectRole || p.LeadHandle != "" || p.Crew != designCrew {
		t.Fatalf("policy at open %+v %v %v", p, ok, err)
	}

	// The lead's first hook: a real adoption in the lead's pane.
	e.AMQ = &amq.Fake{}
	term.mu.Lock()
	term.label = panes[1].Label
	term.windows["block:epic-1-2"] = "window:epic-1-2"
	term.mu.Unlock()
	if _, err := e.Hook(ctx, EventSessionStart, core.Claude, Location{Block: "block:epic-1-2"}, HookInput{SessionID: "lead-session", AgentPID: term.fgPID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: "block:epic-1-2"}, HookInput{}) })
	term.mu.Lock()
	term.label = ""
	term.mu.Unlock()
	lead, ok, err := e.agentAt("block:epic-1-2")
	if err != nil || !ok || lead.Role != architectRole || lead.Parent != "" {
		t.Fatalf("the lead's record %+v %v %v", lead, ok, err)
	}
	if p, _, _ := e.Store.SessionPolicy(res.SessionID); p.LeadHandle != lead.Handle || p.LeadRole != architectRole {
		t.Fatalf("policy after the lead's adoption %+v, want lead_handle %s", p, lead.Handle)
	}

	// A council architect the lead summoned, and a second architect nobody
	// summoned adopted after the lead, beside a worker.
	now := time.Now()
	put(t, e,
		seed{Handle: "codex-04", Kind: core.Codex, Role: architectRole, SessionID: res.SessionID, Parent: lead.Handle, Depth: 1, AdoptedAt: now, AgentPID: os.Getpid()},
		seed{Handle: "agy-05", Kind: core.Agy, Role: architectRole, SessionID: res.SessionID, AdoptedAt: now, AgentPID: os.Getpid()},
		seed{Handle: "pi-06", Kind: core.Pi, Role: "reviewer", SessionID: res.SessionID, AdoptedAt: now, AgentPID: os.Getpid()},
	)
	// A second adoption of the lead's role never moves lead_handle.
	if err := e.claimLead(recordOf(t, e, "agy-05")); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := e.Store.SessionPolicy(res.SessionID); p.LeadHandle != lead.Handle {
		t.Fatalf("lead_handle moved to %s", p.LeadHandle)
	}

	for _, c := range []struct {
		who  core.Handle
		lead bool
	}{{lead.Handle, true}, {"codex-04", false}, {"agy-05", false}, {"pi-06", false}, {"agy-02", false}} {
		rec := recordOf(t, e, c.who)
		got, err := e.sessionLead(rec)
		if err != nil || got != c.lead {
			t.Errorf("sessionLead(%s) = %v %v, want %v", c.who, got, err, c.lead)
		}
		ep, err := e.gatewayEndpoint(rec)
		if err != nil || ep.Lead != c.lead {
			t.Errorf("%s gateway Lead = %v %v", c.who, ep.Lead, err)
		}
		at := Location{Block: rec.BlockID}
		_, err = e.RecordHandBack(ctx, at, "## Design\nspec drafted")
		if c.lead != (err == nil) || !c.lead && !errors.Is(err, ErrRefused) {
			t.Errorf("%s recording a hand-back: %v", c.who, err)
		}
		if c.who == "codex-04" && (err == nil || !strings.Contains(err.Error(), "summoned by "+string(lead.Handle))) {
			t.Errorf("the council architect's refusal does not say it was summoned: %v", err)
		}
		if _, err := e.PreviewHandBack(at, "x"); c.lead != (err == nil) {
			t.Errorf("%s previewing a hand-back: %v", c.who, err)
		}
		if _, err := e.ReadGrant(at, ""); c.lead != (err == nil) {
			t.Errorf("%s reading the grant: %v", c.who, err)
		}
		if c.who != "agy-02" {
			if _, err := e.viewReader(ctx, at, ""); c.lead != (err == nil) {
				t.Errorf("%s reading the view: %v", c.who, err)
			}
		}
		if g := e.grantNotice(rec); c.lead != strings.Contains(g, "Grant: "+e.Paths.GrantPath(res.SessionID)) {
			t.Errorf("%s grant notice %q", c.who, g)
		}
	}

	// The narrator's control mail reaches the lead alone.
	narr, err := e.endpointOf("agy-02")
	if err != nil {
		t.Fatal(err)
	}
	control := core.Operation{Kind: core.OpControl, ControlRequestID: "req-design-1"}
	for _, h := range []core.Handle{lead.Handle, "codex-04", "agy-05"} {
		to, err := e.endpointOf(h)
		if err != nil {
			t.Fatal(err)
		}
		if d := core.CanExchange(narr, to, control, nil); d.Allow != (h == lead.Handle) {
			t.Errorf("narrator control to %s: %+v", h, d)
		}
	}

	// The lead keeps the session's watcher and runs its watchdog; a council
	// architect does neither.
	rule, err := e.leadRuleOf(core.SessionKey{Server: fakeServer, SessionID: res.SessionID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !watches(recordOf(t, e, lead.Handle), rule) || watches(recordOf(t, e, "codex-04"), rule) {
		t.Error("the watcher is kept by someone other than the lead")
	}

	// Departures: a worker's goes to the lead; the lead's goes to one peer
	// to report that the goal stopped, never as if the lead heard it.
	others := []core.Peer{}
	for _, h := range []core.Handle{lead.Handle, "codex-04", "pi-06"} {
		pp := peerOf(recordOf(t, e, h))
		pp.Lead = rule.holds(recordOf(t, e, h))
		others = append(others, pp)
	}
	worker := peerOf(recordOf(t, e, "agy-05"))
	if to, _, _ := core.DepartureMail(worker, "", others); len(to) != 1 || to[0] != lead.Handle {
		t.Errorf("a worker's departure went to %v, want the lead %s", to, lead.Handle)
	}
	left := others[0]
	if to, _, body := core.DepartureMail(left, "", others[1:]); len(to) != 1 || to[0] == lead.Handle || !strings.Contains(body, "do not take over") {
		t.Errorf("the lead's departure went to %v: %q", to, body)
	}
}

// A session whose policy predates lead_role (or that no Epic opened) keeps
// the rule the adapter used then: its orchestrator leads, and in a pair
// crew with no orchestrator, live or departed, its implementer; a Hub
// session is led by nobody.
func TestDesignLeadLegacyFallback(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	now := time.Now()
	// A policy written before policies had a kind or a lead role.
	writeFile(t, filepath.Join(e.Paths.SessionDir("session:old"), "policy.json"), `{"session_id":"session:old","unattended":true}`)
	for session, c := range map[string]string{"session:pair": pairCrew, "session:had-orch": pairCrew} {
		if err := e.Store.PutSessionCrew(session, store.SessionCrew{Crew: c, ClaimedBy: "claude-01", ClaimedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	putHub(t, e, "session:hub", core.HubKey("hub:/x"))
	put(t, e,
		seed{Handle: "claude-01", Role: core.OrchestratorRole, SessionID: "session:old", AdoptedAt: now},
		seed{Handle: "claude-02", Role: core.OrchestratorRole, SessionID: "session:old", Parent: "claude-01", Depth: 1, AdoptedAt: now},
		seed{Handle: "claude-10", Role: implementerRole, SessionID: "session:pair", AdoptedAt: now},
		seed{Handle: "claude-20", Role: implementerRole, SessionID: "session:had-orch", AdoptedAt: now},
		seed{Handle: "claude-21", Role: core.OrchestratorRole, SessionID: "session:had-orch", AdoptedAt: now, DepartedAt: &now},
		seed{Handle: "claude-30", Role: core.OrchestratorRole, SessionID: "session:hub", AdoptedAt: now},
	)
	for h, want := range map[core.Handle]bool{"claude-01": true, "claude-02": false, "claude-10": true, "claude-20": false, "claude-30": false} {
		if got, err := e.sessionLead(recordOf(t, e, h)); err != nil || got != want {
			t.Errorf("sessionLead(%s) = %v %v, want %v", h, got, err, want)
		}
	}
}

// A design Epic's lead is briefed read-only, never to run a goal: no
// `/goal`, no run-goal, whatever its harness. The brief chairs the design
// crew's deliberation, convenes the council of architects in the Epic's
// directory under the notarized grant, drafts the spec and goal files
// outside the repository, lints each goal file, and hands them back as
// artifacts. Every other crew's lead still runs the goal contract.
func TestDesignBrief(t *testing.T) {
	for _, harness := range []string{"", "codex"} {
		t.Run("harness="+harness, func(t *testing.T) {
			e, term, loc, goal := openEpicWorld(t)
			dir := filepath.Dir(goal)
			e.Crews = crew.Load
			adoptPane(t, e, "block:epic-1-1", "agy-02", core.NarratorRole)
			adoptPane(t, e, "block:epic-1-2", "claude-03", architectRole)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "design it", Dir: dir, Goal: goal, Crew: designCrew, Harness: harness, Asked: "design the cache"}); err != nil {
				t.Fatal(err)
			}
			cmd := term.sessions_[0].panes[1].Command
			brief := cmd[len(cmd)-1]
			for _, forbidden := range []string{"/goal", "run-goal", "Execute the goal contract"} {
				if strings.Contains(brief, forbidden) {
					t.Errorf("the design brief contains %q: %q", forbidden, brief)
				}
			}
			for _, want := range []string{
				"read-only",
				"design crew's conventions",
				"rex-amq-adapter agent summon architect --council --cwd " + dir + " --grant ",
				"--kind brainstorm",
				"`spec.md`", "`NN-<slug>.md`", "never in the repository",
				"lint_goal.py",
				"rex-amq-adapter epic handback @",
				"--artifact spec.md=@",
				goal,
			} {
				if !strings.Contains(brief, want) {
					t.Errorf("the design brief lacks %q: %q", want, brief)
				}
			}
		})
	}
	// Without a contract the brief carries the user's words.
	if b := designBrief(epicGrant{asked: "pick a queue"}, "/repo"); !strings.Contains(b, "pick a queue") || strings.Contains(b, "/goal") {
		t.Errorf("brief on the user's words: %q", b)
	}
	// Every other crew's lead still runs the goal on a claude harness.
	if b := orchestratorBrief(core.Claude, epicGrant{goal: "/g.md"}, "dev", "/repo"); !strings.HasPrefix(b, "/goal Load skill run-goal") {
		t.Errorf("a dev lead's brief: %q", b)
	}
}
