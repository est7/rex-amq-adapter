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

// A peer labels the pane with a role; the handle carries it, the kind, and a
// number, and re-adopting the same agent keeps it.
func TestPaneLabelIsTheRole(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	a := adoptIn(t, e, block, "s1", 0)
	if a.Handle != "codex-reviewer-01" || term.label != "codex-reviewer-01" {
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
