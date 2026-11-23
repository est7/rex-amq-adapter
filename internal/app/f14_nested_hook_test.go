package app

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// F14: a live agent's tool shell runs a headless harness (`codex exec`,
// `claude -p`, `agy -p`, `pi -p`). It inherits the pane's REX_BLOCK, so its
// hooks name the pane, and they used to adopt it in the live agent's place:
// the agent was parked as replaced, its narrator told the personal agent
// the orchestrator had left, and the next tool call took the pane back.

// Pids of the scripted ancestry above a nested harness's hook. They are
// fake: only the owner is a real process, so collection sees it running.
const (
	f14Hook      = 0x3ffffe00
	f14HookShell = 0x3ffffe01
	f14Nested    = 0x3ffffe02
	f14ToolShell = 0x3ffffe03
)

// liveProcess is a process that runs for the test: an agent whose pid
// collection's processGone finds alive.
func liveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

type f14Owner struct {
	kind core.Kind
	name string // its process name
}

var f14Owners = []f14Owner{{core.Agy, "agy"}, {core.Claude, "claude"}, {core.Codex, "codex"}, {core.Pi, "pi"}}

// ownInput is the payload of the owner's own hook.
func (o f14Owner) ownInput(pid int) HookInput {
	switch o.kind {
	case core.Agy:
		return HookInput{ConversationID: "owner-session"}
	case core.Codex:
		return HookInput{SessionID: "owner-session", TranscriptPath: []byte(`"/tmp/owner.jsonl"`)}
	}
	return HookInput{SessionID: "owner-session", AgentPID: pid} // CLAUDE_PID, Pi's process.pid
}

type f14CLI struct {
	name string
	kind core.Kind
	proc string
	in   func(owner int) HookInput
}

var f14NestedCLIs = []f14CLI{
	{"codex exec", core.Codex, "codex", func(int) HookInput {
		return HookInput{SessionID: "nested", TranscriptPath: []byte(`"/tmp/nested.jsonl"`)}
	}},
	{"claude -p without CLAUDE_PID", core.Claude, "claude", func(int) HookInput { return HookInput{SessionID: "nested"} }},
	{"claude -p with its own CLAUDE_PID", core.Claude, "claude", func(int) HookInput { return HookInput{SessionID: "nested", AgentPID: f14Nested} }},
	{"claude -p with the owner's leaked CLAUDE_PID", core.Claude, "claude", func(owner int) HookInput { return HookInput{SessionID: "nested", AgentPID: owner} }},
	{"agy -p", core.Agy, "agy", func(int) HookInput { return HookInput{ConversationID: "nested"} }},
	{"pi -p", core.Pi, "pi", func(int) HookInput { return HookInput{SessionID: "nested", AgentPID: f14Nested} }},
}

var f14Events = []string{EventSessionStart, EventPrompt, EventPreInvocation, EventPostTool, EventStop, EventStopFailure, EventSettle, EventMail, EventSessionEnd}

// f14World is a pane owned by a live agent of o's kind (a real process),
// adopted, with an unread message and a turn-error mark.
func f14World(t *testing.T, o f14Owner, pid int) (Env, *fakeTerm, agent) {
	t.Helper()
	term := &fakeTerm{state: "working", windows: map[string]string{}, fgs: map[string]rex.Process{block: {PID: pid, Name: o.name}}}
	e := realAmqEnv(t, term)
	now := time.Now().UTC()
	owner := seed{Handle: core.Handle(string(o.kind) + "-07"), Kind: o.kind, Server: "unix:///fake.sock", BlockID: block, SessionID: "session:s", WindowID: "window:1",
		AgentPID: pid, AgentSessionID: "owner-session", AdoptedAt: now, FirstAdoptedAt: now, Role: core.OrchestratorRole}
	add(t, e, owner)
	mail(t, e.Paths.Root(), string(owner.Handle), "inbox/new", "m1", "claude-09", "for the owner")
	planted := recordNamed(t, e, owner.Handle)
	if err := e.Store.PutTurnError(owner.Handle, store.TurnError{At: now, By: turnKey(planted)}); err != nil {
		t.Fatal(err)
	}
	return e, term, planted
}

// nestedAncestry is a nested harness's hook: hook ← sh ← the nested CLI ←
// the owner's tool shell ← the owner.
func nestedAncestry(n f14CLI, o f14Owner, owner int) func() ([]proc, error) {
	return func() ([]proc, error) {
		return []proc{{f14Hook, "rex-amq-adapter"}, {f14HookShell, "sh"}, {f14Nested, n.proc}, {f14ToolShell, "bash"}, {owner, o.name}, {1, "launchd"}}, nil
	}
}

// Every hook event of a nested harness, whatever the owner's harness, and
// whether the owner or the nested CLI holds the pane's foreground, leaves
// the pane as it was and answers nothing, successfully.
func TestF14NestedHarnessHooksLeaveTheLiveOwnerAlone(t *testing.T) {
	pid := liveProcess(t)
	for _, o := range f14Owners {
		for _, n := range f14NestedCLIs {
			for _, fg := range []string{"owner", "nested"} {
				t.Run(string(o.kind)+"/"+n.name+"/foreground="+fg, func(t *testing.T) {
					e, term, owner := f14World(t, o, pid)
					if fg == "nested" {
						term.fgs[block] = rex.Process{PID: f14Nested, Name: n.proc}
					}
					e.Ancestors = nestedAncestry(n, o, pid)
					before, _, _ := e.agentAt(block)
					beforeTE, _, _ := e.Store.TurnError(owner.Handle)
					ctx := context.Background()
					for _, event := range f14Events {
						r, err := e.Hook(ctx, event, n.kind, Location{Block: block}, n.in(pid))
						if err != nil || r != (Reply{}) {
							t.Errorf("%s: reply %+v, err %v; want nothing, successfully", event, r, err)
						}
					}
					after, ok, _ := e.agentAt(block)
					if !ok || !reflect.DeepEqual(after, before) {
						t.Errorf("the owner's record changed:\nbefore %+v\nafter  %+v", before, after)
					}
					if recs, _ := e.agents(); len(recs) != 1 {
						t.Errorf("records %+v, want the owner's alone", recs)
					}
					if te, ok, _ := e.Store.TurnError(owner.Handle); !ok || te != beforeTE {
						t.Errorf("the owner's turn-error mark changed: %+v %v", te, ok)
					}
					if ledger, _ := e.Store.Announced(owner.Handle); len(ledger.IDs) != 0 {
						t.Errorf("the owner's mail was announced to the nested harness: %v", ledger.IDs)
					}
					if ids := unread(t, e, string(owner.Handle)); len(ids) != 1 {
						t.Errorf("the owner's unread mail: %v", ids)
					}
				})
			}
		}
	}
}

// The guard leaves the owner's own hooks working: they come from the owner
// itself (hook ← sh ← owner), and announce its mail.
func TestF14OwnersOwnHooksStillAnnounce(t *testing.T) {
	pid := liveProcess(t)
	for _, o := range f14Owners {
		t.Run(string(o.kind), func(t *testing.T) {
			e, _, _ := f14World(t, o, pid)
			e.Ancestors = func() ([]proc, error) {
				return []proc{{f14Hook, "rex-amq-adapter"}, {f14HookShell, "sh"}, {pid, o.name}, {1, "launchd"}}, nil
			}
			r, err := e.Hook(context.Background(), EventPostTool, o.kind, Location{Block: block}, o.ownInput(pid))
			if err != nil || !strings.Contains(r.Notice, "1 unread") {
				t.Fatalf("the owner's own hook: %+v %v", r, err)
			}
		})
	}
}

// An ancestry that cannot be read proves nothing: the guard lets the hook
// through rather than mute the pane's own agent.
func TestF14UnreadableAncestryDoesNotMuteTheOwner(t *testing.T) {
	pid := liveProcess(t)
	o := f14Owner{core.Claude, "claude"}
	e, _, _ := f14World(t, o, pid)
	e.Ancestors = func() ([]proc, error) { return nil, errors.New("sysctl: no such process") }
	r, err := e.Hook(context.Background(), EventPostTool, o.kind, Location{Block: block}, o.ownInput(pid))
	if err != nil || !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("the owner's own hook with unreadable ancestry: %+v %v", r, err)
	}
}

// A harness that does not run under the owner (another program the pane
// now shows) still replaces it, as before: the guard is about descent.
func TestF14AHarnessNotUnderTheOwnerStillReplacesIt(t *testing.T) {
	pid := liveProcess(t)
	e, term, owner := f14World(t, f14Owner{core.Claude, "claude"}, pid)
	term.fgs[block] = rex.Process{PID: f14Nested, Name: "codex"}
	e.Ancestors = func() ([]proc, error) {
		return []proc{{f14Hook, "rex-amq-adapter"}, {f14HookShell, "zsh"}, {f14Nested, "codex"}, {1, "launchd"}}, nil
	}
	ctx := context.Background()
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, Location{Block: block}, HookInput{SessionID: "next", TranscriptPath: []byte(`"/tmp/next.jsonl"`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, Location{Block: block}, HookInput{}) })
	if r := recordOf(t, e, owner.Handle); r.Live() || r.departReason() != store.DepartReplaced {
		t.Errorf("the owner: live %v, reason %q; want replaced", r.Live(), r.departReason())
	}
	if rec, _, _ := e.agentAt(block); !rec.Live() || rec.Kind != core.Codex || rec.Session() != "next" {
		t.Errorf("the new harness was not adopted: %+v", rec)
	}
}

func TestRunsUnderSeesHarnessesStartedInThePane(t *testing.T) {
	const self, sh, nested, tool, agent = 10, 11, 12, 13, 14
	for _, c := range []struct {
		name      string
		kind      core.Kind
		ancestors []proc
		want      error
	}{
		{"claude's own hook", core.Claude, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {agent, "claude"}}, nil},
		{"claude under a node launcher", core.Claude, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {agent, "node"}}, nil},
		{"claude -p in a claude pane", core.Claude, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {nested, "claude"}, {tool, "bash"}, {agent, "claude"}}, ErrStartedInPane},
		{"claude -p started without a shell", core.Claude, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {nested, "claude"}, {agent, "claude"}}, ErrStartedInPane},
		{"pi's own hook", core.Pi, []proc{{self, "rex-amq-adapter"}, {agent, "pi"}}, nil},
		{"pi -p in a pi pane", core.Pi, []proc{{self, "rex-amq-adapter"}, {nested, "pi"}, {tool, "zsh"}, {agent, "pi"}}, ErrStartedInPane},
		{"codex exec in a codex pane", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "zsh"}, {nested, "codex"}, {tool, "zsh"}, {agent, "codex"}}, ErrStartedInPane},
		{"codex exec in a codex pane under a launcher", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {nested, "codex"}, {tool, "bash"}, {15, "codex"}, {agent, "node"}}, ErrStartedInPane},
		// A tool shell that execs its only command leaves no shell between
		// the two: indistinguishable from Codex's shared daemon, and refused
		// as one.
		// A launcher chain that runs Codex through a shell (npx: npm ← sh ←
		// node ← codex) reads as a command in the pane, and is refused: the
		// brief's rule that a shell above the harness marks a tool shell.
		{"codex under npx's shell", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {nested, "codex"}, {15, "node"}, {tool, "sh"}, {agent, "node"}}, ErrStartedInPane},
		{"codex exec exec'd by the tool shell", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "zsh"}, {nested, "codex"}, {agent, "codex"}}, ErrSharedHarness},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := runsUnder(c.kind, c.ancestors, agent); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Errorf("runsUnder = %v, want %v", err, c.want)
			}
		})
	}
}
