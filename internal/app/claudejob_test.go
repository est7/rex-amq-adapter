package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A Claude Code supervisor fixture: block:tui is the terminal that started
// the supervisor, so every job inherits its REX_BLOCK; block:view is another
// pane, whose terminal client shows the job "fix the build".
const (
	tuiBlock  = "block:tui"
	viewBlock = "block:view"
	jobName   = "fix the build"
	jobSess   = "job-session"
)

type jobWorld struct {
	e      Env
	term   *fakeTerm
	jobPID int // the job's own process: alive, and no pane's foreground
}

func newJobWorld(t *testing.T) jobWorld {
	t.Helper()
	job := exec.Command("sleep", "60")
	if err := job.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Process.Kill(); _ = job.Wait() })
	term := &fakeTerm{
		state:   "idle",
		windows: map[string]string{tuiBlock: "window:1", viewBlock: "window:2"},
		fgs: map[string]rex.Process{
			tuiBlock:  {PID: os.Getpid(), Name: "claude"}, // the terminal session's own TUI
			viewBlock: {PID: 1, Name: "claude"},           // a client showing the job
		},
		titles: map[string]string{tuiBlock: "✳ claude-01", viewBlock: "✳ " + jobName},
	}
	e := realAmqEnv(t, term)
	e.ClaudeDir = t.TempDir()
	w := jobWorld{e: e, term: term, jobPID: job.Process.Pid}
	w.roster(t, map[string]string{"a1b2c3d4": jobSess})
	w.name(t, "a1b2c3d4", jobName)
	// The terminal session in block:tui, adopted before the job started.
	ctx := context.Background()
	if _, err := e.Hook(ctx, EventSessionStart, core.Claude, Location{Block: tuiBlock}, HookInput{SessionID: "tui-session", AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		recs, _ := e.Store.Records()
		for _, r := range recs {
			if r.Live() {
				_ = e.park(ctx, r)
			}
		}
	})
	return w
}

func (w jobWorld) roster(t *testing.T, workers map[string]string) {
	t.Helper()
	ws := map[string]any{}
	for short, session := range workers {
		ws[short] = map[string]string{"sessionId": session}
	}
	writeJSON(t, filepath.Join(w.e.ClaudeDir, "daemon", "roster.json"), map[string]any{"workers": ws})
}

func (w jobWorld) name(t *testing.T, short, name string) {
	t.Helper()
	writeJSON(t, filepath.Join(w.e.ClaudeDir, "jobs", short, "state.json"), map[string]string{"name": name})
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w jobWorld) jobHook(t *testing.T, event string) Reply {
	t.Helper()
	// The job's hook carries the supervisor's REX_BLOCK: block:tui.
	r, err := w.e.Hook(context.Background(), event, core.Claude, Location{Block: tuiBlock}, HookInput{SessionID: jobSess, AgentPID: w.jobPID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (w jobWorld) tuiRecord(t *testing.T) store.Record {
	t.Helper()
	rec, ok, err := w.e.Store.Record(tuiBlock)
	if err != nil || !ok {
		t.Fatalf("block:tui's record: %+v %v %v", rec, ok, err)
	}
	return rec
}

// The incident: a background job's hooks carry REX_BLOCK of the terminal
// that started the supervisor. The job must be adopted in the pane that
// shows it, under a handle of its own, and the terminal session keeps its own.
func TestBackgroundJobIsAdoptedInThePaneThatShowsIt(t *testing.T) {
	w := newJobWorld(t)
	before := w.tuiRecord(t)
	w.jobHook(t, EventSessionStart)
	rec, ok, err := w.e.liveRecordOf(core.Claude, jobSess)
	if err != nil || !ok {
		t.Fatalf("the job was not adopted: %v", err)
	}
	if rec.BlockID != viewBlock || rec.AgentPID != w.jobPID || rec.Handle == before.Handle {
		t.Errorf("job adopted as %s in %s with pid %d; want its own handle in %s with pid %d", rec.Handle, rec.BlockID, rec.AgentPID, viewBlock, w.jobPID)
	}
	if after := w.tuiRecord(t); after.Handle != before.Handle || !after.Live() || after.AgentSessionID != "tui-session" {
		t.Errorf("the terminal session's record changed: %+v", after)
	}
}

// A job no pane shows has no pane, and REX_BLOCK names another agent's: its
// hook does nothing.
func TestBackgroundJobNoPaneShowsGetsNothing(t *testing.T) {
	w := newJobWorld(t)
	w.term.titles[viewBlock] = "something else"
	if r := w.jobHook(t, EventSessionStart); r.Notice != "" {
		t.Errorf("an unplaced job was told %q", r.Notice)
	}
	if _, ok, _ := w.e.liveRecordOf(core.Claude, jobSess); ok {
		t.Error("an unplaced job was adopted")
	}
}

// Only a Claude Code client's title can place a job: a shell titled with the
// same words is not taken for it.
func TestATitledShellIsNotTakenForTheJob(t *testing.T) {
	w := newJobWorld(t)
	w.term.fgs[viewBlock] = rex.Process{PID: 1, Name: "zsh"}
	w.jobHook(t, EventSessionStart)
	if _, ok, _ := w.e.liveRecordOf(core.Claude, jobSess); ok {
		t.Error("a shell's pane was taken for the job")
	}
}

// Once adopted, the pane's title becomes the job's pane label (syncTitle),
// while the supervisor's file keeps the old name. Either names the job, and
// the doorbell may type only while one does.
func TestJobDoorbellBindsWhileItsPaneShowsIt(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess)
	for title, bound := range map[string]bool{
		"✳ " + jobName:                        true,
		"✳ " + core.PaneLabel(rec.Handle, ""): true,
		"2 awaiting input · claude agents":    false,
		"another conversation":                false,
	} {
		w.term.titles[viewBlock] = title
		b, err := w.e.observeBinding(context.Background(), rec)
		if err != nil {
			t.Fatal(err)
		}
		if o, ok := core.CheckBinding(b); ok != bound || !ok && o.Progress != core.Deferred {
			t.Errorf("title %q: bound %v (%+v), want %v", title, ok, o, bound)
		}
	}
}

// A terminal session that moved its conversation to the background (`←`)
// keeps its process as the pane's foreground, now a client. Agent view or a
// job's name in its title means the conversation left the pane for good: it
// departs, and nothing is typed into the list or the other job.
func TestATerminalSessionThatMovedToTheBackgroundLeftItsPane(t *testing.T) {
	w := newJobWorld(t)
	rec := w.tuiRecord(t)
	for title, want := range map[string]string{
		"2 awaiting input · claude agents":    "agent_left_pane",
		"✳ " + jobName:                        "agent_left_pane",
		"✳ " + core.PaneLabel(rec.Handle, ""): "",
	} {
		w.term.titles[tuiBlock] = title
		b, err := w.e.observeBinding(context.Background(), rec)
		if err != nil {
			t.Fatal(err)
		}
		if o, _ := core.CheckBinding(b); o.Code != want {
			t.Errorf("title %q: %+v, want code %q", title, o, want)
		}
	}
}

// The supervisor may drop a job from its roster before the job's SessionEnd
// runs. Its own record departs, not the one REX_BLOCK names.
func TestJobSessionEndParksItsOwnRecord(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	w.roster(t, map[string]string{})
	w.jobHook(t, EventSessionEnd)
	if _, ok, _ := w.e.liveRecordOf(core.Claude, jobSess); ok {
		t.Error("the job's record is still live")
	}
	if tui := w.tuiRecord(t); !tui.Live() {
		t.Errorf("SessionEnd of the job parked the terminal session: %+v", tui)
	}
}

// A job shown in another pane is followed there at its next prompt.
func TestJobShownInAnotherPaneMovesThere(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	first, _, _ := w.e.liveRecordOf(core.Claude, jobSess)
	const next = "block:next"
	w.term.windows[next] = "window:3"
	w.term.fgs[next] = rex.Process{PID: 2, Name: "claude"}
	w.term.titles[next] = w.term.titles[viewBlock]
	w.term.titles[viewBlock] = "2 awaiting input · claude agents"
	w.jobHook(t, EventPrompt)
	rec, ok, _ := w.e.liveRecordOf(core.Claude, jobSess)
	if !ok || rec.BlockID != next || rec.Handle != first.Handle {
		t.Errorf("job is %+v, want %s in %s", rec, first.Handle, next)
	}
}

// A `claude -p` that a program in the pane runs inherits REX_BLOCK but is not
// the pane's foreground: it is not the pane's agent, and it reads none of the
// agent's mail.
func TestANestedClaudeIsNotThePanesAgent(t *testing.T) {
	w := newJobWorld(t)
	before := w.tuiRecord(t)
	nested := HookInput{SessionID: "nested", AgentPID: w.jobPID}
	r, _ := w.e.Hook(context.Background(), EventSessionStart, core.Claude, Location{Block: tuiBlock}, nested)
	if r.Notice != "" {
		t.Errorf("the nested session was told %q", r.Notice)
	}
	r, _ = w.e.Hook(context.Background(), EventPostTool, core.Claude, Location{Block: tuiBlock}, nested)
	if r.Notice != "" {
		t.Errorf("the nested session was told %q", r.Notice)
	}
	if after := w.tuiRecord(t); after.Handle != before.Handle || !after.Live() || after.AgentSessionID != "tui-session" {
		t.Errorf("the pane's agent was replaced: %+v", after)
	}
}

// A command in a job's tool shell has the job's identity while a pane shows
// it, and none otherwise; REX_BLOCK is never used for it.
func TestCallerOfABackgroundJob(t *testing.T) {
	w := newJobWorld(t)
	ctx := context.Background()
	here := Location{Block: tuiBlock}
	if _, err := w.e.Caller(ctx, here, jobSess); err == nil {
		t.Error("an unadopted job was given a location")
	}
	w.jobHook(t, EventSessionStart)
	if loc, err := w.e.Caller(ctx, here, jobSess); err != nil || loc.Block != viewBlock {
		t.Errorf("adopted job: %+v %v", loc, err)
	}
	w.term.titles[viewBlock] = "2 awaiting input · claude agents"
	if _, err := w.e.Caller(ctx, here, jobSess); err == nil {
		t.Error("a job its pane no longer shows was given that pane")
	}
	if loc, err := w.e.Caller(ctx, here, "tui-session"); err != nil || loc.Block != tuiBlock {
		t.Errorf("terminal session: %+v %v", loc, err)
	}
	// A `claude -p` an agent runs in the pane has a session no hook adopted.
	if _, err := w.e.Caller(ctx, here, "nested"); err == nil {
		t.Error("a nested session was given the pane's identity")
	}
	if loc, err := w.e.Caller(ctx, here, ""); err != nil || loc.Block != tuiBlock {
		t.Errorf("a shell with no Claude session: %+v %v", loc, err)
	}
}

// The terminal client showing a job reports no program status, so the job's
// supervisor state decides: a working job is not typed into.
func TestAWorkingJobIsNotTypedInto(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess)
	writeJSON(t, filepath.Join(w.e.ClaudeDir, "jobs", "a1b2c3d4", "state.json"), map[string]string{"name": jobName, "state": "working"})
	w.term.state = "" // the client reports nothing
	o := w.e.Inject(context.Background(), InjectRequest{Block: rec.BlockID, Handle: rec.Handle, Root: w.e.Paths.Root()})
	if o.Code != "agent_working" {
		t.Errorf("a working job: %+v, want agent_working", o)
	}
}

// The job sets its task mid-turn: the record's label changes before Claude
// Code draws the new title, which still shows the handle with no task. The
// handle names it.
func TestAJobStaysBoundAfterItSetsItsTask(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess)
	w.term.titles[viewBlock] = "✳ " + string(rec.Handle) // drawn at SessionStart
	if _, err := w.e.SetTask(context.Background(), Location{Block: rec.BlockID}, "fix the linker"); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(w.e.ClaudeDir, "jobs", "a1b2c3d4", "state.json"), map[string]string{"name": "something new"})
	rec, _, _ = w.e.liveRecordOf(core.Claude, jobSess)
	b, err := w.e.observeBinding(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := core.CheckBinding(b); !ok {
		t.Errorf("a job whose title has not caught up with its task: %+v", o)
	}
}

// A first adoption tells the agent who it is. A Stop hook with no mail would
// drop that notice, so a job is adopted only at a start or a prompt.
func TestAJobIsNotFirstAdoptedAtStop(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventStop)
	if _, ok, _ := w.e.liveRecordOf(core.Claude, jobSess); ok {
		t.Error("a job was first adopted by a Stop hook")
	}
}

// A job shown in another pane of the same tab moves there without telling
// its tab it left and joined.
func TestAJobMovingWithinItsTabIsNoNews(t *testing.T) {
	w := newJobWorld(t)
	w.term.windows[tuiBlock] = "window:2" // the terminal session shares the job's tab
	w.jobHook(t, EventSessionStart)
	tui := w.tuiRecord(t)
	if err := w.e.Store.ClearWindowNotes(tui.Handle); err != nil {
		t.Fatal(err)
	}
	const next = "block:next"
	w.term.windows[next] = "window:2"
	w.term.fgs[next] = rex.Process{PID: 2, Name: "claude"}
	w.term.titles[next] = w.term.titles[viewBlock]
	w.term.titles[viewBlock] = "claude agents"
	w.jobHook(t, EventPrompt)
	if rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess); rec.BlockID != next {
		t.Fatalf("job did not move: %+v", rec)
	}
	notes, err := w.e.Store.TakeWindowNotes(tui.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("a move within the tab was announced: %q", notes)
	}
}

// The terminal session's client opens an adopted job, titled with that job's
// handle: the terminal session's own conversation is gone from the pane.
func TestATerminalSessionShowingAnotherAgentLeftItsPane(t *testing.T) {
	w := newJobWorld(t)
	w.jobHook(t, EventSessionStart)
	job, _, _ := w.e.liveRecordOf(core.Claude, jobSess)
	tui := w.tuiRecord(t)
	w.term.titles[tuiBlock] = "✳ " + core.PaneLabel(job.Handle, "fix the linker")
	b, err := w.e.observeBinding(context.Background(), tui)
	if err != nil {
		t.Fatal(err)
	}
	if o, _ := core.CheckBinding(b); o.Code != "agent_left_pane" {
		t.Errorf("a terminal session whose pane shows %s: %+v", job.Handle, o)
	}
	// The job's last pane closed and its record departed; it still runs.
	if err := w.e.park(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if b, err = w.e.observeBinding(context.Background(), tui); err != nil {
		t.Fatal(err)
	}
	if o, _ := core.CheckBinding(b); o.Code != "agent_left_pane" {
		t.Errorf("a terminal session whose pane shows the departed %s: %+v", job.Handle, o)
	}
}

// A move within the tab tells no one, but it is still a transition: if the
// waker cannot follow the job, the record stays unfinished, so the next hook
// moves it again instead of trusting it.
func TestAJobMoveThatFailsIsFinishedLater(t *testing.T) {
	w := newJobWorld(t)
	w.term.windows[tuiBlock] = "window:2"
	w.jobHook(t, EventSessionStart)
	const next = "block:next"
	w.term.windows[next] = "window:2"
	w.term.fgs[next] = rex.Process{PID: 2, Name: "claude"}
	w.term.titles[next] = w.term.titles[viewBlock]
	w.term.titles[viewBlock] = "claude agents"
	// The waker cannot start: its injector's directory is a file.
	dir := filepath.Dir(w.e.injector())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) })
	if _, err := w.e.Hook(context.Background(), EventPrompt, core.Claude, Location{Block: tuiBlock}, HookInput{SessionID: jobSess, AgentPID: w.jobPID}); err == nil {
		t.Fatal("setup: the move should fail")
	}
	if rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess); rec.BlockID != next || rec.Unfinished != store.Adopting {
		t.Errorf("after a failed move: %+v, want it in %s and unfinished", rec, next)
	}
}

// A job's record can name a pane that does not show it (written by an older
// adapter, or left behind). A frequent hook trusts the record's block, and
// its pid mismatch asks for an adoption: the job must not be adopted into a
// pane that does not show it. Found on the real system after the v0.8.0
// upgrade, where a PostToolUse re-adopted a job into another conversation's
// pane.
func TestAJobIsNeverAdoptedIntoAPaneThatDoesNotShowIt(t *testing.T) {
	w := newJobWorld(t)
	// An old adapter's record: the job bound to another conversation's pane,
	// with that pane's foreground pid.
	const stale = "block:stale"
	w.term.windows[stale] = "window:3"
	w.term.fgs[stale] = rex.Process{PID: 4242, Name: "claude"}
	w.term.titles[stale] = "✳ another conversation"
	if err := w.e.Store.PutRecord(store.Record{Handle: "claude-old", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: stale, AgentPID: 4242, AgentSessionID: jobSess}); err != nil {
		t.Fatal(err)
	}
	w.jobHook(t, EventPostTool)
	if rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess); rec.BlockID == stale && rec.AgentPID == w.jobPID {
		t.Errorf("the job was adopted into a pane that shows something else: %+v", rec)
	}
	if label, ok := w.term.renamed[stale]; ok {
		t.Errorf("the other pane was relabelled %q", label)
	}
	// Its next prompt finds the pane that shows it.
	w.jobHook(t, EventPrompt)
	if rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess); rec.BlockID != viewBlock || rec.AgentPID != w.jobPID {
		t.Errorf("the job's next prompt did not move it to its pane: %+v", rec)
	}
}

// A tool call of a job whose record is stale (an older adapter's, bound to
// another pane with that pane's pid) finds the job's pane at once, and never
// marks the other pane's adoption as failed, which would hold back the
// adoption of the agent that really runs there.
func TestAStaleJobRecordIsMovedByAToolCall(t *testing.T) {
	w := newJobWorld(t)
	const stale = "block:stale"
	w.term.windows[stale] = "window:3"
	w.term.fgs[stale] = rex.Process{PID: 4242, Name: "claude"}
	w.term.titles[stale] = "✳ another conversation"
	if err := w.e.Store.PutRecord(store.Record{Handle: "claude-old", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: stale, AgentPID: 4242, AgentSessionID: jobSess}); err != nil {
		t.Fatal(err)
	}
	w.jobHook(t, EventPostTool)
	if rec, _, _ := w.e.liveRecordOf(core.Claude, jobSess); rec.BlockID != viewBlock || rec.AgentPID != w.jobPID || rec.Handle != "claude-old" {
		t.Errorf("a tool call did not move the stale job to its pane: %+v", rec)
	}
	if _, failed, _ := w.e.Store.AdoptFailedAt(stale); failed {
		t.Error("the job's failed adoption was recorded against another agent's pane")
	}
}
