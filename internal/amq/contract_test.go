package amq

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// queue is what the adapter drives (app.Queue); CLI and *Fake both provide it.
type queue interface {
	EnsureMailbox(ctx context.Context, root, handle string) error
	Ensure(ctx context.Context, w Waker, logDir string) (WakeState, WakeDecision, error)
	RetireFor(ctx context.Context, root, handle, block string) (bool, error)
	RetireHandle(ctx context.Context, root, handle string) (bool, error)
	ConfigureHolds(ctx context.Context, root, handle string, normal, low time.Duration) error
	Post(ctx context.Context, root, verb string, args ...string) (Sent, []byte, error)
	ThreadOpen(ctx context.Context, root, me string, handles []string) (OpenObligations, error)
	ThreadRead(ctx context.Context, root, me, thread string, agents, ids []string) ([]ThreadMessage, error)
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error)
}

var _ = []queue{CLI{}, &Fake{}}

type contractTarget struct {
	name  string
	q     queue
	wakes func(ctx context.Context, root, handle string) (WakeState, error)
	// kill makes handle's waker die as a crash would, leaving its lock.
	kill func(t *testing.T, root, handle string)
}

// targets is the fake, always, and amq itself when it is installed: every
// contract below must hold for both, so the fake cannot drift from amq.
func targets(t *testing.T) []contractTarget {
	f := &Fake{}
	out := []contractTarget{{"fake", f,
		func(_ context.Context, root, h string) (WakeState, error) { return f.Waker(root, h), nil },
		func(_ *testing.T, root, h string) { f.Kill(root, h) }}}
	if bin, err := ResolveBin(); err == nil {
		out = append(out, contractTarget{"amq", CLI{Bin: bin},
			func(ctx context.Context, root, h string) (WakeState, error) { return WakeCheck(ctx, bin, root, h) },
			func(t *testing.T, root, h string) {
				st, err := WakeCheck(context.Background(), bin, root, h)
				if err != nil || st.PID <= 0 {
					t.Fatalf("no waker to kill: %+v %v", st, err)
				}
				if err := syscall.Kill(st.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 50; i++ {
					if st, _ := WakeCheck(context.Background(), bin, root, h); !st.Live {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				t.Fatal("the killed waker still reports live")
			}})
	} else {
		t.Logf("amq not installed: the contract runs against the fake only (%v)", err)
	}
	return out
}

func TestQueueContract(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			root := filepath.Join(t.TempDir(), "root")
			q := tc.q

			for _, h := range []string{"bob", "alice", "bob"} {
				if err := q.EnsureMailbox(ctx, root, h); err != nil {
					t.Fatalf("mailbox %s: %v", h, err)
				}
			}
			if got, err := Agents(root); err != nil || strings.Join(got, ",") != "alice,bob" {
				t.Fatalf("registered agents: %v %v (want alice,bob once each, sorted)", got, err)
			}
			if !mailboxPresent(root, "alice") || !mailboxPresent(root, "bob") {
				t.Fatal("mailbox directories missing after EnsureMailbox")
			}

			sent, _, err := q.Post(ctx, root, "send", "--me", "alice", "--to", "bob", "--subject", "hi", "--body", "hello",
				"--kind", "question", "--priority", "urgent", "--labels", "l1,l2", "--context", `{"k":{"v":1}}`)
			if err != nil || sent.ID == "" || strings.Join(sent.To, ",") != "bob" || sent.Thread != "p2p/alice__bob" {
				t.Fatalf("send: %+v %v", sent, err)
			}
			if ids, err := Unread(root, "bob"); err != nil || !slices.Equal(ids, []string{sent.ID}) {
				t.Fatalf("bob's unread: %v %v", ids, err)
			}
			h, err := ReadHeader(root, "bob", BoxUnread, sent.ID)
			if err != nil || h.From != "alice" || h.Subject != "hi" || h.Kind != "question" || h.Priority != "urgent" ||
				!slices.Equal(h.Labels, []string{"l1", "l2"}) || string(h.Context["k"]) == "" || h.Thread != sent.Thread {
				t.Fatalf("delivered header: %+v %v", h, err)
			}
			if sh, err := SentHeader(root, "alice", sent.ID); err != nil || sh.ID != sent.ID {
				t.Fatalf("alice's sent copy: %+v %v", sh, err)
			}

			reply, _, err := q.Post(ctx, root, "reply", "--me", "bob", "--id", sent.ID, "--body", "pong")
			if err != nil || strings.Join(reply.To, ",") != "alice" || reply.Thread != sent.Thread {
				t.Fatalf("reply: %+v %v", reply, err)
			}
			rh, err := ReadHeader(root, "alice", BoxUnread, reply.ID)
			if err != nil || rh.Subject != "Re: hi" || rh.From != "bob" {
				t.Fatalf("reply header: %+v %v", rh, err)
			}

			if _, _, err := q.Post(ctx, root, "send", "--me", "alice", "--to", "alice", "--subject", "s", "--body", "b"); err == nil {
				t.Fatal("a send to oneself must be refused")
			}

			if self, _, err := q.Post(ctx, root, "send", "--me", "alice", "--to", "alice", "--allow-self", "--subject", "note", "--body", "n"); err != nil || self.ID == "" {
				t.Fatalf("--allow-self sends to oneself: %+v %v", self, err)
			}
			waited, _, err := q.Post(ctx, root, "send", "--me", "alice", "--to", "bob", "--subject", "w", "--body", "w", "--wait-for", "drained", "--wait-timeout", "20ms")
			if err == nil || waited.ID == "" {
				t.Fatalf("a wait nobody satisfies fails after delivering, with the id: %+v %v", waited, err)
			}
			if ids, _ := Unread(root, "bob"); !slices.Contains(ids, waited.ID) {
				t.Fatalf("the waited send was not delivered: %v", ids)
			}
			withRefs, _, err := q.Post(ctx, root, "send", "--me", "alice", "--to", "bob", "--subject", "r", "--body", "r", "--refs", sent.ID)
			if err != nil || withRefs.ID == "" {
				t.Fatalf("--refs: %+v %v", withRefs, err)
			}

			msgs, err := q.ThreadRead(ctx, root, "alice", sent.Thread, []string{"alice", "bob"}, []string{sent.ID, reply.ID})
			if err != nil || len(msgs) != 2 || msgs[0].Body != "hello\n" || msgs[1].Body != "pong\n" || msgs[0].Kind != "question" || msgs[0].Priority != "urgent" || msgs[0].From != "alice" {
				t.Fatalf("thread read: %+v %v", msgs, err)
			}
			if _, err := q.ThreadRead(ctx, root, "alice", sent.Thread, []string{"alice", "bob"}, []string{sent.ID, "no-such-id"}); err == nil {
				t.Fatal("a thread read naming a missing id must fail as a whole")
			}

			if err := q.ConfigureHolds(ctx, root, "bob", 30*time.Second, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			if n, l, err := ReadHolds(root, "bob"); err != nil || n != 30*time.Second || l != 2*time.Minute {
				t.Fatalf("holds: %v %v %v", n, l, err)
			}
			if err := q.ConfigureHolds(ctx, root, "bob", 0, 0); err != nil {
				t.Fatal(err)
			}
			if n, l, err := ReadHolds(root, "bob"); err != nil || n != 0 || l != 0 {
				t.Fatalf("unset holds: %v %v %v", n, l, err)
			}

			dir := t.TempDir()
			injector := filepath.Join(dir, "inject.sh")
			if err := os.WriteFile(injector, []byte("#!/bin/sh\necho AMQ_INJECT_PROGRESS=deferred >&2\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			w := Waker{InjectVia: injector, Server: "unix:///tmp/rex.sock", Block: "block:one", Handle: "bob", Root: root}
			st, decision, err := q.Ensure(ctx, w, filepath.Join(dir, "logs"))
			if err != nil || decision != DecisionStart || !st.Live || st.Generation == "" {
				t.Fatalf("first ensure: %+v %s %v", st, decision, err)
			}
			t.Cleanup(func() { _, _ = q.RetireHandle(context.Background(), root, "bob") })
			if again, decision, err := q.Ensure(ctx, w, filepath.Join(dir, "logs")); err != nil || decision != DecisionKeep || again.Generation != st.Generation {
				t.Fatalf("second ensure keeps the waker: %+v %s %v", again, decision, err)
			}
			if got, err := tc.wakes(ctx, root, "bob"); err != nil || !got.Live || got.Generation != st.Generation || !slices.Equal(got.Target.InjectArgs, w.Target().InjectArgs) {
				t.Fatalf("waker as checked: %+v %v", got, err)
			}
			if retired, err := q.RetireFor(ctx, root, "bob", "block:other"); err != nil || retired {
				t.Fatalf("a waker for another block is left alone: %v %v", retired, err)
			}
			if retired, err := q.RetireFor(ctx, root, "bob", "block:one"); err != nil || !retired {
				t.Fatalf("retire the block's waker: %v %v", retired, err)
			}
			if retired, err := q.RetireHandle(ctx, root, "bob"); err != nil || retired {
				t.Fatalf("nothing left to retire: %v %v", retired, err)
			}
			if got, err := tc.wakes(ctx, root, "bob"); err != nil || got.Live {
				t.Fatalf("after retiring, no live waker: %+v %v", got, err)
			}
		})
	}
}

// An override that names no binary is an error, so a run without amq skips
// its real-amq tests instead of failing each one on a missing executable.
func TestResolveBinRejectsAMissingOverride(t *testing.T) {
	t.Setenv("REX_AMQ_AMQ", filepath.Join(t.TempDir(), "no-amq"))
	if bin, err := ResolveBin(); err == nil {
		t.Fatalf("resolved %q for a path that does not exist", bin)
	}
	t.Setenv("REX_AMQ_AMQ", t.TempDir())
	if bin, err := ResolveBin(); err == nil {
		t.Fatalf("resolved directory %q as amq", bin)
	}
}

// An injector amq would not run starts no waker, on either target: a missing
// file, a file that is not executable, and one under a directory other users
// can write (amq refuses --inject-via there).
func TestQueueContractRefusesAnUnsafeInjector(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			if err := tc.q.EnsureMailbox(context.Background(), root, "bob"); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			noExec := filepath.Join(dir, "noexec.sh")
			if err := os.WriteFile(noExec, []byte("#!/bin/sh\nexit 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			shared := filepath.Join(dir, "shared")
			if err := os.Mkdir(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(shared, 0o777); err != nil {
				t.Fatal(err)
			}
			inShared := filepath.Join(shared, "inject.sh")
			if err := os.WriteFile(inShared, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, inject := range map[string]string{"missing": filepath.Join(dir, "absent"), "not executable": noExec, "shared directory": inShared} {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				w := Waker{InjectVia: inject, Server: "unix:///tmp/rex.sock", Block: "block:one", Handle: "bob", Root: root}
				st, _, err := tc.q.Ensure(ctx, w, filepath.Join(dir, "logs"))
				cancel()
				if err == nil || st.Live {
					t.Errorf("%s injector: a waker came up: %+v %v", name, st, err)
				}
				_, _ = tc.q.RetireHandle(context.Background(), root, "bob")
			}
		})
	}
}

// Open obligations are amq's to compute: amq reports an unanswered question,
// and the fake refuses rather than report none.
func TestThreadOpenIsAmqsOrStated(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "root")
			for _, h := range []string{"alice", "bob"} {
				if err := tc.q.EnsureMailbox(ctx, root, h); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := tc.q.Post(ctx, root, "send", "--me", "alice", "--to", "bob", "--kind", "question", "--subject", "q", "--body", "?"); err != nil {
				t.Fatal(err)
			}
			open, err := tc.q.ThreadOpen(ctx, root, "alice", []string{"alice", "bob"})
			if f, ok := tc.q.(*Fake); ok {
				if !errors.Is(err, ErrNotEmulated) {
					t.Fatalf("the fake invented obligations: %+v %v", open, err)
				}
				f.OpenObligations = &OpenObligations{}
				if got, err := f.ThreadOpen(ctx, root, "alice", []string{"bob"}); err != nil || len(got.Obligations) != 0 {
					t.Fatalf("a stated world: %+v %v", got, err)
				}
				return
			}
			if err != nil || len(open.Obligations) != 1 || open.Obligations[0].OwedBy.Handle != "bob" {
				t.Fatalf("amq's open obligations: %+v %v", open, err)
			}
		})
	}
}

// A crashed waker whose injector became unusable is not brought back: a
// recovery restarts the process, and amq checks the injector again.
func TestQueueContractDoesNotRecoverAWakerWithABrokenInjector(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			root := filepath.Join(t.TempDir(), "root")
			if err := tc.q.EnsureMailbox(ctx, root, "bob"); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			inject := filepath.Join(dir, "inject.sh")
			if err := os.WriteFile(inject, []byte("#!/bin/sh\necho AMQ_INJECT_PROGRESS=deferred >&2\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			w := Waker{InjectVia: inject, Server: "unix:///tmp/rex.sock", Block: "block:one", Handle: "bob", Root: root}
			if st, _, err := tc.q.Ensure(ctx, w, filepath.Join(dir, "logs")); err != nil || !st.Live {
				t.Fatalf("start: %+v %v", st, err)
			}
			t.Cleanup(func() { _, _ = tc.q.RetireHandle(context.Background(), root, "bob") })
			tc.kill(t, root, "bob")
			if err := os.Chmod(inject, 0o666); err != nil {
				t.Fatal(err)
			}
			short, stop := context.WithTimeout(ctx, 4*time.Second)
			st, _, err := tc.q.Ensure(short, w, filepath.Join(dir, "logs"))
			stop()
			if err == nil || st.Live {
				t.Fatalf("recovered a waker with an unusable injector: %+v %v", st, err)
			}
			if got, _ := tc.wakes(ctx, root, "bob"); got.Live {
				t.Fatalf("a waker is live after the refused recovery: %+v", got)
			}
		})
	}
}
