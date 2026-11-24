package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// Op ids of narrator mail are scoped by Hub and Epic, with caller ownership
// checked inside that scope: two Epics of the same Hub both send their first
// hand-back notice under --op-id hb-1 without colliding, while another handle
// in the same Epic reusing the same op id is a conflict.
func TestNarratorOpIDScopedByHubAndEpic(t *testing.T) {
	e := routeWorld(t)
	hubKey := core.NewHubKey(fakeServer, "/hub")
	put(t, e,
		seed{Handle: "agy-06", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:e2", AdoptedAt: time.Now()},
		seed{Handle: "agy-09", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:e", AdoptedAt: time.Now()},
	)
	for _, h := range []string{"agy-06", "agy-09"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: hubKey, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:claude-02"}, PreferredPersonal: "claude-02", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:e2"}}); err != nil {
		t.Fatal(err)
	}
	first, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-1", "--body", "epic 1 done")
	if err != nil || first["inbox_seq"] != float64(1) {
		t.Fatalf("epic 1 hb-1: %v %v", first, err)
	}
	second, err := managedSend(t, e, "block:agy-06", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-1", "--body", "epic 2 done")
	if err != nil || second["inbox_seq"] != float64(2) {
		t.Fatalf("epic 2 hb-1: %v %v", second, err)
	}
	// Retry of epic 1 replays seq 1 without posting again.
	replay, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-1", "--body", "epic 1 done")
	if err != nil || replay["inbox_seq"] != float64(1) {
		t.Fatalf("epic 1 replay: %v %v", replay, err)
	}
	// Another caller in the same Epic reusing hb-1 is refused even with identical payload.
	if _, err := managedSend(t, e, "block:agy-09", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-1", "--body", "epic 1 done"); !isCode(err, fault.Conflict) {
		t.Errorf("different caller in same Epic reusing op-id: %v", err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-02", amq.BoxUnread); len(ids) != 2 {
		t.Errorf("delivered %d messages, want 2", len(ids))
	}
}

// The prompted narrator hand-back command (`mail send --to personal --op-id hb-1`)
// works on a session-keyed link without a Hub: Hub dedupe applies only to
// Hub-linked delivery, and a session-keyed link delivers via AMQ only with
// explicit no_hub_dedupe semantics.
func TestNarratorOpIDWorksOnSessionKeyedLink(t *testing.T) {
	e := routeWorld(t)
	diag := captureDiagnostics(t)
	put(t, e,
		seed{Handle: "agy-07", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:s", AdoptedAt: time.Now()},
		seed{Handle: "claude-09", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:p9", AdoptedAt: time.Now()},
	)
	for _, h := range []string{"agy-07", "claude-09"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p9"}, PreferredPersonal: "claude-09", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:s"}}); err != nil {
		t.Fatal(err)
	}
	send := []string{"--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-1", "--body", "PR #1"}
	first, err := managedSend(t, e, "block:agy-07", "send", send...)
	if err != nil || first["routed_to"] != "claude-09" || first["inbox_seq"] != nil || first["no_hub_dedupe"] != true {
		t.Fatalf("session-keyed send with --op-id: %v %v", first, err)
	}
	if !strings.Contains(diag.String(), "session-keyed") {
		t.Errorf("expected explicit diagnostic for session-keyed --op-id, got %q", diag.String())
	}
	// AMQ-only: no Hub inbox written, and a retry delivers via AMQ again.
	if second, err := managedSend(t, e, "block:agy-07", "send", send...); err != nil || second["routed_to"] != "claude-09" || second["inbox_seq"] != nil || second["no_hub_dedupe"] != true {
		t.Fatalf("session-keyed retry with --op-id: %v %v", second, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-09", amq.BoxUnread); len(ids) != 2 {
		t.Errorf("session-keyed AMQ deliveries = %d, want 2", len(ids))
	}
	if x, _ := e.Store.HubInbox("/hub"); len(x) != 0 {
		t.Errorf("session-keyed mail reached Hub inbox: %+v", x)
	}
}

// blockingPost pauses its first Post call while counting every Post call,
// so a test can verify concurrent op-id serialization and that inbox.lock is
// not held across AMQ I/O.
type blockingPost struct {
	Queue
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (q *blockingPost) Post(ctx context.Context, root, verb string, args ...string) (amq.Sent, []byte, error) {
	if q.calls.Add(1) == 1 {
		q.once.Do(func() { close(q.entered) })
		<-q.release
	}
	return q.Queue.Post(ctx, root, verb, args...)
}

// Concurrent retries with the same --op-id serialize lookup, claim, and AMQ
// delivery so AMQ receives one message, while hub-inbox.lock stays off the
// AMQ path so other Hub inbox appends proceed unblocked.
func TestNarratorOpIDConcurrentDeliverySendsOnce(t *testing.T) {
	e := routeWorld(t)
	bp := &blockingPost{Queue: e.AMQ, entered: make(chan struct{}), release: make(chan struct{})}
	e.AMQ = bp
	args := []string{"--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "conc-1", "--body", "PR #8"}
	var relOnce sync.Once
	release := func() { relOnce.Do(func() { close(bp.release) }) }
	t.Cleanup(release)
	type res struct {
		out map[string]any
		err error
	}
	chA := make(chan res, 1)
	go func() {
		out, err := managedSend(t, e, "block:agy-05", "send", args...)
		chA <- res{out, err}
	}()
	select {
	case <-bp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first send never entered AMQ Post")
	}
	// While AMQ Post is in flight, inbox.lock must NOT be held: another inbox
	// append completes immediately.
	appendDone := make(chan error, 1)
	go func() {
		_, _, err := e.Store.AppendHubInbox(context.Background(), "/hub", store.HubInboxEntry{
			Epic: "session:other", Kind: store.InboxHandBack, HandBackSeq: 1, By: "claude-01", DedupeKey: "handback:session:other#1",
		}, time.Now())
		appendDone <- err
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("unrelated inbox append while AMQ Post blocked: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hub-inbox.lock was held across AMQ I/O")
	}
	// Launch concurrent retry with the same op-id while first Post is in flight.
	chB := make(chan res, 1)
	go func() {
		out, err := managedSend(t, e, "block:agy-05", "send", args...)
		chB <- res{out, err}
	}()
	time.Sleep(100 * time.Millisecond)
	if got := bp.calls.Load(); got != 1 {
		release()
		<-chA
		<-chB
		t.Fatalf("concurrent retry posted to AMQ while first delivery was in flight: calls=%d", got)
	}
	release()
	rA, rB := <-chA, <-chB
	if rA.err != nil || rB.err != nil {
		t.Fatalf("concurrent sends failed: A=%v B=%v", rA.err, rB.err)
	}
	if rA.out["inbox_seq"] != rB.out["inbox_seq"] || rA.out["id"] == nil || rA.out["id"] != rB.out["id"] {
		t.Fatalf("concurrent results diverged: A=%v B=%v", rA.out, rB.out)
	}
	if got := bp.calls.Load(); got != 1 {
		t.Errorf("AMQ Post called %d times, want 1", got)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-02", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("claude-02 received %d messages, want 1", len(ids))
	}
}

// Settled Hub-only, pending, and delivered states are distinct and keep the
// durable delivery token intact: retrying a settled Hub-only send after a
// personal agent becomes reachable replays routed_to hub without posting to
// AMQ, while retrying an unsettled pending send keeps the correlation token
// across attempts and recovery.
func TestNarratorOpIDReplaysSettledHubOnlyAndKeepsToken(t *testing.T) {
	e := routeWorld(t)
	for _, h := range []core.Handle{"claude-01", "claude-02", "claude-03"} {
		leave(t, e, h)
	}
	hubOnlyArgs := []string{"--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--op-id", "hb-only", "--body", "PR #7"}
	first, err := managedSend(t, e, "block:agy-05", "send", hubOnlyArgs...)
	if err != nil || first["routed_to"] != "hub" || first["inbox_seq"] != float64(1) || first["id"] != nil {
		t.Fatalf("initial Hub-only send: %v %v", first, err)
	}
	raw, err := e.Store.HubInbox("/hub")
	if err != nil || len(raw) != 1 || raw[0].Delivery == "" || raw[0].DeliveryState != store.DeliveryHubOnly {
		t.Fatalf("settled Hub-only entry state/token: %+v %v", raw, err)
	}
	// Bring a personal agent online in the Hub, then retry twice.
	hubKey := core.NewHubKey(fakeServer, "/hub")
	put(t, e, seed{Handle: "claude-04", Kind: core.Claude, Role: core.PersonalRole, SessionID: "session:claude-04", AdoptedAt: time.Now()})
	putHub(t, e, "session:claude-04", hubKey)
	if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), "claude-04"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		again, err := managedSend(t, e, "block:agy-05", "send", hubOnlyArgs...)
		if err != nil || again["routed_to"] != "hub" || again["inbox_seq"] != float64(1) || again["id"] != nil {
			t.Fatalf("retry %d of settled Hub-only send: %v %v", i, again, err)
		}
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-04", amq.BoxUnread); len(ids) != 0 {
		t.Errorf("settled Hub-only retry delivered %d AMQ message(s) to claude-04", len(ids))
	}

	// Unsettled pending attempt (AMQ failure) keeps its durable token across
	// retry and outbox recovery when the delivery sidecar cannot be written.
	real := e.AMQ
	e.AMQ = postFails{Queue: real}
	pendArgs := []string{"--to", "personal", "--kind", "status", "--subject", "retry me", "--op-id", "hb-pend", "--body", "try again"}
	if got, err := managedSend(t, e, "block:agy-05", "send", pendArgs...); err != nil || got["routed_to"] != "hub" || got["inbox_seq"] != float64(2) {
		t.Fatalf("failed AMQ attempt: %v %v", got, err)
	}
	e.AMQ = real
	// Block delivery sidecar directory to prove the retry still carries the
	// durable token in AMQ context for outbox recovery.
	deliveredDir := filepath.Join(filepath.Dir(e.Paths.HubInboxPath("/hub")), "delivered")
	if err := os.RemoveAll(deliveredDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deliveredDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deliv, err := managedSend(t, e, "block:agy-05", "send", pendArgs...)
	if err != nil || deliv["routed_to"] != "claude-04" || deliv["inbox_seq"] != float64(2) || deliv["id"] == nil {
		t.Fatalf("retry after AMQ recovery: %v %v", deliv, err)
	}
	// A third attempt recovers the delivery from BoxSent via the durable token
	// and does not post a duplicate.
	replay, err := managedSend(t, e, "block:agy-05", "send", pendArgs...)
	if err != nil || replay["inbox_seq"] != float64(2) || replay["id"] != deliv["id"] {
		t.Fatalf("replay recovered from sent box: %v %v", replay, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-04", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("claude-04 received %d messages for pending retry, want 1", len(ids))
	}
}

// Op-id conflict comparison covers the immutable normalized request (requested
// thread and all message-affecting flags), independent of AMQ-assigned thread
// or dynamically selected live recipient.
func TestNarratorOpIDRequestConflictChecksAllMessageFlags(t *testing.T) {
	e := routeWorld(t)
	// 1. Implicit thread + live recipient failover must still replay cleanly,
	// while adding an explicit --thread on retry must conflict.
	implicit := []string{"--to", "personal", "--subject", "s", "--body", "b", "--op-id", "t-implicit"}
	first, err := managedSend(t, e, "block:agy-05", "send", implicit...)
	if err != nil || first["routed_to"] != "claude-02" || first["inbox_seq"] != float64(1) {
		t.Fatalf("implicit first: %v %v", first, err)
	}
	leave(t, e, "claude-02")
	e.touchActivity(recordNamed(t, e, "claude-03"))
	replay, err := managedSend(t, e, "block:agy-05", "send", implicit...)
	if err != nil || replay["inbox_seq"] != float64(1) || replay["id"] != first["id"] || replay["routed_to"] != "claude-02" {
		t.Fatalf("implicit replay after recipient failover: %v %v", replay, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-03", amq.BoxUnread); len(ids) != 0 {
		t.Errorf("replay posted to new live recipient claude-03: %v", ids)
	}
	if _, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "b", "--thread", "custom/1", "--op-id", "t-implicit"); !isCode(err, fault.Conflict) {
		t.Errorf("adding explicit --thread on retry: %v, want conflict", err)
	}

	// 2. Explicit thread and message-affecting flags: normalized equivalent
	// retries succeed; changing any message-affecting flag conflicts.
	base := []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "high", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}
	expFirst, err := managedSend(t, e, "block:agy-05", "send", base...)
	if err != nil || expFirst["inbox_seq"] != float64(2) {
		t.Fatalf("explicit first: %v %v", expFirst, err)
	}
	equiv := []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "high", "--labels", "a, b", "--refs", "m1", "--context", `{"b":2,"a":1}`, "--op-id", "t-explicit"}
	if expReplay, err := managedSend(t, e, "block:agy-05", "send", equiv...); err != nil || expReplay["inbox_seq"] != float64(2) || expReplay["id"] != expFirst["id"] {
		t.Fatalf("normalized equivalent retry: %v %v", expReplay, err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"omit thread", []string{"--to", "personal", "--subject", "s", "--body", "b", "--kind", "status", "--priority", "high", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other thread", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t2", "--kind", "status", "--priority", "high", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other kind", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "question", "--priority", "high", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other priority", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "normal", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other labels", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "high", "--labels", "a,c", "--refs", "m1", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other refs", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "high", "--labels", "a,b", "--refs", "m2", "--context", `{"a":1,"b":2}`, "--op-id", "t-explicit"}},
		{"other context", []string{"--to", "personal", "--subject", "s", "--body", "b", "--thread", "epic/t1", "--kind", "status", "--priority", "high", "--labels", "a,b", "--refs", "m1", "--context", `{"a":1,"b":3}`, "--op-id", "t-explicit"}},
	} {
		if _, err := managedSend(t, e, "block:agy-05", "send", tc.args...); !isCode(err, fault.Conflict) {
			t.Errorf("%s: got %v, want conflict", tc.name, err)
		}
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-03", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("claude-03 received %d messages, want 1", len(ids))
	}
}

// When amq writes the recipient's inbox and dies before writing the sender's
// outbox (deliveredThenKilled), retrying under the same --op-id reconciles
// the recipient's inbox by delivery token instead of posting a duplicate. When
// an attempted recipient's mailbox cannot be read to confirm absence, retry
// returns an explicit pending state without auto-resending.
func TestNarratorOpIDReconcilesInboxWhenOutboxMissing(t *testing.T) {
	e := routeWorld(t)
	real := e.AMQ
	e.AMQ = deliveredThenKilled{Queue: real}
	args := []string{"--to", "personal", "--subject", "s", "--body", "b", "--op-id", "kill-1"}
	first, err := managedSend(t, e, "block:agy-05", "send", args...)
	if err != nil || first["inbox_seq"] != float64(1) || first["id"] != "fault-1" {
		t.Fatalf("killed-after-delivery attempt: %v %v", first, err)
	}
	e.AMQ = real
	replay, err := managedSend(t, e, "block:agy-05", "send", args...)
	if err != nil || replay["inbox_seq"] != float64(1) || replay["id"] != "fault-1" || replay["subject"] != "s" {
		t.Fatalf("retry after outbox-missing delivery: %v %v", replay, err)
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-02", amq.BoxUnread); !slices.Equal(ids, []string{"fault-1"}) {
		t.Errorf("claude-02 inbox = %v, want only [fault-1]", ids)
	}

	// Simulate a hard crash that left DeliveryAttempted on disk and the
	// recipient's inbox written with the token, with no outbox copy and no
	// DeliveryDelivered sidecar: retry reconciles the recipient inbox before
	// calling Post and does not resend.
	hubKey := core.NewHubKey(fakeServer, "/hub")
	crashReq := narratorMailRequest{To: []core.Handle{"personal"}, Subject: "crash-sub", Body: "crash-body"}
	crashEntry, _, err := e.Store.AppendHubInbox(context.Background(), "/hub", store.HubInboxEntry{
		Epic: "session:e", HubKey: hubKey, Kind: store.InboxNarrator, Subject: crashReq.Subject, Body: crashReq.Body,
		By: "agy-05", DedupeKey: narratorOpKey("session:e", "kill-hard"), RequestHash: crashReq.hash(), Delivery: "tok-crash",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutHubInboxDelivery("/hub", "tok-crash", store.HubInboxDelivery{
		State: store.DeliveryAttempted, AttemptedTo: []core.Handle{"claude-02"}, RoutedTo: "claude-02",
	}); err != nil {
		t.Fatal(err)
	}
	crashMsg := "---json\n{\"schema\":1,\"id\":\"fault-crash\",\"from\":\"agy-05\",\"to\":[\"claude-02\"],\"thread\":\"p2p/agy-05__claude-02\",\"subject\":\"crash-sub\",\"created\":\"2026-11-24T00:00:00Z\",\"context\":{\"rex_amq_adapter_delivery\":{\"token\":\"tok-crash\"}}}\n---\ncrash-body\n"
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "claude-02"), "fault-crash.md"), []byte(crashMsg), 0o600); err != nil {
		t.Fatal(err)
	}
	bp := &blockingPost{Queue: real, entered: make(chan struct{}), release: make(chan struct{})}
	close(bp.release)
	e.AMQ = bp
	reconciled, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "crash-sub", "--body", "crash-body", "--op-id", "kill-hard")
	if err != nil || reconciled["inbox_seq"] != float64(crashEntry.Seq) || reconciled["id"] != "fault-crash" || reconciled["subject"] != "crash-sub" {
		t.Fatalf("hard-crash inbox reconciliation: %v %v", reconciled, err)
	}
	if got := bp.calls.Load(); got != 0 {
		t.Errorf("hard-crash reconciliation called AMQ Post %d times, want 0", got)
	}

	// Uncertain outcome: DeliveryAttempted recorded for claude-03 whose inbox
	// has an unreadable corrupt message file so absence cannot be confirmed ->
	// retry must NOT auto-resend and returns explicit delivery_state: pending.
	uncReq := narratorMailRequest{To: []core.Handle{"personal"}, Subject: "unc-sub", Body: "unc-body"}
	uncEntry, _, err := e.Store.AppendHubInbox(context.Background(), "/hub", store.HubInboxEntry{
		Epic: "session:e", HubKey: hubKey, Kind: store.InboxNarrator, Subject: uncReq.Subject, Body: uncReq.Body,
		By: "agy-05", DedupeKey: narratorOpKey("session:e", "kill-uncertain"), RequestHash: uncReq.hash(), Delivery: "tok-uncertain",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutHubInboxDelivery("/hub", "tok-uncertain", store.HubInboxDelivery{
		State: store.DeliveryAttempted, AttemptedTo: []core.Handle{"claude-03"}, RoutedTo: "claude-03",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "claude-03"), "corrupt.md"), []byte("not a valid header"), 0o600); err != nil {
		t.Fatal(err)
	}
	uncOut, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "unc-sub", "--body", "unc-body", "--op-id", "kill-uncertain")
	if err != nil || uncOut["inbox_seq"] != float64(uncEntry.Seq) || uncOut["routed_to"] != "hub" || uncOut["delivery_state"] != string(store.DeliveryPending) || uncOut["id"] != nil {
		t.Fatalf("uncertain retry: %v %v", uncOut, err)
	}
	if got := bp.calls.Load(); got != 0 {
		t.Errorf("uncertain retry auto-resent %d times, want 0", got)
	}
}

// A first hand-back stored while lifecycle.json was unwritable is backfilled
// into the lifecycle and idempotently kept in the Hub inbox before a new
// hand-back replaces it once lifecycle.json recovers.
func TestHandBackRecoveredLifecycleKeepsBothInHubInbox(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	hub := core.NewHubKey(fakeServer, "/hub")
	if err := e.Store.PutLink(core.Link{Hub: hub, Epic: key}); err != nil {
		t.Fatal(err)
	}
	orch := Location{Block: "block:o"}
	lifecycle := filepath.Join(e.Paths.SessionDir(key.SessionID), "lifecycle.json")
	if err := os.MkdirAll(lifecycle, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := e.RecordHandBack(ctx, orch, "first report")
	if err == nil || first.Seq != 1 || !strings.Contains(err.Error(), "the hand-back is stored") {
		t.Fatalf("first, lifecycle unwritable: %+v %v", first, err)
	}
	if err := os.Remove(lifecycle); err != nil {
		t.Fatal(err)
	}
	second, err := e.RecordHandBack(ctx, orch, "updated report")
	if err != nil || second.Seq != 2 {
		t.Fatalf("second after lifecycle recovery: %+v %v", second, err)
	}
	x, err := e.Store.HubInbox("/hub")
	if err != nil || len(x) != 2 || x[0].HandBackSeq != 1 || x[0].Digest != first.Digest || x[1].HandBackSeq != 2 || x[1].Digest != second.Digest {
		t.Fatalf("Hub inbox after lifecycle recovery: %+v %v", x, err)
	}
}

// Delivered --op-id retries preserve the full public AMQ receipt (including
// subject and every recipient on a multi-recipient send with --thread).
func TestNarratorOpIDReplayPreservesSubjectAndMultiTo(t *testing.T) {
	e := routeWorld(t)
	singleArgs := []string{"--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--body", "PR #1", "--op-id", "single-1"}
	first, err := managedSend(t, e, "block:agy-05", "send", singleArgs...)
	if err != nil || first["id"] == nil || first["subject"] != "hand-back ready" {
		t.Fatalf("single first: %v %v", first, err)
	}
	replay, err := managedSend(t, e, "block:agy-05", "send", singleArgs...)
	if err != nil || replay["id"] != first["id"] || replay["subject"] != "hand-back ready" || replay["thread"] != first["thread"] || replay["routed_to"] != "claude-02" || replay["inbox_seq"] != float64(1) {
		t.Fatalf("single replay: %v %v (first %v)", replay, err, first)
	}
	if to, _ := replay["to"].([]any); len(to) != 1 || to[0] != "claude-02" {
		t.Errorf("single replay to = %v, want [claude-02]", replay["to"])
	}

	multiArgs := []string{"--to", "personal,claude-03", "--thread", "epic/multi", "--kind", "status", "--subject", "both see", "--body", "hello", "--op-id", "multi-1"}
	mFirst, err := managedSend(t, e, "block:agy-05", "send", multiArgs...)
	if err != nil || mFirst["id"] == nil || mFirst["subject"] != "both see" {
		t.Fatalf("multi first: %v %v", mFirst, err)
	}
	mReplay, err := managedSend(t, e, "block:agy-05", "send", multiArgs...)
	if err != nil || mReplay["id"] != mFirst["id"] || mReplay["subject"] != "both see" || mReplay["thread"] != "epic/multi" || mReplay["routed_to"] != "claude-02" || mReplay["inbox_seq"] != float64(2) {
		t.Fatalf("multi replay: %v %v (first %v)", mReplay, err, mFirst)
	}
	if to, _ := mReplay["to"].([]any); len(to) != 2 || to[0] != "claude-02" || to[1] != "claude-03" {
		t.Errorf("multi replay to = %v, want [claude-02 claude-03]", mReplay["to"])
	}
	if ids, _ := amq.IDs(e.Paths.Root(), "claude-03", amq.BoxUnread); len(ids) != 1 {
		t.Errorf("claude-03 received %d messages for multi-1, want 1", len(ids))
	}

	// Sidecar-blocked outbox-recovered multi-recipient retry must also preserve
	// subject and every recipient.
	deliveredDir := filepath.Join(filepath.Dir(e.Paths.HubInboxPath("/hub")), "delivered")
	if err := os.RemoveAll(deliveredDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deliveredDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	recArgs := []string{"--to", "personal,claude-03", "--thread", "epic/multi2", "--kind", "status", "--subject", "recovered multi", "--body", "hello2", "--op-id", "multi-rec"}
	rFirst, err := managedSend(t, e, "block:agy-05", "send", recArgs...)
	if err != nil || rFirst["id"] == nil || rFirst["subject"] != "recovered multi" {
		t.Fatalf("multi recovered first: %v %v", rFirst, err)
	}
	rReplay, err := managedSend(t, e, "block:agy-05", "send", recArgs...)
	if err != nil || rReplay["id"] != rFirst["id"] || rReplay["subject"] != "recovered multi" || rReplay["thread"] != "epic/multi2" || rReplay["inbox_seq"] != float64(3) {
		t.Fatalf("multi recovered replay: %v %v (first %v)", rReplay, err, rFirst)
	}
	if to, _ := rReplay["to"].([]any); len(to) != 2 || to[0] != "claude-02" || to[1] != "claude-03" {
		t.Errorf("multi recovered replay to = %v, want [claude-02 claude-03]", rReplay["to"])
	}
}
