package app

import (
	"bytes"
	"context"
	"encoding/json"
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

// managedWorld is three agents on a real amq: two members of Epic a, one of
// Epic b.
func managedWorld(t *testing.T) (Env, context.Context) {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{})
	ctx := context.Background()
	put(t, e,
		store.Record{Handle: "codex-01", Role: "reviewer", SessionID: "session:a"},
		store.Record{Handle: "claude-02", Role: "implementer", SessionID: "session:a"},
		store.Record{Handle: "claude-09", SessionID: "session:b"},
	)
	for _, h := range []string{"codex-01", "claude-02", "claude-09"} {
		if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), h); err != nil {
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
	out, _, err := run(t, e, ctx, "codex-01", "send", "--to", "claude-02", "--subject", "first", "--body", "one", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var first amq.Sent
	_ = json.Unmarshal([]byte(out), &first)
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "claude-09", "claude-02", "other epic", "raw"); err != nil {
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
	rec, _, _ := e.Store.RecordOf("claude-02")
	put(t, e, rec.Placed(rec.Server, "session:b", "window:X", time.Now()).Placed(rec.Server, "session:a", "window:X", time.Now()))
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
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "claude-09", "claude-02", "x", "raw"); err != nil {
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

func TestManagedMailTakeFlag(t *testing.T) {
	for _, c := range []struct {
		args   []string
		name   string
		valued bool
		value  string
		rest   string
		found  bool
	}{
		{[]string{"--to", "a", "--body", "b"}, "to", true, "a", "--body b", true},
		{[]string{"-to=a", "x"}, "to", true, "a", "x", true},
		{[]string{"--json", "--to", "a"}, "json", false, "true", "--to a", true},
		{[]string{"--body", "--to looks like a flag"}, "to", true, "", "--body --to looks like a flag", false},
		{[]string{"--", "--to", "a"}, "to", true, "", "-- --to a", false},
	} {
		v, rest, found := takeFlag(c.args, c.name, c.valued)
		if v != c.value || strings.Join(rest, " ") != c.rest || found != c.found {
			t.Errorf("takeFlag(%q, %s) = %q %q %v", c.args, c.name, v, rest, found)
		}
	}
}

// A narrator's one write into its Epic is a user-control directive to the
// orchestrator, sent with --control <request id> (core.OpControl). Without
// it, or to anyone else, the gateway refuses, and what arrives through the
// managed send passes the orchestrator's arrival gate.
func TestManagedMailNarratorControlReachesOnlyTheOrchestrator(t *testing.T) {
	e, ctx := managedWorld(t)
	put(t, e,
		store.Record{Handle: "claude-05", Role: core.NarratorRole, SessionID: "session:a"},
		store.Record{Handle: "claude-03", Role: core.OrchestratorRole, SessionID: "session:a"},
	)
	for _, h := range []string{"claude-05", "claude-03"} {
		if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), h); err != nil {
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
	orch, _, _ := e.Store.Record("block:claude-03")
	kept, err := e.admit(orch)
	if err != nil || len(kept) != 1 || kept[0] != sent.ID {
		t.Fatalf("the orchestrator's gate kept %v (%v), want %s", kept, err, sent.ID)
	}
}
