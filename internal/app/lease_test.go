package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// The recorded pid is alive but is not the process this hook comes from: the
// pid was reused by another program after agy exited, or the old agy lingers
// in the background. The hook must still rebind the record to its own process.
func TestHookRebindsWhenTheRecordedPIDIsAliveButNotItsAncestor(t *testing.T) {
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	term := &fakeTerm{state: "idle", fgPID: other.Process.Pid}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	before, _, _ := e.Store.Record(block)
	if before.AgentPID != other.Process.Pid {
		t.Fatalf("setup: pid %d", before.AgentPID)
	}
	term.mu.Lock()
	term.fgPID = os.Getpid()
	term.mu.Unlock()
	if _, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := e.Store.Record(block)
	if after.AgentPID != os.Getpid() || after.Handle != before.Handle {
		t.Errorf("record not rebound to the hook's process: %+v", after)
	}
	b, err := e.observeBinding(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if o, bound := core.CheckBinding(b); !bound {
		t.Errorf("doorbell binding check: %+v", o)
	}
}

// agy exits without SessionEnd and Codex starts in the same pane; Codex runs
// its hooks only once the user approves them, so its first hook may be a
// prompt, not SessionStart. That hook must not be taken for agy's.
func TestAnotherKindInTheBlockIsAdoptedByItsFirstHook(t *testing.T) {
	const exitedAgyPID = 0x3ffffff0 // no such process
	term := &fakeTerm{state: "working", fgPID: exitedAgyPID}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	agy, _, _ := e.Store.Record(block)
	term.mu.Lock()
	term.fgPID = os.Getpid()
	term.mu.Unlock()
	if _, err := e.Hook(ctx, EventPrompt, core.Codex, loc, HookInput{SessionID: "cx1", TranscriptPath: []byte(`"/tmp/cx1.jsonl"`)}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := e.Store.Record(block)
	if after.Kind != core.Codex || after.AgentSessionID != "cx1" || after.AgentPID != os.Getpid() {
		t.Fatalf("Codex's first hook stayed bound to agy's record: %+v", after)
	}
	if left := recordOf(t, e, agy.Handle); left.Live() {
		t.Errorf("agy's record still holds the pane: %+v", left)
	}
}

// The replacement can even inherit the exited agy's pid, so the process
// check passes and Codex, which announces its sessions, reports one the record
// does not hold. The kind alone shows the record is not its own.
func TestAnotherKindThatReusedThePIDIsNotTakenForTheRecordsAgent(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	agy, _, _ := e.Store.Record(block)
	if _, err := e.Hook(ctx, EventPrompt, core.Codex, loc, HookInput{SessionID: "cx1", TranscriptPath: []byte(`"/tmp/cx1.jsonl"`)}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := e.Store.Record(block)
	if after.Kind != core.Codex || after.AgentSessionID != "cx1" {
		t.Fatalf("Codex's hook was taken for agy's: %+v", after)
	}
	if after.Handle == agy.Handle {
		t.Errorf("Codex took agy's handle %s: a handle must name its own harness", agy.Handle)
	}
}

// While adoption backs off, a hook that is not the record's agent reads none
// of the record's mail: it must not be told of it or mark it announced. The
// record is another agent's when it is of another kind, or when another
// process holds another session (a crashed Codex replaced by a new one whose
// hooks are not yet approved for SessionStart).
func TestAnotherAgentWaitingToBeAdoptedReadsNoMail(t *testing.T) {
	const exitedPID = 0x3ffffff0 // no such process
	for _, c := range []struct {
		name string
		kind core.Kind
		pid  int
		in   HookInput
	}{
		{"another kind", core.Agy, os.Getpid(), HookInput{ConversationID: "current"}},
		{"another process and session", core.Codex, exitedPID, HookInput{SessionID: "current", TranscriptPath: []byte(`"/tmp/current.jsonl"`)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := fixture(t, &fakeTerm{state: "working"}, "old-session-mail")
			rec, _, err := e.Store.Record(block)
			if err != nil {
				t.Fatal(err)
			}
			rec.AgentPID, rec.AgentSessionID = c.pid, "previous"
			if err := e.Store.PutRecord(rec); err != nil {
				t.Fatal(err)
			}
			if err := e.Store.SetAdoptFailed(block, time.Now()); err != nil {
				t.Fatal(err)
			}
			r, err := e.Hook(context.Background(), EventPostTool, c.kind, Location{Block: block}, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if r.Notice != "" {
				t.Errorf("another agent was told of the record's mail: %q", r.Notice)
			}
			ledger, err := e.Store.Announced(rec.Handle)
			if err != nil {
				t.Fatal(err)
			}
			if ledger.IDs["old-session-mail"] {
				t.Error("another agent marked the record's mail announced")
			}
		})
	}
}

// An installed hook runs under one or two shells, so the recorded harness is
// a grandparent or further up, not the hook's own process. Finding it there
// makes the record current: the hook announces mail and does not adopt again,
// which would cost every tool call the adoption's time (here adoption cannot
// succeed, so a retry would leave a failure behind).
func TestRecordedProcessFurtherUpTheAncestryIsCurrent(t *testing.T) {
	e := fixture(t, &fakeTerm{state: "working"}, "m1")
	rec, _, err := e.Store.Record(block)
	if err != nil {
		t.Fatal(err)
	}
	rec.AgentSessionID = "s1"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	const adapter, shell = 0x3ffffff1, 0x3ffffff2
	e.Ancestors = func() ([]int, error) { return []int{adapter, shell, rec.AgentPID}, nil }
	r, err := e.Hook(context.Background(), EventPostTool, core.Codex, Location{Block: block}, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Notice, "1 unread") {
		t.Errorf("the hook did not announce the record's mail: %q", r.Notice)
	}
	if _, failed, err := e.Store.AdoptFailedAt(block); err != nil || failed {
		t.Errorf("the hook adopted again (failed %v, err %v)", failed, err)
	}
}

// When the hook cannot read its own ancestry it cannot confirm the record, so
// it adopts again rather than trust a lease it cannot check. The record here
// is in fact current, so only the failed read can cause the adoption, which
// takes the block's foreground process.
func TestUnreadableAncestryAdoptsAgain(t *testing.T) {
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if before, _, _ := e.Store.Record(block); before.AgentPID != os.Getpid() {
		t.Fatalf("setup: pid %d", before.AgentPID)
	}
	e.Ancestors = func() ([]int, error) { return nil, errors.New("sysctl failed") }
	term.mu.Lock()
	term.fgPID = other.Process.Pid
	term.mu.Unlock()
	if _, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := e.Store.Record(block); after.AgentPID != other.Process.Pid {
		t.Errorf("a lease the hook could not check was trusted: %+v", after)
	}
}

func TestProcessAncestorsStartAtThisProcess(t *testing.T) {
	a, err := processAncestors()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 2 || a[0] != os.Getpid() || a[1] != os.Getppid() {
		t.Fatalf("ancestry %v: want this process, then its parent", a)
	}
}
