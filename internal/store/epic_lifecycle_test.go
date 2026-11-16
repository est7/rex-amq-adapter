package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lifecycle lists each hand-back Seq once, whichever entry it is: the same
// report noted again replaces its entry, wherever it is; another digest
// under a Seq already given one panics, as a Seq keeps its digest.
// LatestHandBack is the entry of the highest Seq.
func TestNoteHandBackKeepsOneDigestPerSeq(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	var l EpicLifecycle
	if _, ok := l.LatestHandBack(); ok {
		t.Fatal("an empty lifecycle has a latest hand-back")
	}
	l.NoteHandBack(HandBack{Seq: 1, Digest: "sha256:a", At: at})
	l.NoteHandBack(HandBack{Seq: 2, Digest: "sha256:b", At: at})
	l.NoteHandBack(HandBack{Seq: 1, Digest: "sha256:a", At: at, By: "codex-01"})
	if len(l.HandBacks) != 2 || l.HandBacks[0].By != "codex-01" || l.State != EpicHandBack {
		t.Fatalf("hand-backs %+v, state %q", l.HandBacks, l.State)
	}
	if latest, ok := l.LatestHandBack(); !ok || latest.Seq != 2 {
		t.Fatalf("latest %+v %v", latest, ok)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "seq 2 already has digest sha256:b") {
			t.Fatalf("another digest under seq 2: %v; hand-backs %+v", r, l.HandBacks)
		}
	}()
	l.NoteHandBack(HandBack{Seq: 2, Digest: "sha256:c", At: at})
}

// EpicOps lists every op record of a kind, none before any is written, and
// fails on one it cannot read rather than skip it; DeleteEpicOp removes one,
// and one already gone is not an error.
func TestEpicOpsListsEveryRecordOrFails(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if ops, err := s.EpicOps(EpicOpOpen); err != nil || ops != nil {
		t.Fatalf("no records: %+v %v", ops, err)
	}
	for _, r := range []EpicOpRecord{
		{Kind: EpicOpOpen, OpID: "a", RequestHash: "sha256:1", State: EpicCreating},
		{Kind: EpicOpOpen, OpID: "b", RequestHash: "sha256:2", State: EpicPartialStart, SessionID: "session:b"},
		{Kind: EpicOpClose, OpID: "c", RequestHash: "sha256:3", State: EpicClosing, SessionID: "session:b"},
	} {
		if err := s.PutEpicOp(r); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := s.EpicOps(EpicOpOpen)
	if err != nil || len(ops) != 2 {
		t.Fatalf("open records: %+v %v", ops, err)
	}
	if err := s.DeleteEpicOp(EpicOpOpen, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEpicOp(EpicOpOpen, "a"); err != nil {
		t.Fatalf("deleting a record already gone: %v", err)
	}
	if ops, _ := s.EpicOps(EpicOpOpen); len(ops) != 1 || ops[0].OpID != "b" {
		t.Fatalf("after the delete: %+v", ops)
	}
	if err := os.WriteFile(filepath.Join(s.P.epicOps(), "open-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EpicOps(EpicOpOpen); err == nil {
		t.Fatal("an unreadable record was skipped")
	}
}
