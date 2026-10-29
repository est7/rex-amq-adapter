package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func TestRecordRoundTripAndListing(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if _, ok, err := s.Record("block:none"); ok || err != nil {
		t.Fatalf("missing record: ok=%v err=%v", ok, err)
	}
	gone := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	recs := []Record{
		{Handle: "codex", Kind: core.Codex, BlockID: "block:b", AgentPID: 2},
		{Handle: "claude", Kind: core.Claude, BlockID: "block:a", AgentPID: 1, DepartedAt: &gone},
	}
	for _, r := range recs {
		if err := s.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Record("block:a")
	if err != nil || !ok || got.Handle != "claude" || got.Live() || !got.DepartedAt.Equal(gone) {
		t.Fatalf("record a: %+v ok=%v err=%v", got, ok, err)
	}
	all, err := s.Records()
	if err != nil || len(all) != 2 || all[0].Handle != "claude" || all[1].Handle != "codex" {
		t.Fatalf("records: %+v %v", all, err)
	}
	if err := s.DeleteRecord("claude"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecord("claude"); err != nil {
		t.Fatalf("deleting twice must be a no-op: %v", err)
	}
	if all, _ := s.Records(); len(all) != 1 {
		t.Fatalf("after delete: %+v", all)
	}
}

// A block can hold one live agent and the departed records of earlier
// sessions; Record returns the live one, else the last to depart.
func TestRecordPrefersTheLiveAgentOfABlock(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	early := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	for _, r := range []Record{
		{Handle: "codex-01", BlockID: "block:a", DepartedAt: &early},
		{Handle: "codex-02", BlockID: "block:a", DepartedAt: &late},
		{Handle: "codex-03", BlockID: "block:b"},
	} {
		if err := s.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, _ := s.Record("block:a"); !ok || got.Handle != "codex-02" {
		t.Fatalf("departed only: got %+v, want the last to depart", got)
	}
	if err := s.PutRecord(Record{Handle: "claude-01", BlockID: "block:a"}); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.Record("block:a"); !ok || got.Handle != "claude-01" || !got.Live() {
		t.Fatalf("with a live agent: got %+v", got)
	}
}

// Earlier versions stored a record under its block's name. Writing or
// deleting its handle must not leave that file behind as a second record.
func TestBlockNamedRecordIsReplaced(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if err := os.MkdirAll(s.P.records(), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"handle":"codex-2","kind":"codex","block_id":"block:a","agent_pid":7}`)
	if err := os.WriteFile(filepath.Join(s.P.records(), "block_a.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.Record("block:a"); !ok || got.Handle != "codex-2" {
		t.Fatalf("legacy record unreadable: %+v", got)
	}
	gone := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	if err := s.PutRecord(Record{Handle: "codex-2", BlockID: "block:a", AgentPID: 7, DepartedAt: &gone}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Records()
	if err != nil || len(all) != 1 || all[0].Live() {
		t.Fatalf("after rewrite: %+v %v", all, err)
	}
	if err := s.DeleteRecord("codex-2"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Records(); len(all) != 0 {
		t.Fatalf("after delete: %+v", all)
	}
}

// A handle is reused once its record is collected; the next agent must not
// inherit the previous one's doorbell history or announcements.
func TestDeleteRecordForgetsDeliveryHistory(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	at := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	if err := s.PutRecord(Record{Handle: "codex-01", BlockID: "block:a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnounced("codex-01", []string{"m1"}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordInject("codex-01", core.Outcome{Progress: core.Accepted}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecord("codex-01"); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Announced("codex-01"); err != nil || len(l.IDs) != 0 {
		t.Fatalf("ledger survived: %+v %v", l, err)
	}
	if h, err := s.InjectHistory("codex-01"); err != nil || len(h) != 0 {
		t.Fatalf("inject log survived: %+v %v", h, err)
	}
}

func TestCorruptRecordIsAnError(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if err := s.PutRecord(Record{Handle: "x", BlockID: "block:x"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.P.records(), "block_y.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Records(); err == nil {
		t.Fatal("a corrupt record may hold a waker generation; listing must not skip it")
	}
}

func TestPutRecordNeedsIdentity(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if err := s.PutRecord(Record{BlockID: "block:x"}); err == nil {
		t.Fatal("record without handle accepted")
	}
}

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
	at := time.Date(2026, 10, 18, 3, 4, 5, 6, time.UTC)
	if err := s.SetAdoptFailed("block:a", at); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.AdoptFailedAt("block:a"); !ok || err != nil || !got.Equal(at) {
		t.Fatalf("failure time: %v %v %v", got, ok, err)
	}
	if err := s.ClearAdoptFailed("block:a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AdoptFailedAt("block:a"); ok {
		t.Fatal("cleared failure still present")
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
