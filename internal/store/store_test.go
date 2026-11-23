package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func TestLedger(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	got, err := s.Announced("bob")
	if err != nil || len(got.IDs) != 0 || !got.At.IsZero() {
		t.Fatalf("empty ledger: %+v %v", got, err)
	}
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	if err := s.SetAnnounced("bob", []string{"m2", "m1"}, at); err != nil {
		t.Fatal(err)
	}
	got, err = s.Announced("bob")
	if err != nil || len(got.IDs) != 2 || !got.IDs["m1"] || !got.IDs["m2"] || !got.At.Equal(at) {
		t.Fatalf("ledger: %+v %v", got, err)
	}
	if other, _ := s.Announced("alice"); len(other.IDs) != 0 {
		t.Fatalf("ledgers must be per handle: %+v", other)
	}
}

func TestInjectHistory(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if h, err := s.InjectHistory("bob"); err != nil || len(h) != 0 {
		t.Fatalf("empty history: %v %v", h, err)
	}
	t0 := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	_ = s.RecordInject("bob", core.Outcome{Progress: core.Deferred, Code: "draft_in_box"}, t0)
	_ = s.RecordInject("bob", core.Outcome{Progress: core.Accepted}, t0.Add(time.Second))
	// A torn line from a crash mid-append is skipped, not fatal.
	f, _ := os.OpenFile(s.injectLog("bob"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"at":"2026-10`)
	f.Close()
	h, err := s.InjectHistory("bob")
	if err != nil || len(h) != 2 || h[0].Code != "draft_in_box" || h[1].Progress != core.Accepted || !h[1].At.Equal(t0.Add(time.Second)) {
		t.Fatalf("history: %+v %v", h, err)
	}
}

func TestInjectLogIsTrimmedToItsNewestLines(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	t0 := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	note := strings.Repeat("x", 1200) // about 1.3 KB per line: the cap is crossed near line 800
	n := 0
	for ; n < 2000; n++ {
		if err := s.RecordInject("bob", core.Outcome{Progress: core.Deferred, Code: "agent_working", Note: note}, t0.Add(time.Duration(n)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := os.Stat(s.injectLog("bob"))
	if st.Size() > maxInjectLog {
		t.Fatalf("log grew to %d bytes", st.Size())
	}
	h, _ := s.InjectHistory("bob")
	if len(h) == 0 || !h[len(h)-1].At.Equal(t0.Add(time.Duration(n-1)*time.Second)) {
		t.Fatalf("after trimming: %d entries, newest lost", len(h))
	}
	// Trimming leaves headroom: the newest entries survive in order.
	for i := 1; i < len(h); i++ {
		if !h[i].At.After(h[i-1].At) {
			t.Fatalf("entries out of order at %d", i)
		}
	}
}

func TestPendingAndAdoptionFailures(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if _, ok, err := s.Pending("block:a"); ok || err != nil {
		t.Fatalf("no pending: %v %v", ok, err)
	}
	if err := s.SetPending("block:a", "doorbell"); err != nil {
		t.Fatal(err)
	}
	if d, ok, err := s.Pending("block:a"); !ok || err != nil || d != "doorbell" {
		t.Fatalf("pending: %q %v %v", d, ok, err)
	}
	if _, ok, _ := s.Pending("block:b"); ok {
		t.Fatal("pending must be per block")
	}
	if err := s.ClearPending("block:a"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPending("block:a"); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
}

func TestPathsForRootRoundTrips(t *testing.T) {
	p := Paths{Home: "/var/state/rex-amq-adapter"}
	got, err := PathsForRoot(p.Root())
	if err != nil || got != p {
		t.Fatalf("got %+v %v", got, err)
	}
	for _, bad := range []string{"relative/amq-root", "/var/state/other", ""} {
		if _, err := PathsForRoot(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The tab ledger keeps each window's entry apart from the per-handle notes
// under window/, and a window it never recorded reads as unknown.
func TestWindowLedgerStore(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if _, ok, err := s.Tab("window:1"); ok || err != nil {
		t.Fatalf("unknown window: ok=%v err=%v", ok, err)
	}
	want := Tab{WindowID: "window:1", SessionID: "session:s", Mode: core.WindowAuto, LastLabel: "implementer(pi-01) | fix"}
	if err := s.PutTab(want); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWindowNote("window:1", "a note for a handle that happens to look like a window"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Tab("window:1")
	if err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	if st := got.State(); st.Mode != core.WindowAuto || st.Last != "implementer(pi-01) | fix" {
		t.Fatalf("state %+v", st)
	}
	for _, m := range []core.WindowMode{core.WindowAuto, core.WindowManual} {
		want.Mode = m
		if err := s.PutTab(want); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.Tab("window:1"); got.Mode != m {
			t.Errorf("mode %q read back as %q", m, got.Mode)
		}
	}
	if err := s.PutTab(Tab{Mode: core.WindowAuto}); err == nil {
		t.Error("an entry without a window was recorded")
	}
}

// A tab an earlier version claimed for a crew or a narrator reads as a
// follower, keeping the label it was given, so its agent's pane label
// replaces it; its crew is dropped (a crew is its session's).
func TestWindowLedgerReadsALegacyClaimedTabAsAFollower(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	for mode, label := range map[string]string{"crew": "ship | release", "narrator": "narrator"} {
		raw := `{"window_id":"window:1","session_id":"session:s","mode":"` + mode + `","crew":"ship","last_label":"` + label + `"}` + "\n"
		if err := os.MkdirAll(filepath.Dir(s.tabPath("window:1")), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.tabPath("window:1"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Tab("window:1")
		want := Tab{WindowID: "window:1", SessionID: "session:s", Mode: core.WindowAuto, LastLabel: label}
		if err != nil || !ok || got != want {
			t.Errorf("legacy %s tab read as %+v (ok=%v err=%v), want %+v", mode, got, ok, err, want)
		}
	}
}

// A session's crew reads back as written, and a session without one reads
// as none.
func TestSessionCrewStore(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if _, ok, err := s.SessionCrew("session:s"); ok || err != nil {
		t.Fatalf("no crew yet: ok=%v err=%v", ok, err)
	}
	if err := s.PutSessionCrew("session:s", SessionCrew{}); err == nil {
		t.Error("a session crew without a crew was recorded")
	}
	want := SessionCrew{Crew: "pair", ClaimedBy: "pi-01", ClaimedAt: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	if err := s.PutSessionCrew("session:s", want); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.SessionCrew("session:s"); err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	if _, ok, _ := s.SessionCrew("session:t"); ok {
		t.Error("another session took the crew")
	}
}
