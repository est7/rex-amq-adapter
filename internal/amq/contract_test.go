package amq

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
}

// targets is the fake, always, and amq itself when it is installed: every
// contract below must hold for both, so the fake cannot drift from amq.
func targets(t *testing.T) []contractTarget {
	f := &Fake{}
	out := []contractTarget{{"fake", f, func(_ context.Context, root, h string) (WakeState, error) { return f.Waker(root, h), nil }}}
	if bin, err := ResolveBin(); err == nil {
		out = append(out, contractTarget{"amq", CLI{Bin: bin}, func(ctx context.Context, root, h string) (WakeState, error) { return WakeCheck(ctx, bin, root, h) }})
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
