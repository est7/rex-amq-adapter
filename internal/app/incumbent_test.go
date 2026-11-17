package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Spec §3.2 (incumbent-wins) and §3.3, AC1-AC3: who speaks for a
// conversation two panes run, and when the second takes it over.

// captureDiagnostics points Diagnostics (hooks.log in a hook) at a buffer
// for the test.
func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := Diagnostics
	Diagnostics = &buf
	t.Cleanup(func() { Diagnostics = old })
	return &buf
}

func (w *sbWorld) adoptDuplicate(t *testing.T) (Adoption, error) {
	t.Helper()
	return w.e.Adopt(context.Background(), Location{Block: sbDuplicateBlock}, core.Claude, HookInput{SessionID: sbSession, AgentPID: w.duplicate.Process.Pid, Source: "resume"})
}

func leaseOf(t *testing.T, e Env, h core.Handle) store.Lease {
	t.Helper()
	l, ok, err := e.Store.LeaseOf(h)
	if err != nil || !ok {
		t.Fatalf("%s has no lease: %v", h, err)
	}
	return l
}

func wantDuplicate(t *testing.T, err error, h core.Handle, block string) {
	t.Helper()
	var dup *DuplicateSessionError
	if !errors.As(err, &dup) {
		t.Fatalf("want a duplicate-session refusal, got %v", err)
	}
	want := "conversation " + sbSession + " is live as " + string(h) + " in " + block + "; this pane has no AMQ identity until that pane closes"
	if dup.Error() != want {
		t.Fatalf("refusal %q, want %q", dup.Error(), want)
	}
}

// restartedTerm is a Rex server that restarted: same blocks, a new instance.
type restartedTerm struct {
	*fakeTerm
	instance string
}

func (r restartedTerm) Whoami(ctx context.Context) (rex.Server, error) {
	s, err := r.fakeTerm.Whoami(ctx)
	s.InstanceID = r.instance
	return s, err
}

// Each proof that the incumbent is gone lets the duplicate pane take its
// handle over, at the next epoch. Adopt is driven directly: the hooks collect
// first, which would park a holder whose block or process is gone before the
// takeover check ran.
func TestTakeoverNeedsProofTheHolderIsGone(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		gone   func(t *testing.T, w *sbWorld, holder store.Lease)
	}{
		{"its process exited", "agent_gone", func(t *testing.T, w *sbWorld, _ store.Lease) {
			_ = w.incumbent.Process.Kill()
			_ = w.incumbent.Wait()
		}},
		{"its pid names another process", "incarnation_gone", func(t *testing.T, w *sbWorld, holder store.Lease) {
			holder.Incarnation.StartTime++
			if _, err := w.e.Store.AcquireLease(holder, w.e.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"its server restarted", "server_restarted", func(t *testing.T, w *sbWorld, _ store.Lease) {
			restarted := restartedTerm{w.term, "i-after-restart"}
			w.e.Rex = restarted
			w.e.RexOn = func(string) Terminal { return restarted }
		}},
		{"its block is gone", core.CodeBlockGone, func(t *testing.T, w *sbWorld, _ store.Lease) {
			w.term.mu.Lock()
			w.term.closed = append(w.term.closed, sbIncumbentBlock)
			w.term.mu.Unlock()
		}},
		{"another process is its block's foreground", "agent_not_foreground", func(t *testing.T, w *sbWorld, _ store.Lease) {
			other := sbAgent(t)
			w.term.mu.Lock()
			w.term.fgs[sbIncumbentBlock] = rex.Process{PID: other.Process.Pid, Name: "zsh"}
			w.term.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			diag := captureDiagnostics(t)
			w := newSBWorld(t)
			original := w.startIncumbent(t)
			if _, err := w.adoptDuplicate(t); err == nil {
				t.Fatal("control: the duplicate was adopted while the incumbent runs")
			} else {
				wantDuplicate(t, err, original, sbIncumbentBlock)
			}

			c.gone(t, w, leaseOf(t, w.e, original))
			before := leaseOf(t, w.e, original)
			a, err := w.adoptDuplicate(t)
			if err != nil {
				t.Fatalf("takeover refused: %v", err)
			}
			after := leaseOf(t, w.e, original)
			if a.Agent.Handle != original || after.BlockID != sbDuplicateBlock || after.Incarnation.PID != w.duplicate.Process.Pid {
				t.Fatalf("the duplicate holds %s, lease %+v; want %s in %s", a.Agent.Handle, after, original, sbDuplicateBlock)
			}
			if after.Epoch != before.Epoch+1 {
				t.Fatalf("takeover epoch %d, want %d", after.Epoch, before.Epoch+1)
			}
			if !strings.Contains(diag.String(), "takes over "+string(original)) || !strings.Contains(diag.String(), "("+c.reason+")") {
				t.Errorf("the takeover's reason %q is not in hooks.log:\n%s", c.reason, diag.String())
			}
			// The old process's late release cannot end the new lease (I4).
			if released, err := w.e.Store.ReleaseLease(original, before.Epoch, store.Departure{Reason: store.DepartSessionEnd, At: w.e.Now()}); err != nil || released {
				t.Fatalf("the old epoch released the new lease: %v %v", released, err)
			}
		})
	}
}

// A holder on a server that cannot be reached cannot be proven gone: the
// incumbent keeps its handle.
func TestIncumbentWinsWhileItsServerCannotBeReached(t *testing.T) {
	const remote = "tcp://remote.example:7070"
	w := newSBWorld(t)
	far := sbAgent(t)
	if err := putSeed(w.e.Store, seed{Handle: "claude-07", Kind: core.Claude, Server: remote, BlockID: "block:remote", SessionID: "session:r", AgentPID: far.Process.Pid, AgentSessionID: sbSession, AdoptedAt: w.e.Now()}); err != nil {
		t.Fatal(err)
	}
	local := w.e.RexOn
	w.e.RexOn = func(s string) Terminal {
		if s == remote {
			return unreachableTerm{w.term}
		}
		return local(s)
	}
	_, err := w.adoptDuplicate(t)
	wantDuplicate(t, err, "claude-07", "block:remote")
	if l := leaseOf(t, w.e, "claude-07"); l.Server != remote || l.BlockID != "block:remote" || l.Epoch != 1 {
		t.Fatalf("the unreachable incumbent's lease changed: %+v", l)
	}
}

// AC1: the refusal comes before any side effect. The pane's previous agent is
// not parked, no handle is named, no mailbox is made. The pane is told why at
// SessionStart and at each prompt, and the reason is logged; other hooks say
// nothing and fail nothing.
func TestDuplicateRefusalChangesNothingAndIsToldAtStartAndPrompt(t *testing.T) {
	diag := captureDiagnostics(t)
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	prev := sbAgent(t)
	if err := putSeed(w.e.Store, seed{Handle: "codex-04", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: sbDuplicateBlock, SessionID: "session:s", WindowID: "window:2", AgentPID: prev.Process.Pid, AgentSessionID: "codex-thread", AdoptedAt: w.e.Now()}); err != nil {
		t.Fatal(err)
	}
	ids, _ := w.e.Store.Identities()
	boxes, _ := amq.Agents(w.e.Paths.Root())

	want := "conversation " + sbSession + " is live as " + string(original) + " in " + sbIncumbentBlock + "; this pane has no AMQ identity until that pane closes"
	for _, c := range []struct{ event, notice string }{
		{EventSessionStart, want},
		{EventPrompt, want},
		{EventPostTool, ""},
		{EventStop, ""},
	} {
		r, err := w.e.Hook(context.Background(), c.event, core.Claude, Location{Block: sbDuplicateBlock}, HookInput{SessionID: sbSession, AgentPID: w.duplicate.Process.Pid, Source: "resume"})
		if err != nil || r.Notice != c.notice {
			t.Errorf("%s: notice %q err %v; want %q", c.event, r.Notice, err, c.notice)
		}
	}
	if !strings.Contains(diag.String(), want) {
		t.Errorf("the refusal is not in hooks.log:\n%s", diag.String())
	}
	if l := leaseOf(t, w.e, "codex-04"); l.BlockID != sbDuplicateBlock {
		t.Errorf("the pane's previous agent was moved: %+v", l)
	}
	idsAfter, _ := w.e.Store.Identities()
	boxesAfter, _ := amq.Agents(w.e.Paths.Root())
	if len(idsAfter) != len(ids) || !slices.Equal(boxes, boxesAfter) {
		t.Errorf("the refusal named a handle or made a mailbox: identities %d -> %d, mailboxes %v -> %v", len(ids), len(idsAfter), boxes, boxesAfter)
	}
	if inc := leaseOf(t, w.e, original); inc.BlockID != sbIncumbentBlock || inc.Incarnation.PID != w.incumbent.Process.Pid {
		t.Errorf("the incumbent's lease changed: %+v", inc)
	}
}

// AC2: the refused pane's SessionEnd is a no-op: the incumbent's lease stays,
// at the same epoch.
func TestDuplicateSessionEndKeepsTheIncumbentsLeaseAndEpoch(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	w.resumeDuplicate(t)
	before := leaseOf(t, w.e, original)
	ctx := context.Background()
	for name, end := range map[string]func() error{
		"its SessionEnd hook": func() error {
			_, err := w.e.Hook(ctx, EventSessionEnd, core.Claude, Location{Block: sbDuplicateBlock}, HookInput{SessionID: sbSession, AgentPID: w.duplicate.Process.Pid})
			return err
		},
		// A hook without CLAUDE_PID is placed by its ancestry, which runs
		// under the duplicate's process, not the incumbent's.
		"its SessionEnd hook without a pid": func() error {
			dup, _ := sbShellOf(w.e, w.duplicate, sbDuplicateBlock)
			_, err := dup.Hook(ctx, EventSessionEnd, core.Claude, Location{Block: sbDuplicateBlock}, HookInput{SessionID: sbSession})
			return err
		},
		// Wherever it is routed, the release is the lease process's alone.
		"a departure in the incumbent's block": func() error {
			return w.e.Depart(ctx, Location{Block: sbIncumbentBlock}, HookInput{SessionID: sbSession, AgentPID: w.duplicate.Process.Pid})
		},
	} {
		if err := end(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		after := leaseOf(t, w.e, original)
		if after.Epoch != before.Epoch || after.BlockID != sbIncumbentBlock || after.Incarnation != before.Incarnation {
			t.Fatalf("%s changed the incumbent's lease: %+v -> %+v", name, before, after)
		}
	}
}

// AC3: once the incumbent's pane closes, the duplicate's next prompt takes
// the original handle over, at the next epoch.
func TestTakeoverAfterTheIncumbentsPaneClosesIsTheNextEpoch(t *testing.T) {
	w := newSBWorld(t)
	original := w.startIncumbent(t)
	w.resumeDuplicate(t)
	before := leaseOf(t, w.e, original)
	w.term.mu.Lock()
	w.term.closed = append(w.term.closed, sbIncumbentBlock)
	w.term.mu.Unlock()
	_ = w.incumbent.Process.Kill()
	_ = w.incumbent.Wait()

	w.hook(t, EventPrompt, sbDuplicateBlock, w.duplicate, "")
	after := leaseOf(t, w.e, original)
	if after.BlockID != sbDuplicateBlock || after.Epoch != before.Epoch+1 || after.Incarnation.PID != w.duplicate.Process.Pid {
		t.Fatalf("after the incumbent's pane closed: lease %+v, want %s in %s at epoch %d", after, original, sbDuplicateBlock, before.Epoch+1)
	}
}

// callerWorld is the store of a conversation whose agent, a live process,
// holds a lease on server.
func callerWorld(t *testing.T, server string, inc func(pid int) store.Incarnation) (Env, int) {
	t.Helper()
	agentProc := sbAgent(t)
	paths := store.Paths{Home: t.TempDir()}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: &fakeTerm{}}
	if err := e.Store.PutIdentity(store.Identity{Handle: "claude-01", Kind: core.Claude, Sessions: []string{sbSession}}); err != nil {
		t.Fatal(err)
	}
	pid := agentProc.Process.Pid
	if _, err := e.Store.AcquireLease(store.Lease{Handle: "claude-01", Place: store.Place{Server: server, BlockID: sbIncumbentBlock}, Incarnation: inc(pid)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return e, pid
}

func shellUnder(e Env, pid int) Env {
	e.Ancestors = func() ([]proc, error) {
		return []proc{{os.Getpid(), "rex-amq-adapter"}, {999990, "zsh"}, {pid, "claude"}, {1, "launchd"}}, nil
	}
	return e
}

// AC1b: a command whose environment names the conversation, but which does
// not run under the lease's process, has no identity.
func TestCallerRunsUnderTheLeasesIncarnation(t *testing.T) {
	realStart := func(pid int) store.Incarnation {
		st, err := processStartTime(pid)
		if err != nil {
			t.Fatal(err)
		}
		return store.Incarnation{PID: pid, StartTime: st}
	}
	env := envOf("CLAUDE_CODE_SESSION_ID", sbSession, "REX_BLOCK", sbDuplicateBlock)

	e, pid := callerWorld(t, "unix:///fake.sock", realStart)
	if loc, err := shellUnder(e, pid).Caller(context.Background(), env); err != nil || loc.Block != sbIncumbentBlock {
		t.Fatalf("the agent's own shell: %+v %v", loc, err)
	}
	other := sbAgent(t)
	_, err := shellUnder(e, other.Process.Pid).Caller(context.Background(), env)
	wantDuplicate(t, err, "claude-01", sbIncumbentBlock)

	// The pid is in the ancestry, but the lease's process started at another
	// time: the kernel reused its pid.
	e, pid = callerWorld(t, "unix:///fake.sock", func(pid int) store.Incarnation {
		inc := realStart(pid)
		inc.StartTime++
		return inc
	})
	_, err = shellUnder(e, pid).Caller(context.Background(), env)
	wantDuplicate(t, err, "claude-01", sbIncumbentBlock)

	// A remote server's processes are not this machine's: not checked.
	e, _ = callerWorld(t, "tcp://remote.example:7070", func(pid int) store.Incarnation { return store.Incarnation{PID: pid} })
	if loc, err := shellUnder(e, other.Process.Pid).Caller(context.Background(), env); err != nil || loc.Block != sbIncumbentBlock {
		t.Fatalf("a remote agent's shell: %+v %v", loc, err)
	}

	// Ancestry that cannot be read proves nothing: refused.
	e, _ = callerWorld(t, "unix:///fake.sock", realStart)
	e.Ancestors = func() ([]proc, error) { return nil, errors.New("sysctl failed") }
	if _, err := e.Caller(context.Background(), env); err == nil {
		t.Fatal("a command whose ancestry cannot be read was given the identity")
	}
}
