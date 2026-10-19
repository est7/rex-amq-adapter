package amq

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLifecycleAgainstRealAmq exercises the amq contract the adapter relies
// on: mailbox registration merges handles, Unread sees a sent message, the
// waker amq starts calls the injector with exactly the target's argv, Ensure
// keeps a matching live waker, and RetireFor stops only the waker of its block.
func TestLifecycleAgainstRealAmq(t *testing.T) {
	amqBin, err := ResolveBin()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	logs := filepath.Join(dir, "logs")

	for _, h := range []string{"bob", "alice"} {
		if err := EnsureMailbox(ctx, amqBin, root, h); err != nil {
			t.Fatalf("mailbox %s: %v", h, err)
		}
	}
	var cfg struct{ Agents []string }
	b, _ := os.ReadFile(filepath.Join(root, "meta", "config.json"))
	if err := json.Unmarshal(b, &cfg); err != nil || strings.Join(cfg.Agents, ",") != "alice,bob" {
		t.Fatalf("merged agent list: %v %v", cfg.Agents, err)
	}

	send := exec.CommandContext(ctx, amqBin, "--no-update-check", "send", "--root", root, "--me", "alice", "--to", "bob", "--subject", "s", "--body", "hello")
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("amq send: %v: %s", err, out)
	}
	ids, err := Unread(root, "bob")
	if err != nil || len(ids) != 1 {
		t.Fatalf("unread: %v %v", ids, err)
	}

	argvFile := filepath.Join(dir, "argv")
	injector := filepath.Join(dir, "inject.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\necho AMQ_INJECT_PROGRESS=accepted >&2\nexit 0\n"
	if err := os.WriteFile(injector, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	w := Waker{InjectVia: injector, Server: "unix:///tmp/rex.sock", Block: "block:test", Handle: "bob", Root: root}

	st, decision, err := Ensure(ctx, amqBin, w, logs)
	if err != nil || decision != DecisionStart || !st.Live || st.Generation == "" {
		t.Fatalf("first ensure: %+v %s %v", st, decision, err)
	}
	t.Cleanup(func() { _, _ = RetireFor(context.Background(), amqBin, root, "bob", w.Block) })
	again, decision, err := Ensure(ctx, amqBin, w, logs)
	if err != nil || decision != DecisionKeep || again.Generation != st.Generation {
		t.Fatalf("second ensure must keep the same waker: %+v %s %v", again, decision, err)
	}

	var argv []string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if b, err := os.ReadFile(argvFile); err == nil {
			argv = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			break
		}
	}
	want := w.Target().InjectArgs
	if len(argv) != len(want)+1 {
		t.Fatalf("injector argv %q, want %q plus the payload", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("injector arg %d: got %q want %q", i, argv[i], want[i])
		}
	}
	if argv[len(argv)-1] == "" {
		t.Fatal("amq passed an empty payload")
	}

	if retired, err := RetireFor(ctx, amqBin, root, "bob", "block:another"); err != nil || retired {
		t.Fatalf("a waker for another block must be left alone: %v %v", retired, err)
	}
	if retired, err := RetireFor(ctx, amqBin, root, "bob", w.Block); err != nil || !retired {
		t.Fatalf("the block's own waker must retire: %v %v", retired, err)
	}
	after, err := WakeCheck(ctx, amqBin, root, "bob")
	if err != nil || after.Live {
		t.Fatalf("waker still live after retire: %+v %v", after, err)
	}
}
