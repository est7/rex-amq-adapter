package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Adopt runs as Probe (Rex, unlocked), Commit (records, under the lifecycle
// lock), and Reconcile (mailbox, waker, labels, unlocked). These tests pin
// what each phase owes the next.

// A hook that dies after Commit leaves the lease adopting, with neither
// mailbox nor waker: the next hook finishes the adoption.
func TestAnAdoptionCutShortAfterItsCommitIsFinishedByTheNextHook(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1"}
	p, err := e.probeAdoption(ctx, loc, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.commitAdoption(ctx, p, core.Codex, in); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := e.agentAt(block)
	if err != nil || !ok || !rec.Live() || rec.Adopted() {
		t.Fatalf("after Commit alone: %+v ok=%v err=%v", rec, ok, err)
	}
	if _, err := os.Stat(amq.Inbox(e.Paths.Root(), string(rec.Handle))); err == nil || wakerLive(t, e, string(rec.Handle)) {
		t.Fatal("Commit made a mailbox or a waker: it runs no amq")
	}
	if _, err := e.Hook(ctx, EventPrompt, core.Codex, loc, in); err != nil {
		t.Fatal(err)
	}
	again, _, _ := e.agentAt(block)
	if !again.Adopted() || again.Handle != rec.Handle || again.lease.Epoch != rec.lease.Epoch || !wakerLive(t, e, string(rec.Handle)) {
		t.Fatalf("the next hook did not finish the adoption: %+v waker=%v", again, wakerLive(t, e, string(rec.Handle)))
	}
	if _, err := os.Stat(amq.Inbox(e.Paths.Root(), string(rec.Handle))); err != nil {
		t.Fatalf("no mailbox after the adoption finished: %v", err)
	}
}

// A probe's verdict that the incumbent is gone is about the lease it saw. A
// lease taken since (the incumbent came back, or a third pane took over) is
// not released on it: Commit refuses the stale probe.
func TestCommitRefusesATakeoverVerdictAboutAnOlderLease(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	put(t, e, seed{Handle: "codex-01", Kind: core.Codex, BlockID: "block:old", SessionID: "session:s", AgentSessionID: "conv", AgentPID: 999999, AdoptedAt: time.Now()})
	in := HookInput{SessionID: "conv"}
	p, err := e.probeAdoption(ctx, Location{Block: block}, core.Codex, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.holder == nil || !p.holder.gone {
		t.Fatalf("setup: the probe should find the incumbent (pid 999999) gone: %+v", p.holder)
	}
	// The incumbent's lease is taken again by another process before Commit.
	old, _, _ := e.agentOf("codex-01")
	l := *old.lease
	l.Incarnation.PID, l.BlockID = os.Getppid(), "block:third"
	if _, err := e.Store.AcquireLease(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.commitAdoption(ctx, p, core.Codex, in); err != errStaleProbe {
		t.Fatalf("Commit on a stale takeover verdict: %v, want errStaleProbe", err)
	}
	now, _, _ := e.agentOf("codex-01")
	if !now.Live() || now.BlockID != "block:third" {
		t.Fatalf("the newer lease was released or moved: %+v", now)
	}
}

// The waker is derived from the lease: reconciling a handle whose lease is
// gone retires its waker, and one whose lease is live brings it back,
// whichever order the adoption and the departure that raced ran in.
func TestReconcileWakerFollowsTheLease(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	killWaker(t, e, "codex-01")
	if _, err := e.reconcileWaker(ctx, "codex-01"); err != nil || !wakerLive(t, e, "codex-01") {
		t.Fatalf("a live lease's waker was not brought back: %v", err)
	}
	rec, _, _ := e.agentOf("codex-01")
	if _, err := e.Store.ReleaseLease(rec.Handle, rec.lease.Epoch, store.Departure{Reason: store.DepartSessionEnd, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A late reconcile of the adoption that leased it runs after the release.
	if _, err := e.reconcileWaker(ctx, "codex-01"); err != nil || wakerLive(t, e, "codex-01") {
		t.Fatalf("a released lease kept its waker: %v", err)
	}
}

// An idle lead or narrator runs no hook, and an adoption whose hook ran out
// of time before its waker came up is finished only by the agent's next
// hook: the session watcher brings back both wakers on each look. A worker
// that finished its adoption keeps its own (its Stop hook supervises it).
func TestTheSessionWatcherKeepsTheWakersNoHookWould(t *testing.T) {
	e, _, key := watcherWorld(t)
	add(t, e, seed{Handle: "codex-04", Kind: core.Codex, Role: "implementer", Server: fakeServer, SessionID: "session:s", BlockID: "block:u", AgentPID: os.Getpid(), AdoptedAt: e.Now(), Adopting: true})
	keepers := []agent{recordOf(t, e, "codex-01"), recordOf(t, e, "codex-02")}
	if err := e.watchOnce(context.Background(), key, keepers); err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]bool{"codex-01": true, "codex-02": true, "codex-03": false, "codex-04": true} {
		if got := wakerLive(t, e, h); got != want {
			t.Errorf("%s waker live=%v after the watcher's look, want %v", h, got, want)
		}
	}
}
