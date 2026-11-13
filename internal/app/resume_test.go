package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// adoptIn adopts a Codex session in block b and departs it at cleanup.
func adoptIn(t *testing.T, e Env, b, session string, pid int) store.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := e.Adopt(ctx, Location{Block: b}, core.Codex, HookInput{SessionID: session, AgentPID: pid})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: b}, HookInput{}) })
	return a.Record
}

func depart(t *testing.T, e Env, b, session string) {
	t.Helper()
	if err := e.Depart(context.Background(), Location{Block: b}, HookInput{SessionID: session}); err != nil {
		t.Fatal(err)
	}
}

func recordOf(t *testing.T, e Env, h core.Handle) store.Record {
	t.Helper()
	recs, err := e.Store.Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Handle == h {
			return r
		}
	}
	t.Fatalf("no record for %s in %+v", h, recs)
	return store.Record{}
}

// Closing a pane and resuming its session in a new one keeps the handle, so
// peers' mail and the conversation's references to it still reach it. The
// reservation holds while other agents start meanwhile.
func TestResumedSessionTakesItsHandleBackInANewPane(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	first := adoptIn(t, e, "block:a", "s1", 0)
	depart(t, e, "block:a", "s1")
	other := adoptIn(t, e, "block:c", "s2", 0)
	if other.Handle == first.Handle {
		t.Fatalf("a new session took %s, reserved for a departed one", first.Handle)
	}
	resumed := adoptIn(t, e, "block:b", "s1", 0)
	if resumed.Handle != first.Handle || resumed.BlockID != "block:b" || !resumed.Live() {
		t.Fatalf("resume in a new pane: got %+v, want %s bound to block:b", resumed, first.Handle)
	}
	if !wakerLive(t, e, string(first.Handle)) {
		t.Fatal("the resumed agent has no waker")
	}
	if r, _, _ := e.Store.Record("block:a"); r.Handle == first.Handle && r.BlockID == "block:a" {
		t.Fatalf("the old pane still holds the resumed handle: %+v", r)
	}
}

// A new session in a pane is a new identity; the pane's previous session
// keeps its handle reserved for its own resume.
func TestNewSessionInAPaneGetsANewHandle(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	first := adoptIn(t, e, block, "s1", 0)
	depart(t, e, block, "s1")
	second := adoptIn(t, e, block, "s2", 0)
	if second.Handle == first.Handle {
		t.Fatalf("a new session inherited %s", first.Handle)
	}
	if r := recordOf(t, e, first.Handle); r.Live() || r.AgentSessionID != "s1" {
		t.Fatalf("the previous session's reservation was lost: %+v", r)
	}
}

// A process that switches to a reserved session (/resume inside the agent)
// takes that session's handle; the session it left keeps its own.
func TestSessionSwitchInOneProcessFollowsTheSession(t *testing.T) {
	term := &fakeTerm{}
	e := realAmqEnv(t, term)
	elsewhere := adoptIn(t, e, "block:b", "s2", 4242)
	depart(t, e, "block:b", "s2")
	before := adoptIn(t, e, block, "s1", 0)
	after := adoptIn(t, e, block, "s2", 0)
	if after.Handle != elsewhere.Handle || after.AgentPID != before.AgentPID {
		t.Fatalf("switch to s2: got %+v, want %s", after, elsewhere.Handle)
	}
	left := recordOf(t, e, before.Handle)
	if left.Live() || left.AgentSessionID != "s1" || wakerLive(t, e, string(before.Handle)) {
		t.Fatalf("the session left behind: %+v (waker live=%v)", left, wakerLive(t, e, string(before.Handle)))
	}
}

// A peer labels the pane with a role; the record keeps it, the handle is the
// kind and a number, the pane shows both, and re-adopting the agent keeps it.
func TestPaneLabelIsTheRole(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	a := adoptIn(t, e, block, "s1", 0)
	if a.Handle != "codex-01" || a.Role != "reviewer" || term.label != "reviewer(codex-01)" {
		t.Fatalf("handle %s, pane label %q", a.Handle, term.label)
	}
	again := adoptIn(t, e, block, "s1", 0)
	if again.Handle != a.Handle {
		t.Fatalf("re-adoption renamed %s to %s", a.Handle, again.Handle)
	}
}

// A record parked while its agent still runs (collection that could not see
// the block) is taken back by the agent's next hook. The agent was told its
// handle when this session first adopted it, so re-adoption says nothing:
// for Pi an identity-only notice from the mail watcher starts a turn of its
// own. A different session in the pane does not know its handle and is told.
func TestReadoptionOfTheSameSessionRepeatsNoIdentity(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	loc := Location{Block: block}
	pid := os.Getpid()
	if _, err := e.Hook(ctx, EventSessionStart, core.Pi, loc, HookInput{SessionID: "s1", AgentPID: pid}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	depart(t, e, block, "s1")

	r, err := e.Hook(ctx, EventMail, core.Pi, loc, HookInput{SessionID: "s1", AgentPID: pid, Idle: true})
	if err != nil || r.Notice != "" {
		t.Fatalf("re-adopting the same session: notice %q, err %v", r.Notice, err)
	}
	if rec := recordOf(t, e, "pi-01"); !rec.Live() || rec.AgentSessionID != "s1" {
		t.Fatalf("the session did not take pi-01 back: %+v", rec)
	}

	depart(t, e, block, "s1")
	r, err = e.Hook(ctx, EventMail, core.Pi, loc, HookInput{SessionID: "s2", AgentPID: pid, Idle: true})
	if err != nil || !strings.Contains(r.Notice, "you are pi-02") {
		t.Fatalf("a new session in the pane must learn its handle: notice %q, err %v", r.Notice, err)
	}
}

// Antigravity names its session conversationId; re-adopting the same
// conversation is the same session and repeats no identity either.
func TestAgyReadoptionByConversationRepeatsNoIdentity(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	depart(t, e, block, "c1")
	r, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c1"})
	if err != nil || r.Notice != "" {
		t.Fatalf("re-adopting the same conversation: notice %q, err %v", r.Notice, err)
	}
	depart(t, e, block, "c1")
	r, err = e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c2"})
	if err != nil || !strings.Contains(r.Notice, "you are agy-02") {
		t.Fatalf("a new conversation must learn its handle: notice %q, err %v", r.Notice, err)
	}
}

// Antigravity fires SessionStart only for a new conversation and has no
// SessionEnd, so an agy that quits and is /resumed by a new agy process in the
// same pane reaches the adapter first as a PreInvocation. That hook must rebind
// the record to the live process: otherwise the doorbell's binding check
// reports the agent gone and the next collect parks it.
func TestAgyResumedInANewProcessRebindsItsRecord(t *testing.T) {
	const deadPID = 0x3ffffff0 // no such process
	term := &fakeTerm{state: "idle", fgPID: deadPID}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	e.Ancestors = agyAt(deadPID)
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	e.Ancestors = nil
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	before, _, _ := e.Store.Record(block)
	if before.AgentPID != deadPID {
		t.Fatalf("setup: pid %d", before.AgentPID)
	}

	term.mu.Lock()
	term.fgPID = os.Getpid() // the replacement agy, alive and in the foreground
	term.mu.Unlock()
	r, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Notice != "" {
		t.Errorf("rebinding the same conversation repeated its identity: %q", r.Notice)
	}
	after, _, _ := e.Store.Record(block)
	if after.AgentPID != os.Getpid() || after.Handle != before.Handle {
		t.Errorf("record not rebound to the live process: %+v", after)
	}
	b, err := e.observeBinding(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if o, bound := core.CheckBinding(b); !bound {
		t.Errorf("doorbell binding check fails: %+v", o)
	}
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, e, before.Handle); !rec.Live() {
		t.Errorf("collect parked the live resumed agent: %+v", rec)
	}
}

// agy runs a subagent as a conversation of its own inside the same process,
// and its hooks carry the subagent's conversationId from the parent's pane
// (seen 2026-11-05: 3 of 20 agy records held a subagent's id). The pane must
// stay the parent's: same handle, nothing parked. The subagent's SessionStart
// rewrites the record's session (a known gap); after it, no hook of either
// conversation adopts again, so none repeats the identity or rewrites the
// record.
func TestAgySubagentDoesNotTakeThePane(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	loc := Location{Block: block}
	for _, conversation := range []string{"parent", "child"} {
		if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: conversation}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	started, _, _ := e.Store.Record(block)
	for _, conversation := range []string{"child", "parent", "child", "parent"} {
		r, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: conversation})
		if err != nil {
			t.Fatal(err)
		}
		if r.Notice != "" {
			t.Errorf("a hook from %s adopted again: %q", conversation, r.Notice)
		}
		if now, _, _ := e.Store.Record(block); now != started {
			t.Fatalf("a hook from %s rewrote the pane's record: %+v, was %+v", conversation, now, started)
		}
	}
	recs, err := e.Store.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Errorf("a subagent left records behind: %+v", recs)
	}
}
