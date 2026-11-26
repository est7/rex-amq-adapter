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

// A passive banner with a user draft in the input box: still at prompt with a
// draft (draft_in_box), never a blocked permission prompt.
func passiveDraftCase() statusCase {
	return statusCase{
		name: "draft in the box under passive Approve plan",
		kind: core.Agy,
		st:   approvePlan,
		vt:   func(t *testing.T) string { return agyFixture(t, "rex-agy-draft") },
	}
}

// Prompts that wait on the human, and statuses of other harnesses: each
// stays blocked.
func blockedCases() []statusCase {
	empty := func(t *testing.T) string { return agyFixture(t, "rex-agy-empty") }
	with := func(f func(*rex.Status)) rex.Status { s := approvePlan; f(&s); return s }
	return []statusCase{
		{"artifact viewer open", core.Agy, approvePlan, func(*testing.T) string { return agyArtifactViewer }},
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
		if got := screen.Inspect("agy", c.vt(t)).Box; got != screen.BoxEmpty {
			t.Errorf("%s: box %v, want empty", c.name, got)
		}
	}
	if got := screen.Inspect("agy", passiveDraftCase().vt(t)).Box; got != screen.BoxDraft {
		t.Errorf("draft fixture: box %v, want draft", got)
	}
	if got := screen.Inspect("agy", agyArtifactViewer).Box; got != screen.BoxMissing {
		t.Errorf("artifact viewer: box %v, want missing", got)
	}
	if got := screen.Inspect("agy", agyTrust).TrustPhrase; got == "" {
		t.Error("trust fixture: no trust screen")
	}
}

func TestOnlyAgysPassiveApprovePlanIsAtPrompt(t *testing.T) {
	assess := func(kind core.Kind, st rex.Status, vt string) core.AgentActivity {
		sig := core.ExternalSignal{
			Source: core.SignalRexStatus, State: core.AgentState(st.State),
			Kind: st.Kind, App: st.App, EffectiveApp: st.EffectiveApp, Msg: st.Msg,
		}
		profile := core.ProfileForSignal(kind, sig)
		surf := screen.Inspect(string(profile.Kind), vt)
		return core.AssessActivity(profile, core.ActivityEvidence{External: sig, Surface: &surf})
	}
	for _, c := range passiveCases() {
		got := assess(c.kind, c.st, c.vt(t))
		if ap, ok := got.(core.AtPrompt); !ok || ap.Box != screen.BoxEmpty {
			t.Errorf("%s: %#v, want AtPrompt(empty)", c.name, got)
		}
	}
	draft := passiveDraftCase()
	wantDraft := core.AtPrompt{Box: screen.BoxDraft, Source: core.SignalRexStatus, RawState: core.StateIdle}
	if got := assess(draft.kind, draft.st, draft.vt(t)); got != wantDraft {
		t.Errorf("passive banner + draft: %#v, want AtPrompt(draft)", got)
	}
	// agy names itself in the status before adoption (kind "").
	wantEmpty := core.AtPrompt{Box: screen.BoxEmpty, Source: core.SignalRexStatus, RawState: core.StateIdle}
	for _, st := range []rex.Status{approvePlan, {State: "blocked", Kind: "permission", App: "antigravity", Msg: "Approve plan"}, {State: "blocked", Kind: "permission", EffectiveApp: "antigravity", Msg: "Approve plan"}} {
		if got := assess("", st, agyFixture(t, "rex-agy-empty")); got != wantEmpty {
			t.Errorf("unadopted %+v: %#v, want AtPrompt(empty)", st, got)
		}
	}
	for _, c := range blockedCases() {
		got := assess(c.kind, c.st, c.vt(t))
		if _, ok := got.(core.AwaitingUser); !ok {
			t.Errorf("%s: %#v, want AwaitingUser", c.name, got)
		}
	}
	gotOther := assess("", rex.Status{State: "blocked", Kind: "permission", Msg: "Approve plan"}, agyFixture(t, "rex-agy-empty"))
	if _, ok := gotOther.(core.AwaitingUser); !ok {
		t.Errorf("unadopted, not agy: %#v, want AwaitingUser", gotOther)
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
	// Consistency fix (a): a user draft under agy's passive Approve plan defers
	// with draft_in_box rather than agent_blocked.
	draft := passiveDraftCase()
	e, term := agyEnv(t, draft.kind, draft.st, draft.vt(t), "m1")
	if o := inject(e); o.Progress != core.Deferred || o.Code != core.CodeDraftInBox || len(term.writes) != 0 {
		t.Errorf("%s: %+v, writes %d; want deferred draft_in_box", draft.name, o, len(term.writes))
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
	for _, c := range append(passiveCases(), passiveDraftCase()) {
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
	for _, c := range append(passiveCases(), passiveDraftCase()) {
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
	for _, c := range append(passiveCases(), passiveDraftCase()) {
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
	for _, c := range append(passiveCases(), passiveDraftCase()) {
		o := observe(c.st, c.vt(t))
		if _, ok := o.Activity.(core.AtPrompt); !ok {
			t.Errorf("%s: %#v, want AtPrompt", c.name, o.Activity)
		}
	}
	for _, c := range blockedCases() {
		st := c.st
		if c.kind != core.Agy {
			st.App, st.EffectiveApp = "", "" // another harness does not name itself antigravity
		}
		o := observe(st, c.vt(t))
		if _, ok := o.Activity.(core.AwaitingUser); !ok {
			t.Errorf("%s: %#v, want AwaitingUser", c.name, o.Activity)
		}
	}
}

// The watchdog's stuck-mail look sees the passive banner as AtPrompt (Empty or
// Draft), so its recovery/draft reporting is not skipped as a blocked dialog.
func TestTheWatchdogLooksPastAPassiveApprovePlan(t *testing.T) {
	look := func(c statusCase) core.AgentActivity {
		e, _ := agyEnv(t, c.kind, c.st, c.vt(t))
		history := []core.Attempt{
			{At: e.Now().Add(-2 * core.StuckFor), Outcome: core.Outcome{Progress: core.Deferred, Code: "agent_blocked"}},
			{At: e.Now().Add(-time.Second), Outcome: core.Outcome{Progress: core.Deferred, Code: "agent_blocked"}},
		}
		l, stuck, err := e.observe(context.Background(), recordNamed(t, e, "codex"), 1, history)
		if err != nil || !stuck {
			t.Fatalf("%s: stuck %v, %v", c.name, stuck, err)
		}
		return l.activity
	}
	for _, c := range passiveCases() {
		if ap, ok := look(c).(core.AtPrompt); !ok || ap.Box != screen.BoxEmpty {
			t.Errorf("%s: %#v, want AtPrompt(empty)", c.name, look(c))
		}
	}
	draft := passiveDraftCase()
	if ap, ok := look(draft).(core.AtPrompt); !ok || ap.Box != screen.BoxDraft {
		t.Errorf("%s: %#v, want AtPrompt(draft)", draft.name, look(draft))
	}
	for _, c := range blockedCases() {
		act := look(c)
		if _, ok := act.(core.AwaitingUser); !ok {
			t.Errorf("%s: %#v, want AwaitingUser", c.name, act)
		}
	}
}

// Consistency fix (b): health() and watchdog recover() go through the unified
// activity boundary and honor Claude Code's background-job supervisor status.
func TestHealthAndWatchdogRecoverHonorClaudeJobSupervisor(t *testing.T) {
	t.Run("health sees working Claude background job when terminal reports no OSC status", func(t *testing.T) {
		e, term, orch, s, now, _ := healthWorld(t)
		e.ClaudeDir = t.TempDir()
		rec := claude04(t, e)
		// Terminal client shows no OSC status, while Claude's supervisor says working.
		term.states = map[string]rex.Status{"block:c": {}}
		writeJSON(t, filepath.Join(e.ClaudeDir, "daemon", "roster.json"), map[string]any{
			"workers": map[string]any{"job1": map[string]string{"sessionId": rec.Session()}},
		})
		writeJSON(t, filepath.Join(e.ClaudeDir, "jobs", "job1", "state.json"), map[string]string{"name": "bg", "state": "working"})

		p := filepath.Join(e.TranscriptHome, ".claude/projects/-Users-x--app/sid-c4.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(claudeOK, "01:58:00", "01:30:00", 1)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-20 * time.Minute)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if err := e.health(context.Background(), orch, s, rec); err != nil {
			t.Fatal(err)
		}
		inbox := inboxOf(t, e, "orch-01")
		if len(inbox) != 1 || !strings.Contains(mailBody(t, e, "orch-01", inbox[0]), core.NoticeStalled) {
			t.Fatalf("expected stalled notice for working Claude background job, got inbox %v", inbox)
		}
	})

	t.Run("watchdog recover aborts when Claude background job supervisor reports working", func(t *testing.T) {
		w := newJobWorld(t)
		w.jobHook(t, EventSessionStart)
		rec, ok, err := w.e.liveRecordOf(core.Claude, jobSess)
		if err != nil || !ok {
			t.Fatalf("live job record: %v %v", ok, err)
		}
		writeJSON(t, filepath.Join(w.e.ClaudeDir, "jobs", "a1b2c3d4", "state.json"), map[string]string{"name": jobName, "state": "working"})
		w.term.state = "" // terminal client reports no OSC status
		w.term.rows = map[string]int{rec.BlockID: 3}
		w.term.owner = map[string]string{rec.BlockID: "client:app"}
		orch := w.tuiRecord(t)
		s, err := w.e.FindSession(context.Background(), rec.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		ep := core.Episode{ID: "ep-1", Handle: rec.Handle, Block: rec.BlockID, PID: rec.pid(), Session: s.id}
		v, err := w.e.recover(context.Background(), orch, rec, &ep, s, look{window: w.term.windows[rec.BlockID]})
		if err != nil || v != notReady || len(w.term.detaches) != 0 {
			t.Fatalf("recover on working Claude job: v=%v err=%v detaches=%v, want abort with 0 detaches", v, err, w.term.detaches)
		}
	})
}
