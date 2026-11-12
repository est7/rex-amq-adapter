package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func TestLinkAndUnlink(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	p := core.SessionKey{Server: "unix:///rex.sock", SessionID: "session:p"}
	a := core.Link{Personal: p, Epic: core.SessionKey{Server: "unix:///rex.sock", SessionID: "session:a"}}
	b := core.Link{Personal: p, Epic: core.SessionKey{Server: "unix:///rex.sock", SessionID: "session:b"}}
	if links, err := s.Links(); err != nil || len(links) != 0 {
		t.Fatalf("fresh links %v %v", links, err)
	}
	for _, l := range []core.Link{a, b, a} {
		if err := s.PutLink(l); err != nil {
			t.Fatal(err)
		}
	}
	links, err := s.Links()
	if err != nil || len(links) != 2 || links[0] != a || links[1] != b {
		t.Fatalf("links %v %v", links, err)
	}
	if existed, err := s.DeleteLink(a); err != nil || !existed {
		t.Fatalf("unlink: %v %v", existed, err)
	}
	if existed, err := s.DeleteLink(a); err != nil || existed {
		t.Fatalf("second unlink: %v %v", existed, err)
	}
	if links, _ := s.Links(); len(links) != 1 || links[0] != b {
		t.Fatalf("after unlink %v", links)
	}
	// Under the AMQ root's extension area, where amq cleanup leaves it.
	if _, err := os.Stat(filepath.Join(s.P.Root(), "extensions", "rex-amq-adapter", "links")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLink(core.Link{Personal: p}); err == nil {
		t.Fatal("a link without an Epic was recorded")
	}
}

// A message checked before its send returned is found by the token its
// header carries, for its own sender and recipients only; after the commit,
// by its id.
func TestProvenancePendingThenCommitted(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	from := core.Endpoint{Handle: "claude-01", Server: "unix:///rex.sock", SessionID: "session:a"}
	to := core.Endpoint{Handle: "codex-02", Server: "unix:///rex.sock", SessionID: "session:a", Generation: 2}
	p := Provenance{Token: "tok", Op: core.OpSend, From: from, To: []core.Endpoint{to}, At: time.Now()}
	if err := s.PendProvenance(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ProvenanceOf("m1", "tok", "claude-01", []string{"codex-02"})
	if err != nil || !ok || got.From != from {
		t.Fatalf("pending: %+v %v %v", got, ok, err)
	}
	if op := got.Operation("codex-02"); op.Addressed == nil || *op.Addressed != to || op.System {
		t.Fatalf("operation %+v", op)
	}
	for _, c := range []struct {
		from core.Handle
		to   []string
	}{{"pi-03", []string{"codex-02"}}, {"claude-01", []string{"pi-03"}}, {"claude-01", []string{"codex-02", "pi-03"}}} {
		if _, ok, _ := s.ProvenanceOf("m1", "tok", c.from, c.to); ok {
			t.Errorf("token matched a message from %s to %v", c.from, c.to)
		}
	}
	p.ID = "m1"
	if err := s.CommitProvenance(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.P.Extensions(), "provenance", "m1.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ProvenanceOf("m1", "", "", nil); !ok {
		t.Fatal("committed provenance not found by id")
	}
	if _, ok, _ := s.ProvenanceOf("m2", "tok", "claude-01", []string{"codex-02"}); ok {
		t.Fatal("a committed token still vouches for another message")
	}
}

func TestQuarantineMovesTheMessage(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	inbox := filepath.Join(s.P.Root(), "agents", "codex", "inbox", "new")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(inbox, "m1.md")
	if err := os.WriteFile(src, []byte("---json\n{}\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	q := Quarantined{ID: "m1", Handle: "codex", From: core.Endpoint{Handle: "claude-01"}, Decision: core.Decision{Reason: core.ReasonCrossEpic}}
	if moved, err := s.Quarantine(src, q); err != nil || !moved {
		t.Fatalf("quarantine: %v %v", moved, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("still in the inbox: %v", err)
	}
	if ids, _ := s.QuarantinedIDs("codex"); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("quarantined %v", ids)
	}
	if r, err := s.QuarantineReason("codex", "m1"); err != nil || r.Decision.Reason != core.ReasonCrossEpic {
		t.Fatalf("reason %+v %v", r, err)
	}
	// Gone already (drained or quarantined by another reader): no move, no
	// reason left behind.
	if moved, err := s.Quarantine(filepath.Join(inbox, "m2.md"), Quarantined{ID: "m2", Handle: "codex"}); err != nil || moved {
		t.Fatalf("missing source: %v %v", moved, err)
	}
	if ids, _ := s.QuarantinedIDs("codex"); len(ids) != 1 {
		t.Fatalf("quarantined %v", ids)
	}
	// A second check of a message another one already quarantined leaves
	// that one's reason in place.
	if moved, err := s.Quarantine(src, q); err != nil || moved {
		t.Fatalf("second quarantine: %v %v", moved, err)
	}
	if r, err := s.QuarantineReason("codex", "m1"); err != nil || r.Decision.Reason != core.ReasonCrossEpic {
		t.Fatalf("reason after a second check: %+v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(s.P.Extensions(), "quarantine", "codex", "m2.json")); !os.IsNotExist(err) {
		t.Fatalf("reason left for a missing message: %v", err)
	}
}

// A move to another session starts a generation; a move within the session,
// or the first placement, does not.
func TestPlacedCountsSessionMoves(t *testing.T) {
	r := Record{Handle: "codex", Server: "unix:///rex.sock"}
	r = r.Placed("unix:///rex.sock", "session:a", "window:1")
	if r.Generation != 0 {
		t.Fatalf("first placement: %d", r.Generation)
	}
	r = r.Placed("unix:///rex.sock", "session:a", "window:2")
	if r.Generation != 0 || r.WindowID != "window:2" {
		t.Fatalf("tab move: %+v", r)
	}
	r = r.Placed("unix:///rex.sock", "session:b", "window:3")
	r = r.Placed("unix:///rex.sock", "session:a", "window:1")
	if r.Generation != 2 || r.Endpoint().Generation != 2 || r.Endpoint().SessionID != "session:a" {
		t.Fatalf("after leaving and returning: %+v", r)
	}
}
