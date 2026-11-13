package app

import (
	"context"
	"os"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A headless `agy -p` inherits REX_BLOCK from the shell that runs it, and the
// user's agy hooks fire for it (observed 2026-11-12 with agy 1.5.7: SessionStart,
// PreInvocation, Stop, under the print run's own conversation id). Its hook
// chain is hook <- sh <- agy -p <- [timeout] <- the tool shell <- the pane's
// agent, so it descends from the pane's foreground without being it.
const (
	printAdapter = 0x3ffffff1
	printSh      = 0x3ffffff2
	printAgy     = 0x3ffffff3
	printTimeout = 0x3ffffff4
	printShell   = 0x3ffffff5
)

func printRunUnder(agent int, agentName string) func() ([]proc, error) {
	return func() ([]proc, error) {
		return []proc{{printAdapter, "rex-amq-adapter"}, {printSh, "sh"}, {printAgy, "agy"}, {printTimeout, "timeout"}, {printShell, "bash"}, {agent, agentName}}, nil
	}
}

var printEvents = []string{EventSessionStart, EventPreInvocation, EventStop}

// claude-14 ran `agy -p` three times; each run took its pane as agy-08..10,
// parked claude-14 as replaced, and relabelled the pane.
func TestAnAgyPrintRunInAClaudePaneIsNotThePanesAgent(t *testing.T) {
	term := &fakeTerm{state: "working"}
	e := realAmqEnv(t, term)
	term.fgs = map[string]rex.Process{block: {PID: os.Getpid(), Name: "claude"}}
	claude := store.Record{Handle: "claude-01", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: block, AgentPID: os.Getpid(), AgentSessionID: "tui"}
	if err := e.Store.PutRecord(claude); err != nil {
		t.Fatal(err)
	}
	e.Ancestors = printRunUnder(os.Getpid(), "claude")
	ctx := context.Background()
	for _, event := range printEvents {
		r, _ := e.Hook(ctx, event, core.Agy, Location{Block: block}, HookInput{ConversationID: "print", TerminationReason: "NO_TOOL_CALL"})
		if r.Notice != "" {
			t.Errorf("%s: the print run was told %q", event, r.Notice)
		}
		if now, ok, _ := e.Store.Record(block); !ok || now != claude {
			t.Fatalf("%s: the pane's record is %+v, want claude's %+v", event, now, claude)
		}
	}
	recs, _ := e.Store.Records()
	for _, r := range recs {
		if r.Kind == core.Agy {
			t.Errorf("the print run got a record: %+v", r)
		}
	}
	if term.renamed[block] != "" {
		t.Errorf("the pane was relabelled %q", term.renamed[block])
	}
}

// agy-04 ran `agy -p` from its own tool shell; the run's SessionStart claimed
// agy-04's record by its pid and wrote the run's conversation into it, so
// every later command in agy-04's shell had no identity.
func TestAnAgyPrintRunInAnAgyPaneKeepsTheParentsIdentity(t *testing.T) {
	term := &fakeTerm{state: "working"}
	e := realAmqEnv(t, term)
	term.fgs = map[string]rex.Process{block: {PID: os.Getpid(), Name: "agy"}}
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "parent"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	parent, _, _ := e.Store.Record(block)
	if parent.AgentSessionID != "parent" || parent.AgentPID != os.Getpid() {
		t.Fatalf("setup: %+v", parent)
	}
	e.Ancestors = printRunUnder(os.Getpid(), "agy")
	for _, event := range printEvents {
		r, _ := e.Hook(ctx, event, core.Agy, loc, HookInput{ConversationID: "print", TerminationReason: "NO_TOOL_CALL"})
		if r.Notice != "" {
			t.Errorf("%s: the print run was told %q", event, r.Notice)
		}
		if now, _, _ := e.Store.Record(block); now != parent {
			t.Fatalf("%s: the parent's record became %+v, was %+v", event, now, parent)
		}
	}
	if _, err := e.Caller(ctx, envOf("ANTIGRAVITY_CONVERSATION_ID", "parent", "REX_BLOCK", block)); err != nil {
		t.Errorf("the parent's tool shell lost its identity: %v", err)
	}
}

// The pane's own agy is adopted whether its hooks run under a shell or not.
func TestThePanesOwnAgyIsStillAdopted(t *testing.T) {
	const agy = 0x3ffffff6
	cases := []struct {
		name      string
		fg        rex.Process
		ancestors []proc
	}{
		{"hook under sh", rex.Process{PID: agy, Name: "agy"}, []proc{{printAdapter, "rex-amq-adapter"}, {printSh, "sh"}, {agy, "agy"}}},
		{"hook run directly", rex.Process{PID: agy, Name: "agy"}, []proc{{printAdapter, "rex-amq-adapter"}, {agy, "agy"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			term := &fakeTerm{state: "working"}
			e := realAmqEnv(t, term)
			term.fgs = map[string]rex.Process{block: c.fg}
			e.Ancestors = func() ([]proc, error) { return c.ancestors, nil }
			ctx := context.Background()
			loc := Location{Block: block}
			r, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "own"})
			t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
			if err != nil || r.Notice == "" {
				t.Fatalf("SessionStart: notice %q, err %v", r.Notice, err)
			}
			if rec, ok, _ := e.Store.Record(block); !ok || rec.Kind != core.Agy || rec.AgentPID != c.fg.PID || rec.AgentSessionID != "own" {
				t.Errorf("record %+v", rec)
			}
		})
	}
}

// agyAt places a test's agy hooks under the agy process pid, which an agy
// hook must run under to adopt pid as its pane's agent.
func agyAt(pid int) func() ([]proc, error) {
	return func() ([]proc, error) { return []proc{{os.Getpid(), "rex-amq-adapter"}, {pid, "agy"}}, nil }
}

// A rejected print run must not hold back the pane's own agy: one /resumed in
// a new process rebinds its record at its next model call, which does not
// retry an adoption that failed within a minute (AdoptRetryAfter).
func TestARejectedAgyPrintRunDoesNotDelayThePanesAgy(t *testing.T) {
	const exited = 0x3ffffff0
	term := &fakeTerm{state: "working", fgPID: exited}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	e.Ancestors = agyAt(exited)
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	term.mu.Lock()
	term.fgPID = os.Getpid() // agy resumed c1 in a new process
	term.mu.Unlock()
	e.Ancestors = printRunUnder(os.Getpid(), "agy")
	_, _ = e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "print"})
	e.Ancestors = nil
	if _, err := e.Hook(ctx, EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := e.Store.Record(block); rec.AgentPID != os.Getpid() || rec.AgentSessionID != "c1" {
		t.Errorf("the pane's agy was not rebound after a rejected print run: %+v", rec)
	}
}
