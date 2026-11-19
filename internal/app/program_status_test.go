package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
)

// agyTerm shows block as an agy pane: status st (working once a doorbell is
// written) and screen vt. Every other block is fakeTerm's.
type agyTerm struct {
	*fakeTerm
	block    string
	st       rex.Status
	vt       string
	captures int
}

func (a *agyTerm) ProgramStatus(ctx context.Context, block string) (rex.Status, bool, error) {
	if block != a.block {
		return a.fakeTerm.ProgramStatus(ctx, block)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.writes) > 0 {
		return rex.Status{State: "working", App: "antigravity"}, true, nil
	}
	return a.st, true, nil
}

func (a *agyTerm) CaptureVT(ctx context.Context, block string) (string, error) {
	if block != a.block {
		return a.fakeTerm.CaptureVT(ctx, block)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.captures++
	return a.vt, nil
}

func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "screen", "testdata", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var approvePlan = rex.Status{State: "blocked", Kind: "permission", App: "antigravity", EffectiveApp: "antigravity", Msg: "Approve plan"}

// The full-screen /artifact viewer: agy's review, with no ruled > box.
const agyArtifactViewer = "  implementation_plan.md\n\n  # Plan\n  1. Fix the banner\n\n  a approve · c comment · esc close\n"

// A trust dialog, with no box under it.
const agyTrust = "  Trust this folder?\n  /Users/x/project\n\n  > Yes, trust this folder\n    No, exit\n"

type statusCase struct {
	name string
	kind core.Kind
	st   rex.Status
	vt   func(t *testing.T) string
}

// The passive banner: agy's "Approve plan" over an empty ruled > box and no
// trust dialog. Each fixture of agy's idle box counts.
func passiveCases() []statusCase {
	var out []statusCase
	for _, f := range []string{"rex-agy-empty", "rex-agy-auto-mode", "rex-agy-plan-mode"} {
		out = append(out, statusCase{name: f, kind: core.Agy, st: approvePlan, vt: func(t *testing.T) string { return agyFixture(t, f) }})
	}
	return out
}

// Prompts that wait on the human, and statuses of other harnesses: each
// stays blocked.
func blockedCases() []statusCase {
	empty := func(t *testing.T) string { return agyFixture(t, "rex-agy-empty") }
	with := func(f func(*rex.Status)) rex.Status { s := approvePlan; f(&s); return s }
	return []statusCase{
		{"artifact viewer open", core.Agy, approvePlan, func(*testing.T) string { return agyArtifactViewer }},
		{"draft in the box", core.Agy, approvePlan, func(t *testing.T) string { return agyFixture(t, "rex-agy-draft") }},
		{"trust screen", core.Agy, approvePlan, func(*testing.T) string { return agyTrust }},
		{"tool permission", core.Agy, with(func(s *rex.Status) { s.Msg = "Approve bash: rm -rf" }), empty},
		{"question", core.Agy, with(func(s *rex.Status) { s.Kind, s.Msg = "question", "Answer question" }), empty},
		{"subagent action", core.Agy, with(func(s *rex.Status) { s.Msg = "Approve subagent action" }), empty},
		{"claude", core.Claude, rex.Status{State: "blocked", Kind: "permission", Msg: "Approve plan"}, empty},
		{"codex", core.Codex, rex.Status{State: "blocked", Kind: "permission", Msg: "Approve plan"}, empty},
		{"pi", core.Pi, rex.Status{State: "blocked", Kind: "permission", Msg: "Approve plan"}, empty},
	}
}

func TestStatusFixturesShowTheBoxTheyClaim(t *testing.T) {
	for _, c := range passiveCases() {
		if got := screen.Check("agy", c.vt(t)); got != screen.Empty {
			t.Errorf("%s: box %v, want empty", c.name, got)
		}
	}
	if got := screen.Check("agy", agyArtifactViewer); got != screen.Unknown {
		t.Errorf("artifact viewer: box %v, want unknown", got)
	}
	if _, trust := screen.TrustScreen("agy", agyTrust); !trust {
		t.Error("trust fixture: no trust screen")
	}
}

func TestOnlyAgysPassiveApprovePlanIsIdle(t *testing.T) {
	for _, c := range passiveCases() {
		got := normalizeProgramStatus(c.kind, c.st, c.vt(t))
		if got.State != "idle" || got.Kind != "" || got.Msg != "" || got.App != "antigravity" {
			t.Errorf("%s: %+v, want idle", c.name, got)
		}
	}
	// agy names itself in the status before adoption (kind "").
	for _, st := range []rex.Status{approvePlan, {State: "blocked", Kind: "permission", App: "antigravity", Msg: "Approve plan"}, {State: "blocked", Kind: "permission", EffectiveApp: "antigravity", Msg: "Approve plan"}} {
		if got := normalizeProgramStatus("", st, agyFixture(t, "rex-agy-empty")); got.State != "idle" {
			t.Errorf("unadopted %+v: %+v, want idle", st, got)
		}
	}
	for _, c := range blockedCases() {
		if got := normalizeProgramStatus(c.kind, c.st, c.vt(t)); got != c.st {
			t.Errorf("%s: %+v, want unchanged", c.name, got)
		}
	}
	if got := normalizeProgramStatus("", rex.Status{State: "blocked", Kind: "permission", Msg: "Approve plan"}, agyFixture(t, "rex-agy-empty")); got.State != "blocked" {
		t.Errorf("unadopted, not agy: %+v", got)
	}
}

// agyEnv is fixture's env with its agent ("codex", in block) of kind kind, on
// an agyTerm showing st over vt.
func agyEnv(t *testing.T, kind core.Kind, st rex.Status, vt string, unread ...string) (Env, *agyTerm) {
	t.Helper()
	term := &agyTerm{fakeTerm: &fakeTerm{submit: true}, block: block, st: st, vt: vt}
	e := fixture(t, term.fakeTerm, unread...)
	e.Rex, e.RexOn = term, func(string) Terminal { return term }
	if err := putSeed(e.Store, seed{Handle: "codex", Kind: kind, Server: "unix:///fake.sock", BlockID: block, AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	return e, term
}

func TestADoorbellReachesAgyUnderAPassiveApprovePlan(t *testing.T) {
	for _, c := range passiveCases() {
		e, term := agyEnv(t, c.kind, c.st, c.vt(t), "m1")
		if o := inject(e); o.Progress != core.Accepted || len(term.writes) != 1 {
			t.Errorf("%s: %+v, writes %d", c.name, o, len(term.writes))
		}
	}
	for _, c := range blockedCases() {
		e, term := agyEnv(t, c.kind, c.st, c.vt(t), "m1")
		if o := inject(e); o.Progress != core.Deferred || o.Code != "agent_blocked" || len(term.writes) != 0 {
			t.Errorf("%s: %+v, writes %d", c.name, o, len(term.writes))
		}
	}
}

func TestAPassiveApprovePlanIsNoBlockedEpisode(t *testing.T) {
	run := func(c statusCase) (lines []string, desktop []string, orchInbox int) {
		e, term, orch, s, now, dt := healthWorld(t)
		a := &agyTerm{fakeTerm: term, block: block, st: c.st, vt: c.vt(t)}
		e.Rex, e.RexOn = a, func(string) Terminal { return a }
		r := seedOf(recordNamed(t, e, "codex"))
		r.Kind = c.kind
		rec := plant(t, e, r)
		if err := e.blocked(context.Background(), orch, s, rec); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(core.StuckFor + time.Second)
		if err := e.blocked(context.Background(), orch, s, rec); err != nil {
			t.Fatal(err)
		}
		return blockedLines(t, e), *dt, len(inboxOf(t, e, "orch-01"))
	}
	for _, c := range passiveCases() {
		if lines, desktop, n := run(c); len(lines) != 0 || len(desktop) != 0 || n != 0 {
			t.Errorf("%s: blocked.log %v, desktop %v, orchestrator notices %d", c.name, lines, desktop, n)
		}
	}
	for _, c := range blockedCases() {
		if lines, _, n := run(c); len(lines) != 1 || n != 1 {
			t.Errorf("%s: blocked.log %v, orchestrator notices %d, want one each", c.name, lines, n)
		}
	}
}

func TestObservedActivityOfAPassiveApprovePlanIsIdle(t *testing.T) {
	look := func(c statusCase) (core.AgentState, int) {
		e, term := agyEnv(t, c.kind, c.st, c.vt(t))
		a := e.observeActivity(context.Background(), []agent{recordNamed(t, e, "codex")})["codex"]
		if a == nil || a.Err {
			t.Fatalf("%s: %+v", c.name, a)
		}
		return a.State, term.captures
	}
	for _, c := range passiveCases() {
		if st, _ := look(c); st != core.StateIdle {
			t.Errorf("%s: %s, want idle", c.name, st)
		}
	}
	for _, c := range blockedCases() {
		if st, _ := look(c); st != core.StateBlocked {
			t.Errorf("%s: %s, want blocked", c.name, st)
		}
	}
	// A status that is not agy's Approve plan costs no capture.
	for _, st := range []rex.Status{{State: "idle"}, {State: "working", App: "antigravity"}, {State: "blocked", Kind: "permission", App: "antigravity", Msg: "Approve bash: ls"}} {
		if _, n := look(statusCase{name: st.Msg, kind: core.Agy, st: st, vt: func(*testing.T) string { return "" }}); n != 0 {
			t.Errorf("%+v: %d captures, want 0", st, n)
		}
	}
}

func TestAPassiveApprovePlanIsAtRestForClose(t *testing.T) {
	for _, c := range passiveCases() {
		e, term := agyEnv(t, c.kind, c.st, c.vt(t))
		if why, err := e.unrest(context.Background(), term, recordNamed(t, e, "codex")); err != nil || why != "" {
			t.Errorf("%s: %q %v, want at rest", c.name, why, err)
		}
	}
	for _, c := range blockedCases() {
		e, term := agyEnv(t, c.kind, c.st, c.vt(t))
		if why, err := e.unrest(context.Background(), term, recordNamed(t, e, "codex")); err != nil || !strings.HasSuffix(why, ": blocked") {
			t.Errorf("%s: %q %v, want blocked", c.name, why, err)
		}
	}
	// Any other status is judged without a capture.
	e, term := agyEnv(t, core.Agy, rex.Status{State: "working"}, "")
	if why, _ := e.unrest(context.Background(), term, recordNamed(t, e, "codex")); why != "codex: working" || term.captures != 0 {
		t.Errorf("working: %q, %d captures", why, term.captures)
	}
}

// Before adoption the kind is unknown: agy's status names the app.
func TestAStartingAgyUnderAPassiveApprovePlanIsNotBlocked(t *testing.T) {
	observe := func(st rex.Status, vt string) core.StartupObservation {
		e, _ := agyEnv(t, core.Agy, st, vt)
		if err := unplant(e.Store, "codex"); err != nil {
			t.Fatal(err)
		}
		o, err := e.observeStartup(context.Background(), block)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for _, c := range passiveCases() {
		if o := observe(c.st, c.vt(t)); o.State != core.StateIdle || o.BlockedKind != "" {
			t.Errorf("%s: %+v, want idle", c.name, o)
		}
	}
	for _, c := range blockedCases() {
		st := c.st
		if c.kind != core.Agy {
			st.App, st.EffectiveApp = "", "" // another harness does not name itself antigravity
		}
		if o := observe(st, c.vt(t)); o.State != core.StateBlocked {
			t.Errorf("%s: %+v, want blocked", c.name, o)
		}
	}
}

// The watchdog's stuck-mail look sees the passive banner as idle, so its
// recovery is not skipped as the blocked look's to report.
func TestTheWatchdogLooksPastAPassiveApprovePlan(t *testing.T) {
	look := func(c statusCase) core.AgentState {
		e, _ := agyEnv(t, c.kind, c.st, c.vt(t))
		history := []core.Attempt{
			{At: e.Now().Add(-2 * core.StuckFor), Outcome: core.Outcome{Progress: core.Deferred, Code: "agent_blocked"}},
			{At: e.Now().Add(-time.Second), Outcome: core.Outcome{Progress: core.Deferred, Code: "agent_blocked"}},
		}
		l, stuck, err := e.observe(context.Background(), recordNamed(t, e, "codex"), 1, history)
		if err != nil || !stuck {
			t.Fatalf("%s: stuck %v, %v", c.name, stuck, err)
		}
		return l.status
	}
	for _, c := range passiveCases() {
		if st := look(c); st != core.StateIdle {
			t.Errorf("%s: %s, want idle", c.name, st)
		}
	}
	for _, c := range blockedCases() {
		if st := look(c); st != core.StateBlocked {
			t.Errorf("%s: %s, want blocked", c.name, st)
		}
	}
}
