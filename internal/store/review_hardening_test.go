package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A lockless reader lists a directory while a writer that holds the lifecycle
// lock removes one of its files (ReleaseLease, ForgetIdentity): the file that
// vanished between the listing and the read was never there. A dangling
// symlink is that window held open.
func TestLeaseListingSurvivesReleaseRace(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "claude-01", Kind: core.Claude})
	mustLease(t, s, leaseIn("claude-01", "block:a", 7), t0)
	for _, dir := range []string{s.P.leases(), s.P.identities()} {
		if err := os.Symlink(filepath.Join(dir, "gone.target"), filepath.Join(dir, "codex-02.json")); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := s.Leases()
	if err != nil || len(leases) != 1 || leases[0].Handle != "claude-01" {
		t.Fatalf("leases with one removed mid-listing: %+v %v", leases, err)
	}
	ids, err := s.Identities()
	if err != nil || len(ids) != 1 || ids[0].Handle != "claude-01" {
		t.Fatalf("identities with one removed mid-listing: %+v %v", ids, err)
	}
	if _, ok, err := s.LeaseAt("block:a"); err != nil || !ok {
		t.Fatalf("LeaseAt with a lease removed mid-listing: ok=%v err=%v", ok, err)
	}
}

// The same race with real writers: one handle acquires and releases its lease
// over and over while lockless readers list everything.
func TestLeaseReadersDuringReleaseRace(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "claude-01", Kind: core.Claude})
	mustLease(t, s, leaseIn("claude-01", "block:keep", 7), t0)
	mustIdentity(t, s, Identity{Handle: "codex-02", Kind: core.Codex})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			l, err := s.AcquireLease(leaseIn("codex-02", "block:churn", 8), t0)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := s.ReleaseLease("codex-02", l.Epoch, Departure{Reason: DepartSessionEnd, At: t0}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if _, err := s.Leases(); err != nil {
			t.Errorf("a lockless reader racing ReleaseLease failed: %v", err)
			break
		}
		if _, _, err := s.LeaseAt("block:keep"); err != nil {
			t.Errorf("LeaseAt racing ReleaseLease failed: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

// Two servers' processes are different processes even when their pids are
// equal, and a remote incarnation's start time is unknown: a lease acquired
// on another server starts the next epoch, so the old holder's epoch cannot
// release it (I4).
func TestLeaseAdvancesEpochAcrossServers(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "claude-01", Kind: core.Claude})
	old := Lease{Handle: "claude-01", Place: Place{Server: "tcp://old-host", BlockID: "block:old"}, Incarnation: Incarnation{PID: 42}}
	first := mustLease(t, s, old, t0)
	moved := old
	moved.Place = Place{Server: "tcp://new-host", BlockID: "block:new"}
	next := mustLease(t, s, moved, t0)
	if next.Epoch != first.Epoch+1 {
		t.Fatalf("the same pid on another server kept epoch %d -> %d", first.Epoch, next.Epoch)
	}
	if released, err := s.ReleaseLease("claude-01", first.Epoch, Departure{Reason: DepartSessionEnd, At: t0}); err != nil || released {
		t.Fatalf("the old server's epoch released the new lease: %v %v", released, err)
	}
	// The same process acquiring again on its own server keeps its epoch.
	again := mustLease(t, s, moved, t0)
	if again.Epoch != next.Epoch {
		t.Fatalf("the same process re-acquiring moved the epoch %d -> %d", next.Epoch, again.Epoch)
	}
}
