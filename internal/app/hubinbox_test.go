package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// inboxWorld is operatorWorld (no Rex at all: Env.Rex is nil) with the Hub
// hub holding session:a, linked, and session:closed, whose links went with
// its close; session:other is another Hub's.
func inboxWorld(t *testing.T) (Env, string) {
	t.Helper()
	e := operatorWorld(t)
	hub, other := physical(t, t.TempDir()), physical(t, t.TempDir())
	key := core.NewHubKey(fakeServer, hub)
	if err := e.Store.PutLink(core.Link{Hub: key, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:a"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutLink(core.Link{Hub: core.NewHubKey(fakeServer, other), Epic: core.SessionKey{Server: fakeServer, SessionID: "session:other"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:closed", Server: fakeServer, Hub: key, State: store.EpicClosed}); err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	for _, x := range []store.HubInboxEntry{
		{Epic: "session:a", HubKey: key, Kind: store.InboxHandBack, HandBackSeq: 1, By: "claude-02", DedupeKey: "handback:session:a#1"},
		{Epic: "session:closed", HubKey: key, Kind: store.InboxNarrator, Body: "done", By: "agy-03", DedupeKey: "n1"},
		{Epic: "session:a", HubKey: key, Kind: store.InboxNarrator, Body: "hand-back ready", By: "agy-01", DedupeKey: "n2"},
	} {
		if _, _, err := e.Store.AppendHubInbox(ctx, hub, x, now); err != nil {
			t.Fatal(err)
		}
	}
	return e, hub
}

// The Operator reads its Hub's inbox from its records alone: after --since,
// narrowed to one Epic of the Hub (a closed one too) and one kind, at most
// --limit. An --epic of another Hub is refused, and so is a caller that is
// no Operator.
func TestReadHubInbox(t *testing.T) {
	e, hub := inboxWorld(t)
	ctx := context.Background()
	user := func(string) string { return "" }
	seqs := func(p HubInboxPage) []int64 {
		var out []int64
		for _, x := range p.Items {
			out = append(out, x.Seq)
		}
		return out
	}
	for _, c := range []struct {
		r         HubInboxRequest
		want      []int64
		next      int64
		truncated bool
	}{
		{HubInboxRequest{Hub: hub}, []int64{1, 2, 3}, 3, false},
		{HubInboxRequest{Hub: hub, Since: 1}, []int64{2, 3}, 3, false},
		{HubInboxRequest{Hub: hub, Epic: "session:a"}, []int64{1, 3}, 3, false},
		{HubInboxRequest{Hub: hub, Epic: "session:closed"}, []int64{2}, 2, false},
		{HubInboxRequest{Hub: hub, Kind: store.InboxNarrator}, []int64{2, 3}, 3, false},
		{HubInboxRequest{Hub: hub, Limit: 2}, []int64{1, 2}, 2, true},
		{HubInboxRequest{Hub: hub, Since: 3}, nil, 3, false},
	} {
		p, err := e.ReadHubInbox(ctx, user, c.r)
		if err != nil || !equalSeqs(seqs(p), c.want) || p.NextSince != c.next || p.Truncated != c.truncated || p.Hub != hub {
			t.Errorf("%+v: %v next %d truncated %v hub %s (%v), want %v next %d", c.r, seqs(p), p.NextSince, p.Truncated, p.Hub, err, c.want, c.next)
		}
	}
	if _, err := e.ReadHubInbox(ctx, user, HubInboxRequest{Hub: hub, Epic: "session:other"}); !errors.Is(err, ErrRefused) {
		t.Errorf("another Hub's Epic: %v", err)
	}
	if _, err := e.ReadHubInbox(ctx, func(k string) string { return map[string]string{"ME_UNATTENDED": "1"}[k] }, HubInboxRequest{Hub: hub}); operatorReason(err) != GuardUnattended {
		t.Errorf("an unattended caller: %v", err)
	}
}

// --wait holds no lock and returns as soon as an entry after --since comes,
// or empty once it has waited.
func TestReadHubInboxWaits(t *testing.T) {
	e, hub := inboxWorld(t)
	ctx := context.Background()
	user := func(string) string { return "" }
	start := time.Now()
	if p, err := e.ReadHubInbox(ctx, user, HubInboxRequest{Hub: hub, Since: 3, Wait: 300 * time.Millisecond}); err != nil || len(p.Items) != 0 || time.Since(start) < 300*time.Millisecond {
		t.Errorf("a wait with nothing new: %+v %v after %v", p, err, time.Since(start))
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		if _, _, err := e.Store.AppendHubInbox(ctx, hub, store.HubInboxEntry{Epic: "session:a", Kind: store.InboxNarrator, By: "agy-01", DedupeKey: "n3"}, time.Now()); err != nil {
			t.Error(err)
		}
	}()
	p, err := e.ReadHubInbox(ctx, user, HubInboxRequest{Hub: hub, Since: 3, Wait: 10 * time.Second})
	if err != nil || len(p.Items) != 1 || p.Items[0].Seq != 4 || p.NextSince != 4 {
		t.Fatalf("a wait that something came to: %+v %v", p, err)
	}
}

func equalSeqs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Every hand-back the lifecycle lists as the latest is in the Hub's inbox
// once, its seq and digest without its body: recorded again unchanged, it
// is not kept twice; a new report is the next entry. A run that stored the
// report and failed to keep it (the inbox unwritable) is made good by the
// same report again, though that one changes nothing.
func TestHandBackIsKeptInTheHubInbox(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	hub := core.NewHubKey(fakeServer, "/hub")
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: key.SessionID, Server: key.Server, Hub: hub, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	inbox := func() []store.HubInboxEntry {
		t.Helper()
		x, err := e.Store.HubInbox("/hub")
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	// The inbox cannot be written: a file stands where its directory goes.
	blocker := filepath.Join(e.Paths.Extensions(), "hub-inbox")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "## Review queue\nPR #9\n")
	if err == nil || first.Seq != 1 {
		t.Fatalf("a hand-back the inbox could not keep: %+v %v", first, err)
	}
	if stored, ok, _ := e.Store.HandBack(key.SessionID); !ok || stored.Seq != 1 {
		t.Fatalf("the report was not stored: %+v", stored)
	}
	os.Remove(blocker)
	if h, changed, err := e.RecordHandBackWith(ctx, Location{Block: "block:o"}, HandBackRequest{Body: "## Review queue\nPR #9\n"}); err != nil || changed || h.Seq != 1 {
		t.Fatalf("the same report again: %+v %v %v", h, changed, err)
	}
	got := inbox()
	if len(got) != 1 || got[0].Kind != store.InboxHandBack || got[0].Epic != key.SessionID || got[0].HandBackSeq != 1 || got[0].Digest != first.Digest ||
		got[0].By != "codex-01" || got[0].Body != "" || got[0].HubKey != hub || got[0].DedupeKey != "handback:"+key.SessionID+"#1" {
		t.Fatalf("inbox after the retry: %+v", got)
	}
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "## Review queue\nPR #9\n"); err != nil || len(inbox()) != 1 {
		t.Errorf("an unchanged hand-back kept twice: %v %+v", err, inbox())
	}
	second, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "## Review queue\nPR #9 merged\n")
	if got := inbox(); err != nil || len(got) != 2 || got[1].HandBackSeq != 2 || got[1].Digest != second.Digest || got[1].Seq != 2 {
		t.Errorf("a new hand-back: %+v %v", got, err)
	}
}

// A narrator's mail to `personal` is kept in its Hub's inbox, by link:
// with a Hub link, kept with the amq message that carried it to the live
// personal agent, or alone (routed_to hub) when no personal agent of the
// Hub, live or recorded, could take it; with a session-keyed link, amq
// alone carries it; unlinked, it is refused.
func TestNarratorMailIsKeptInTheHubInbox(t *testing.T) {
	e := routeWorld(t)
	hubKey := core.NewHubKey(fakeServer, "/hub")
	sent, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--body", "PR #3 waits on you")
	if err != nil || sent["routed_to"] != "claude-02" || sent["inbox_seq"] != float64(1) {
		t.Fatalf("to the live personal agent: %v %v", sent, err)
	}
	got, _ := e.Store.HubInbox("/hub")
	if len(got) != 1 || got[0].Kind != store.InboxNarrator || got[0].MsgID != sent["id"] || got[0].Epic != "session:e" || got[0].By != "agy-05" ||
		got[0].Body != "PR #3 waits on you" || got[0].Subject != "hand-back ready" || got[0].Thread == "" || got[0].HubKey != hubKey {
		t.Fatalf("inbox %+v", got)
	}
	// The same words again are another message, another entry.
	if again, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "hand-back ready", "--body", "PR #3 waits on you"); err != nil || again["inbox_seq"] != float64(2) {
		t.Errorf("the same words again: %v %v", again, err)
	}

	// session:h: an Epic the user opened for the Hub, linked with no
	// preferred personal agent, and no personal agent of the Hub live.
	put(t, e, seed{Handle: "agy-06", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:h", AdoptedAt: time.Now()})
	if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), "agy-06"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutLink(core.Link{Hub: hubKey, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:h"}}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []core.Handle{"claude-01", "claude-02", "claude-03"} {
		leave(t, e, h)
	}
	kept, err := managedSend(t, e, "block:agy-06", "send", "--to", "personal", "--kind", "status", "--subject", "waits on the user", "--body", "the lead asks")
	if err != nil || kept["routed_to"] != "hub" || kept["inbox_seq"] != float64(3) || kept["id"] != nil {
		t.Fatalf("with no personal agent of the Hub: %v %v", kept, err)
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 3 || x[2].MsgID != "" || x[2].Epic != "session:h" || x[2].Body != "the lead asks" {
		t.Errorf("inbox %+v", x)
	}
	for _, h := range []string{"claude-01", "claude-02", "claude-03"} {
		if n, _ := os.ReadDir(amq.Inbox(e.Paths.Root(), h)); len(n) > 2 {
			t.Errorf("%s got mail for the Hub's inbox alone: %v", h, n)
		}
	}

	// session:s: linked to a personal agent's session, with no Hub.
	put(t, e, seed{Handle: "agy-07", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:s", AdoptedAt: time.Now()},
		seed{Handle: "claude-09", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:p9", AdoptedAt: time.Now()})
	for _, h := range []string{"agy-07", "claude-09"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p9"}, PreferredPersonal: "claude-09", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:s"}}); err != nil {
		t.Fatal(err)
	}
	if amqOnly, err := managedSend(t, e, "block:agy-07", "send", "--to", "personal", "--subject", "s", "--body", "b"); err != nil || amqOnly["routed_to"] != "claude-09" || amqOnly["inbox_seq"] != nil {
		t.Errorf("a session-keyed link: %v %v", amqOnly, err)
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 3 {
		t.Errorf("a session-keyed link's mail reached a Hub inbox: %+v", x)
	}

	// session:u: linked to nothing.
	put(t, e, seed{Handle: "agy-08", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:u", AdoptedAt: time.Now()})
	if _, err := managedSend(t, e, "block:agy-08", "send", "--to", "personal", "--subject", "s", "--body", "b"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), core.ReasonNotLinked) {
		t.Errorf("unlinked: %v", err)
	}
}

// postFails is a queue whose sends fail: with an id (delivered, then a
// receipt wait timed out) or without one (nothing delivered).
type postFails struct {
	Queue
	deliveredAs string
}

func (q postFails) Post(ctx context.Context, root, verb string, args ...string) (amq.Sent, []byte, error) {
	if q.deliveredAs != "" {
		sent, out, _ := q.Queue.Post(ctx, root, verb, args...)
		return sent, out, errors.New("the drained receipt timed out")
	}
	return amq.Sent{}, nil, errors.New("amq is down")
}

// A narrator's mail to the Hub's personal agent is kept before it is sent:
// a delivery that fails still leaves it in the inbox (routed_to hub, exit
// 0), and one that delivered and then failed its receipt wait is kept with
// the id it was delivered as. A send the gateway refuses keeps nothing.
func TestNarratorMailIsKeptBeforeItsDelivery(t *testing.T) {
	e := routeWorld(t)
	real := e.AMQ
	e.AMQ = postFails{Queue: real}
	got, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "kept though amq is down")
	if err != nil || got["routed_to"] != "hub" || got["rerouted_from"] != "claude-02" || got["inbox_seq"] != float64(1) {
		t.Fatalf("a failed delivery: %v %v", got, err)
	}
	e.AMQ = postFails{Queue: real, deliveredAs: "yes"}
	if got, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "delivered, receipt lost"); err != nil || got["routed_to"] != "claude-02" || got["inbox_seq"] != float64(2) {
		t.Fatalf("a delivery whose receipt wait failed: %v %v", got, err)
	}
	x, err := e.Store.HubInbox("/hub")
	if err != nil || len(x) != 2 || x[0].MsgID != "" || x[0].Body != "kept though amq is down" || x[1].MsgID == "" || x[1].Thread == "" {
		t.Fatalf("inbox %+v %v", x, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-02", amq.BoxUnread); !slices.Contains(ids, x[1].MsgID) {
		t.Errorf("entry 2's id %s is not the message claude-02 got (%v)", x[1].MsgID, ids)
	}
	// The gateway refuses mail to the lead without --control: nothing kept.
	e.AMQ = real
	put(t, e, seed{Handle: "claude-06", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:e", AdoptedAt: time.Now()})
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal,claude-06", "--subject", "s", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a refused send: %v", err)
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 2 {
		t.Errorf("a refused send was kept: %+v", x)
	}
}

// The inbox keeps a narrator's body as the message is: the file --body
// @PATH names, not the path; a body over the limit is refused before
// anything is kept or sent, and the inbox stays readable and appendable.
// No entry longer than a reader reads is ever written.
func TestNarratorBodyIsResolvedAndBounded(t *testing.T) {
	e := routeWorld(t)
	doc := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(doc, []byte("## Review queue\nPR #4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "@"+doc); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(t.TempDir(), "big.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxNarratorBody+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "@"+big); !errors.Is(err, ErrUsage) {
		t.Errorf("an oversized body: %v", err)
	}
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "after"); err != nil {
		t.Fatal(err)
	}
	x, err := e.Store.HubInbox("/hub")
	if err != nil || len(x) != 2 || x[0].Body != "## Review queue\nPR #4\n" || x[1].Body != "after" {
		t.Fatalf("inbox %+v %v", x, err)
	}
	if hdr, err := amq.ReadHeader(e.Paths.Root(), "claude-02", amq.BoxUnread, x[0].MsgID); err != nil {
		t.Errorf("the @file message as delivered: %+v %v", hdr, err)
	} else if b, _ := amq.BodyPrefix(e.Paths.Root(), "claude-02", amq.BoxUnread, x[0].MsgID, 1<<10); !strings.Contains(b, "PR #4") {
		t.Errorf("claude-02 got %q, not the file", b)
	}
	escaped := strings.Repeat("<", store.HubInboxMaxLine/6+1)
	if _, _, err := e.Store.AppendHubInbox(context.Background(), "/hub", store.HubInboxEntry{Epic: "session:e", Kind: store.InboxNarrator, Body: escaped, By: "agy-05", DedupeKey: "big"}, time.Now()); err == nil {
		t.Error("an entry past the line limit was written")
	}
	if x, err := e.Store.HubInbox("/hub"); err != nil || len(x) != 2 {
		t.Errorf("the inbox after a refused entry: %d %v", len(x), err)
	}
}

// A hand-back the inbox failed to keep is kept by the next hand-back run
// even when that one replaces it: every revision stored stays in the
// inbox, in order.
func TestAReplacedHandBackIsStillKept(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	hub := core.NewHubKey(fakeServer, "/hub")
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: key.SessionID, Server: key.Server, Hub: hub, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(e.Paths.Extensions(), "hub-inbox")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first, _, err := e.RecordHandBackWith(ctx, Location{Block: "block:o"}, HandBackRequest{Body: "rev 1", OpID: "hb-1"})
	if err == nil {
		t.Fatal("the inbox could not be written, and the hand-back said nothing")
	}
	os.Remove(blocker)
	second, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "rev 2")
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := e.RecordHandBackWith(ctx, Location{Block: "block:o"}, HandBackRequest{Body: "rev 1", OpID: "hb-1"}); err != nil || changed {
		t.Fatalf("the replay of revision 1: %v %v", changed, err)
	}
	x, err := e.Store.HubInbox("/hub")
	if err != nil || len(x) != 2 || x[0].HandBackSeq != 1 || x[0].Digest != first.Digest || x[1].HandBackSeq != 2 || x[1].Digest != second.Digest {
		t.Fatalf("inbox %+v %v", x, err)
	}
}

// A narrator's retry under the same --op-id, after a run whose answer was
// lost, keeps and delivers its message once; the op id with another
// message is a conflict; --op-id on mail that is not to a Hub's personal
// agent is usage. Each is decided before anything is kept.
func TestNarratorMailOpIDRetriesOnce(t *testing.T) {
	e := routeWorld(t)
	send := []string{"--to", "personal", "--subject", "s", "--body", "once", "--op-id", "narr-1"}
	first, err := managedSend(t, e, "block:agy-05", "send", send...)
	if err != nil || first["inbox_seq"] != float64(1) {
		t.Fatalf("first: %v %v", first, err)
	}
	again, err := managedSend(t, e, "block:agy-05", "send", send...)
	if err != nil || again["inbox_seq"] != float64(1) {
		t.Fatalf("retry: %v %v", again, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-02", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("delivered %d times", len(ids))
	}
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "another", "--op-id", "narr-1"); !isCode(err, fault.Conflict) {
		t.Errorf("the op id with another message: %v", err)
	}
	put(t, e, seed{Handle: "claude-06", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:e", AdoptedAt: time.Now()})
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "claude-06", "--control", "c1", "--subject", "s", "--body", "b", "--op-id", "narr-2"); !errors.Is(err, ErrUsage) {
		t.Errorf("--op-id on control mail: %v", err)
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 1 {
		t.Errorf("inbox %+v", x)
	}
}

// The delivery's amq id is found again when its record could not be
// written: the message carries its entry's token, and the narrator's sent
// mail is read for it.
func TestNarratorDeliveryIDIsRecoverable(t *testing.T) {
	e := routeWorld(t)
	inboxDir := filepath.Dir(e.Paths.HubInboxPath("/hub"))
	if err := os.MkdirAll(inboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inboxDir, "delivered"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sent, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "b")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := e.Store.HubInbox("/hub"); err != nil || len(raw) != 1 || raw[0].MsgID != "" {
		t.Fatalf("setup: the delivery record was written: %+v (sent %v)", raw, sent)
	}
	x, err := e.hubInbox("/hub")
	if err != nil || len(x) != 1 || x[0].MsgID != sent["id"] || x[0].Thread == "" {
		t.Errorf("the recovered entry %+v %v, want msg_id %v", x, err, sent["id"])
	}
}

// A request the adapter refuses whole keeps nothing: a --context that is
// no JSON object. Text read from stdin that starts with @ is text, never a
// file to read.
func TestNarratorMailIsCheckedWholeBeforeItIsKept(t *testing.T) {
	e := routeWorld(t)
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "b", "--context", "[1]"); err == nil {
		t.Error("a context that is no object was sent")
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 0 {
		t.Errorf("a refused request was kept: %+v", x)
	}
	var out, errOut strings.Builder
	if _, err := e.ManagedMail(context.Background(), Location{Block: "block:agy-05"}, "send", []string{"--to", "personal", "--subject", "s", "--body", "-", "--json"},
		strings.NewReader("@claude-02 merge it?\n"), &out, &errOut); err != nil {
		t.Fatalf("stdin text starting with @: %v %s", err, errOut.String())
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 1 || x[0].Body != "@claude-02 merge it?\n" {
		t.Errorf("inbox %+v", x)
	}
}

// Only a hand-back that was stored is kept: one the lifecycle lists but
// whose storing failed is not, and the stored one before it is kept before
// the next replaces it.
func TestOnlyStoredHandBacksAreKept(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	hub := core.NewHubKey(fakeServer, "/hub")
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: key.SessionID, Server: key.Server, Hub: hub, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "rev 1"); err != nil {
		t.Fatal(err)
	}
	// Revision 2 listed, as a run does before it stores, then never stored.
	lc, _, _ := e.Store.EpicLifecycle(key.SessionID)
	lc.NoteHandBack(store.HandBack{By: "codex-01", At: time.Now(), Seq: 2, Digest: store.HandBackDigest("rev 2")})
	if err := e.Store.PutEpicLifecycle(lc); err != nil {
		t.Fatal(err)
	}
	third, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "rev 3")
	if err != nil || third.Seq != 3 {
		t.Fatalf("rev 3: %+v %v", third, err)
	}
	x, _ := e.Store.HubInbox("/hub")
	var seqs []int
	for _, y := range x {
		seqs = append(seqs, y.HandBackSeq)
	}
	if !slices.Equal(seqs, []int{1, 3}) {
		t.Errorf("kept hand-backs %v, want [1 3]", seqs)
	}
}
