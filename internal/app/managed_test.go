package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// managedWorld is three agents on a real amq: two members of Epic a, one of
// Epic b.
func managedWorld(t *testing.T) (Env, context.Context) {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{})
	ctx := context.Background()
	put(t, e,
		seed{Handle: "codex-01", Role: "reviewer", SessionID: "session:a"},
		seed{Handle: "claude-02", Role: "implementer", SessionID: "session:a"},
		seed{Handle: "claude-09", SessionID: "session:b"},
	)
	for _, h := range []string{"codex-01", "claude-02", "claude-09"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	return e, ctx
}

func run(t *testing.T, e Env, ctx context.Context, as core.Handle, verb string, args ...string) (string, int, error) {
	t.Helper()
	var out bytes.Buffer
	code, err := e.ManagedMail(ctx, Location{Block: "block:" + string(as)}, verb, args, strings.NewReader(""), &out, &out)
	return out.String(), code, err
}

// A managed send goes only where the gateway allows, records where both
// sides were, and names its sender itself.
func TestManagedMailSend(t *testing.T) {
	e, ctx := managedWorld(t)
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--to", "claude-09", "--subject", "s", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("cross-Epic send: %v", err)
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "claude-09"); len(ids) != 0 {
		t.Fatalf("delivered across Epics: %v", ids)
	}
	for _, f := range []string{"--me", "--root", "--session", "--project"} {
		if _, code, err := run(t, e, ctx, "claude-02", "send", f, "x", "--to", "codex-01", "--body", "b"); !errors.Is(err, ErrUsage) || code != 2 {
			t.Fatalf("%s: %d %v", f, code, err)
		}
	}
	out, code, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--subject", "review", "--body", "please", "--kind", "review_request", "--json", "--context", `{"orchestrator":"claude-03"}`)
	if err != nil || code != 0 {
		t.Fatalf("send: %d %v %s", code, err, out)
	}
	var sent amq.Sent
	if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.ID == "" {
		t.Fatalf("output %q", out)
	}
	p, ok, err := e.Store.ProvenanceOf(sent.ID, "", "", nil)
	if err != nil || !ok || p.Op != core.OpSend || p.System || p.From.Handle != "claude-02" || p.From.SessionID != "session:a" || len(p.To) != 1 || p.To[0].Handle != "codex-01" {
		t.Fatalf("provenance %+v %v %v", p, ok, err)
	}
	h, err := amq.ReadHeader(e.Paths.Root(), "codex-01", amq.BoxUnread, sent.ID)
	if err != nil || h.From != "claude-02" {
		t.Fatalf("header %+v %v", h, err)
	}
	var orch string
	if !h.ContextOf("orchestrator", &orch) || orch != "claude-03" {
		t.Fatalf("the sender's context was lost: %v", h.Context)
	}
	// --context @file reads the object from the file, as amq does.
	ctxFile := filepath.Join(t.TempDir(), "ctx.json")
	if err := os.WriteFile(ctxFile, []byte(`{"orchestrator":"claude-04"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--body", "b", "--json", "--context", "@"+ctxFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(out), &sent)
	h, _ = amq.ReadHeader(e.Paths.Root(), "codex-01", amq.BoxUnread, sent.ID)
	if !h.ContextOf("orchestrator", &orch) || orch != "claude-04" {
		t.Fatalf("context from a file: %v", h.Context)
	}
	// The body comes from stdin when --body names none.
	var o bytes.Buffer
	if _, err := e.ManagedMail(ctx, Location{Block: "block:claude-02"}, "send", []string{"--to", "codex-01", "--subject", "piped"}, strings.NewReader("from stdin"), &o, &o); err != nil || !strings.HasPrefix(o.String(), "Sent ") {
		t.Fatalf("stdin body: %v %q", err, o.String())
	}
}

// Reading, draining, and threads show only what the gateway allows: mail
// from another Epic is quarantined before amq drains, and mail addressed to
// an agent before it left its session is not readable after it returns.
func TestManagedMailReadDrainAndThread(t *testing.T) {
	e, ctx := managedWorld(t)
	needsAmq(t, e, "amq's read, drain, thread and list verbs")
	out, _, err := run(t, e, ctx, "codex-01", "send", "--to", "claude-02", "--subject", "first", "--body", "one", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var first amq.Sent
	_ = json.Unmarshal([]byte(out), &first)
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "claude-09", "claude-02", "other epic", "raw"); err != nil {
		t.Fatal(err)
	}
	out, code, err := run(t, e, ctx, "claude-02", "drain", "--include-body", "--json")
	if err != nil || code != 0 || !strings.Contains(out, first.ID) || strings.Contains(out, "other epic") {
		t.Fatalf("drain: %d %v %s", code, err, out)
	}
	if got := quarantined(t, e, "claude-02"); len(got) != 1 {
		t.Fatalf("quarantine %v", got)
	}
	receipts, _ := amq.Receipts(e.Paths.Root(), "claude-02")
	if len(receipts) != 1 || !strings.HasPrefix(receipts[0], first.ID) {
		t.Fatalf("receipts %v", receipts)
	}
	if out, code, err := run(t, e, ctx, "claude-02", "read", "--id", first.ID); err != nil || code != 0 || !strings.Contains(out, "one") {
		t.Fatalf("read: %d %v %s", code, err, out)
	}
	if out, code, err := run(t, e, ctx, "claude-02", "thread", "--id", "p2p/claude-02__codex-01", "--json"); err != nil || code != 0 || !strings.Contains(out, first.ID) {
		t.Fatalf("thread: %d %v %s", code, err, out)
	}
	if out, _, err := run(t, e, ctx, "claude-02", "list", "--cur"); err != nil || !strings.Contains(out, first.ID) {
		t.Fatalf("list: %v %s", err, out)
	}
	// claude-02 leaves Epic a and comes back: its old mail is fenced.
	moveThrough(t, e, "claude-02", "window:X", "session:b", "session:a")
	for _, c := range [][]string{{"read", "--id", first.ID}, {"trace", first.ID}} {
		if _, _, err := run(t, e, ctx, "claude-02", c[0], c[1:]...); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s after the move: %v", c[0], err)
		}
	}
	if _, code, err := run(t, e, ctx, "claude-02", "thread", "--id", "p2p/claude-02__codex-01"); code != 3 || err == nil {
		t.Fatalf("thread after the move: %d %v", code, err)
	}
	if out, _, err := run(t, e, ctx, "claude-02", "list", "--cur"); err != nil || strings.Contains(out, first.ID) {
		t.Fatalf("list after the move: %v %s", err, out)
	}
	// The sender's own copy stays readable to it.
	if _, code, err := run(t, e, ctx, "codex-01", "trace", first.ID); err != nil || code != 0 {
		t.Fatalf("sender trace: %d %v", code, err)
	}
}

// A send to one member keeps amq's p2p thread; a send to several members of
// the caller's Epic under a named thread reaches each of them on that thread.
// The adapter names no thread itself, so the bare multicast stays refused.
func TestManagedMailMultiRecipientThread(t *testing.T) {
	e, ctx := managedWorld(t)
	put(t, e, seed{Handle: "agy-03", Role: "architect", SessionID: "session:a"})
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "agy-03"); err != nil {
		t.Fatal(err)
	}
	send := func(args ...string) amq.Sent {
		t.Helper()
		out, code, err := run(t, e, ctx, "claude-02", "send", append(args, "--subject", "ledger", "--body", "round 1", "--kind", "status", "--json")...)
		if err != nil || code != 0 {
			t.Fatalf("send %q: %d %v %s", args, code, err, out)
		}
		var sent amq.Sent
		if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.ID == "" {
			t.Fatalf("send %q output %q", args, out)
		}
		return sent
	}

	one := send("--to", "codex-01")
	if one.Thread != "p2p/claude-02__codex-01" || strings.Join(one.To, ",") != "codex-01" {
		t.Fatalf("a single recipient left the p2p default: %+v", one)
	}

	if _, _, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01,agy-03", "--subject", "s", "--body", "b"); err == nil {
		t.Fatal("a multicast with no thread was sent")
	}

	many := send("--to", "codex-01,agy-03", "--thread", "design/board")
	if many.Thread != "design/board" || strings.Join(many.To, ",") != "codex-01,agy-03" {
		t.Fatalf("named-thread multicast: %+v", many)
	}
	for _, h := range []string{"codex-01", "agy-03"} {
		ids, _ := amq.Unread(e.Paths.Root(), h)
		if len(ids) == 0 || ids[len(ids)-1] != many.ID {
			t.Fatalf("%s did not get the multicast: %v", h, ids)
		}
		hd, err := amq.ReadHeader(e.Paths.Root(), h, amq.BoxUnread, many.ID)
		if err != nil || hd.Thread != "design/board" || hd.From != "claude-02" {
			t.Fatalf("%s's copy: %+v %v", h, hd, err)
		}
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "agy-03"); len(ids) != 1 {
		t.Fatalf("agy-03 holds only the named-thread multicast: %v", ids)
	}
}

// A managed reply goes to the original's sender, as a reply, under the same
// gateway.
func TestManagedMailReply(t *testing.T) {
	e, ctx := managedWorld(t)
	out, _, err := run(t, e, ctx, "codex-01", "send", "--to", "claude-02", "--subject", "q", "--body", "?", "--kind", "question", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var q amq.Sent
	_ = json.Unmarshal([]byte(out), &q)
	out, code, err := run(t, e, ctx, "claude-02", "reply", "--id", q.ID, "--body", "!")
	if err != nil || code != 0 || !strings.HasPrefix(out, "Replied ") {
		t.Fatalf("reply: %d %v %s", code, err, out)
	}
	ids, _ := amq.Unread(e.Paths.Root(), "codex-01")
	if len(ids) != 1 {
		t.Fatalf("codex-01 inbox %v", ids)
	}
	if p, ok, _ := e.Store.ProvenanceOf(ids[0], "", "", nil); !ok || p.Op != core.OpReply || p.To[0].Handle != "codex-01" {
		t.Fatalf("provenance %+v %v", p, ok)
	}
	// A message from another Epic cannot be replied to.
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "claude-09", "claude-02", "x", "raw"); err != nil {
		t.Fatal(err)
	}
	unread, _ := amq.Unread(e.Paths.Root(), "claude-02")
	var stray string
	for _, id := range unread {
		if h, _ := amq.ReadHeader(e.Paths.Root(), "claude-02", amq.BoxUnread, id); h.From == "claude-09" {
			stray = id
		}
	}
	if _, _, err := run(t, e, ctx, "claude-02", "reply", "--id", stray, "--body", "no"); !errors.Is(err, ErrRefused) {
		t.Fatalf("reply across Epics: %v", err)
	}
}

// A narrator's one write into its Epic is a user-control directive to the
// orchestrator, sent with --control <request id> (core.OpControl). Without
// it, or to anyone else, the gateway refuses, and what arrives through the
// managed send passes the orchestrator's arrival gate.
func TestManagedMailNarratorControlReachesOnlyTheOrchestrator(t *testing.T) {
	e, ctx := managedWorld(t)
	put(t, e,
		seed{Handle: "claude-05", Role: core.NarratorRole, SessionID: "session:a"},
		seed{Handle: "claude-03", Role: core.OrchestratorRole, SessionID: "session:a"},
	)
	for _, h := range []string{"claude-05", "claude-03"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := run(t, e, ctx, "claude-05", "send", "--to", "claude-03", "--subject", "s", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a narrator's plain send to the orchestrator: %v", err)
	}
	if _, code, err := run(t, e, ctx, "claude-05", "send", "--to", "claude-03", "--control", "", "--body", "b"); !errors.Is(err, ErrUsage) || code != 2 {
		t.Fatalf("--control without a request id: %d %v", code, err)
	}
	if _, _, err := run(t, e, ctx, "claude-05", "send", "--to", "claude-02", "--control", "req-7", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a narrator's control to a worker: %v", err)
	}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--to", "claude-03", "--control", "req-8", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a worker sending a control directive: %v", err)
	}
	out, code, err := run(t, e, ctx, "claude-05", "send", "--to", "claude-03", "--control", "req-7", "--subject", "directive", "--body", "the user: ship it", "--json")
	if err != nil || code != 0 {
		t.Fatalf("control: %d %v %s", code, err, out)
	}
	var sent amq.Sent
	if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.ID == "" {
		t.Fatalf("output %q", out)
	}
	p, ok, err := e.Store.ProvenanceOf(sent.ID, "", "", nil)
	if err != nil || !ok || p.Op != core.OpControl || p.ControlRequestID != "req-7" {
		t.Fatalf("provenance %+v %v %v", p, ok, err)
	}
	orch, _, _ := e.agentAt("block:claude-03")
	kept, err := e.admit(context.Background(), orch)
	if err != nil || len(kept) != 1 || kept[0] != sent.ID {
		t.Fatalf("the orchestrator's gate kept %v (%v), want %s", kept, err, sent.ID)
	}
}

// A strict send names only handles with a mailbox: a peer retired from the
// Epic, whose mailbox the roster retirement removed (its identity kept for
// the narrator), is unknown to it, and no mailbox is made again for it. A
// departed peer that may be resumed keeps its mailbox and still gets mail.
func TestManagedStrictSendRefusesARetiredPeersRemovedMailbox(t *testing.T) {
	e, ctx := managedWorld(t)
	needsAmq(t, e, "amq send --strict")
	past := time.Now().Add(-time.Hour)
	put(t, e,
		seed{Handle: "codex-03", Kind: core.Codex, SessionID: "session:a", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartRetired},
		seed{Handle: "codex-04", Kind: core.Codex, SessionID: "session:a", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartSessionEnd},
	)
	for _, h := range []string{"codex-03", "codex-04"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if rep, err := e.retireRoster(ctx, []core.Handle{"codex-03", "codex-04"}, nil); err != nil || !slices.Equal(rep.Pruned, []core.Handle{"codex-03"}) {
		t.Fatalf("setup: %+v %v", rep, err)
	}
	if _, ok, _ := e.agentOf("codex-03"); !ok {
		t.Fatal("setup: the retired peer's identity is gone")
	}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--strict", "--to", "codex-03", "--subject", "s", "--body", "b"); err == nil {
		t.Fatal("a strict send to the retired peer was accepted")
	}
	if inRoster(t, e, "codex-03") {
		t.Fatal("the strict send made the retired peer's mailbox again")
	}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--strict", "--to", "codex-04", "--subject", "s", "--body", "b"); err != nil {
		t.Fatalf("a strict send to a resumable peer: %v", err)
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "codex-04"); len(ids) != 1 {
		t.Fatalf("the resumable peer's unread: %v", ids)
	}
}

// A retirement that starts while a strict send is being delivered waits for
// it: it then finds the delivered mail and keeps the mailbox, instead of
// removing it between the check and the delivery (which amq would then
// make again for the handle it still lists).
func TestManagedStrictSendFencesARetirementOfItsRecipient(t *testing.T) {
	e, ctx := managedWorld(t)
	needsAmq(t, e, "amq send --strict")
	past := time.Now().Add(-time.Hour)
	put(t, e, seed{Handle: "codex-03", Kind: core.Codex, SessionID: "session:a", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartRetired})
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "codex-03"); err != nil {
		t.Fatal(err)
	}
	type retired struct {
		rep RosterReport
		err error
	}
	done := make(chan retired, 1)
	var once sync.Once
	plain := e.AMQ
	e.AMQ = hookQueue{Queue: plain, beforePost: func(string, []string) error {
		once.Do(func() {
			go func() {
				rep, err := e.retireRoster(ctx, []core.Handle{"codex-03"}, nil)
				done <- retired{rep, err}
			}()
			time.Sleep(300 * time.Millisecond)
		})
		return nil
	}}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--strict", "--to", "codex-03", "--subject", "s", "--body", "b"); err != nil {
		t.Fatalf("the strict send, checked while the mailbox was there: %v", err)
	}
	r := <-done
	if r.err != nil || len(r.rep.Pruned) != 0 || r.rep.Kept["codex-03"] == "" {
		t.Fatalf("the retirement removed the mailbox under the send: %+v %v", r.rep, r.err)
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "codex-03"); len(ids) != 1 {
		t.Fatalf("codex-03's unread: %v", ids)
	}
}

// A strict send to a departed peer that may be resumed makes its missing
// mailbox again, as amq does for a handle it lists.
func TestManagedStrictSendRepairsAResumablePeersMailbox(t *testing.T) {
	e, ctx := managedWorld(t)
	needsAmq(t, e, "amq send --strict")
	past := time.Now().Add(-time.Hour)
	put(t, e, seed{Handle: "codex-04", Kind: core.Codex, SessionID: "session:a", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartSessionEnd})
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "codex-04"); err != nil {
		t.Fatal(err)
	}
	if r, err := e.AMQ.RemoveMailbox(ctx, e.Paths.Root(), "codex-04"); err != nil || !r.Removed {
		t.Fatalf("setup: %+v %v", r, err)
	}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--strict", "--to", "codex-04", "--subject", "s", "--body", "b"); err != nil {
		t.Fatalf("a strict send to a resumable peer: %v", err)
	}
	if ids, _ := amq.Unread(e.Paths.Root(), "codex-04"); len(ids) != 1 {
		t.Fatalf("the resumable peer's unread: %v", ids)
	}
}
