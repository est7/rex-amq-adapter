package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func testStore(t *testing.T) Store {
	t.Helper()
	return Store{P: Paths{Home: t.TempDir()}}
}

func mustIdentity(t *testing.T, s Store, id Identity) {
	t.Helper()
	if err := s.PutIdentity(id); err != nil {
		t.Fatal(err)
	}
}

func mustLease(t *testing.T, s Store, l Lease, at time.Time) Lease {
	t.Helper()
	got, err := s.AcquireLease(l, at)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func leaseIn(h core.Handle, block string, pid int) Lease {
	return Lease{Handle: h, Place: Place{Server: "unix:///rex.sock", SessionID: "session:a", WindowID: "window:1", BlockID: block}, Incarnation: Incarnation{PID: pid, StartTime: int64(pid) * 10}}
}

func TestIdentityRoundTripsAndLists(t *testing.T) {
	s := testStore(t)
	if _, ok, err := s.IdentityOf("claude-01"); ok || err != nil {
		t.Fatalf("missing identity: ok=%v err=%v", ok, err)
	}
	want := Identity{Handle: "codex-01", Kind: core.Codex, Role: "reviewer", Task: "review", Cwd: "/w", FirstAdoptedAt: t0, Sessions: []string{"s1", "s2"}}
	mustIdentity(t, s, want)
	mustIdentity(t, s, Identity{Handle: "claude-01", Kind: core.Claude})
	got, ok, err := s.IdentityOf("codex-01")
	if err != nil || !ok || got.Role != "reviewer" || got.Session() != "s2" || !got.FirstAdoptedAt.Equal(t0) {
		t.Fatalf("identity: %+v ok=%v err=%v", got, ok, err)
	}
	all, err := s.Identities()
	if err != nil || len(all) != 2 || all[0].Handle != "claude-01" || all[1].Handle != "codex-01" {
		t.Fatalf("identities sorted by handle: %+v %v", all, err)
	}
}

func TestIdentityNeedsAHandle(t *testing.T) {
	s := testStore(t)
	if err := s.PutIdentity(Identity{Kind: core.Claude}); err == nil {
		t.Fatal("identity without a handle accepted")
	}
}

// I1: an agent session belongs to one handle. A second handle of the same
// kind that names it, as its current session or an earlier one, is refused;
// another kind's session of the same id is another session.
func TestIdentityRefusesAnotherHandlesSession(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "claude-38", Kind: core.Claude, Sessions: []string{"s0", "5e13"}})
	for _, sessions := range [][]string{{"5e13"}, {"s0"}, {"new", "s0"}} {
		err := s.PutIdentity(Identity{Handle: "claude-132", Kind: core.Claude, Sessions: sessions})
		if !errors.Is(err, ErrSessionTaken) {
			t.Fatalf("sessions %v: err %v, want ErrSessionTaken", sessions, err)
		}
	}
	if _, ok, _ := s.IdentityOf("claude-132"); ok {
		t.Fatal("a refused identity was written")
	}
	if err := s.PutIdentity(Identity{Handle: "codex-01", Kind: core.Codex, Sessions: []string{"5e13"}}); err != nil {
		t.Fatalf("another kind's session: %v", err)
	}
	if err := s.PutIdentity(Identity{Handle: "claude-38", Kind: core.Claude, Sessions: []string{"s0", "5e13", "s2"}}); err != nil {
		t.Fatalf("the owner rewriting its own sessions: %v", err)
	}
}

func TestIdentityWithSessionMakesItCurrent(t *testing.T) {
	id := Identity{Handle: "claude-01", Kind: core.Claude}
	id = id.WithSession("s1").WithSession("s2").WithSession("").WithSession("s2")
	if got := id.Sessions; len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("sessions %v", got)
	}
	if got := id.WithSession("s1").Sessions; len(got) != 2 || got[0] != "s2" || got[1] != "s1" {
		t.Fatalf("a resumed earlier session becomes current once: %v", got)
	}
	if len(id.Sessions) != 2 || id.Sessions[1] != "s2" {
		t.Fatalf("WithSession changed its receiver: %v", id.Sessions)
	}
}

// I2: a handle has one lease, the file named for it; leasing another block
// moves it.
func TestLeaseIsOnePerHandle(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "codex-01", Kind: core.Codex})
	mustLease(t, s, leaseIn("codex-01", "block:a", 10), t0)
	mustLease(t, s, leaseIn("codex-01", "block:b", 10), t0.Add(time.Minute))
	entries, err := os.ReadDir(s.P.leases())
	if err != nil || len(entries) != 1 || entries[0].Name() != "codex-01.json" {
		t.Fatalf("lease files %v (%v), want codex-01.json alone", entries, err)
	}
	if _, ok, _ := s.LeaseAt("block:a"); ok {
		t.Fatal("the lease stayed on the block it left")
	}
	if l, ok, _ := s.LeaseAt("block:b"); !ok || l.Handle != "codex-01" {
		t.Fatalf("lease at block:b: %+v %v", l, ok)
	}
}

func TestLeaseNeedsAnIdentity(t *testing.T) {
	s := testStore(t)
	if _, err := s.AcquireLease(leaseIn("ghost", "block:a", 1), t0); err == nil {
		t.Fatal("a lease without an identity was acquired")
	}
	if _, err := s.AcquireLease(Lease{Handle: "ghost"}, t0); err == nil {
		t.Fatal("a lease without a block was acquired")
	}
}

// I3: a block runs one agent. Acquiring it releases the lease another
// handle held there, archived in that identity as replaced.
func TestLeaseOnABlockReplacesAnotherHandles(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "codex-01", Kind: core.Codex})
	mustIdentity(t, s, Identity{Handle: "claude-02", Kind: core.Claude})
	old := mustLease(t, s, leaseIn("codex-01", "block:a", 10), t0)
	at := t0.Add(time.Hour)
	mustLease(t, s, leaseIn("claude-02", "block:a", 20), at)
	if _, live, _ := s.LeaseOf("codex-01"); live {
		t.Fatal("the replaced handle kept its lease")
	}
	if l, ok, _ := s.LeaseAt("block:a"); !ok || l.Handle != "claude-02" {
		t.Fatalf("lease at block:a: %+v", l)
	}
	id, _, _ := s.IdentityOf("codex-01")
	p := id.LastPlacement
	if p == nil || p.DepartReason != DepartReplaced || p.BlockID != "block:a" || p.Epoch != old.Epoch || !p.DepartedAt.Equal(at) || !p.Unfinished {
		t.Fatalf("archived placement %+v, want block:a replaced at %v with notices owed", p, at)
	}
	if got, ok, _ := s.DepartedFrom("block:a"); !ok || got.Handle != "codex-01" {
		t.Fatalf("departed from block:a: %+v %v", got, ok)
	}
	// Another server's block of the same id is another block.
	mustIdentity(t, s, Identity{Handle: "pi-03", Kind: core.Pi})
	remote := leaseIn("pi-03", "block:a", 30)
	remote.Server = "unix:///elsewhere.sock"
	mustLease(t, s, remote, at)
	if _, live, _ := s.LeaseOf("claude-02"); !live {
		t.Fatal("a lease on another server's block of the same id replaced this one")
	}
}

// I4: release is a compare-and-swap on the epoch. A stale epoch (a late
// hook of a process that no longer holds the lease) changes nothing.
func TestLeaseReleaseWithAStaleEpochIsANoOp(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "claude-38", Kind: core.Claude})
	first := mustLease(t, s, leaseIn("claude-38", "block:a", 10), t0)
	again := mustLease(t, s, leaseIn("claude-38", "block:a", 10), t0.Add(time.Minute))
	if again.Epoch != first.Epoch {
		t.Fatalf("the same process re-acquiring moved the epoch %d -> %d", first.Epoch, again.Epoch)
	}
	next := mustLease(t, s, leaseIn("claude-38", "block:b", 11), t0.Add(2*time.Minute))
	if next.Epoch != first.Epoch+1 {
		t.Fatalf("a new process took epoch %d, want %d", next.Epoch, first.Epoch+1)
	}
	released, err := s.ReleaseLease("claude-38", first.Epoch, Departure{Reason: DepartSessionEnd, At: t0.Add(3 * time.Minute)})
	if err != nil || released {
		t.Fatalf("stale release: released=%v err=%v", released, err)
	}
	if l, live, _ := s.LeaseOf("claude-38"); !live || l.Epoch != next.Epoch || l.BlockID != "block:b" {
		t.Fatalf("after a stale release the lease is %+v live=%v", l, live)
	}
	if id, _, _ := s.IdentityOf("claude-38"); id.LastPlacement != nil {
		t.Fatalf("a stale release archived %+v", id.LastPlacement)
	}
	gone := t0.Add(4 * time.Minute)
	released, err = s.ReleaseLease("claude-38", next.Epoch, Departure{Reason: DepartSessionEnd, At: gone})
	if err != nil || !released {
		t.Fatalf("current release: released=%v err=%v", released, err)
	}
	id, _, _ := s.IdentityOf("claude-38")
	if p := id.LastPlacement; p == nil || p.BlockID != "block:b" || p.Epoch != next.Epoch || p.DepartReason != DepartSessionEnd || p.Unfinished {
		t.Fatalf("archived %+v", p)
	}
	if released, _ := s.ReleaseLease("claude-38", next.Epoch, Departure{Reason: DepartSessionEnd, At: gone}); released {
		t.Fatal("releasing twice released again")
	}
	// The count continues past the departure, so the released epoch can never
	// release the next lease.
	if l := mustLease(t, s, leaseIn("claude-38", "block:c", 10), gone); l.Epoch != next.Epoch+1 {
		t.Fatalf("epoch after a departure %d, want %d", l.Epoch, next.Epoch+1)
	}
	if released, _ := s.ReleaseLease("claude-38", next.Epoch, Departure{Reason: DepartSessionEnd, At: gone}); released {
		t.Fatal("the departed epoch released its successor")
	}
}

// A pid the kernel reused for another process is another incarnation; an
// unreadable start time cannot tell them apart.
func TestIncarnationIsPIDAndStartTime(t *testing.T) {
	for _, c := range []struct {
		a, b Incarnation
		same bool
	}{
		{Incarnation{PID: 1, StartTime: 5}, Incarnation{PID: 1, StartTime: 5}, true},
		{Incarnation{PID: 1, StartTime: 5}, Incarnation{PID: 1, StartTime: 6}, false},
		{Incarnation{PID: 1, StartTime: 5}, Incarnation{PID: 2, StartTime: 5}, false},
		{Incarnation{PID: 1}, Incarnation{PID: 1, StartTime: 6}, true},
	} {
		if got := c.a.Same(c.b); got != c.same {
			t.Errorf("%+v.Same(%+v) = %v", c.a, c.b, got)
		}
	}
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "pi-01", Kind: core.Pi})
	l := leaseIn("pi-01", "block:a", 7)
	first := mustLease(t, s, l, t0)
	l.Incarnation.StartTime++
	if reused := mustLease(t, s, l, t0); reused.Epoch != first.Epoch+1 {
		t.Fatalf("a reused pid kept epoch %d", reused.Epoch)
	}
}

// The fence counts moves between sessions, through leases and Moved alike:
// a move between tabs of a session is not one.
func TestLeaseMovesTheFenceBetweenSessions(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "codex", Kind: core.Codex, Role: "reviewer"})
	l := mustLease(t, s, leaseIn("codex", "block:a", 1), t0)
	fence := func() Fence { id, _, _ := s.IdentityOf("codex"); return id.Fence }
	if f := fence(); f.Generation != 0 || f.SessionID != "session:a" || !f.PlacedAt.IsZero() {
		t.Fatalf("first placement: %+v", f)
	}
	if err := s.Moved("codex", "session:a", "window:2", t0); err != nil {
		t.Fatal(err)
	}
	if f := fence(); f.Generation != 0 {
		t.Fatalf("tab move: %+v", f)
	}
	if l, _, _ := s.LeaseOf("codex"); l.WindowID != "window:2" || l.Epoch != 1 {
		t.Fatalf("lease after a tab move: %+v", l)
	}
	if err := s.Moved("codex", "session:b", "window:X", t0); err != nil {
		t.Fatal(err)
	}
	back := t0.Add(time.Hour)
	l.SessionID = "session:a"
	mustLease(t, s, l, back)
	id, _, _ := s.IdentityOf("codex")
	if f := id.Fence; f.Generation != 2 || !f.PlacedAt.Equal(back) {
		t.Fatalf("after leaving and returning: %+v", f)
	}
	if ep := id.Endpoint(); ep.Generation != 2 || ep.SessionID != "session:a" || ep.Role != "reviewer" {
		t.Fatalf("endpoint %+v", ep)
	}
	// A departed handle's block can still move; its last placement follows.
	if _, err := s.ReleaseLease("codex", 1, Departure{Reason: DepartSessionEnd, At: back}); err != nil {
		t.Fatal(err)
	}
	if err := s.Moved("codex", "session:c", "window:9", back); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.IdentityOf("codex")
	if p := id.LastPlacement; p.SessionID != "session:c" || p.WindowID != "window:9" || id.Fence.Generation != 3 {
		t.Fatalf("departed move: %+v fence %+v", p, id.Fence)
	}
}

func TestLeaseFinishAdoptionIsForItsEpoch(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "codex-01", Kind: core.Codex})
	l := leaseIn("codex-01", "block:a", 1)
	l.Adopting = true
	l = mustLease(t, s, l, t0)
	if err := s.FinishAdoption("codex-01", l.Epoch+1); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LeaseOf("codex-01"); !got.Adopting {
		t.Fatal("another epoch finished the adoption")
	}
	if err := s.FinishAdoption("codex-01", l.Epoch); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LeaseOf("codex-01"); got.Adopting {
		t.Fatal("the adoption stayed unfinished")
	}
}

// A block can hold one live agent and the departed identities of earlier
// ones; DepartedFrom returns the last to leave that is not running elsewhere.
func TestIdentityDepartedFromIsTheLastToLeaveTheBlock(t *testing.T) {
	s := testStore(t)
	for i, h := range []core.Handle{"codex-01", "codex-02", "codex-03"} {
		mustIdentity(t, s, Identity{Handle: h, Kind: core.Codex})
		l := mustLease(t, s, leaseIn(h, "block:a", i+1), t0)
		if _, err := s.ReleaseLease(h, l.Epoch, Departure{Reason: DepartSessionEnd, At: t0.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, _ := s.DepartedFrom("block:a"); !ok || got.Handle != "codex-03" {
		t.Fatalf("departed only: got %+v, want the last to depart", got)
	}
	mustLease(t, s, leaseIn("codex-03", "block:b", 9), t0.Add(5*time.Hour))
	if got, ok, _ := s.DepartedFrom("block:a"); !ok || got.Handle != "codex-02" {
		t.Fatalf("one resumed elsewhere: got %+v, want codex-02", got)
	}
	if _, ok, _ := s.DepartedFrom("block:none"); ok {
		t.Fatal("a block nobody left has a departed identity")
	}
}

// A handle is reused once its identity is forgotten; the next agent must
// not inherit the previous one's doorbell history or announcements. A
// running handle is never forgotten.
func TestIdentityForgetDropsDeliveryHistory(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "codex-01", Kind: core.Codex})
	l := mustLease(t, s, leaseIn("codex-01", "block:a", 1), t0)
	if err := s.SetAnnounced("codex-01", []string{"m1"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInject("codex-01", core.Outcome{Progress: core.Accepted}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetIdentity("codex-01"); err == nil {
		t.Fatal("a running handle was forgotten")
	}
	if _, err := s.ReleaseLease("codex-01", l.Epoch, Departure{Reason: DepartSessionEnd, At: t0}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.ForgetIdentity("codex-01"); err != nil {
			t.Fatalf("forgetting (twice is a no-op): %v", err)
		}
	}
	if _, ok, _ := s.IdentityOf("codex-01"); ok {
		t.Fatal("identity survived")
	}
	if l, err := s.Announced("codex-01"); err != nil || len(l.IDs) != 0 {
		t.Fatalf("ledger survived: %+v %v", l, err)
	}
	if h, err := s.InjectHistory("codex-01"); err != nil || len(h) != 0 {
		t.Fatalf("inject log survived: %+v %v", h, err)
	}
}

// An unreadable identity or lease is an error rather than skipped: it may
// hold the session I1 protects, or the lease that names a waker.
func TestIdentityAndLeaseCorruptFilesAreErrors(t *testing.T) {
	s := testStore(t)
	mustIdentity(t, s, Identity{Handle: "x", Kind: core.Codex})
	mustLease(t, s, leaseIn("x", "block:x", 1), t0)
	for _, dir := range []string{s.P.identities(), s.P.leases()} {
		if err := os.WriteFile(filepath.Join(dir, "y.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Identities(); err == nil {
		t.Fatal("a corrupt identity was skipped")
	}
	if _, err := s.Leases(); err == nil {
		t.Fatal("a corrupt lease was skipped")
	}
	if err := s.PutIdentity(Identity{Handle: "z", Kind: core.Codex}); err == nil {
		t.Fatal("an identity was written without checking every other one (I1)")
	}
}

func TestLeaseChangesMoveAgentsChanged(t *testing.T) {
	s := testStore(t)
	if at, err := s.AgentsChanged(); err != nil || !at.IsZero() {
		t.Fatalf("empty state: %v %v", at, err)
	}
	mustIdentity(t, s, Identity{Handle: "x", Kind: core.Codex})
	before, _ := s.AgentsChanged()
	if before.IsZero() {
		t.Fatal("an identity write left no stamp")
	}
}
