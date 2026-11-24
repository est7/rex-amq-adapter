package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
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
