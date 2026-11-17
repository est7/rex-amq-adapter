package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The injector is told a handle, not a block (spec §4.3): it finds the block
// from the handle's lease when the doorbell fires.

func TestInjectRingsTheBlockTheLeaseNames(t *testing.T) {
	const moved = "block:b"
	f := &fakeTerm{state: "idle", submit: true, closed: []string{block}}
	e := fixture(t, f, "m1")
	l := leaseOf(t, e, "codex")
	l.BlockID = moved
	if _, err := e.Store.AcquireLease(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if o := inject(e); o.Progress != core.Accepted || len(f.writes) != 1 {
		t.Fatalf("a doorbell for a lease in %s, with %s closed: %+v writes=%q", moved, block, o, f.writes)
	}
}

func TestInjectWithoutALeaseFailsNoLease(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m1")
	l := leaseOf(t, e, "codex")
	if ok, err := e.Store.ReleaseLease("codex", l.Epoch, store.Departure{Reason: store.DepartSessionEnd, At: time.Now()}); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	if o := inject(e); o.Progress != core.Failed || o.Code != "no_lease" || len(f.writes) != 0 {
		t.Fatalf("a handle that runs nowhere: %+v writes=%q", o, f.writes)
	}
}

// A dead holder fails the cohort and changes nothing: releasing its lease is
// collection's, which also tells its session.
func TestInjectToADeadHolderFailsAndLeavesTheLease(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m1")
	got, _, _ := e.agentAt(block)
	s := seedOf(got)
	s.AgentPID = 0x3ffffff0 // no such process
	if err := putSeed(e.Store, s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Paths.Home, "leases", "codex.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if o := inject(e); o.Progress != core.Failed || o.Code != "holder_gone" || len(f.writes) != 0 {
		t.Fatalf("a dead holder: %+v writes=%q", o, f.writes)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("the injector changed the lease:\n%s\nthen\n%s (%v)", before, after, err)
	}
}

// A lease that moves while the injector waits for its block's lock defers
// the doorbell instead of ringing the block it locked.
func TestInjectDefersWhenTheLeaseMovesWhileItWaits(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m1")
	ctx := context.Background()
	unlock, err := e.Store.LockInject(ctx, block)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan core.Outcome, 1)
	go func() { done <- inject(e) }()
	// The injector reads the lease, then waits on the lock held above.
	time.Sleep(300 * time.Millisecond)
	l := leaseOf(t, e, "codex")
	l.BlockID = "block:b"
	if _, err := e.Store.AcquireLease(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case o := <-done:
		if o.Progress != core.Deferred || o.Code != "lease_moved" || len(f.writes) != 0 {
			t.Fatalf("a lease moved under the lock: %+v writes=%q", o, f.writes)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the injector never finished")
	}
}
