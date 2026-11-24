package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// relayWorld is a real amq root with the Hub hub: session:e its Epic (the
// narrator agy-05 live, linked to the Hub preferring claude-02), session:x
// another Hub's Epic, and claude-02 and claude-03 personal agents of hub.
func relayWorld(t *testing.T) (Env, string) {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{})
	e.Ancestors = func() ([]proc, error) { return []proc{{PID: 9001}}, nil }
	hub, other := physical(t, t.TempDir()), physical(t, t.TempDir())
	key := core.NewHubKey(fakeServer, hub)
	old := time.Now().Add(-time.Hour)
	for _, h := range []core.Handle{"claude-02", "claude-03"} {
		session := "session:" + string(h)
		put(t, e, seed{Handle: h, Kind: core.Claude, Role: core.PersonalRole, SessionID: session, AdoptedAt: old})
		putHub(t, e, session, key)
	}
	put(t, e, seed{Handle: "agy-05", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:e", AdoptedAt: old},
		seed{Handle: "agy-06", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:x", AdoptedAt: old})
	for _, h := range []string{"claude-02", "claude-03", "agy-05", "agy-06"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []core.Link{
		{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:claude-02"}, PreferredPersonal: "claude-02", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:e"}},
		{Hub: core.NewHubKey(fakeServer, other), Epic: core.SessionKey{Server: fakeServer, SessionID: "session:x"}},
	} {
		if err := e.Store.PutLink(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:e", Server: fakeServer, Hub: key, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	return e, hub
}

var userEnv = func(string) string { return "" }

// The Operator's message to an Epic of its Hub is kept in the Hub's inbox
// (kind operator) and mailed to the narrator in its own name, which the
// gateway admits on arrival as adapter_operator_relay; its subject names
// hub:<seq>, the control id the narrator relays it under. A retry under
// the same --op-id sends nothing again; the op id with another message is
// a conflict. Another Hub's Epic, and an Epic with no live narrator, are
// refused before anything is kept.
func TestOperatorSendReachesTheNarrator(t *testing.T) {
	e, hub := relayWorld(t)
	ctx := context.Background()
	req := HubSendRequest{Hub: hub, Session: "session:e", Body: "also cover the logout path", OpID: "me:note-1"}
	res, err := e.SendToNarrator(ctx, userEnv, req)
	if err != nil || res.Seq != 1 || res.Narrator != "agy-05" || res.MsgID == "" || res.Control != "hub:1" || res.Replayed {
		t.Fatalf("send: %+v %v", res, err)
	}
	x, _ := e.Store.HubInbox(hub)
	if len(x) != 1 || x[0].Kind != store.InboxOperator || x[0].By != core.UserHandle || x[0].Body != req.Body || x[0].Epic != "session:e" || x[0].Thread != res.Thread {
		t.Fatalf("inbox %+v", x)
	}
	hdr, err := amq.ReadHeader(e.Paths.Root(), "agy-05", amq.BoxUnread, res.MsgID)
	if err != nil || hdr.From != "agy-05" || !strings.HasPrefix(hdr.Subject, "operator hub:1: also cover") {
		t.Fatalf("the narrator's mail %+v %v", hdr, err)
	}
	kept, err := e.admit(ctx, recordNamed(t, e, "agy-05"))
	if err != nil || len(kept) != 1 || kept[0] != res.MsgID {
		t.Fatalf("the narrator's arrival gate kept %v %v", kept, err)
	}
	again, err := e.SendToNarrator(ctx, userEnv, req)
	if err != nil || !again.Replayed || again.Seq != 1 || again.MsgID != res.MsgID {
		t.Errorf("the retry: %+v %v", again, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "agy-05", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("the retry mailed again: %v", ids)
	}
	other := req
	other.Body = "something else"
	if _, err := e.SendToNarrator(ctx, userEnv, other); !isCode(err, fault.Conflict) {
		t.Errorf("the op id with another message: %v", err)
	}
	if _, err := e.SendToNarrator(ctx, userEnv, HubSendRequest{Hub: hub, Session: "session:x", Body: "hi"}); !errors.Is(err, ErrRefused) {
		t.Errorf("another Hub's Epic: %v", err)
	}
	leave(t, e, "agy-05")
	if _, err := e.SendToNarrator(ctx, userEnv, HubSendRequest{Hub: hub, Session: "session:e", Body: "hi"}); !isCode(err, fault.FailedPrecondition) {
		t.Errorf("no live narrator: %v", err)
	}
	if x, _ := e.Store.HubInbox(hub); len(x) != 1 {
		t.Errorf("a refused message was kept: %+v", x)
	}
	if _, err := e.SendToNarrator(ctx, func(k string) string { return map[string]string{"ME_UNATTENDED": "1"}[k] }, req); operatorReason(err) != GuardUnattended {
		t.Errorf("an unattended caller: %v", err)
	}
}

// hub reply answers an inbox entry: to its Epic's narrator, on its
// thread, naming the entry (in_reply_to) and the message it was.
func TestOperatorReplyAnswersAnInboxEntry(t *testing.T) {
	e, hub := relayWorld(t)
	ctx := context.Background()
	asked, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--kind", "question", "--subject", "merge?", "--body", "PR #3 is green: merge it?")
	if err != nil || asked["inbox_seq"] != float64(1) {
		t.Fatalf("the narrator's question: %v %v", asked, err)
	}
	q, _ := e.Store.HubInbox(hub)
	res, err := e.SendToNarrator(ctx, userEnv, HubSendRequest{Hub: hub, ReplyTo: 1, Body: "yes, merge it"})
	if err != nil || res.Epic != "session:e" || res.InReplyTo != 1 || res.Thread != q[0].Thread || res.Seq != 2 {
		t.Fatalf("reply: %+v %v", res, err)
	}
	b, err := readMessage(e, "agy-05", res.MsgID)
	if err != nil || !strings.Contains(b, q[0].MsgID) {
		t.Errorf("the relay does not name the message it answers (%s): %s %v", q[0].MsgID, b, err)
	}
	if x, _ := e.Store.HubInbox(hub); len(x) != 2 || x[1].InReplyTo != 1 || x[1].Kind != store.InboxOperator {
		t.Errorf("inbox %+v", x)
	}
	if _, err := e.SendToNarrator(ctx, userEnv, HubSendRequest{Hub: hub, ReplyTo: 99, Body: "?"}); !isCode(err, fault.NotFound) {
		t.Errorf("an entry that is not there: %v", err)
	}
}

// A personal agent's reply to a narrator is kept as kind reply, answering
// the entry of the message it replies to: found by id, also through a
// delivery again to another personal agent (rerouted_from) and the reply
// that names it; a send naming nothing answers no entry.
func TestPersonalReplyIsMirrored(t *testing.T) {
	e, hub := relayWorld(t)
	ctx := context.Background()
	sent, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "merge?", "--body", "merge it?")
	if err != nil {
		t.Fatal(err)
	}
	orig := sent["id"].(string)
	rep, err := managedSend(t, e, "block:claude-02", "reply", "--id", orig, "--body", "yes")
	if err != nil {
		t.Fatal(err)
	}
	// claude-02 leaves with the narrator's second message unread: it is
	// delivered again to claude-03, which answers the copy.
	second, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "and the docs?", "--body", "docs too?")
	if err != nil {
		t.Fatal(err)
	}
	leave(t, e, "claude-02")
	e.touchActivity(recordNamed(t, e, "claude-03"))
	if err := e.redeliverFrom(ctx, recordNamed(t, e, "claude-02")); err != nil {
		t.Fatal(err)
	}
	r, ok, err := e.Store.Redelivered(second["id"].(string))
	if err != nil || !ok {
		t.Fatalf("redelivery of %v: %+v %v %v", second["id"], r, ok, err)
	}
	if _, err := managedSend(t, e, "block:claude-03", "reply", "--id", r.NewID, "--body", "docs too"); err != nil {
		t.Fatal(err)
	}
	if _, err := managedSend(t, e, "block:claude-03", "send", "--to", "agy-05", "--subject", "and", "--body", "one more thing"); err != nil {
		t.Fatal(err)
	}
	x, err := e.Store.HubInbox(hub)
	if err != nil || len(x) != 5 {
		t.Fatalf("inbox %+v %v", x, err)
	}
	for i, want := range []struct {
		kind      string
		inReplyTo int64
		by        core.Handle
	}{{store.InboxNarrator, 0, "agy-05"}, {store.InboxReply, 1, "claude-02"}, {store.InboxNarrator, 0, "agy-05"}, {store.InboxReply, 3, "claude-03"}, {store.InboxReply, 0, "claude-03"}} {
		if x[i].Kind != want.kind || x[i].InReplyTo != want.inReplyTo || x[i].By != want.by {
			t.Errorf("entry %d %+v, want %+v", i+1, x[i], want)
		}
	}
	if x[1].MsgID != rep["id"] || x[1].Body != "yes" {
		t.Errorf("the reply entry %+v", x[1])
	}
}

// readMessage is message id in h's mailbox, whole.
func readMessage(e Env, h, id string) (string, error) {
	for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained} {
		if b, err := amq.BodyPrefix(e.Paths.Root(), h, box, id, 1<<20); err == nil {
			hdr, _ := amq.ReadHeader(e.Paths.Root(), h, box, id)
			return strings.Join(hdr.Refs, ",") + "\n" + b, nil
		}
	}
	return "", errors.New("no such message")
}
