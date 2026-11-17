package app

// Regression tests for the session-binding defects of the claude-38 ->
// claude-132 incident (spec: me/epics/2026-11-16-rex-amq-session-binding,
// §0.1 D-a..D-d, §3, §4, §6 AC1..AC4). They drive the adapter only through
// its entry points (hooks, collect, Caller + Exports as `env` runs them,
// ManagedMail as `drain` runs it) and observe only what an agent or the user
// sees (who's agents, which handle a pane holds, the env exports, drain's
// exit code and the inbox, the waker's pid and generation), so they keep
// their meaning when store.Record and the 5-arg waker target are replaced.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
)

const (
	sbIncumbentBlock = "block:incumbent"
	sbDuplicateBlock = "block:duplicate"
	sbPeerBlock      = "block:peer"
	sbSession        = "5e13b28a-resumed-conversation"
)

// sbAgent starts a live process standing in for a Claude Code agent: alive,
// and not an ancestor of the test, as an agent in another pane is.
func sbAgent(t *testing.T) *exec.Cmd {
	t.Helper()
	p := exec.Command("sleep", "60")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Process.Kill(); _ = p.Wait() })
	return p
}

// sbWorld is one Rex session with two panes running the same Claude
// conversation: the incumbent started it, the duplicate ran
// `claude --resume` on it while the incumbent still runs.
type sbWorld struct {
	e                    Env
	term                 *fakeTerm
	incumbent, duplicate *exec.Cmd
}

func newSBWorld(t *testing.T) *sbWorld {
	t.Helper()
	w := &sbWorld{incumbent: sbAgent(t), duplicate: sbAgent(t)}
	w.term = &fakeTerm{
		state:   "idle",
		windows: map[string]string{sbIncumbentBlock: "window:1", sbDuplicateBlock: "window:2"},
		fgs: map[string]rex.Process{
			sbIncumbentBlock: {PID: w.incumbent.Process.Pid, Name: "claude"},
			sbDuplicateBlock: {PID: w.duplicate.Process.Pid, Name: "claude"},
		},
	}
	w.e = realAmqEnv(t, w.term)
	w.e.Now = sbClock()
	t.Cleanup(func() {
		// Retire whatever the hooks started; failures here change no verdict.
		_ = w.e.Collect(context.Background())
	})
	return w
}

// sbClock is a clock that moves one second on every reading, so two
// adoptions are never at the same instant.
func sbClock() func() time.Time {
	var mu sync.Mutex
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
}

func (w *sbWorld) hook(t *testing.T, event, blk string, agent *exec.Cmd, source string) Reply {
	t.Helper()
	r, err := w.e.Hook(context.Background(), event, core.Claude, Location{Block: blk}, HookInput{SessionID: sbSession, AgentPID: agent.Process.Pid, Source: source})
	if err != nil {
		t.Fatalf("%s in %s: %v", event, blk, err)
	}
	return r
}

// startIncumbent runs the incumbent's SessionStart and returns the handle
// its pane got.
func (w *sbWorld) startIncumbent(t *testing.T) core.Handle {
	t.Helper()
	w.hook(t, EventSessionStart, sbIncumbentBlock, w.incumbent, "startup")
	a, ok := sbAgentIn(t, w.e, sbIncumbentBlock)
	if !ok || a.Lifecycle.State != core.LifeLive {
		t.Fatalf("setup: the incumbent was not adopted: %+v", sbWho(t, w.e))
	}
	return a.Handle
}

// resumeDuplicate runs `claude --resume` of the incumbent's conversation in
// the duplicate pane.
func (w *sbWorld) resumeDuplicate(t *testing.T) Reply {
	t.Helper()
	return w.hook(t, EventSessionStart, sbDuplicateBlock, w.duplicate, "resume")
}

// sbWho is `who --all --json` for the session of the incumbent's pane: every
// agent there, live or departed.
func sbWho(t *testing.T, e Env) []core.Agent {
	t.Helper()
	s, err := e.FindSession(context.Background(), sbIncumbentBlock)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := e.SessionAgents(s, true)
	if err != nil {
		t.Fatal(err)
	}
	return agents
}

// sbAgentIn is the agent who lists in blk, the live one first.
func sbAgentIn(t *testing.T, e Env, blk string) (core.Agent, bool) {
	t.Helper()
	var found core.Agent
	ok := false
	for _, a := range sbWho(t, e) {
		if a.BlockID != blk {
			continue
		}
		if !ok || a.Lifecycle.State == core.LifeLive {
			found, ok = a, true
		}
	}
	return found, ok
}

func sbHandles(agents []core.Agent) []string {
	var out []string
	for _, a := range agents {
		out = append(out, string(a.Handle)+"@"+a.BlockID+"("+a.Lifecycle.State+")")
	}
	return out
}

// sbShellOf is the Env a command in agent's tool shell runs with: its
// ancestry passes through agent, and its environment names the conversation
// and the pane it was started in.
func sbShellOf(e Env, agent *exec.Cmd, blk string) (Env, func(string) string) {
	e.Ancestors = func() ([]proc, error) {
		return []proc{{PID: os.Getpid(), Name: "rex-amq-adapter"}, {PID: 999990, Name: "zsh"}, {PID: agent.Process.Pid, Name: "claude"}, {PID: 1, Name: "launchd"}}, nil
	}
	return e, envOf("REX_BLOCK", blk, "CLAUDE_CODE_SESSION_ID", sbSession)
}

// sbEnv runs `rex-amq-adapter env` as main.go does: the caller's location,
// then its exports.
func sbEnv(e Env, getenv func(string) string) (string, error) {
	loc, err := e.Caller(context.Background(), getenv)
	if err != nil {
		return "", err
	}
	return e.Exports(loc)
}

// sbDrain runs `rex-amq-adapter drain --json` as main.go does and returns its
// exit code and output.
func sbDrain(e Env, getenv func(string) string) (int, string) {
	loc, err := e.Caller(context.Background(), getenv)
	if err != nil {
		return 1, err.Error()
	}
	var out bytes.Buffer
	code, err := e.ManagedMail(context.Background(), loc, "drain", []string{"--json"}, strings.NewReader(""), &out, &out)
	if err != nil && code == 0 {
		code = 1
	}
	if err != nil {
		out.WriteString(err.Error())
	}
	return code, out.String()
}

// sbReadingQueue is amq.Fake plus amq's `read --id`, which drain runs per
// message: it moves the message to inbox/cur and prints it. Every amq run is
// recorded with its argv.
type sbReadingQueue struct {
	Queue
	mu   sync.Mutex
	runs [][]string
}

func (q *sbReadingQueue) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	q.mu.Lock()
	q.runs = append(q.runs, slices.Clone(args))
	q.mu.Unlock()
	code, err := q.Queue.Run(ctx, stdin, stdout, stderr, args...)
	if !errors.Is(err, amq.ErrNotEmulated) || len(args) == 0 || args[0] != "read" {
		return code, err
	}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	root, me, id := flag("--root"), flag("--me"), flag("--id")
	h, err := amq.ReadHeader(root, me, amq.BoxUnread, id)
	if errors.Is(err, os.ErrNotExist) {
		return 3, nil
	}
	if err != nil {
		return 1, err
	}
	box := filepath.Join(root, "agents", me, "inbox")
	if err := os.MkdirAll(filepath.Join(box, "cur"), 0o700); err != nil {
		return 1, err
	}
	if err := os.Rename(filepath.Join(box, "new", id+".md"), filepath.Join(box, "cur", id+".md")); err != nil {
		return 1, err
	}
	b, err := json.Marshal(map[string]any{"header": h, "body": ""})
	if err != nil {
		return 1, err
	}
	_, err = stdout.Write(b)
	return 0, err
}

// readsAs reports the amq runs that read or drained handle's mailbox.
func (q *sbReadingQueue) readsAs(handle core.Handle) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, a := range q.runs {
		if (a[0] == "read" || a[0] == "drain") && slices.Contains(a, string(handle)) {
			out = append(out, strings.Join(a, " "))
		}
	}
	return out
}

// D-a, AC1 (and AC3): while the incumbent runs a conversation, a second pane
// resuming it gets no handle of its own; the conversation stays one identity
// and the duplicate pane's `env` fails. Once the incumbent's pane closes, the
// duplicate's next prompt takes over the original handle.
// On ba1eb51 the resume finds the incumbent "running elsewhere"
// (adopt.go:420-425) and NameHandle mints a second handle.
func TestSessionBindingDaConcurrentResumeKeepsOneIdentity(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)

	r := w.resumeDuplicate(t)
	t.Logf("duplicate's SessionStart notice: %q", r.Notice)
	if dup, ok := sbAgentIn(t, w.e, sbDuplicateBlock); ok {
		t.Fatalf("D-a (AC1): a concurrent --resume of the conversation %s held by %s minted a second handle %s in the duplicate pane; who lists %v",
			sbSession, original, dup.Handle, sbHandles(sbWho(t, w.e)))
	}
	if agents := sbWho(t, w.e); len(agents) != 1 || agents[0].Handle != original || agents[0].Lifecycle.State != core.LifeLive {
		t.Fatalf("D-a (AC1): want only %s, live in %s; who lists %v", original, sbIncumbentBlock, sbHandles(agents))
	}
	dupEnv, dupShell := sbShellOf(w.e, w.duplicate, sbDuplicateBlock)
	if out, err := sbEnv(dupEnv, dupShell); err == nil {
		t.Fatalf("D-a (AC1): `env` in the duplicate pane succeeded: %q", out)
	}

	// AC3: the incumbent's pane closes; the duplicate's next prompt is the
	// conversation's only process, and it takes the original handle back.
	w.term.closed = append(w.term.closed, sbIncumbentBlock)
	_ = w.incumbent.Process.Kill()
	_ = w.incumbent.Wait()
	w.hook(t, EventPrompt, sbDuplicateBlock, w.duplicate, "")
	if a, ok := sbAgentIn(t, w.e, sbDuplicateBlock); !ok || a.Handle != original || a.Lifecycle.State != core.LifeLive {
		t.Fatalf("D-a (AC3): after the incumbent's pane closed, the duplicate pane holds %+v, want %s live; who lists %v", a.Handle, original, sbHandles(sbWho(t, w.e)))
	}
}

// D-b, AC2: the duplicate's SessionEnd ends nothing of the incumbent's: the
// incumbent stays live in its pane, with the same waker.
// On ba1eb51 jobLocation (claudejob.go:262) routes SessionEnd by session id
// alone to the incumbent's block, and Depart (lifecycle.go:111) parks it.
func TestSessionBindingDbDuplicateSessionEndLeavesIncumbentLive(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	before := wakeState(t, w.e, string(original))
	if !before.Live {
		t.Fatalf("setup: %s has no live waker: %+v", original, before)
	}
	w.resumeDuplicate(t)

	w.hook(t, EventSessionEnd, sbDuplicateBlock, w.duplicate, "")

	a, ok := sbAgentIn(t, w.e, sbIncumbentBlock)
	if !ok || a.Handle != original || a.Lifecycle.State != core.LifeLive {
		t.Fatalf("D-b (AC2): the duplicate pane's SessionEnd parked the live incumbent: %s in %s is %q (reason %q), want live; who lists %v",
			a.Handle, sbIncumbentBlock, a.Lifecycle.State, a.Lifecycle.DepartReason, sbHandles(sbWho(t, w.e)))
	}
	if after := wakeState(t, w.e, string(original)); !after.Live || after.PID != before.PID || after.Generation != before.Generation {
		t.Fatalf("D-b (AC2): the duplicate pane's SessionEnd retired or replaced %s's waker: before %+v, after %+v", original, before, after)
	}
}

// D-c, AC3: the conversation keeps its original handle. After the duplicate
// pane is gone, the incumbent's next SessionStart (a compaction) in its own
// pane keeps the handle it has held all along.
// On ba1eb51 claim -> resumable (adopt.go:426) picks the conversation's
// record with the newest AdoptedAt, the duplicate's, and renames the
// incumbent to it.
func TestSessionBindingDcIncumbentKeepsItsHandleAfterDuplicateLeft(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	w.resumeDuplicate(t)
	t.Logf("after the duplicate's resume, who lists %v", sbHandles(sbWho(t, w.e)))

	// The duplicate pane is closed; collection reconciles it.
	w.term.closed = append(w.term.closed, sbDuplicateBlock)
	_ = w.duplicate.Process.Kill()
	_ = w.duplicate.Wait()
	if err := w.e.Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	t.Logf("after collection, who lists %v", sbHandles(sbWho(t, w.e)))

	r := w.hook(t, EventSessionStart, sbIncumbentBlock, w.incumbent, "compact")
	a, ok := sbAgentIn(t, w.e, sbIncumbentBlock)
	if !ok || a.Handle != original || a.Lifecycle.State != core.LifeLive {
		t.Fatalf("D-c (AC3): the incumbent's compaction renamed it from %s to %s (the newest record of %s won); notice %q; who lists %v",
			original, a.Handle, sbSession, r.Notice, sbHandles(sbWho(t, w.e)))
	}
}

// D-d, AC1b: a command in the duplicate pane's tool shell has no identity:
// `env` and `drain` exit non-zero and the incumbent's unread mail stays
// unread. A command in the incumbent's own shell still drains it.
// On ba1eb51 Caller -> sessionRecord (caller.go:63,75) takes the one live
// record of the session id without checking the command descends from its
// agent, so the duplicate's shell is the incumbent.
func TestSessionBindingDdDuplicateShellCannotReadIncumbentMail(t *testing.T) {
	w := newSBWorld(t)
	q := &sbReadingQueue{Queue: w.e.AMQ}
	w.e.AMQ = q
	original := w.startIncumbent(t)

	// A peer in the same Rex session mails the incumbent.
	peer := sbAgent(t)
	w.term.windows[sbPeerBlock] = "window:3"
	w.term.fgs[sbPeerBlock] = rex.Process{PID: peer.Process.Pid, Name: "claude"}
	if _, err := w.e.Hook(context.Background(), EventSessionStart, core.Claude, Location{Block: sbPeerBlock}, HookInput{SessionID: "peer-session", AgentPID: peer.Process.Pid, Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	var sent bytes.Buffer
	if code, err := w.e.ManagedMail(context.Background(), Location{Block: sbPeerBlock}, "send", []string{"--to", string(original), "--subject", "for the incumbent", "--body", "b"}, strings.NewReader(""), &sent, &sent); err != nil || code != 0 {
		t.Fatalf("setup: the peer's send: %d %v %s", code, err, sent.String())
	}
	unread, err := amq.Unread(w.e.Paths.Root(), string(original))
	if err != nil || len(unread) != 1 {
		t.Fatalf("setup: %s's unread mail: %v %v", original, unread, err)
	}

	incEnv, incShell := sbShellOf(w.e, w.incumbent, sbIncumbentBlock)
	if out, err := sbEnv(incEnv, incShell); err != nil || !strings.Contains(out, string(original)) {
		t.Fatalf("control: `env` in the incumbent's own shell: %q %v, want AM_ME=%s", out, err, original)
	}

	w.resumeDuplicate(t)
	dupEnv, dupShell := sbShellOf(w.e, w.duplicate, sbDuplicateBlock)
	if out, err := sbEnv(dupEnv, dupShell); err == nil {
		t.Errorf("D-d (AC1b): `env` in the duplicate pane's shell exited 0 with %q; it must have no identity (the incumbent is %s)", strings.TrimSpace(out), original)
	}
	code, out := sbDrain(dupEnv, dupShell)
	left, _ := amq.Unread(w.e.Paths.Root(), string(original))
	if code == 0 || !slices.Equal(left, unread) || len(q.readsAs(original)) > 0 {
		t.Fatalf("D-d (AC1b): `drain` in the duplicate pane's shell exited %d and read the incumbent %s's mailbox: unread %v -> %v, amq runs %q; output %s",
			code, original, unread, left, q.readsAs(original), out)
	}

	if code, out := sbDrain(incEnv, incShell); code != 0 || !strings.Contains(out, unread[0]) {
		t.Fatalf("control: `drain` in the incumbent's own shell: exit %d, output %s, want %s", code, out, unread[0])
	}
}

// waker-move, AC4: a Claude Code background job shown in another pane keeps
// its handle and the same waker: amq wake reports the same pid and
// generation after the move.
// On ba1eb51 the waker targets the block (amq/wake.go:44-50), so the move
// (adopt.go:147-150) replaces it with a new one.
func TestSessionBindingWakerMoveKeepsTheJobsWaker(t *testing.T) {
	w := newJobWorld(t)
	w.e.Now = sbClock()
	w.jobHook(t, EventSessionStart)
	// who: the session is read afresh on each call, as the command does.
	jobIn := func(blk string) (core.Agent, bool) {
		s, err := w.e.FindSession(context.Background(), tuiBlock)
		if err != nil {
			t.Fatal(err)
		}
		agents, err := w.e.SessionAgents(s, false)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(agents, func(a core.Agent) bool { return a.BlockID == blk })
		if i < 0 {
			return core.Agent{}, false
		}
		return agents[i], true
	}
	job, ok := jobIn(viewBlock)
	if !ok {
		t.Fatalf("setup: the job was not adopted in %s", viewBlock)
	}
	before := wakeState(t, w.e, string(job.Handle))
	if !before.Live {
		t.Fatalf("setup: %s has no live waker: %+v", job.Handle, before)
	}

	const next = "block:next"
	w.term.windows[next] = "window:3"
	w.term.fgs[next] = rex.Process{PID: 2, Name: "claude"}
	w.term.titles[next] = w.term.titles[viewBlock]
	w.term.titles[viewBlock] = "2 awaiting input · claude agents"
	w.jobHook(t, EventPrompt)
	if moved, ok := jobIn(next); !ok || moved.Handle != job.Handle {
		t.Fatalf("the job did not follow its pane to %s under %s: %+v", next, job.Handle, moved)
	}

	after := wakeState(t, w.e, string(job.Handle))
	if !after.Live || after.PID != before.PID || after.Generation != before.Generation {
		t.Fatalf("waker-move (AC4): moving %s from %s to %s restarted its waker: pid %d gen %q -> pid %d gen %q (live %v)",
			job.Handle, viewBlock, next, before.PID, before.Generation, after.PID, after.Generation, after.Live)
	}
}
