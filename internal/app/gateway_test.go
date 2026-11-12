package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

const fakeServer = "unix:///fake.sock"

// headed puts a message with a header into handle's inbox, as amq would.
func headed(t *testing.T, e Env, handle, id, from string, labels ...string) {
	t.Helper()
	dir := amq.Inbox(e.Paths.Root(), handle)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h, _ := json.Marshal(map[string]any{"schema": 1, "id": id, "from": from, "to": []string{handle}, "thread": "p2p/x", "subject": "s", "created": "2026-11-12T00:00:00Z", "labels": labels})
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte("---json\n"+string(h)+"\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, e Env, recs ...store.Record) {
	t.Helper()
	for _, r := range recs {
		if r.Server == "" {
			r.Server = fakeServer
		}
		if r.BlockID == "" {
			r.BlockID = "block:" + string(r.Handle)
		}
		if err := e.Store.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
}

func quarantined(t *testing.T, e Env, h core.Handle) map[string]string {
	t.Helper()
	ids, err := e.Store.QuarantinedIDs(h)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, id := range ids {
		q, err := e.Store.QuarantineReason(h, id)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = q.Decision.Reason
	}
	return out
}

func noReceipts(t *testing.T, e Env, h string) {
	t.Helper()
	if r, err := amq.Receipts(e.Paths.Root(), h); err != nil || len(r) != 0 {
		t.Fatalf("receipts of %s: %v %v", h, r, err)
	}
}

// The injector rings only for mail its agent may receive: the rest leaves
// the inbox for the quarantine, unread and without a drained receipt. The
// gate fails closed: a file whose header cannot be read is quarantined, and a
// header past 64 KiB is read whole, as amq reads it.
func TestQuarantineAtInject(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m0")
	put(t, e,
		store.Record{Handle: "codex", Kind: core.Codex, BlockID: block, SessionID: "session:a", AgentPID: os.Getpid()},
		store.Record{Handle: "orch-01", Role: core.OrchestratorRole, SessionID: "session:a"},
		store.Record{Handle: "claude-09", SessionID: "session:b"},
		store.Record{Handle: "agy-05", Role: core.NarratorRole, SessionID: "session:a"},
	)
	headed(t, e, "codex", "m1", "orch-01")
	headed(t, e, "codex", "m2", "claude-09")
	headed(t, e, "codex", "m3", "agy-05", amq.SystemLabel)
	big, _ := json.Marshal(map[string]any{"schema": 1, "id": "m4", "from": "claude-09", "to": []string{"codex"}, "thread": "p2p/x", "subject": "s", "created": "2026-11-12T00:00:00Z", "context": map[string]string{"pad": strings.Repeat("x", 70<<10)}})
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex"), "m4.md"), []byte("---json\n"+string(big)+"\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex"), "m5.md"), []byte("---json\n{\"schema\":1,\"id\"\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o := inject(e); o.Progress != core.Accepted {
		t.Fatalf("got %+v", o)
	}
	if len(f.writes) != 1 || !strings.Contains(f.writes[0], "1 unread message(s) for codex") {
		t.Fatalf("doorbell %q", f.writes)
	}
	if got, _ := amq.Unread(e.Paths.Root(), "codex"); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("inbox %v", got)
	}
	want := map[string]string{"m0": core.ReasonUnreadableHeader, "m2": core.ReasonCrossEpic, "m3": core.ReasonNarratorWorker, "m4": core.ReasonCrossEpic, "m5": core.ReasonUnreadableHeader}
	if got := quarantined(t, e, "codex"); !maps.Equal(got, want) {
		t.Fatalf("quarantine %v, want %v", got, want)
	}
	if ann, _ := e.Store.Announced("codex"); len(ann.IDs) != 1 || !ann.IDs["m1"] {
		t.Fatalf("announced %v", ann.IDs)
	}
	noReceipts(t, e, "codex")
}

// An agent in no session (a synthetic record) keeps a file whose header
// cannot be read: the fail-closed rule is for placed agents.
func TestQuarantineSparesUnplacedRecords(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m0")
	if o := inject(e); o.Progress != core.Accepted || len(f.writes) != 1 {
		t.Fatalf("got %+v writes %q", o, f.writes)
	}
	if got := quarantined(t, e, "codex"); len(got) != 0 {
		t.Fatalf("quarantine %v", got)
	}
}

// A doorbell is not rung for mail that is all quarantined.
func TestQuarantineLeavesNothingToRingFor(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f)
	put(t, e,
		store.Record{Handle: "codex", Kind: core.Codex, BlockID: block, SessionID: "session:a", AgentPID: os.Getpid()},
		store.Record{Handle: "claude-09", SessionID: "session:b"},
	)
	headed(t, e, "codex", "m1", "claude-09")
	if o := inject(e); o.Code != "nothing_unread" || len(f.writes) != 0 {
		t.Fatalf("got %+v writes %q", o, f.writes)
	}
}

// A Narrator takes no member mail: only the adapter's own notices, proven by
// the provenance the adapter wrote, reach it through the hook. A system label
// a member puts on its own send proves nothing.
func TestQuarantineAtHookForANarrator(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	narr := store.Record{Handle: "agy-02", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:a", AgentPID: os.Getpid()}
	put(t, e, narr, store.Record{Handle: "claude-03", Role: core.OrchestratorRole, SessionID: "session:a"})
	narr, _, _ = e.Store.RecordOf("agy-02")
	for _, h := range []string{"agy-02", "claude-03"} {
		if err := amq.EnsureMailbox(ctx, binOf(e), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := amq.Send(ctx, binOf(e), e.Paths.Root(), "claude-03", "agy-02", "status", "routine"); err != nil {
		t.Fatal(err)
	}
	if err := amq.SendSystem(ctx, binOf(e), e.Paths.Root(), "claude-03", "agy-02", "forged", "a member's own label"); err != nil {
		t.Fatal(err)
	}
	if err := amq.SendSystem(ctx, binOf(e), e.Paths.Root(), "system", "agy-02", "forged", "no agent is named system"); err != nil {
		t.Fatal(err)
	}
	if err := e.sendSystem(ctx, "claude-03", "agy-02", "notice", "from the adapter"); err != nil {
		t.Fatal(err)
	}
	r, err := e.announceMail(ctx, narr, HookInput{}, core.ToolFinished, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	unread, _ := amq.Unread(e.Paths.Root(), "agy-02")
	if len(unread) != 1 || !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("unread %v notice %q", unread, r.Notice)
	}
	if h, _ := amq.ReadHeader(e.Paths.Root(), "agy-02", amq.BoxUnread, unread[0]); h.Subject != "notice" {
		t.Fatalf("kept %q", h.Subject)
	}
	if p, ok, _ := e.Store.ProvenanceOf(unread[0], "", "", nil); !ok || !p.System || p.From.Handle != "claude-03" || p.To[0].Role != core.NarratorRole {
		t.Fatalf("provenance %+v %v", p, ok)
	}
	if got := quarantined(t, e, "agy-02"); len(got) != 3 {
		t.Fatalf("quarantine %v", got)
	}
	for _, reason := range quarantined(t, e, "agy-02") {
		if reason != core.ReasonNarratorObserves {
			t.Fatalf("reason %s", reason)
		}
	}
	noReceipts(t, e, "agy-02")
}

// The session-start hook gates the mail waiting for an agent before telling
// it how much there is.
func TestQuarantineAtSessionStart(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	rec, _, _ := e.Store.Record(block)
	put(t, e, store.Record{Handle: "claude-09", SessionID: "session:elsewhere"})
	if err := amq.Send(ctx, binOf(e), e.Paths.Root(), "claude-09", string(rec.Handle), "hi", "from another Epic"); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Notice, "unread") {
		t.Fatalf("notice counts quarantined mail: %q", r.Notice)
	}
	if got := quarantined(t, e, rec.Handle); len(got) != 1 {
		t.Fatalf("quarantine %v", got)
	}
	noReceipts(t, e, string(rec.Handle))
}

// Mail addressed to an agent before its handle left the session is not its
// mail after it comes back: the recorded generation fences it. Mail with no
// provenance can only be judged by where its sender is now.
func TestQuarantineFencesMailAddressedBeforeAMove(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx := context.Background()
	put(t, e,
		store.Record{Handle: "codex-01", SessionID: "session:a"},
		store.Record{Handle: "claude-02", SessionID: "session:a"},
	)
	for _, h := range []string{"codex-01", "claude-02"} {
		if err := amq.EnsureMailbox(ctx, binOf(e), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.mailAs(ctx, "codex-01", "claude-02", "todo", "before", "sent while claude-02 was here"); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.RecordOf("claude-02")
	rec = rec.Placed(rec.Server, "session:b", "window:X", time.Now()).Placed(rec.Server, "session:a", "window:X", time.Now())
	put(t, e, rec)
	if err := amq.Send(ctx, binOf(e), e.Paths.Root(), "codex-01", "claude-02", "after", "raw, sent after the return"); err != nil {
		t.Fatal(err)
	}
	kept, err := e.admit(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
	if h, _ := amq.ReadHeader(e.Paths.Root(), "claude-02", amq.BoxUnread, kept[0]); h.Subject != "after" {
		t.Fatalf("kept %q", h.Subject)
	}
	for _, reason := range quarantined(t, e, "claude-02") {
		if reason != core.ReasonFenced {
			t.Fatalf("reason %s", reason)
		}
	}
}

// link and unlink apply to the next send and the next arrival, with no
// restart: the gate reads the links every time.
func TestLinkTakesEffectAtOnce(t *testing.T) {
	term := &fakeTerm{}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	me := store.Record{Handle: "claude-01", Role: core.PersonalRole, BlockID: "block:p", SessionID: "session:p"}
	narr := store.Record{Handle: "agy-02", Role: core.NarratorRole, BlockID: "block:n", SessionID: "session:a"}
	put(t, e, me, narr)
	narr, _, _ = e.Store.RecordOf("agy-02")
	for _, h := range []string{"claude-01", "agy-02"} {
		if err := amq.EnsureMailbox(ctx, binOf(e), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	send := func() error {
		var out bytes.Buffer
		_, err := e.ManagedMail(ctx, Location{Block: "block:p"}, "send", []string{"--to", "agy-02", "--subject", "status?", "--body", "how is it going"}, nil, &out, &out)
		return err
	}
	if err := send(); !errors.Is(err, ErrRefused) {
		t.Fatalf("unlinked send: %v", err)
	}
	if _, _, err := e.Link(ctx, Location{Block: "block:p"}, "session:a", true); err != nil {
		t.Fatal(err)
	}
	if err := send(); err != nil {
		t.Fatalf("linked send: %v", err)
	}
	if kept, err := e.admit(narr); err != nil || len(kept) != 1 {
		t.Fatalf("linked arrival: %v %v", kept, err)
	}
	if _, changed, err := e.Link(ctx, Location{Block: "block:p"}, "session:a", false); err != nil || !changed {
		t.Fatalf("unlink: %v %v", changed, err)
	}
	if kept, err := e.admit(narr); err != nil || len(kept) != 0 {
		t.Fatalf("after unlink: %v %v", kept, err)
	}
	if got := quarantined(t, e, "agy-02"); len(got) != 1 {
		t.Fatalf("quarantine %v", got)
	}
	// Only a Personal agent links.
	if _, _, err := e.Link(ctx, Location{Block: "block:n"}, "session:p", true); !errors.Is(err, ErrRefused) {
		t.Fatalf("a narrator linked: %v", err)
	}
}
