package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
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
	if err := e.finishAdoption(ctx, c); !errors.Is(err, errSuperseded) {
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
	_, _ = e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal})
	if len(term.sessions_) != 1 {
		t.Fatalf("setup: sessions created %d", len(term.sessions_))
	}
	if _, ok, err := e.Store.SessionPolicy("session:epic-1"); err != nil || !ok {
		t.Fatalf("the created Epic session has no policy: %v %v", ok, err)
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
