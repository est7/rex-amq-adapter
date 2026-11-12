package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// sendJSON is a managed send that returns the new message's id.
func sendJSON(t *testing.T, e Env, ctx context.Context, as core.Handle, args ...string) string {
	t.Helper()
	out, _, err := run(t, e, ctx, as, "send", append(args, "--json")...)
	if err != nil {
		t.Fatalf("send: %v %s", err, out)
	}
	var s amq.Sent
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.ID == "" {
		t.Fatalf("send output %q", out)
	}
	return s.ID
}

// [F1] A managed drain consumes the ids the gate admitted and no other: a
// message that arrives after the gate stays unread, with no receipt.
func TestManagedMailDrainTakesOnlyAdmittedMail(t *testing.T) {
	e, ctx := managedWorld(t)
	first := sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "s", "--body", "admitted")
	rec, _, _ := e.Store.RecordOf("claude-02")
	kept, err := e.admit(rec)
	if err != nil || len(kept) != 1 {
		t.Fatalf("admit %v %v", kept, err)
	}
	// Arrives between the gate and the consumption.
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "claude-09", "claude-02", "late", "cross-Epic, after the gate"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m := managed{e: e, rec: rec, root: e.Paths.Root(), stdout: &out, stderr: &out}
	drained, _, err := m.consume(ctx, kept, 20, true)
	if err != nil || len(drained) != 1 || drained[0]["id"] != first || drained[0]["body"] != "admitted\n" {
		t.Fatalf("drained %v %v", drained, err)
	}
	unread, _ := amq.Unread(e.Paths.Root(), "claude-02")
	if len(unread) != 1 {
		t.Fatalf("the late message was consumed: %v", unread)
	}
	receipts, _ := amq.Receipts(e.Paths.Root(), "claude-02")
	if len(receipts) != 1 || !strings.HasPrefix(receipts[0], first) {
		t.Fatalf("receipts %v", receipts)
	}
	// Through the command: mail arriving between its gate and its reads
	// stays unread.
	afterAdmit = func() {
		if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "claude-09", "claude-02", "later", "cross-Epic, inside the drain"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterAdmit = func() {} })
	sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "s", "--body", "second")
	if out, _, err := run(t, e, ctx, "claude-02", "drain", "--json"); err != nil || strings.Contains(out, "later") || !strings.Contains(out, `"count": 1`) {
		t.Fatalf("drain with a late arrival: %v %s", err, out)
	}
	afterAdmit = func() {}
	// The earlier late one met this drain's gate; the one inside it waits.
	unread, _ = amq.Unread(e.Paths.Root(), "claude-02")
	if len(unread) != 1 {
		t.Fatalf("unread %v", unread)
	}
	if h, _ := amq.ReadHeader(e.Paths.Root(), "claude-02", amq.BoxUnread, unread[0]); h.Subject != "later" {
		t.Fatalf("left %q", h.Subject)
	}
	// The next managed drain judges them: quarantined, not drained.
	if out, _, err := run(t, e, ctx, "claude-02", "drain", "--json"); err != nil || strings.Contains(out, "late") || !strings.Contains(out, `"count": 0`) {
		t.Fatalf("second drain: %v %s", err, out)
	}
	if got := quarantined(t, e, "claude-02"); len(got) != 2 {
		t.Fatalf("quarantine %v", got)
	}
}

// [F1] Two managed drains of one agent never both report a message, and
// --limit bounds each.
func TestManagedMailConcurrentDrainsReportEachMessageOnce(t *testing.T) {
	e, ctx := managedWorld(t)
	const n = 6
	for i := 0; i < n; i++ {
		sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "s", "--body", "b", "--kind", "todo", "--priority", "urgent")
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if _, err := e.ManagedMail(ctx, Location{Block: "block:claude-02"}, "drain", []string{"--json", "--limit", "0"}, nil, &out, &out); err != nil {
				t.Error(err)
				return
			}
			var doc struct {
				Drained []map[string]any `json:"drained"`
				Count   int              `json:"count"`
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Count != len(doc.Drained) {
				t.Errorf("output %q", out.String())
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, it := range doc.Drained {
				seen[it["id"].(string)]++
				if it["kind"] != "todo" || it["priority"] != "urgent" || it["moved_to_cur"] != true {
					t.Errorf("item fields %v", it)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("drained %d distinct, want %d: %v", len(seen), n, seen)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("%s reported %d times", id, c)
		}
	}
	for i := 0; i < 3; i++ {
		sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "s", "--body", "b")
	}
	if out, _, err := run(t, e, ctx, "claude-02", "drain", "--json", "--limit", "2"); err != nil || !strings.Contains(out, `"count": 2`) {
		t.Fatalf("limit: %v %s", err, out)
	}
	if out, _, err := run(t, e, ctx, "claude-02", "drain", "--include-body"); err != nil || !strings.HasPrefix(out, "[AMQ] 1 new message(s) for claude-02") || !strings.Contains(out, "  Body:\nb\n") {
		t.Fatalf("text drain: %v %q", err, out)
	}
	if out, _, err := run(t, e, ctx, "claude-02", "drain"); err != nil || out != "" {
		t.Fatalf("empty drain: %v %q", err, out)
	}
}

// [F2] A cross-Epic message whose context is past 64 KiB is judged by its
// sender like any other, not passed through as unreadable; a corrupt one is
// quarantined, never drained.
func TestQuarantineReadsLongHeadersAndFailsClosed(t *testing.T) {
	e, ctx := managedWorld(t)
	pad, _ := json.Marshal(map[string]string{"pad": strings.Repeat("x", 70<<10)})
	if _, _, err := amq.Post(ctx, e.AmqBin, e.Paths.Root(), "send", "--me", "claude-09", "--to", "claude-02", "--subject", "big", "--body", "b", "--context", string(pad)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "claude-02"), "corrupt.md"), []byte("---json\n{\"schema\":1,\"id\"\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, e, ctx, "claude-02", "drain", "--json", "--include-body")
	if err != nil || !strings.Contains(out, `"count": 0`) {
		t.Fatalf("drain: %v %s", err, out)
	}
	got := quarantined(t, e, "claude-02")
	if got["corrupt"] != core.ReasonUnreadableHeader || len(got) != 2 {
		t.Fatalf("quarantine %v", got)
	}
	for id, reason := range got {
		if id != "corrupt" && reason != core.ReasonCrossEpic {
			t.Fatalf("%s: %s", id, reason)
		}
	}
	noReceipts(t, e, "claude-02")
}

// [F3] A send that delivered and then failed (its --wait-for timed out)
// keeps its provenance, so the generation fence still holds for it.
func TestQuarantineKeepsProvenanceOfASendThatTimedOut(t *testing.T) {
	e, ctx := managedWorld(t)
	_, _, err := run(t, e, ctx, "codex-01", "send", "--to", "claude-02", "--subject", "s", "--body", "b", "--wait-for", "drained", "--wait-timeout", "20ms")
	if err == nil {
		t.Fatal("the wait did not time out")
	}
	unread, _ := amq.Unread(e.Paths.Root(), "claude-02")
	if len(unread) != 1 {
		t.Fatalf("not delivered: %v", unread)
	}
	if _, ok, _ := e.Store.ProvenanceOf(unread[0], "", "", nil); !ok {
		t.Fatal("provenance dropped for a delivered message")
	}
	rec, _, _ := e.Store.RecordOf("claude-02")
	rec = rec.Placed(rec.Server, "session:b", "w", time.Now()).Placed(rec.Server, "session:a", "w", time.Now())
	put(t, e, rec)
	if kept, _ := e.admit(rec); len(kept) != 0 {
		t.Fatalf("fence lost: kept %v", kept)
	}
	if got := quarantined(t, e, "claude-02"); got[unread[0]] != core.ReasonFenced {
		t.Fatalf("quarantine %v", got)
	}
}

// [F3] amq delivers before it writes the outbox copy and reports the id: a
// send killed in between leaves neither, and its pending provenance must
// still vouch for the delivered copy.
func TestQuarantineKeepsPendingProvenanceWhenDeliveryIsUnknown(t *testing.T) {
	e, ctx := managedWorld(t)
	fake := filepath.Join(t.TempDir(), "amq")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --to) to=$2; shift ;;
    --me) me=$2; shift ;;
    --context) ctx=$2; shift ;;
    --root) root=$2; shift ;;
  esac
  shift
done
mkdir -p "$root/agents/$to/inbox/new"
printf -- '---json\n{"schema":1,"id":"fault-1","from":"%s","to":["%s"],"thread":"t","subject":"s","created":"2026-11-12T00:00:00Z","context":%s}\n---\nbody\n' "$me" "$to" "$ctx" > "$root/agents/$to/inbox/new/fault-1.md"
echo "killed after delivery" >&2
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	real := e.AmqBin
	e.AmqBin = fake
	if err := e.mailAs(ctx, "codex-01", "claude-02", "todo", "s", "b"); err == nil {
		t.Fatal("the fake amq did not fail")
	}
	e.AmqBin = real
	hdr, err := amq.ReadHeader(e.Paths.Root(), "claude-02", amq.BoxUnread, "fault-1")
	if err != nil {
		t.Fatal(err)
	}
	var ac adapterContext
	hdr.ContextOf(store.ProvenanceContextKey, &ac)
	if p, ok, err := e.Store.ProvenanceOf("fault-1", ac.Token, "codex-01", hdr.To); err != nil || !ok || p.To[0].Generation != 0 {
		t.Fatalf("pending provenance %+v %v %v", p, ok, err)
	}
	rec, _, _ := e.Store.RecordOf("claude-02")
	rec = rec.Placed(rec.Server, "session:b", "w", time.Now()).Placed(rec.Server, "session:a", "w", time.Now())
	put(t, e, rec)
	if kept, _ := e.admit(rec); len(kept) != 0 {
		t.Fatalf("fence lost: kept %v", kept)
	}
}

// [F4] What an agent sent from a placement it has left is fenced from it,
// with provenance or without; what it sends after the move is its own.
func TestManagedMailFencesSentMailAfterAMove(t *testing.T) {
	e, ctx := managedWorld(t)
	before := sendJSON(t, e, ctx, "claude-02", "--to", "codex-01", "--subject", "s", "--body", "sent in Epic a", "--thread", "t/a")
	raw, _, err := amq.Post(ctx, e.AmqBin, e.Paths.Root(), "send", "--me", "claude-02", "--to", "codex-01", "--subject", "raw", "--body", "raw in Epic a", "--thread", "t/a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	rec, _, _ := e.Store.RecordOf("claude-02")
	put(t, e, rec.Placed(rec.Server, "session:b", "w", time.Now()))
	for _, id := range []string{before, raw.ID} {
		_, _, err := run(t, e, ctx, "claude-02", "read", "--id", id)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), core.ReasonFenced) {
			t.Fatalf("read %s after the move: %v", id, err)
		}
	}
	if _, code, err := run(t, e, ctx, "claude-02", "thread", "--id", "t/a", "--include-body"); code != 3 || err == nil {
		t.Fatalf("thread after the move: %d %v", code, err)
	}
	put(t, e, store.Record{Handle: "pi-04", SessionID: "session:b"})
	if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), "pi-04"); err != nil {
		t.Fatal(err)
	}
	after := sendJSON(t, e, ctx, "claude-02", "--to", "pi-04", "--subject", "s", "--body", "sent in Epic b", "--thread", "t/b")
	if out, _, err := run(t, e, ctx, "claude-02", "thread", "--id", "t/b", "--include-body", "--json"); err != nil || !strings.Contains(out, after) || !strings.Contains(out, "sent in Epic b") {
		t.Fatalf("thread after: %v %s", err, out)
	}
}

// [F5][F6] A managed trace takes the id before amq's flags, and shows only
// evidence scoped to the caller: its own mailbox, the traced message's
// copies, and messages it may read; not a reference from another Epic, nor
// one in a peer's mailbox that the caller cannot read.
func TestManagedMailTraceShowsOnlyScopedEvidence(t *testing.T) {
	e, ctx := managedWorld(t)
	put(t, e, store.Record{Handle: "pi-03", SessionID: "session:a"})
	// The fixture's codex mailbox is partial; amq trace scans every one.
	for _, h := range []string{"pi-03", "codex"} {
		if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	q := sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "q", "--body", "?", "--kind", "question")
	mine := sendJSON(t, e, ctx, "claude-02", "--to", "codex-01", "--subject", "r1", "--body", "to you", "--refs", q)
	peer := sendJSON(t, e, ctx, "claude-02", "--to", "pi-03", "--subject", "r2", "--body", "to a peer", "--refs", q)
	other, _, err := amq.Post(ctx, e.AmqBin, e.Paths.Root(), "send", "--me", "claude-09", "--to", "claude-02", "--subject", "r3", "--body", "from Epic b", "--refs", q)
	if err != nil {
		t.Fatal(err)
	}
	out, code, err := run(t, e, ctx, "codex-01", "trace", q, "--json")
	if err != nil || code != 0 {
		t.Fatalf("trace --json: %d %v %s", code, err, out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v %s", err, out)
	}
	if !strings.Contains(out, mine) || !strings.Contains(out, "agents/claude-02/inbox/new/"+q) || !strings.Contains(out, "agents/codex-01/outbox/sent/"+q) {
		t.Fatalf("scoped evidence missing: %s", out)
	}
	for _, hidden := range []string{peer, other.ID, "claude-09", "pi-03"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("trace shows %s: %s", hidden, out)
		}
	}
	text, code, err := run(t, e, ctx, "codex-01", "trace", q)
	if err != nil || code != 0 || !strings.HasPrefix(text, "Trace "+q) || strings.Contains(text, other.ID) {
		t.Fatalf("trace text: %d %v %s", code, err, text)
	}
}

// [F7] --context must be an object: null, a list, or a scalar is refused
// before anything is sent.
func TestManagedMailRefusesANonObjectContext(t *testing.T) {
	e, ctx := managedWorld(t)
	for _, c := range []string{"null", "[1]", "3", `"s"`} {
		if _, _, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--body", "b", "--context", c); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
			t.Fatalf("--context %s: %v", c, err)
		}
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "codex-01"); len(ids) != 0 {
		t.Fatalf("sent: %v", ids)
	}
}

// [F1] A kept message that a raw amq drain took between the gate and the
// read (it now lies in inbox/cur, where amq read still finds it) is not
// reported twice.
func TestManagedMailDrainSkipsWhatARawDrainTook(t *testing.T) {
	e, ctx := managedWorld(t)
	id := sendJSON(t, e, ctx, "codex-01", "--to", "claude-02", "--subject", "s", "--body", "b")
	afterAdmit = func() {
		if _, err := amq.Run(ctx, e.AmqBin, nil, &bytes.Buffer{}, &bytes.Buffer{}, "drain", "--root", e.Paths.Root(), "--me", "claude-02"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterAdmit = func() {} })
	if out, _, err := run(t, e, ctx, "claude-02", "drain", "--json"); err != nil || strings.Contains(out, id) || !strings.Contains(out, `"count": 0`) {
		t.Fatalf("drain: %v %s", err, out)
	}
}
