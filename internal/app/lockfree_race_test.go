package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// These tests schedule two lifecycle operations against each other at the
// points where, with the lifecycle lock no longer held across Rex and amq,
// one can run inside the other: each checks that the operation that read the
// older state does not undo the newer one.

// claudePane makes block show a Claude Code process pid.
func claudePane(term *fakeTerm, b string, pid int) {
	term.mu.Lock()
	defer term.mu.Unlock()
	if term.fgs == nil {
		term.fgs = map[string]rex.Process{}
	}
	term.fgs[b] = rex.Process{PID: pid, Name: "claude"}
}

// A hook that probed the pane before a newer agent was adopted there must
// not evict that agent's lease when it commits.
func TestAStaleProbeOfThePaneDoesNotEvictItsNewerAgent(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	oldPID, newPID := os.Getpid(), os.Getppid()
	claudePane(term, block, oldPID)
	old := HookInput{SessionID: "old", AgentPID: oldPID}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Claude, old)
	if err != nil {
		t.Fatal(err)
	}
	claudePane(term, block, newPID)
	a, err := e.Adopt(ctx, Location{Block: block}, core.Claude, HookInput{SessionID: "new", AgentPID: newPID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.commitAdoption(ctx, p, core.Claude, old); !errors.Is(err, errStaleProbe) {
		t.Fatalf("the stale probe committed: %v, want errStaleProbe", err)
	}
	if now, _, _ := e.agentAt(block); now.Handle != a.Agent.Handle || !now.Live() || now.pid() != newPID {
		t.Fatalf("the pane's newer agent lost its lease: %+v", now)
	}
}

// An adoption replaced in its pane while it reconciled neither relabels the
// pane nor reports the agent adopted (its hook then tells nobody anything).
func TestASupersededReconcileLeavesTheReplacementAlone(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	oldPID, newPID := os.Getpid(), os.Getppid()
	claudePane(term, block, oldPID)
	old := HookInput{SessionID: "old", AgentPID: oldPID}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Claude, old)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.commitAdoption(ctx, p, core.Claude, old)
	if err != nil {
		t.Fatal(err)
	}
	claudePane(term, block, newPID)
	a, err := e.Adopt(ctx, Location{Block: block}, core.Claude, HookInput{SessionID: "new", AgentPID: newPID})
	if err != nil {
		t.Fatal(err)
	}
	want := term.renamed[block]
	if _, err := e.reconcileAdoption(ctx, p, c); !errors.Is(err, errSuperseded) {
		t.Fatalf("the superseded reconcile: %v, want errSuperseded", err)
	}
	if got := term.renamed[block]; got != want || got != core.PaneLabel(a.Agent.Handle, a.Agent.Role, a.Agent.Task) {
		t.Fatalf("the pane shows %q, want the replacement's %q", got, want)
	}
	if wakerLive(t, e, string(c.rec.Handle)) {
		t.Fatal("the superseded handle kept a waker")
	}
	// Its last step alone refuses too, however far the rest got.
	if _, err := e.finishAdoption(ctx, c.rec); !errors.Is(err, errSuperseded) {
		t.Fatalf("finishing the superseded adoption: %v, want errSuperseded", err)
	}
}

// A SessionEnd of a conversation the same process has since left (/clear)
// must not park the conversation it runs now, even when the switch lands
// while the SessionEnd retires the waker.
func TestALateSessionEndDoesNotParkTheProcessesNextConversation(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	claudePane(term, block, os.Getpid())
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Claude, HookInput{SessionID: "old", AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	fake := e.AMQ.(*amq.Fake)
	var once sync.Once
	e.AMQ = fake.WithFail(func(op string, _ []string) error {
		if op == "wake retire" {
			once.Do(func() {
				// The new conversation's hook commits meanwhile.
				in := HookInput{SessionID: "new", AgentPID: os.Getpid()}
				p, err := e.probeAdoption(ctx, Location{Block: block}, core.Claude, in)
				if err == nil {
					_, err = e.commitAdoption(ctx, p, core.Claude, in)
				}
				if err != nil {
					t.Errorf("the new conversation's commit: %v", err)
				}
			})
		}
		return nil
	})
	if err := e.Depart(ctx, Location{Block: block}, HookInput{SessionID: "old", AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	now, _, _ := e.agentAt(block)
	if !now.Live() || now.Session() != "new" {
		t.Fatalf("the late SessionEnd parked the new conversation: live=%v session=%q", now.Live(), now.Session())
	}
}

// Retiring a peer holds the lifecycle lock for its records only: a waker
// operation in progress on the peer's handle (here its lock, held) delays
// the waker's retirement, never every other agent's adoption.
func TestRetireDoesNotHoldTheLifecycleLockWhileItsWakerIsBusy(t *testing.T) {
	e, _, _, _ := retireWorld(t)
	release, err := e.Store.LockHandle(context.Background(), "codex-04")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := e.Retire(ctx, Location{Block: "block:o"}, RetireRequest{Handle: "codex-04", Force: true})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for recordOf(t, e, "codex-04").Live() {
		if time.Now().After(deadline) {
			t.Fatal("the retire did not depart its target")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		t.Fatalf("the lifecycle lock waits for the retired peer's waker: %v", err)
	}
	unlock()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if wakerLive(t, e, "codex-04") {
		t.Fatal("the retired peer's waker survived once its lock was free")
	}
	if p := recordOf(t, e, "codex-04").departure(); p == nil || p.Unfinished || p.DepartReason != store.DepartRetired {
		t.Fatalf("the quiet departure was not finished: %+v", p)
	}
}

// A finisher that read a departure does not finish another one: a handle
// forgotten and named afresh starts its epochs over at 1.
func TestAFinisherDoesNotFinishAReusedHandlesDeparture(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	first := time.Now().Add(-time.Hour)
	put(t, e, seed{Handle: "codex-07", Kind: core.Codex, AdoptedAt: first, DepartedAt: &first, DepartReason: store.DepartSessionEnd, Departing: true})
	stale := recordOf(t, e, "codex-07")
	if err := e.Store.ForgetIdentity("codex-07"); err != nil {
		t.Fatal(err)
	}
	second := time.Now()
	put(t, e, seed{Handle: "codex-07", Kind: core.Codex, AdoptedAt: second, DepartedAt: &second, DepartReason: store.DepartSessionEnd, Departing: true})
	if a, b := stale.departure(), recordOf(t, e, "codex-07").departure(); a.Epoch != b.Epoch {
		t.Fatalf("setup: both departures should be of epoch %d, got %d", a.Epoch, b.Epoch)
	}
	if err := e.finishDeparture(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if p := recordOf(t, e, "codex-07").departure(); !p.Unfinished {
		t.Fatal("a stale finisher marked the reused handle's departure finished")
	}
}

// An Epic session Rex created gets its records even when the opener's
// context ends while another holder has the lifecycle lock: its agents stop
// waiting for them once the opening lock is released.
func TestAnEpicCreatedJustBeforeItsOpenerGivesUpStillGetsItsPolicy(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := make(chan struct{})
	term.sessionStarted = func(string, []string) {
		unlock, err := e.Store.LockLifecycle(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		go func() {
			<-held
			time.Sleep(200 * time.Millisecond)
			unlock()
		}()
		close(held)
		cancel()
	}
	e.Crews = func(string) crew.Library { return crew.Load("") }
	_, _ = e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal, Asked: "open it", Crew: "pair"})
	if len(term.sessions_) != 1 {
		t.Fatalf("setup: sessions created %d", len(term.sessions_))
	}
	if _, ok, err := e.Store.SessionPolicy("session:epic-1"); err != nil || !ok {
		t.Fatalf("the created Epic session has no policy: %v %v", ok, err)
	}
	if c, ok, err := e.Store.SessionCrew("session:epic-1"); err != nil || !ok || c.Crew != "pair" {
		t.Fatalf("the created Epic session has no crew: %+v %v %v", c, ok, err)
	}
	if g, ok, err := e.Store.Grant("session:epic-1"); err != nil || !ok || g.Asked != "open it" {
		t.Fatalf("the created Epic session has no grant: %+v %v %v", g, ok, err)
	}
}

// A departed personal agent's narrator mail is delivered again once, even
// when a departure's finisher and another one try it at the same time with
// no lifecycle lock held.
func TestTwoFinishersRedeliverANarratorMessageOnce(t *testing.T) {
	e, term, hubDir := hubWorld(t)
	key := core.NewHubKey(fakeServer, hubDir)
	hookAs(t, e, term, "block:a", "s1", "", HookInput{})
	p1 := recordOf(t, e, handleIn(t, e, "block:a"))
	put(t, e, seed{Handle: "claude-02", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:p2", SessionID: "session:p2", AgentPID: os.Getpid(), AdoptedAt: time.Now()})
	putHub(t, e, "session:p2", key)
	put(t, e, seed{Handle: "agy-05", Kind: core.Agy, Role: core.NarratorRole, BlockID: "block:n", SessionID: "session:e", AdoptedAt: time.Now()})
	for _, h := range []string{"claude-02", "agy-05"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p2"}, PreferredPersonal: "claude-02", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:e"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := managedSend(t, e, "block:n", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--body", "done"); err != nil {
		t.Fatal(err)
	}
	left, err := e.releaseLease(recordOf(t, e, "claude-02"), store.Departure{Reason: "block_gone", At: time.Now(), Unfinished: true})
	if err != nil {
		t.Fatal(err)
	}
	// The first send waits until the second finisher has had its chance.
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.AMQ = e.AMQ.(*amq.Fake).WithFail(func(op string, _ []string) error {
		if op == "send" {
			once.Do(func() {
				close(entered)
				<-proceed
			})
		}
		return nil
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = e.redeliverFrom(context.Background(), left) }()
	<-entered
	go func() { defer wg.Done(); _ = e.redeliverFrom(context.Background(), left) }()
	time.Sleep(200 * time.Millisecond)
	close(proceed)
	wg.Wait()
	if ids, _ := amq.Unread(e.Paths.Root(), string(p1.Handle)); len(ids) != 1 {
		t.Fatalf("%s holds %d copies, want 1", p1.Handle, len(ids))
	}
}

// A departure that retired the waker and then could not take the lifecycle
// lock (its hook ran out of time) leaves the lease live: the waker comes back
// with it, so the agent is not left deaf.
func TestADepartureThatCannotReleaseBringsTheWakerBack(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	if _, err := e.Adopt(context.Background(), Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var release func()
	fake := e.AMQ.(*amq.Fake)
	e.AMQ = fake.WithFail(func(op string, _ []string) error {
		if op == "wake retire" && release == nil {
			unlock, err := e.Store.LockLifecycle(context.Background())
			if err != nil {
				t.Error(err)
				return nil
			}
			release = unlock
			cancel()
		}
		return nil
	})
	if err := e.Depart(ctx, Location{Block: block}, HookInput{SessionID: "s1"}); err == nil {
		t.Fatal("setup: the departure should fail to take the lock")
	}
	release()
	if rec, _, _ := e.agentAt(block); !rec.Live() || !wakerLive(t, e, "codex-01") {
		t.Fatalf("after a departure that could not release: live=%v waker=%v", rec.Live(), wakerLive(t, e, "codex-01"))
	}
}

// A probe of an agent that a session reconciliation (followMoves) moved
// since does not write its older placement back: Commit finds it stale.
func TestAProbeOfAMovedAgentDoesNotWriteItsOldPlaceBack(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	in := HookInput{SessionID: "s1"}
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, in); err != nil {
		t.Fatal(err)
	}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = e.Store.Moved("codex-01", "session:moved", "window:moved", time.Now())
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.commitAdoption(ctx, p, core.Codex, in); !errors.Is(err, errStaleProbe) {
		t.Fatalf("committing a probe older than the move: %v, want errStaleProbe", err)
	}
	if now := recordOf(t, e, "codex-01"); now.SessionID != "session:moved" {
		t.Fatalf("the move was undone: %+v", now.Place)
	}
}

// The arrival gate runs before the lease is marked adopted: a gate that
// fails leaves the adoption unfinished, so the next hook adopts again and
// tells the agent who it is.
func TestAFailedArrivalGateLeavesTheAdoptionToTheNextHook(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	in := HookInput{SessionID: "s1"}
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, in); err != nil {
		t.Fatal(err)
	}
	// An adoption left unfinished (its hook ran out of time), with mail
	// waiting and a link record the gate cannot read.
	s := seedOf(recordOf(t, e, "codex-01"))
	s.Adopting = true
	if err := putSeed(e.Store, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-01"), "m1.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := filepath.Join(e.Paths.Extensions(), "links")
	if err := os.MkdirAll(links, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(links, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, in); err == nil {
		t.Fatal("setup: the gate should fail on the broken link record")
	}
	if rec := recordOf(t, e, "codex-01"); !rec.Live() || rec.Adopted() {
		t.Fatalf("after a failed gate: live=%v adopted=%v, want live and still adopting", rec.Live(), rec.Adopted())
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(ctx, EventPostTool, core.Codex, Location{Block: block}, in)
	if err != nil || !strings.Contains(r.Notice, "you are codex-01") {
		t.Fatalf("the next hook did not finish the adoption: %q %v", r.Notice, err)
	}
}

// Replaying an Epic close finishes the quiet departures its first run could
// not (a waker that would not retire), without killing the session again.
func TestACloseReplayFinishesItsPendingDepartures(t *testing.T) {
	e, term, loc := closeWorld(t)
	worker := handleIn(t, e, "block:w")
	fake := e.AMQ.(*amq.Fake)
	e.AMQ = fake.WithFail(func(op string, args []string) error {
		if op == "wake check" && slices.Contains(args, string(worker)) {
			return errors.New("injected retire failure")
		}
		return nil
	})
	req := CloseRequest{Session: "session:epic", Asked: "close it", OpID: "me:close#1"}
	if _, err := closeEpic(e, loc, req); err == nil {
		t.Fatal("setup: the first close should report the unfinished departure")
	}
	e.AMQ = fake.WithFail(nil)
	if _, err := closeEpic(e, loc, req); err != nil {
		t.Fatal(err)
	}
	// Finished, the closed Epic's handle then leaves the root with its
	// identity (roster.go).
	if rec, ok, _ := e.agentOf(worker); ok || wakerLive(t, e, string(worker)) {
		t.Fatalf("the replay left the departure unfinished: %+v waker=%v", rec.departure(), wakerLive(t, e, string(worker)))
	}
	if !slices.Equal(term.killed, []string{"session:epic"}) {
		t.Errorf("killed %v, want one kill", term.killed)
	}
}

// Two collectors that read one unfinished departure tell its orchestrator
// once: the second finds it finished.
func TestTwoFinishersOfOneDepartureTellTheOrchestratorOnce(t *testing.T) {
	e, _, _ := watcherWorld(t)
	left, err := e.releaseLease(recordOf(t, e, "codex-03"), store.Departure{Reason: "agent_gone", At: time.Now(), Unfinished: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.finishDeparture(ctx, left); err != nil {
		t.Fatal(err)
	}
	told, _ := amq.Unread(e.Paths.Root(), "codex-01")
	if len(told) == 0 {
		t.Fatal("setup: the orchestrator was not told")
	}
	if err := e.finishDeparture(ctx, left); err != nil {
		t.Fatal(err)
	}
	if again, _ := amq.Unread(e.Paths.Root(), "codex-01"); len(again) != len(told) {
		t.Fatalf("the orchestrator was told %d times", len(again))
	}
}

// Two overlapping `agent title` calls leave the pane showing the task
// recorded last.
func TestOverlappingTaskWritesLeaveTheLatestLabel(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	done := make(chan error, 1)
	term.renaming = func(_, label string) {
		if strings.Contains(label, "older task") {
			once.Do(func() {
				go func() { _, err := e.SetTask(ctx, Location{Block: block}, "newer task"); done <- err }()
				time.Sleep(200 * time.Millisecond)
			})
		}
	}
	if _, err := e.SetTask(ctx, Location{Block: block}, "older task"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rec, got := recordOf(t, e, "codex-01"), term.renamed[block]; rec.Task != "newer task" || !strings.Contains(got, "newer task") {
		t.Fatal(fmt.Sprintf("recorded task %q, pane shows %q", rec.Task, got))
	}
}

// The arrival gate reads and may quarantine every unread message: however
// many there are, it holds no lock every hook on the host waits for.
func TestAnArrivalBacklogDoesNotHoldTheLifecycleLock(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{block: "window:1"}}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	in := HookInput{SessionID: "s1"}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.commitAdoption(ctx, p, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), string(c.rec.Handle)); err != nil {
		t.Fatal(err)
	}
	inbox := amq.Inbox(e.Paths.Root(), string(c.rec.Handle))
	for i := 0; i < 600; i++ {
		if err := os.WriteFile(filepath.Join(inbox, fmt.Sprintf("m%05d.md", i)), []byte("not a message"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { _, err := e.reconcileAdoption(ctx, p, c); done <- err }()
	var longest time.Duration
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if longest > time.Second {
				t.Fatalf("the lifecycle lock was held %v while the gate read the backlog", longest)
			}
			return
		default:
		}
		start := time.Now()
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		unlock, err := e.Store.LockLifecycle(lctx)
		cancel()
		if err != nil {
			t.Fatalf("the lifecycle lock was held through the backlog: %v", err)
		}
		unlock()
		longest = max(longest, time.Since(start))
		time.Sleep(5 * time.Millisecond)
	}
}

// An adoption whose record a session reconciliation moved while it
// reconciled is adopted again from a new probe, never reported with the old
// placement beside the new record.
func TestAnAdoptionMovedWhileItReconcilesIsTakenAgain(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	in := HookInput{SessionID: "s1"}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.commitAdoption(ctx, p, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	unlock, _ := e.Store.LockLifecycle(ctx)
	err = e.Store.Moved(c.rec.Handle, "session:moved", "window:moved", time.Now())
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reconcileAdoption(ctx, p, c); !errors.Is(err, errStaleProbe) {
		t.Fatalf("reconciling a moved adoption: %v, want errStaleProbe", err)
	}
	a, err := e.Adopt(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Session != a.Agent.SessionID || a.Window != a.Agent.WindowID {
		t.Fatalf("the result mixes placements: adoption %s/%s, record %s/%s", a.Session, a.Window, a.Agent.SessionID, a.Agent.WindowID)
	}
}

// A replacement whose probe saw the pane already showing its label still
// writes it under the pane lock: a superseded adoption's late write cannot
// leave the pane showing the old handle.
func TestAReplacementRelabelsAPaneASupersededAdoptionWroteLate(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	oldPID, newPID := os.Getpid(), os.Getppid()
	claudePane(term, block, oldPID)
	old := HookInput{SessionID: "old", AgentPID: oldPID}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Claude, old)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.commitAdoption(ctx, p, core.Claude, old)
	if err != nil {
		t.Fatal(err)
	}
	replaced := make(chan error, 1)
	var once sync.Once
	term.renaming = func(_, label string) {
		if !strings.HasPrefix(label, string(c.rec.Handle)) {
			return
		}
		once.Do(func() {
			// While the old write waits, the pane shows the replacement's
			// label and its agent starts.
			claudePane(term, block, newPID)
			term.mu.Lock()
			term.label = core.PaneLabel("claude-02", "", "")
			term.mu.Unlock()
			go func() {
				_, err := e.Adopt(ctx, Location{Block: block}, core.Claude, HookInput{SessionID: "new", AgentPID: newPID})
				replaced <- err
			}()
			time.Sleep(300 * time.Millisecond)
		})
	}
	if _, err := e.reconcileAdoption(ctx, p, c); !errors.Is(err, errSuperseded) {
		t.Fatalf("the old reconcile: %v, want errSuperseded", err)
	}
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
	if got := term.renamed[block]; got != core.PaneLabel("claude-02", "", "") {
		t.Fatalf("the pane shows %q, want the replacement's", got)
	}
}

// A close the guards refuse finishes nothing, not even a departure of the
// Epic it named.
func TestARefusedCloseFinishesNoDeparture(t *testing.T) {
	e, _, _ := closeWorld(t)
	worker := handleIn(t, e, "block:w")
	if _, err := e.releaseLease(recordOf(t, e, worker), store.Departure{Reason: store.DepartEpicClosed, At: time.Now(), Unfinished: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := closeEpic(e, Location{Block: "block:x"}, CloseRequest{Session: "session:epic", Asked: "close it"}); err == nil {
		t.Fatal("setup: a worker of another session should be refused")
	}
	if p := recordOf(t, e, worker).departure(); !p.Unfinished {
		t.Fatal("a refused close finished a departure of the Epic it named")
	}
}

// A retire's preview changes nothing, and a retire finishes only a
// retirement: another kind of pending departure is collection's to finish.
func TestRetireFinishesOnlyARetirementAndNeverInAPreview(t *testing.T) {
	e, _, _, _ := retireWorld(t)
	ctx := context.Background()
	for h, reason := range map[core.Handle]string{"codex-03": store.DepartRetired, "codex-04": store.DepartSessionEnd} {
		if _, err := e.releaseLease(recordOf(t, e, h), store.Departure{Reason: reason, At: time.Now(), Unfinished: true}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.Retire(ctx, Location{Block: "block:o"}, RetireRequest{Handle: "codex-03", DryRun: true})
	if err != nil || res.Action != RetireAlreadyDeparted {
		t.Fatalf("preview: %+v %v", res, err)
	}
	if !recordOf(t, e, "codex-03").departure().Unfinished || !wakerLive(t, e, "codex-03") {
		t.Fatal("a preview finished the retirement")
	}
	told, _ := amq.Unread(e.Paths.Root(), "codex-01")
	if _, err := e.Retire(ctx, Location{Block: "block:o"}, RetireRequest{Handle: "codex-04"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := amq.Unread(e.Paths.Root(), "codex-01"); !recordOf(t, e, "codex-04").departure().Unfinished || len(again) != len(told) {
		t.Fatalf("a retire finished a session_end departure: unfinished=%v notices %d -> %d", recordOf(t, e, "codex-04").departure().Unfinished, len(told), len(again))
	}
	if _, err := e.Retire(ctx, Location{Block: "block:o"}, RetireRequest{Handle: "codex-03"}); err != nil {
		t.Fatal(err)
	}
	if recordOf(t, e, "codex-03").departure().Unfinished || wakerLive(t, e, "codex-03") {
		t.Fatal("a retire did not finish the earlier retirement")
	}
}

// A reader moved while its arrival gate runs loses no mail it may read: each
// quarantine is decided again, under the lock placement writers hold, by the
// placements as they are then, and the gate stops once the reader moved.
func TestAReaderMovedDuringItsArrivalGateLosesNoMail(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{block: "window:1"}, sessions: map[string]string{"window:1": "session:s"}}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	in := HookInput{SessionID: "s1"}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.commitAdoption(ctx, p, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	reader := c.rec.Handle
	put(t, e, seed{Handle: "codex-09", Kind: core.Codex, BlockID: "block:s", SessionID: c.rec.SessionID, WindowID: "window:2", AgentPID: os.Getppid(), AdoptedAt: time.Now()})
	for _, h := range []string{string(reader), "codex-09"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), string(reader)), "0000-bad.md"), []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	const peerMail = 300
	for i := 0; i < peerMail; i++ {
		if _, _, err := e.AMQ.Post(ctx, e.Paths.Root(), "send", "--me", "codex-09", "--to", string(reader), "--subject", "s", "--body", "b"); err != nil {
			t.Fatal(err)
		}
	}
	// As the gate denies the malformed file, both agents move to another
	// session, as a session reconciliation records it.
	var once sync.Once
	now := e.Now
	e.Now = func() time.Time {
		once.Do(func() {
			go func() {
				unlock, err := e.Store.LockLifecycle(context.Background())
				if err != nil {
					return
				}
				defer unlock()
				_ = e.Store.Moved(reader, "session:new", "window:n1", time.Now())
				_ = e.Store.Moved("codex-09", "session:new", "window:n2", time.Now())
			}()
		})
		return now()
	}
	_, _, _ = e.admitHeld(ctx, c)
	ids, _ := e.Store.QuarantinedIDs(reader)
	for _, id := range ids {
		if q, _ := e.Store.QuarantineReason(reader, id); q.Decision.Reason != core.ReasonUnreadableHeader {
			t.Fatalf("peer mail %s was quarantined (%s) across the move", id, q.Decision.Reason)
		}
	}
	unread, _ := amq.Unread(e.Paths.Root(), string(reader))
	if n := len(slices.DeleteFunc(unread, func(id string) bool { return id == "0000-bad" })); n != peerMail {
		t.Fatalf("%d of %d peer messages left unread", n, peerMail)
	}
}

// A close whose op id another request claimed meanwhile is refused, and its
// refusal settles nothing.
func TestAConflictingCloseOpIDSettlesNothing(t *testing.T) {
	e, _, loc := closeWorld(t)
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "me:close#1"}); err != nil {
		t.Fatal(err)
	}
	caller := recordOf(t, e, "claude-01")
	me, err := e.gatewayEndpoint(caller)
	if err != nil {
		t.Fatal(err)
	}
	var settle closeSettle
	var res CloseResult
	if _, err := e.closedAlready(caller, me, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "me:close#1", Force: true}, "another-hash", &res, &settle); err == nil {
		t.Fatal("setup: the reused op id should conflict")
	}
	if settle.key != (core.SessionKey{}) {
		t.Fatalf("a conflicting close was authorized to settle %+v", settle.key)
	}
}

// adoptedWithBadMail adopts codex-01 in a placed block and leaves a
// malformed file in its inbox, which the arrival gate quarantines.
func adoptedWithBadMail(t *testing.T) Env {
	t.Helper()
	term := &fakeTerm{state: "idle", windows: map[string]string{block: "window:1"}, sessions: map[string]string{"window:1": "session:s"}}
	e := realAmqEnv(t, term)
	if _, err := e.Adopt(context.Background(), Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-01"), "0000-bad.md"), []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// A hook's arrival gate, whose quarantines take the lifecycle lock, runs
// before the hook takes its ledger lock: `agent join` takes a ledger lock
// under the lifecycle lock, so the other order could deadlock them.
func TestTheHookGateHoldsNoLedgerLockWhileItWaitsForTheLifecycleLock(t *testing.T) {
	e := adoptedWithBadMail(t)
	ctx := context.Background()
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Hook(ctx, EventPrompt, core.Codex, Location{Block: block}, HookInput{SessionID: "s1"})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // the hook now waits in its gate
	lctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	ledger, lerr := e.Store.LockLedger(lctx, "codex-01")
	cancel()
	if lerr == nil {
		ledger()
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if lerr != nil {
		t.Fatalf("the hook held its ledger lock while it waited for the lifecycle lock: %v", lerr)
	}
}

// A gate that cannot take the lifecycle lock gives up when its caller's
// time runs out, as a hook's does.
func TestTheGateWaitsForTheLifecycleLockOnlyAsLongAsItsCaller(t *testing.T) {
	e := adoptedWithBadMail(t)
	unlock, err := e.Store.LockLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.admit(ctx, recordOf(t, e, "codex-01")); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the gate quarantined while the lifecycle lock was held")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the gate kept waiting after its caller's time ran out")
	}
}

// A quarantine's reason names the sender it was judged by.
func TestAQuarantineReasonNamesItsSender(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{block: "window:1"}, sessions: map[string]string{"window:1": "session:s"}}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	put(t, e, seed{Handle: "codex-02", Kind: core.Codex, BlockID: "block:far", SessionID: "session:far", WindowID: "window:far", AgentPID: os.Getppid(), AdoptedAt: time.Now()})
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "codex-02"); err != nil {
		t.Fatal(err)
	}
	sent, _, err := e.AMQ.Post(ctx, e.Paths.Root(), "send", "--me", "codex-02", "--to", "codex-01", "--subject", "s", "--body", "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admit(ctx, recordOf(t, e, "codex-01")); err != nil {
		t.Fatal(err)
	}
	q, err := e.Store.QuarantineReason("codex-01", sent.ID)
	if err != nil || q.Decision.Allow || q.From.Handle != "codex-02" {
		t.Fatalf("quarantine reason %+v %v, want one naming codex-02", q, err)
	}
}
