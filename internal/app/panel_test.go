package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// panelT0 is when the world's attention marks were set.
var panelT0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// panelWorld is an Epic, session:s labeled auth-refresh, working as crew
// pair: its orchestrator claude-01 (agent process 701), implementer
// claude-02 (702, whose last turn ended in an error), and narrator pi-04
// (703, stalled), with a stored view, lifecycle, and hand-back; and the
// personal agent pi-05 (704) in session:p, linked to it. block:human is a
// pane of session:s with no agent. The test process descends from no agent.
func panelWorld(t *testing.T) (Env, *fakeTerm) {
	t.Helper()
	term := &fakeTerm{state: "idle", sessionLabel: "auth-refresh",
		windows:  map[string]string{"block:claude-01": "window:1", "block:claude-02": "window:1", "block:pi-04": "window:2", "block:human": "window:2", "block:pi-05": "window:9"},
		sessions: map[string]string{"window:9": "session:p"}}
	paths := store.Paths{Home: t.TempDir()}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: term, RexOn: func(string) Terminal { return term }, SelfBin: "/x/rex-amq-adapter", Now: time.Now,
		Crews:     func(string) crew.Library { return crew.Builtin() },
		Ancestors: func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 9000}}, nil }}
	for i, r := range []store.Record{
		{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:s", WindowID: "window:1"},
		{Handle: "claude-02", Kind: core.Claude, Role: "implementer", SessionID: "session:s", WindowID: "window:1"},
		{Handle: "pi-04", Kind: core.Pi, Role: core.NarratorRole, SessionID: "session:s", WindowID: "window:2"},
		{Handle: "pi-05", Kind: core.Pi, Role: core.PersonalRole, SessionID: "session:p", WindowID: "window:9"},
	} {
		r.AgentPID, r.AgentSessionID, r.AdoptedAt = 701+i, "harness-"+string(r.Handle), panelT0
		put(t, e, r)
	}
	must(t, e.Store.PutSessionCrew("session:s", store.SessionCrew{Crew: "pair", ClaimedBy: "claude-01", ClaimedAt: panelT0}))
	must(t, e.Store.PutTurnError("block:claude-02", store.TurnError{At: panelT0, By: "claude-02/harness-claude-02"}))
	must(t, e.Store.PutHealthMark("block:pi-04", store.HealthMark{LastStallAt: panelT0}))
	must(t, e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:s", Server: fakeServer, Name: "auth-refresh", State: store.EpicOpened, PersonalHandle: "pi-05", UpdatedAt: panelT0}))
	must(t, e.Store.PutHandBack("session:s", store.HandBack{By: "claude-01", At: panelT0, Body: "## Review queue\nPR #3", Seq: 1}))
	must(t, e.Store.PutView("session:s", panelStoredView(panelT0, "quiet", 5)))
	must(t, e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:s"}}))
	mail(t, e.Paths.Root(), "claude-02", "inbox/new", "2026-11-16T09-00-00.000Z_m1", "claude-01", "build it")
	return e, term
}

// panelStoredView is session:s's view.json as its narrator writes it at at,
// with narrative text and one open obligation age seconds old.
func panelStoredView(at time.Time, text string, age int64) SessionView {
	return SessionView{Schema: ViewSchema, AsOf: ViewCursor{At: at, Pulse: "pulse-" + at.Format(time.RFC3339Nano)},
		Session: ViewSession{Server: fakeServer, SessionID: "session:s", Epic: "auth-refresh"}, Narrator: "pi-04",
		Narrative: &ViewNarrative{Text: text, At: panelT0},
		Members:   []ViewMember{{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, Live: true}},
		Obligations: []ViewObligation{{Ref: "o1", Thread: "p2p/x", Kind: "request", Subject: "build it", OwedBy: "claude-02", OwedTo: "claude-01",
			State: "open", AgeSeconds: age, Provenance: "adapter"}},
		Activity: []ViewMessage{}, ControlPlane: []ViewMessage{}, CoverageGaps: []CoverageGap{}, Sources: []string{"records"}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func panelEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// startPanel streams the panel of session (the caller's when "") for the
// caller whose environment vars is.
func startPanel(t *testing.T, e Env, vars map[string]string, session string) (*syncBuffer, func()) {
	t.Helper()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.Panel(ctx, out, panelEnv(vars), session, StreamTicks{Local: 15 * time.Millisecond, Rex: 30 * time.Millisecond, Heartbeat: 100 * time.Millisecond})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("panel: %v", err)
		}
	}
	t.Cleanup(stop)
	return out, stop
}

var human = map[string]string{"REX_BLOCK": "block:human"}

func panelOf(t *testing.T, f map[string]any) map[string]any {
	t.Helper()
	p, ok := f["panel"].(map[string]any)
	if !ok {
		t.Fatalf("no panel in %v", f)
	}
	return p
}

func panelUpserts(t *testing.T, out *syncBuffer) []map[string]any {
	var ups []map[string]any
	for _, f := range out.frames(t) {
		if f["type"] == "upsert" && f["entity"] == "panel" {
			ups = append(ups, f)
		}
	}
	return ups
}

// settle waits past several local and Rex ticks.
func settle() { time.Sleep(150 * time.Millisecond) }

// expectOneUpsert applies change and asserts that exactly one panel upsert
// follows, keyed by session and naming field as the one that changed.
func expectOneUpsert(t *testing.T, out *syncBuffer, session, field string, change func()) map[string]any {
	t.Helper()
	before := len(panelUpserts(t, out))
	change()
	deadline := time.Now().Add(3 * time.Second)
	for len(panelUpserts(t, out)) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	settle()
	ups := panelUpserts(t, out)[before:]
	if len(ups) != 1 {
		t.Fatalf("%s: %d panel upserts, want 1:\n%s", field, len(ups), dumpFrames(t, out))
	}
	u := ups[0]
	if u["key"] != session || fmt.Sprint(u["changed"]) != fmt.Sprint([]any{field}) {
		t.Errorf("%s: upsert key %v changed %v", field, u["key"], u["changed"])
	}
	return u
}

// rexless fails a test that reaches Rex: a refused caller must not.
type rexless struct {
	Terminal
	t *testing.T
}

func (r rexless) Whoami(context.Context) (rex.Server, error) {
	r.t.Error("a refused caller asked Rex")
	return rex.Server{}, errors.New("no Rex here")
}

// Every agent's caller is refused before any frame or Rex call: a harness
// session the environment names (adopted or not), an unattended shell, a
// pane holding a live agent of any role, and a process under one, by an
// injected and by the real process table. The human's pane, and a shell
// naming the session, are served.
func TestPanelGateRefusesEveryAgentAndServesTheHuman(t *testing.T) {
	e, _ := panelWorld(t)
	ctx := context.Background()
	refusing := e
	refusing.Rex = rexless{t: t}
	for _, c := range []struct {
		name      string
		env       map[string]string
		ancestors []proc
	}{
		{"an unadopted claude -p", map[string]string{"CLAUDE_CODE_SESSION_ID": "nobody", "REX_BLOCK": "block:human"}, nil},
		{"an adopted claude session", map[string]string{"CLAUDE_CODE_SESSION_ID": "harness-claude-01"}, nil},
		{"a codex tool shell", map[string]string{"CODEX_THREAD_ID": "harness-claude-02"}, nil},
		{"a pi tool shell", map[string]string{"PI_SESSION_ID": "harness-pi-05"}, nil},
		{"an antigravity tool shell", map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "x"}, nil},
		{"an unattended shell in the human's pane", map[string]string{"REX_BLOCK": "block:human", "ME_UNATTENDED": "1"}, nil},
		{"the orchestrator's pane", map[string]string{"REX_BLOCK": "block:claude-01"}, nil},
		{"the implementer's pane", map[string]string{"REX_BLOCK": "block:claude-02"}, nil},
		{"the narrator's pane", map[string]string{"REX_BLOCK": "block:pi-04"}, nil},
		{"the personal agent's pane", map[string]string{"REX_BLOCK": "block:pi-05"}, nil},
		{"env -i under the implementer", nil, []proc{{PID: 9001}, {PID: 702}, {PID: 1}}},
		{"env -i under the narrator", nil, []proc{{PID: 703}}},
		{"env -i under the personal agent", map[string]string{"REX_BLOCK": "block:human"}, []proc{{PID: 704}}},
	} {
		e := refusing
		if c.ancestors != nil {
			e.Ancestors = func() ([]proc, error) { return c.ancestors, nil }
		}
		for _, session := range []string{"", "session:s"} {
			out := &syncBuffer{}
			err := e.Panel(ctx, out, panelEnv(c.env), session, StreamTicks{})
			if !errors.Is(err, ErrRefused) || len(out.frames(t)) != 0 {
				t.Errorf("%s, session %q: %v, %d frames; want refused, none", c.name, session, err, len(out.frames(t)))
			}
		}
	}
	// Gate 4 against the real process table: this test process is the agent
	// of a live record.
	real := refusing
	real.Ancestors = nil
	put(t, real, store.Record{Handle: "claude-08", Kind: core.Claude, Role: "reviewer", SessionID: "session:s", AgentPID: os.Getpid()})
	if err := real.Panel(ctx, &syncBuffer{}, panelEnv(nil), "session:s", StreamTicks{}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "claude-08") {
		t.Errorf("under a live agent, by the real process table: %v", err)
	}
	put(t, e, departed(recordOf(t, e, "claude-08"), time.Now()))

	// The human: its own pane, a shell naming the session, and the pane of an
	// agent that departed.
	put(t, e, departed(store.Record{Handle: "codex-09", Kind: core.Codex, SessionID: "session:s", BlockID: "block:gone"}, time.Now()))
	for _, c := range []struct {
		env     map[string]string
		session string
	}{{human, ""}, {nil, "session:s"}, {map[string]string{"REX_BLOCK": "block:gone"}, "session:s"}} {
		out, stop := startPanel(t, e, c.env, c.session)
		snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
		stop()
		if field(snap, "scope", "session_id") != "session:s" || panelOf(t, snap)["session_label"] != "auth-refresh" {
			t.Errorf("%v %q: snapshot %v", c.env, c.session, snap)
		}
	}
	// Outside Rex the session must be named.
	if err := e.Panel(ctx, &syncBuffer{}, panelEnv(nil), "", StreamTicks{}); !errors.Is(err, ErrNotInRex) {
		t.Errorf("outside Rex with no --session: %v", err)
	}
}

// The snapshot carries the session's panel: its label, its crew resolved to
// its lead and members, attention for every live agent (the narrator's
// stall included, [] where nothing waits), and the stored view with the
// recorded hand-back and lifecycle; an Epic session holds no personal agent,
// so it has no cockpit. events --json's snapshot has no panel.
func TestPanelSnapshotCarriesThePanel(t *testing.T) {
	e, _ := panelWorld(t)
	out, stop := startPanel(t, e, human, "")
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	stop()
	p := panelOf(t, snap)
	if p["session_label"] != "auth-refresh" {
		t.Errorf("label %v", p["session_label"])
	}
	c, _ := p["crew"].(map[string]any)
	if c["crew"] != "pair" || c["claimed_by"] != "claude-01" || c["claimed_at"] != "2026-11-16T09:00:00Z" || c["lead"] != "implementer" || fmt.Sprint(c["members"]) != "[implementer reviewer]" {
		t.Errorf("crew %v", c)
	}
	att := fmt.Sprint(p["attention"])
	if want := "map[claude-01:[] claude-02:[turn_error@2026-11-16T09:00:00Z] pi-04:[stalled@2026-11-16T09:00:00Z]]"; att != want {
		t.Errorf("attention %s, want %s", att, want)
	}
	if field(p, "view", "narrative", "text") != "quiet" || field(p, "view", "hand_back", "body") != "## Review queue\nPR #3" || field(p, "view", "lifecycle", "state") != store.EpicOpened {
		t.Errorf("view %v", p["view"])
	}
	if _, ok := p["cockpit"]; ok {
		t.Errorf("an Epic session has a cockpit: %v", p["cockpit"])
	}
	if len(snap["agents"].([]any)) != 3 {
		t.Errorf("the panel's snapshot lost the agents: %v", snap["agents"])
	}

	ev, stopEvents := startStream(t, e, Scope{Kind: ScopeSession, Server: fakeServer, SessionID: "session:s"})
	esnap := await(t, ev, "the events snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	settle()
	stopEvents()
	if _, ok := esnap["panel"]; ok {
		t.Error("events --json's snapshot has a panel")
	}
	for _, f := range ev.frames(t) {
		if f["entity"] == "panel" {
			t.Errorf("events --json told a panel: %v", f)
		}
	}
}

// A quiet session tells no panel; a narrator refresh that changes only the
// view's cursor and the obligations' ages tells none either. A change to
// the label, the crew, an agent's attention (a stall, with no narrator in
// the session), or the view each tells exactly one upsert naming it.
func TestPanelUpsertsOncePerChange(t *testing.T) {
	e, term := panelWorld(t)
	must(t, e.Store.DeleteRecord("pi-04"))
	out, stop := startPanel(t, e, human, "")
	await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	settle()
	if ups := panelUpserts(t, out); len(ups) != 0 {
		t.Fatalf("a quiet session told %d panel upserts", len(ups))
	}
	must(t, e.Store.PutView("session:s", panelStoredView(panelT0.Add(time.Hour), "quiet", 3605)))
	settle()
	if ups := panelUpserts(t, out); len(ups) != 0 {
		t.Fatalf("a refresh changing only the cursor and ages told %v", ups)
	}

	u := expectOneUpsert(t, out, "session:s", "session_label", func() {
		term.mu.Lock()
		term.sessionLabel = "auth-refresh-v2"
		term.mu.Unlock()
	})
	if field(u, "value", "session_label") != "auth-refresh-v2" {
		t.Errorf("label upsert %v", u)
	}
	u = expectOneUpsert(t, out, "session:s", "crew", func() {
		must(t, e.Store.PutSessionCrew("session:s", store.SessionCrew{Crew: "no-such-crew", ClaimedBy: "claude-02", ClaimedAt: panelT0}))
	})
	if c, _ := field(u, "value", "crew").(map[string]any); c["crew"] != "no-such-crew" || c["claimed_by"] != "claude-02" || len(c) != 3 {
		t.Errorf("an unresolved crew keeps only its claim: %v", c)
	}
	u = expectOneUpsert(t, out, "session:s", "attention", func() {
		must(t, e.Store.PutHealthMark("block:claude-01", store.HealthMark{LastStallAt: panelT0.Add(time.Minute)}))
	})
	if a := fmt.Sprint(field(u, "value", "attention", "claude-01")); a != "[stalled@2026-11-16T09:01:00Z]" {
		t.Errorf("stall without a narrator: %s", a)
	}
	u = expectOneUpsert(t, out, "session:s", "view", func() {
		must(t, e.Store.PutView("session:s", panelStoredView(panelT0.Add(2*time.Hour), "busy", 7205)))
	})
	if field(u, "value", "view", "narrative", "text") != "busy" || field(u, "value", "session_label") != "auth-refresh-v2" {
		t.Errorf("view upsert does not carry the whole panel: %v", u)
	}
	settle()
	stop()
	frames := out.frames(t)
	for i, f := range frames {
		if f["seq"] != float64(i+1) {
			t.Fatalf("frame %d has seq %v", i, f["seq"])
		}
	}
}

// A personal agent's session carries the summary of its linked Epics, and
// one change to them tells one upsert; a personal agent linked to nothing
// has an empty cockpit, not a missing one.
func TestPanelCockpitInAPersonalSession(t *testing.T) {
	e, _ := panelWorld(t)
	out, stop := startPanel(t, e, nil, "session:p")
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	p := panelOf(t, snap)
	cockpit, ok := p["cockpit"].([]any)
	if !ok || len(cockpit) != 1 {
		t.Fatalf("cockpit %v", p["cockpit"])
	}
	ep := cockpit[0].(map[string]any)
	if ep["session_id"] != "session:s" || ep["name"] != "auth-refresh" || ep["has_view"] != true || field(ep, "narrative", "text") != "quiet" ||
		field(ep, "hand_back", "seq") != float64(1) || field(ep, "lifecycle", "state") != store.EpicOpened ||
		ep["active_members"] != float64(2) || ep["open_attention"] != float64(2) || ep["open_obligations"] != float64(1) {
		t.Errorf("linked Epic %v", ep)
	}
	if fmt.Sprint(p["attention"]) != "map[pi-05:[]]" {
		t.Errorf("attention %v", p["attention"])
	}
	settle()
	if ups := panelUpserts(t, out); len(ups) != 0 {
		t.Fatalf("a quiet cockpit told %v", ups)
	}
	u := expectOneUpsert(t, out, "session:p", "cockpit", func() {
		must(t, e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:s", Server: fakeServer, Name: "auth-refresh", State: store.EpicHandBack, PersonalHandle: "pi-05", UpdatedAt: panelT0.Add(time.Hour)}))
	})
	if s := field(u, "value", "cockpit").([]any)[0].(map[string]any); field(s, "lifecycle", "state") != store.EpicHandBack {
		t.Errorf("cockpit upsert %v", s)
	}
	stop()

	lonely := store.Record{Handle: "pi-06", Kind: core.Pi, Role: core.PersonalRole, Server: fakeServer, SessionID: "session:q", BlockID: "block:pi-06"}
	pp, err := e.panelProjection(Scope{Kind: ScopeSession, Server: fakeServer, SessionID: "session:q"}, "", []store.Record{lonely}, []store.Record{lonely})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pp)
	if !strings.Contains(string(b), `"cockpit":[]`) {
		t.Errorf("a personal agent linked to nothing: %s", b)
	}
}

// Before its narrator writes a view, a session with a recorded lifecycle,
// or with a hand-back alone, reads as that with a no_view gap; with
// neither it has no view.
func TestPanelViewFallsBackBeforeTheNarratorWrites(t *testing.T) {
	e, _ := panelWorld(t)
	key := func(id string) core.SessionKey { return core.SessionKey{Server: fakeServer, SessionID: id} }
	must(t, e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:b", Server: fakeServer, Name: "billing", State: store.EpicPartialStart, UpdatedAt: panelT0}))
	must(t, e.Store.PutHandBack("session:c", store.HandBack{By: "claude-01", At: panelT0, Body: "done", Seq: 1}))
	v, err := e.panelView(key("session:b"))
	if err != nil || v == nil || v.Session.Epic != "billing" || v.Lifecycle == nil || v.HandBack != nil || len(v.CoverageGaps) != 1 || v.CoverageGaps[0].Kind != GapNoView || !slices.Equal(v.Sources, []string{"lifecycle.json"}) {
		t.Errorf("lifecycle only: %+v %v", v, err)
	}
	v, err = e.panelView(key("session:c"))
	if err != nil || v == nil || v.HandBack == nil || v.HandBack.Body != "done" || v.Lifecycle != nil || len(v.CoverageGaps) != 1 || v.CoverageGaps[0].Kind != GapNoView || !slices.Equal(v.Sources, []string{"hand-back.json"}) || v.Members == nil {
		t.Errorf("hand-back only: %+v %v", v, err)
	}
	if v, err := e.panelView(key("session:none")); err != nil || v != nil {
		t.Errorf("neither: %+v %v", v, err)
	}
	// Streamed, the fallback is the panel's view.
	must(t, os.Remove(e.Paths.ViewPath("session:s")))
	out, stop := startPanel(t, e, human, "")
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	stop()
	if p := panelOf(t, snap); field(p, "view", "lifecycle", "name") != "auth-refresh" || field(p, "view", "hand_back", "body") != "## Review queue\nPR #3" || !strings.Contains(fmt.Sprint(field(p, "view", "coverage_gaps")), GapNoView) {
		t.Errorf("streamed fallback %v", p["view"])
	}
}

// stateTree is every file and directory under dir: path, size, mode,
// modification time, and content hash.
func stateTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := ""
		if !d.IsDir() {
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum = fmt.Sprintf("%x", sha256.Sum256(c))
		}
		fmt.Fprintf(&b, "%s %d %v %s %s\n", p, info.Size(), info.Mode(), info.ModTime().Format(time.RFC3339Nano), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Streaming the panel of an Epic and of a personal session, with mail,
// attention marks, a crew, a view, a lifecycle, a hand-back, and a link to
// read, changes no byte, mode, or time under the adapter's home.
func TestPanelWritesNothing(t *testing.T) {
	e, _ := panelWorld(t)
	before := stateTree(t, e.Paths.Home)
	if !strings.Contains(before, "crew.json") || !strings.Contains(before, "view.json") || !strings.Contains(before, "_m1.md") {
		t.Fatalf("the world lacks what the panel reads:\n%s", before)
	}
	outS, stopS := startPanel(t, e, human, "")
	outP, stopP := startPanel(t, e, nil, "session:p")
	await(t, outS, "session:s's snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	await(t, outP, "session:p's snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	settle()
	stopS()
	stopP()
	if after := stateTree(t, e.Paths.Home); after != before {
		t.Errorf("the panel changed the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A walk up the process tree that reached its bound (maxAncestors) without
// reaching the root, and without meeting an agent, cannot tell that no agent
// started the command: it is refused before Rex or any frame. One short of
// the bound is served.
func TestPanelGateRefusesATruncatedProcessWalk(t *testing.T) {
	e, _ := panelWorld(t)
	walk := func(n int) func() ([]proc, error) {
		return func() ([]proc, error) {
			ps := make([]proc, n)
			for i := range ps {
				ps[i] = proc{PID: 20000 + i}
			}
			return ps, nil
		}
	}
	refusing := e
	refusing.Rex = rexless{t: t}
	refusing.Ancestors = walk(maxAncestors)
	out := &syncBuffer{}
	if err := refusing.Panel(context.Background(), out, panelEnv(nil), "session:s", StreamTicks{}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "too deep") || len(out.frames(t)) != 0 {
		t.Errorf("a walk stopped at its bound: %v, %d frames", err, len(out.frames(t)))
	}
	e.Ancestors = walk(maxAncestors - 1)
	served, stop := startPanel(t, e, nil, "session:s")
	await(t, served, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	stop()
}

// The real walker: a command more than maxAncestors processes below a live
// agent (here this test process) is refused as too deep, while one a few
// processes below it is refused as that agent's. The helper runs at the
// bottom of a chain of shells.
func TestPanelRefusesACallerDeeperThanTheRealWalk(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process ancestry is read on darwin and linux only")
	}
	home := t.TempDir()
	s := store.Store{P: store.Paths{Home: home}}
	must(t, s.PutRecord(store.Record{Handle: "claude-deep", Kind: core.Claude, Role: "implementer", Server: fakeServer, SessionID: "session:s", BlockID: "block:deep", AgentPID: os.Getpid()}))
	chain := filepath.Join(t.TempDir(), "chain.sh")
	// Each shell waits for the next (the exit after it keeps it from being
	// replaced), so every level is a live process.
	must(t, os.WriteFile(chain, []byte("n=$1; shift\nif [ \"$n\" -gt 0 ]; then sh \"$0\" $((n-1)) \"$@\"; s=$?; exit $s; fi\nexec \"$@\"\n"), 0o700))
	for _, c := range []struct {
		depth int
		want  string
	}{{3, "claude-deep (implementer)"}, {maxAncestors + 6, "too deep"}} {
		cmd := exec.Command("sh", chain, fmt.Sprint(c.depth), os.Args[0], "-test.run=^TestPanelDeepCallerHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "PANEL_DEEP_HELPER=1", "PANEL_DEEP_HOME="+home)
		b, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(b), "PANEL_DEEP_REFUSED: ") || !strings.Contains(string(b), c.want) {
			t.Errorf("%d shells deep: %v\n%s", c.depth, err, b)
		}
	}
}

// TestPanelDeepCallerHelper is the bottom of TestPanelRefusesACallerDeeperThanTheRealWalk's chain.
func TestPanelDeepCallerHelper(t *testing.T) {
	if os.Getenv("PANEL_DEEP_HELPER") != "1" {
		t.Skip("run by TestPanelRefusesACallerDeeperThanTheRealWalk")
	}
	paths := store.Paths{Home: os.Getenv("PANEL_DEEP_HOME")}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: rexless{t: t}, Now: time.Now}
	out := &syncBuffer{}
	err := e.Panel(context.Background(), out, panelEnv(nil), "session:s", StreamTicks{})
	if !errors.Is(err, ErrRefused) || len(out.frames(t)) != 0 {
		t.Fatalf("PANEL_DEEP_SERVED: %v", err)
	}
	fmt.Printf("PANEL_DEEP_REFUSED: %v\n", err)
}

// A crew is resolved where epic resolved it: the Epic's recorded directory,
// not the directory of the personal agent that claimed it; a session with no
// Epic directory resolves from the claimant's. A crew the Epic's directory
// does not define is not looked up elsewhere.
func TestPanelCrewResolvesFromTheEpicDirectory(t *testing.T) {
	e, _ := panelWorld(t)
	proj, hub := t.TempDir(), t.TempDir()
	layer := func(dir, name, header string) crew.Library {
		return crew.Library{{Name: "project", Dir: dir, FS: fstest.MapFS{name + "/team-conventions.md": {Data: []byte("---\n" + header + "---\nconventions\n")}}}}
	}
	e.Crews = func(dir string) crew.Library {
		switch dir {
		case proj:
			return append(layer(proj, "pair", "lead: reviewer\nmembers: reviewer, implementer, qa\n"), crew.Builtin()...)
		case hub:
			return append(layer(hub, "hubonly", "lead: writer\nmembers: writer\n"), crew.Builtin()...)
		}
		return crew.Builtin()
	}
	r := recordOf(t, e, "pi-05")
	r.Cwd = hub
	put(t, e, r)
	recs, err := e.Store.Records()
	must(t, err)
	lifecycle := func(dir string) {
		must(t, e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:s", Server: fakeServer, Name: "auth-refresh", Dir: dir, State: store.EpicOpened, UpdatedAt: panelT0}))
	}
	for _, c := range []struct {
		name, crew, dir, lead string
		members               []string
	}{
		{"the Epic's project override", "pair", proj, "reviewer", []string{"reviewer", "implementer", "qa"}},
		{"the claimant's directory, with no Epic directory", "hubonly", "", "writer", []string{"writer"}},
		{"a crew only the claimant's directory defines", "hubonly", proj, "", nil},
	} {
		must(t, e.Store.PutSessionCrew("session:s", store.SessionCrew{Crew: c.crew, ClaimedBy: "pi-05", ClaimedAt: panelT0}))
		lifecycle(c.dir)
		pc, err := e.panelCrew("session:s", recs)
		if err != nil || pc == nil || pc.Lead != c.lead || !slices.Equal(pc.Members, c.members) {
			t.Errorf("%s: %+v %v", c.name, pc, err)
		}
	}
}
