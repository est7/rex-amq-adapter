package app

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
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

// A late SessionEnd from a process the lease does not name (a Codex hook,
// which reports no pid, from the pane's previous process) does not release
// the current owner's lease: without a pid, its ancestry must place it under
// the lease's process.
func TestSessionEndOwnershipOfALateCodexEndNeedsAncestry(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	owner, old := sbAgent(t), sbAgent(t)
	plant(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: block, AgentPID: owner.Process.Pid, AgentSessionID: "same-thread"})
	e.Ancestors = func() ([]proc, error) {
		return []proc{{PID: os.Getpid(), Name: "rex-amq-adapter"}, {PID: old.Process.Pid, Name: "codex"}}, nil
	}
	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Codex, Location{Block: block}, HookInput{SessionID: "same-thread"}); err != nil {
		t.Fatal(err)
	}
	if _, live, _ := e.Store.LeaseOf("codex-01"); !live {
		t.Fatal("a late SessionEnd from another process released the current owner's lease")
	}
	// The owner's own end, placed by its ancestry, releases it.
	e.Ancestors = func() ([]proc, error) {
		return []proc{{PID: os.Getpid(), Name: "rex-amq-adapter"}, {PID: owner.Process.Pid, Name: "codex"}}, nil
	}
	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Codex, Location{Block: block}, HookInput{SessionID: "same-thread"}); err != nil {
		t.Fatal(err)
	}
	if _, live, _ := e.Store.LeaseOf("codex-01"); live {
		t.Fatal("the owner's own SessionEnd did not release its lease")
	}
}

// On a remote server the hook's ancestry is not checked (the Caller
// exemption), so it is no authority to release: a duplicate pane's
// SessionEnd, whose pid differs from the lease's, leaves the incumbent's
// lease to collection. The incumbent's own end, naming its pid, releases it.
func TestSessionEndOwnershipOfARemoteDuplicateIsNotProven(t *testing.T) {
	const remote = "tcp://remote.example:7070"
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	plant(t, e, seed{Handle: "claude-01", Kind: core.Claude, Server: remote, BlockID: "block:remote-incumbent", AgentPID: 4101, AgentSessionID: sbSession})
	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Claude, Location{Server: remote, Block: "block:duplicate"}, HookInput{SessionID: sbSession, AgentPID: 4102}); err != nil {
		t.Fatal(err)
	}
	if _, live, _ := e.Store.LeaseOf("claude-01"); !live {
		t.Fatal("a remote duplicate's SessionEnd released the live incumbent although the reported pids differ")
	}
	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Claude, Location{Server: remote, Block: "block:remote-incumbent"}, HookInput{SessionID: sbSession, AgentPID: 4101}); err != nil {
		t.Fatal(err)
	}
	if _, live, _ := e.Store.LeaseOf("claude-01"); live {
		t.Fatal("the remote incumbent's own SessionEnd did not release its lease")
	}
}

// The incumbent opened an editor (or a pager, or a shell escape): the
// editor, its child, is its block's foreground. The incumbent still runs the
// conversation, so a duplicate pane is refused, as at any other time. A
// foreground that is not the incumbent's child proves it left
// (TestTakeoverNeedsProofTheHolderIsGone).
func TestForegroundDescendantOfTheIncumbentKeepsItsLease(t *testing.T) {
	w := newSBWorld(t)
	owner := exec.Command("/bin/sh", "-c", "sleep 60 & echo $!; wait")
	pipe, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	editor, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(editor); err == nil {
			_ = p.Kill()
		}
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	w.incumbent = owner
	w.term.mu.Lock()
	w.term.fgs[sbIncumbentBlock] = rex.Process{PID: owner.Process.Pid, Name: "claude"}
	w.term.mu.Unlock()
	original := w.startIncumbent(t)
	w.term.mu.Lock()
	w.term.fgs[sbIncumbentBlock] = rex.Process{PID: editor, Name: "vim"}
	w.term.mu.Unlock()
	a, err := w.adoptDuplicate(t)
	if err == nil {
		t.Fatalf("the live incumbent %s lost its lease to the duplicate while its editor was the foreground: %s", original, a.Agent.BlockID)
	}
	wantDuplicate(t, err, original, sbIncumbentBlock)
	if l := leaseOf(t, w.e, original); l.BlockID != sbIncumbentBlock || l.Incarnation.PID != owner.Process.Pid {
		t.Fatalf("the incumbent's lease changed: %+v", l)
	}
}

// orphan starts a live process that descends from no process of the test's:
// its parent shell exits, and the process is reparented to init.
func orphan(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	return pid
}

// A pane's agent switches, inside its own process, to a conversation another
// pane's live agent holds (Claude's /resume). Its process still matches its
// pane's lease, but the conversation is another identity's: the pane is told
// at each prompt that it has no identity (AC1), and takes the conversation's
// handle over once the incumbent is gone (AC3).
func TestInProcessResumeIntoAHeldConversationIsADuplicate(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	if _, err := w.e.Hook(context.Background(), EventSessionStart, core.Claude, Location{Block: sbDuplicateBlock}, HookInput{SessionID: "fresh-conv", AgentPID: w.duplicate.Process.Pid, Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	prev, ok, err := w.e.liveAt(sbDuplicateBlock)
	if err != nil || !ok || prev.Handle == original {
		t.Fatalf("setup: the duplicate pane's own agent: %+v ok=%v err=%v", prev, ok, err)
	}
	r := w.hook(t, EventPrompt, sbDuplicateBlock, w.duplicate, "")
	wantNotice := "conversation " + sbSession + " is live as " + string(original) + " in " + sbIncumbentBlock + "; this pane has no AMQ identity until that pane closes"
	if r.Notice != wantNotice {
		t.Fatalf("AC1: a prompt after an in-process resume into %s's conversation: notice %q, want %q", original, r.Notice, wantNotice)
	}
	_ = w.incumbent.Process.Kill()
	_ = w.incumbent.Wait()
	w.hook(t, EventPrompt, sbDuplicateBlock, w.duplicate, "")
	if l := leaseOf(t, w.e, original); l.BlockID != sbDuplicateBlock || l.Incarnation.PID != w.duplicate.Process.Pid {
		t.Fatalf("AC3: after the incumbent closed, %s is %+v; want it in %s", original, l, sbDuplicateBlock)
	}
	if r := recordNamed(t, w.e, prev.Handle); r.Live() {
		t.Fatalf("the pane's previous agent %s still runs beside the conversation it switched to", prev.Handle)
	}
}

// A refused duplicate is not a failed adoption: its pane does not back off.
// Its next hook after the incumbent closed, a tool call within the backoff
// window included, takes the handle over (AC3).
func TestDuplicateNoBackoffTakesOverAtTheNextToolCall(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	if r := w.resumeDuplicate(t); !strings.Contains(r.Notice, "this pane has no AMQ identity") {
		t.Fatalf("setup: the duplicate was not refused: %q", r.Notice)
	}
	_ = w.incumbent.Process.Kill()
	_ = w.incumbent.Wait()
	w.hook(t, EventPostTool, sbDuplicateBlock, w.duplicate, "")
	if l := leaseOf(t, w.e, original); l.BlockID != sbDuplicateBlock || l.Incarnation.PID != w.duplicate.Process.Pid {
		t.Fatalf("AC3: the tool call after the incumbent closed left %s at %+v; want it in %s", original, l, sbDuplicateBlock)
	}
}

// A block id is one server's (I3 is per server and block): two servers'
// blocks of the same id are two panes. What runs in one is not the other's
// agent, and an adoption in one parks nothing in the other.
func TestServerScopeOfABlockIsItsServer(t *testing.T) {
	const elsewhere = "unix:///elsewhere.sock"
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	far := sbAgent(t)
	plant(t, e, seed{Handle: "agy-01", Kind: core.Agy, Server: elsewhere, BlockID: "block:a", AgentPID: far.Process.Pid, AgentSessionID: "conv-far", AdoptedAt: e.Now(), FirstAdoptedAt: e.Now()})
	here := Location{Server: fakeServer, Block: "block:a"}
	if out, err := e.Exports(here); err == nil {
		t.Fatalf("this server's block:a runs nothing, yet it exports %q", out)
	}
	a, err := e.Adopt(context.Background(), here, core.Codex, HookInput{SessionID: "thread-here"})
	if err != nil {
		t.Fatal(err)
	}
	if r := recordNamed(t, e, "agy-01"); !r.Live() || r.Server != elsewhere {
		t.Fatalf("adopting %s on %s parked agy-01 on %s: live=%v reason=%q", a.Agent.Handle, fakeServer, elsewhere, r.Live(), r.departReason())
	}
	out, err := e.Exports(here)
	if err != nil || !strings.Contains(out, "AM_ME='"+string(a.Agent.Handle)+"'") {
		t.Fatalf("exports of %s on %s: %q %v; want %s", here.Block, here.Server, out, err, a.Agent.Handle)
	}
	if out, err := e.Exports(Location{Server: elsewhere, Block: "block:a"}); err != nil || !strings.Contains(out, "AM_ME='agy-01'") {
		t.Fatalf("exports of block:a on %s: %q %v; want agy-01", elsewhere, out, err)
	}
}

// The fallback that keeps a pane's agent its handle across a new session in
// the same process (/clear) matches its block and process on its own server
// only: a remote lease whose block id and pid happen to equal a new local
// conversation's is another agent, and the local one is named afresh. The
// lookup by session stays global, so a resume on another server still finds
// its identity.
func TestServerScopeOfTheIdentityFallback(t *testing.T) {
	const remote = "tcp://remote.example:7070"
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	plant(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: remote, BlockID: block, AgentPID: os.Getpid(), AgentSessionID: "unrelated-remote-thread"})
	e.RexOn = func(server string) Terminal {
		if server == remote {
			return unreachableTerm{term}
		}
		return term
	}
	r, err := e.Hook(context.Background(), EventSessionStart, core.Codex, Location{Server: fakeServer, Block: block}, HookInput{SessionID: "new-local-thread"})
	if err != nil {
		t.Fatal(err)
	}
	l, live, err := e.Store.LeaseAt(fakeServer, block)
	if err != nil {
		t.Fatal(err)
	}
	if !live || l.Handle == "codex-01" {
		t.Fatalf("a new local conversation was taken for the remote codex-01 of the same block id and pid: live=%v lease=%+v notice=%q", live, l, r.Notice)
	}
	if far := leaseOf(t, e, "codex-01"); far.Server != remote || far.BlockID != block {
		t.Fatalf("the remote codex-01 moved: %+v", far)
	}
}

// The agent resumes, inside its own process, an earlier conversation of its
// own identity (one it left with /clear). The hook adopts again so that
// conversation is current: its tool shells find their identity by it.
// A conversation the identity never recorded (an agy subagent's) changes
// nothing (TestAgySubagentDoesNotTakeThePane).
func TestInProcessResumeOfAnEarlierOwnSessionMakesItCurrent(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	plant(t, e, seed{Handle: "claude-01", Kind: core.Claude, Server: fakeServer, BlockID: block, AgentPID: os.Getpid(), AgentSessionID: "before-clear"})
	id, _, err := e.Store.IdentityOf("claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Store.PutIdentity(id.WithSession("after-clear")); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Hook(context.Background(), EventPrompt, core.Claude, Location{Server: fakeServer, Block: block}, HookInput{SessionID: "before-clear", AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if got := recordNamed(t, e, "claude-01"); got.Session() != "before-clear" || !got.Live() {
		t.Fatalf("after resuming before-clear in-process, claude-01's current session is %q (live=%v)", got.Session(), got.Live())
	}
	if c, err := e.Caller(context.Background(), envOf("CLAUDE_CODE_SESSION_ID", "before-clear")); err != nil || c.Block != block {
		t.Fatalf("the resumed conversation's tool shell: %+v %v; want claude-01's %s", c, err, block)
	}
}
