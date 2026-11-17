package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An adoption that died between PutIdentity and AcquireLease leaves an
// identity that names the session and was never placed. The next hook
// finishes that adoption, and the agent is told who it is: it was never told.
func TestAdoptFinishesANeverPlacedIdentityWithItsNotice(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	if err := e.Store.PutIdentity(store.Identity{Handle: "codex-01", Kind: core.Codex, Role: "reviewer", FirstAdoptedAt: e.Now(), Sessions: []string{"s1"}}); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventPrompt, core.Codex, Location{Block: block}, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if l := leaseOf(t, e, "codex-01"); l.BlockID != block || l.Adopting {
		t.Fatalf("the never-placed identity was not adopted: %+v", l)
	}
	if want := core.SessionNotice("codex-01", "reviewer", e.SelfBin, 0); !strings.Contains(r.Notice, want) {
		t.Fatalf("finishing a never-placed adoption told the agent nothing of who it is; notice %q, want it to hold %q", r.Notice, want)
	}
}

// reacquire re-acquires h's lease changed by change: a fixture for a lease
// whose process or server is not what it recorded.
func reacquire(t *testing.T, e Env, h core.Handle, change func(*store.Lease)) store.Lease {
	t.Helper()
	l := leaseOf(t, e, h)
	change(&l)
	l, err := e.Store.AcquireLease(l, e.Now())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// selfStart is this test process's start time, as a lease records it.
func selfStart(t *testing.T) int64 {
	t.Helper()
	st, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A lease names a pid that is alive but started at another time: its process
// is gone and another reuses the pid. The pane's agent, at its next prompt, is
// not taken for the lease's process: it adopts again and the lease records
// its own start time, which collection keeps and the injector accepts.
func TestStartTimeOfAReusedPIDIsRefreshedAtAPrompt(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	plant(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: block, AgentPID: os.Getpid(), AgentSessionID: "same-thread"})
	st := selfStart(t)
	reacquire(t, e, "codex-01", func(l *store.Lease) { l.Incarnation.StartTime = st - 1 })
	e.Ancestors = func() ([]proc, error) { return []proc{{PID: os.Getpid(), Name: "rex-amq-adapter"}}, nil }
	if _, err := e.Hook(context.Background(), EventPrompt, core.Codex, Location{Block: block}, HookInput{SessionID: "same-thread"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := leaseOf(t, e, "codex-01")
	if got.Incarnation.StartTime != st {
		o := e.Inject(context.Background(), InjectRequest{Handle: "codex-01", Root: e.Paths.Root()})
		t.Fatalf("prompt and collection kept the stale incarnation %d, actual %d; inject=%+v", got.Incarnation.StartTime, st, o)
	}
}

// The agent crashed back to its shell, and another process on the machine
// now has its pid: collection parks the dead lease instead of keeping it live
// because the pid exists.
func TestPIDReuseElsewhereLetsCollectionParkTheDeadLease(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	shell := sbAgent(t)
	term.mu.Lock()
	term.fgs = map[string]rex.Process{block: {PID: shell.Process.Pid, Name: "zsh"}}
	term.mu.Unlock()
	plant(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: block, AgentPID: os.Getpid(), AgentSessionID: "thread", AdoptedAt: e.Now(), FirstAdoptedAt: e.Now()})
	reacquire(t, e, "codex-01", func(l *store.Lease) { l.Incarnation.StartTime = selfStart(t) - 1 })
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := recordNamed(t, e, "codex-01"); r.Live() || r.departReason() != "agent_gone" {
		t.Fatalf("collection kept a dead lease whose pid another process reuses: live=%v reason=%q", r.Live(), r.departReason())
	}
}

// retire --force of an agent that crashed back to its shell, whose pid
// another process reuses, leaves the user's shell pane open.
func TestPIDReuseRetireForceLeavesTheShellPaneOpen(t *testing.T) {
	e, term, _, _ := retireWorld(t)
	shell := sbAgent(t)
	term.mu.Lock()
	if term.fgs == nil {
		term.fgs = map[string]rex.Process{}
	}
	term.fgs["block:w"] = rex.Process{PID: shell.Process.Pid, Name: "zsh"}
	term.mu.Unlock()
	reacquire(t, e, "codex-03", func(l *store.Lease) {
		l.Incarnation = store.Incarnation{PID: os.Getpid(), StartTime: selfStart(t) - 1}
	})
	res, err := retire(e, "block:o", "codex-03", true)
	if err != nil || res.Pane != PaneLeftOpen || len(term.closed) != 0 {
		t.Fatalf("retire --force of a dead agent whose pid is reused: %+v %v, closed %v; want the shell pane left open", res, err, term.closed)
	}
}

// whoamiCounter is a Rex server that counts whoami calls and reports
// instance as its instance id.
type whoamiCounter struct {
	*fakeTerm
	instance string
	calls    *int
}

func (w whoamiCounter) Whoami(ctx context.Context) (rex.Server, error) {
	*w.calls++
	s, err := w.fakeTerm.Whoami(ctx)
	s.InstanceID = w.instance
	return s, err
}

// A remote Rex server restarted: it restored the block under its id with a
// login shell, and the pid of the lease cannot be probed from here. The new
// server instance proves the lease's process gone, so collection parks it.
func TestPIDReuseCannotHideARemoteServerRestart(t *testing.T) {
	const remote = "tcp://remote.example:7070"
	term := &fakeTerm{state: "idle", fgs: map[string]rex.Process{"block:remote": {PID: 4242, Name: "zsh"}}}
	e := realAmqEnv(t, term)
	plant(t, e, seed{Handle: "claude-07", Kind: core.Claude, Server: remote, BlockID: "block:remote", AgentPID: 4243, AgentSessionID: "s7", AdoptedAt: e.Now(), FirstAdoptedAt: e.Now()})
	reacquire(t, e, "claude-07", func(l *store.Lease) { l.ServerInstance = "i-before" })
	calls := 0
	restarted := whoamiCounter{term, "i-after", &calls}
	e.RexOn = func(string) Terminal { return restarted }
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := recordNamed(t, e, "claude-07"); r.Live() {
		t.Fatalf("collection kept the remote lease across a server restart: %+v", leaseOf(t, e, "claude-07"))
	}
}

// The common case costs no extra Rex call: a local agent that is its block's
// foreground, its own live process, is bound whatever the server's instance.
func TestStartTimeBoundLocalAgentAsksNoServerInstance(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := fixture(t, term)
	reacquire(t, e, "codex", func(l *store.Lease) {
		l.ServerInstance = "i-before"
		l.Incarnation.StartTime = selfStart(t)
	})
	calls := 0
	counting := whoamiCounter{term, "i-before", &calls}
	e.Rex, e.RexOn = counting, func(string) Terminal { return counting }
	b, err := e.observeBinding(context.Background(), recordNamed(t, e, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if o, bound := core.CheckBinding(b); !bound || calls != 0 {
		t.Fatalf("a bound local agent: %+v bound=%v, %d whoami calls; want bound with none", o, bound, calls)
	}
}
