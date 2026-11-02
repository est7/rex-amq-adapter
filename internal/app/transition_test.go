package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// amqThat wraps the real amq in a script, so a test can fail one of its
// commands: while the file fail exists, a command matching the shell pattern
// condition exits 1.
func amqThat(t *testing.T, e *Env, fail, condition string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "amq.sh")
	text := "#!/bin/sh\nif [ -e '" + fail + "' ]; then case \" $* \" in " + condition + ") exit 1 ;; esac; fi\nexec '" + e.AmqBin + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	e.AmqBin = script
}

func windowAgent(t *testing.T, e Env, h core.Handle, kind core.Kind) store.Record {
	t.Helper()
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), string(h)); err != nil {
		t.Fatal(err)
	}
	r := store.Record{Handle: h, Kind: kind, Server: "unix:///fake.sock", BlockID: "block:" + string(h), WindowID: "window:1", AgentPID: os.Getpid(), AgentSessionID: "s-" + string(h)}
	if err := e.Store.PutRecord(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func unreadOf(t *testing.T, e Env, h string) int {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), h)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

func recordNamed(t *testing.T, e Env, h core.Handle) store.Record {
	t.Helper()
	recs, err := e.Store.Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Handle == h {
			return r
		}
	}
	t.Fatalf("no record for %s", h)
	return store.Record{}
}

// One failed send must not leave an idle goal agent unaware that its peer left:
// nothing else would ever tell it.
func TestADepartureFinishesAfterItsMailFailed(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx := context.Background()
	fail := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	windowAgent(t, e, "claude-goal-01", core.Claude)
	peer := windowAgent(t, e, "codex-reviewer-01", core.Codex)
	amqThat(t, &e, fail, `*" send "*`)

	if err := e.park(ctx, peer); err != nil {
		t.Fatalf("a failed notice must not fail the departure: %v", err)
	}
	if r := recordNamed(t, e, peer.Handle); r.Live() || r.Unfinished != store.Departing {
		t.Fatalf("after the failed send: live=%v unfinished=%q", r.Live(), r.Unfinished)
	}
	if err := e.Collect(ctx); err == nil {
		t.Fatal("collection hid that the departure is still unfinished")
	}
	if n := unreadOf(t, e, "claude-goal-01"); n != 0 {
		t.Fatalf("setup: %d messages arrived while send failed", n)
	}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if n := unreadOf(t, e, "claude-goal-01"); n != 1 {
		t.Fatalf("the goal agent got %d messages after the retry", n)
	}
	if r := recordNamed(t, e, peer.Handle); r.Unfinished != "" {
		t.Fatalf("the departure stayed unfinished: %q", r.Unfinished)
	}
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if n := unreadOf(t, e, "claude-goal-01"); n != 1 {
		t.Fatalf("a finished departure was told again: %d messages", n)
	}
}

// An ordinary tab has nothing to finish, whatever the mailers do.
func TestADepartureWithoutAGoalAgentFinishesAtOnce(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	windowAgent(t, e, "claude-01", core.Claude)
	peer := windowAgent(t, e, "codex-reviewer-01", core.Codex)
	if err := e.park(context.Background(), peer); err != nil {
		t.Fatal(err)
	}
	if r := recordNamed(t, e, peer.Handle); r.Live() || r.Unfinished != "" {
		t.Fatalf("live=%v unfinished=%q", r.Live(), r.Unfinished)
	}
}

// An adoption that fails after its record is written is finished by a later
// hook: the agent still learns who it is.
func TestAnInterruptedAdoptionFinishesOnTheNextPrompt(t *testing.T) {
	term := &fakeTerm{state: "idle", label: "reviewer"}
	e := realAmqEnv(t, term)
	mate := windowAgent(t, e, "claude-01", core.Claude)
	term.windows = map[string]string{block: mate.WindowID, mate.BlockID: mate.WindowID}
	ctx := context.Background()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1", TranscriptPath: []byte(`"/tmp/t.jsonl"`)}

	// The waker cannot start: its injector's directory is a file.
	if err := os.MkdirAll(e.Paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(e.injector())
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, in); err == nil {
		t.Fatal("setup: the adoption should fail")
	}
	rec, ok, _ := e.Store.Record(block)
	if !ok || !rec.Live() || rec.Unfinished != store.Adopting {
		t.Fatalf("after the failure: %+v", rec)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })

	// The hook on every tool call waits out the back-off.
	if r, err := e.Hook(ctx, EventPostTool, core.Codex, loc, in); err != nil || r.Notice != "" {
		t.Fatalf("a tool call retried inside the back-off: %q %v", r.Notice, err)
	}
	r, err := e.Hook(ctx, EventPrompt, core.Codex, loc, in)
	if err != nil || !strings.Contains(r.Notice, "you are "+string(rec.Handle)) {
		t.Fatalf("the next prompt must finish the adoption: %q %v", r.Notice, err)
	}
	if got := recordNamed(t, e, rec.Handle); got.Unfinished != "" {
		t.Fatalf("the adoption stayed unfinished: %q", got.Unfinished)
	}
	if term.label != string(rec.Handle) {
		t.Fatalf("pane label %q", term.label)
	}
	if !wakerLive(t, e, string(rec.Handle)) {
		t.Fatal("no waker after the adoption finished")
	}
	notes, err := e.Store.TakeWindowNotes(mate.Handle)
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "joined") {
		t.Fatalf("the mate's notes: %q %v", notes, err)
	}
}

// An adoption repeated for an agent that already finished one tells nobody,
// even when an attempt fails and the next one has to come back to it.
func TestARepeatedAdoptionThatFailsIsNotAnArrival(t *testing.T) {
	term := &fakeTerm{state: "idle", label: "reviewer"}
	e := realAmqEnv(t, term)
	mate := windowAgent(t, e, "claude-01", core.Claude)
	term.windows = map[string]string{block: mate.WindowID, mate.BlockID: mate.WindowID}
	ctx := context.Background()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1"}
	first, err := e.Adopt(ctx, loc, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	clean := e // the cleanup must not meet the fault injected below
	t.Cleanup(func() { _ = clean.Depart(ctx, loc, HookInput{}) })
	if _, err := e.Store.TakeWindowNotes(mate.Handle); err != nil {
		t.Fatal(err)
	}
	if got := recordNamed(t, e, first.Record.Handle); got.Unfinished != "" {
		t.Fatalf("a finished adoption is marked %q", got.Unfinished)
	}
	fail := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	amqThat(t, &e, fail, `*" check "*`)
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := e.Adopt(short, loc, core.Codex, in); err == nil {
		t.Fatal("setup: the repeat should fail")
	}
	if got := recordNamed(t, e, first.Record.Handle); !got.Adopted() {
		t.Fatalf("a failed repeat undid the adoption: %q", got.Unfinished)
	}
	again, err := clean.Adopt(ctx, loc, core.Codex, in)
	if err != nil || !again.Known {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if notes, _ := e.Store.TakeWindowNotes(mate.Handle); len(notes) != 0 {
		t.Fatalf("a repeat told the window: %q", notes)
	}
}

// An agent whose adoption cannot finish has a mailbox: its hooks must keep
// announcing mail however long the waker stays down.
func TestAnUnfinishedAdoptionStillHearsItsMail(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1", TranscriptPath: []byte(`"/tmp/t.jsonl"`)}
	if err := os.MkdirAll(e.Paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(e.injector()), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, in); err == nil {
		t.Fatal("setup: the adoption should fail")
	}
	rec, _, _ := e.Store.Record(block)
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), string(rec.Handle)), "m1.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{EventPostTool, EventPrompt} {
		r, err := e.Hook(ctx, event, core.Codex, loc, in)
		if err != nil || !strings.Contains(r.Notice, "unread") {
			t.Fatalf("%s: %q %v", event, r.Notice, err)
		}
		if err := e.Store.SetAnnounced(rec.Handle, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// who runs when a peer is silent, which may be because a waker cannot start.
// It must not hold the lock every tab's adoption needs for its whole budget.
func TestSupervisingAWindowHoldsTheLockBriefly(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := windowAgent(t, e, "codex-reviewer-01", core.Codex)
	// A started waker exits at once, so it never comes up.
	flag := filepath.Join(t.TempDir(), "on")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	amqThat(t, &e, flag, `*" wake --root "*`)
	w := Window{server: r.Server, id: r.WindowID, windowOf: map[string]string{r.BlockID: r.WindowID}}
	start := time.Now()
	err := e.SuperviseWindow(ctx, w)
	if took := time.Since(start); took > 2*superviseFor {
		t.Fatalf("held the lifecycle lock for %v", took)
	}
	if err == nil {
		t.Fatal("a waker that never came up was reported as fine")
	}
}

// Pi has no Stop hook: settling is the end of its turn, so a goal watcher that
// gave up comes back there.
func TestAPiGoalAgentsWatcherComesBackWhenItSettles(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	started := filepath.Join(t.TempDir(), "started")
	self := filepath.Join(t.TempDir(), "self.sh")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho \"$*\" >>'"+started+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = self
	r := windowAgent(t, e, "pi-goal-01", core.Pi)
	r.BlockID = block
	if err := e.Store.PutRecord(r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(ctx, EventSettle, core.Pi, Location{Block: block}, HookInput{SessionID: r.AgentSessionID}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(started)
		if strings.Contains(string(b), "watch") {
			return
		}
		if time.Now().After(end) {
			t.Fatal("settling did not start the goal watcher")
		}
	}
}

// The role that briefs an agent is the one its handle carries, which is the one
// every other rule reads: a pane's label is overwritten at adoption and a
// resumed session may land in a pane labelled for something else.
func TestAResumedAgentKeepsItsRoleWhateverThePaneSays(t *testing.T) {
	term := &fakeTerm{state: "idle", label: "goal"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	departed := time.Now().Add(-time.Hour)
	if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), "codex-reviewer-01"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutRecord(store.Record{Handle: "codex-reviewer-01", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", AgentSessionID: "s1", DepartedAt: &departed}); err != nil {
		t.Fatal(err)
	}
	a, err := e.Adopt(ctx, loc, core.Codex, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if a.Record.Handle != "codex-reviewer-01" || a.Role != "reviewer" {
		t.Fatalf("handle %s role %q", a.Record.Handle, a.Role)
	}
}

func TestAwaitWaitsForTheAdoptionToFinish(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	if err := e.Store.PutRecord(store.Record{Handle: "codex-01", Kind: core.Codex, BlockID: block, Unfinished: store.Adopting}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := e.AwaitStartup(ctx, block)
	if err != nil || s.Status != core.StartupTimeout {
		t.Fatalf("got %+v %v", s, err)
	}
}
