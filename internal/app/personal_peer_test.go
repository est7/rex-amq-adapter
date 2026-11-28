package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
)

// The personal agents of one Hub mail each other through the managed
// commands, in one Rex session or across the Hub's sessions: a send and a
// reply pass the send-time gateway and the arrival gate (no quarantine, the
// recipient's notice counts them), and neither is kept in the Hub's inbox,
// which holds only what passes between a personal agent and a narrator.
// Another Hub's personal agent, one of no Hub in another session, and an
// Epic's worker stay refused both ways; raw amq mail and mail addressed to
// a placement the peer has left are quarantined on arrival.
func TestPersonalPeerMail(t *testing.T) {
	e, hub := relayWorld(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	put(t, e,
		seed{Handle: "claude-04", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:claude-02", AdoptedAt: old},
		seed{Handle: "claude-07", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:z", AdoptedAt: old},
		seed{Handle: "claude-08", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:loose", AdoptedAt: old},
		seed{Handle: "codex-09", Kind: core.Codex, Role: "implementer", SessionID: "session:e", AdoptedAt: old})
	putHub(t, e, "session:z", core.NewHubKey(fakeServer, physical(t, t.TempDir())))
	for _, h := range []string{"claude-04", "claude-07", "claude-08", "codex-09"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	inbox := func() int {
		t.Helper()
		x, err := e.Store.HubInbox(hub)
		if err != nil {
			t.Fatal(err)
		}
		return len(x)
	}
	admitted := func(h core.Handle, want int) []string {
		t.Helper()
		kept, err := e.admit(ctx, recordNamed(t, e, h))
		if err != nil || len(kept) != want {
			t.Fatalf("%s admitted %v %v, want %d", h, kept, err, want)
		}
		if q := quarantined(t, e, h); len(q) != 0 {
			t.Fatalf("%s quarantined %v", h, q)
		}
		return kept
	}
	refusedFor := func(err error, reason string) bool {
		return errors.Is(err, ErrRefused) && strings.Contains(err.Error(), reason)
	}

	// Across the Hub's sessions: a send, its reply.
	sent, err := managedSend(t, e, "block:claude-02", "send", "--to", "claude-03", "--kind", "todo", "--subject", "take G59", "--body", "context for G59")
	if err != nil {
		t.Fatalf("claude-02 to claude-03 of its Hub: %v", err)
	}
	id := sent["id"].(string)
	if p, ok, err := e.Store.ProvenanceOf(id, "", "claude-02", []string{"claude-03"}); err != nil || !ok || p.Op != core.OpSend || p.System || p.From.Hub == "" {
		t.Fatalf("the send's provenance %+v %v %v", p, ok, err)
	}
	if kept := admitted("claude-03", 1); kept[0] != id {
		t.Fatalf("claude-03 kept %v, want %s", kept, id)
	}
	r, err := e.announceMail(ctx, recordNamed(t, e, "claude-03"), HookInput{}, core.ToolFinished, nil, false, nil, core.ContextCap)
	if err != nil || !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("claude-03's notice %q %v", r.Notice, err)
	}
	rep, err := managedSend(t, e, "block:claude-03", "reply", "--id", id, "--body", "on it")
	if err != nil {
		t.Fatalf("claude-03 replies to claude-02: %v", err)
	}
	if p, ok, err := e.Store.ProvenanceOf(rep["id"].(string), "", "claude-03", []string{"claude-02"}); err != nil || !ok || p.Op != core.OpReply {
		t.Fatalf("the reply's provenance %+v %v %v", p, ok, err)
	}
	if kept := admitted("claude-02", 1); kept[0] != rep["id"] {
		t.Fatalf("claude-02 kept %v, want %v", kept, rep["id"])
	}
	// In one session.
	if _, err := managedSend(t, e, "block:claude-02", "send", "--to", "claude-04", "--subject", "s", "--body", "same session"); err != nil {
		t.Fatalf("claude-02 to claude-04 of its session: %v", err)
	}
	admitted("claude-04", 1)
	if n := inbox(); n != 0 {
		t.Fatalf("peer mail kept in the Hub's inbox: %d entries", n)
	}
	// A narrator and a peer in one send: only the narrator's copy is the
	// Hub's.
	if _, err := managedSend(t, e, "block:claude-02", "send", "--to", "agy-05,claude-03", "--thread", "hub/g59", "--subject", "s", "--body", "both"); err != nil {
		t.Fatalf("claude-02 to its narrator and its peer: %v", err)
	}
	x, err := e.Store.HubInbox(hub)
	if err != nil || len(x) != 1 || x[0].By != "claude-02" || x[0].Epic != "session:e" {
		t.Fatalf("the Hub's inbox after a narrator-and-peer send: %+v %v", x, err)
	}
	admitted("claude-03", 2)

	// Outside the Hub, and an Epic's worker, both ways.
	for _, c := range []struct{ from, to string }{
		{"claude-02", "claude-07"}, {"claude-07", "claude-02"},
		{"claude-02", "claude-08"}, {"claude-08", "claude-02"},
		{"claude-02", "codex-09"}, {"codex-09", "claude-02"},
	} {
		if _, err := managedSend(t, e, "block:"+c.from, "send", "--to", c.to, "--subject", "s", "--body", "b"); !refusedFor(err, core.ReasonPersonalOnly) {
			t.Errorf("%s to %s: %v", c.from, c.to, err)
		}
	}
	for _, h := range []string{"claude-07", "claude-08", "codex-09"} {
		if ids, _ := amq.Unread(e.Paths.Root(), h); len(ids) != 0 {
			t.Errorf("%s got refused mail %v", h, ids)
		}
	}
	if pv, err := e.PreviewPost(Location{Block: "block:claude-02"}, []core.Handle{"claude-03", "claude-07"}, "", ""); !refusedFor(err, core.ReasonPersonalOnly) ||
		len(pv.Recipients) != 2 || !pv.Recipients[0].Allow || pv.Recipients[0].Reason != core.ReasonPersonalPeer || pv.Recipients[1].Allow {
		t.Errorf("--dry-run to a peer and another Hub's personal: %+v %v", pv, err)
	}

	// Raw amq mail between peers proves no sender.
	before, _ := amq.Unread(e.Paths.Root(), "claude-02")
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "claude-03", "claude-02", "raw", "written with amq directly"); err != nil {
		t.Fatal(err)
	}
	if kept, err := e.admit(ctx, recordNamed(t, e, "claude-02")); err != nil || len(kept) != len(before) {
		t.Fatalf("claude-02 kept %v %v, want %d", kept, err, len(before))
	}
	if q := quarantined(t, e, "claude-02"); len(q) != 1 || !onlyReason(q, core.ReasonUnverified) {
		t.Fatalf("raw peer mail quarantined as %v", q)
	}
	// Mail addressed to a placement the peer has left is not its mail.
	if _, err := managedSend(t, e, "block:claude-02", "send", "--to", "claude-04", "--subject", "s", "--body", "before the move"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"session:z", "session:claude-02"} {
		if err := e.Store.Moved("claude-04", session, "window:X", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if kept, err := e.admit(ctx, recordNamed(t, e, "claude-04")); err != nil || len(kept) != 0 {
		t.Fatalf("claude-04 kept %v %v after the move", kept, err)
	}
	if q := quarantined(t, e, "claude-04"); len(q) != 2 || !onlyReason(q, core.ReasonFenced) {
		t.Fatalf("mail addressed before the move quarantined as %v", q)
	}
	if n := inbox(); n != 1 {
		t.Fatalf("the Hub's inbox grew to %d entries", n)
	}
}

// onlyReason reports whether every quarantined message was refused for reason.
func onlyReason(q map[string]string, reason string) bool {
	for _, r := range q {
		if r != reason {
			return false
		}
	}
	return true
}

// A move publishes the handle's fence before its lease (store.Moved), so a
// reader between the two writes sees the new session in the fence and the
// old one in the lease. The sender built from that state is in neither
// placement: it never takes the old session's Hub along into the new
// session, so a send to a peer of each is refused whole, with nothing
// delivered.
func TestPersonalPeerMailMidMove(t *testing.T) {
	e, _ := relayWorld(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	put(t, e, seed{Handle: "claude-07", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:z", AdoptedAt: old})
	putHub(t, e, "session:z", core.NewHubKey(fakeServer, physical(t, t.TempDir())))
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "claude-07"); err != nil {
		t.Fatal(err)
	}
	id, ok, err := e.Store.IdentityOf("claude-02")
	if err != nil || !ok {
		t.Fatalf("identity of claude-02: %v %v", ok, err)
	}
	id.Fence.SessionID, id.Fence.Generation, id.Fence.PlacedAt = "session:z", id.Fence.Generation+1, time.Now().UTC()
	if err := e.Store.PutIdentity(id); err != nil {
		t.Fatal(err)
	}
	if rec := recordNamed(t, e, "claude-02"); rec.SessionID != "session:claude-02" {
		t.Fatalf("the lease moved too: %s", rec.SessionID)
	}
	if _, err := managedSend(t, e, "block:claude-02", "send", "--to", "claude-03,claude-07", "--thread", "hub/move", "--subject", "s", "--body", "mid-move"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), core.ReasonPersonalOnly) {
		t.Fatalf("a send mid-move to the old Hub's peer and the new session's: %v", err)
	}
	for _, h := range []string{"claude-03", "claude-07"} {
		if ids, _ := amq.Unread(e.Paths.Root(), h); len(ids) != 0 {
			t.Errorf("%s got %v mid-move", h, ids)
		}
	}
	from, err := e.endpointOf("claude-02")
	if err != nil || from.SessionID != "session:z" || from.Hub != "" {
		t.Fatalf("claude-02 mid-move is %+v %v, want session:z of no Hub", from, err)
	}
}
