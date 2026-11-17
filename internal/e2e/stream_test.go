package e2e

import (
	"bufio"
	"encoding/json"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// stream events, once connected, prints only schema:1 frames (a snapshot
// first), never a v:2 envelope, and ends after a signal with its status:
// 130 for SIGINT, 143 for SIGTERM.
func TestStreamFramesAndSignalExits(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("alice", "")
	w.ready("alice")
	for _, c := range []struct {
		sig  syscall.Signal
		exit int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		cmd := exec.Command(w.adapter, "stream", "events", "--session", tb.session)
		cmd.Env = append(w.env[:len(w.env):len(w.env)], "REX_SERVER="+w.server)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := make(chan string, 64)
		go func() {
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		frame := func(line string) map[string]any {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("%s: a line that is not JSON: %q", c.sig, line)
			}
			if _, envelope := m["ok"]; envelope || m["schema"] != float64(1) {
				t.Fatalf("%s: not a schema:1 frame: %q", c.sig, line)
			}
			return m
		}
		select {
		case line := <-lines:
			if m := frame(line); m["type"] != "snapshot" {
				t.Fatalf("%s: the first frame is %v, want the snapshot", c.sig, m["type"])
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s: no frame within 15s", c.sig)
		}
		if err := cmd.Process.Signal(c.sig); err != nil {
			t.Fatal(err)
		}
		for line := range lines {
			frame(line)
		}
		err = cmd.Wait()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != c.exit {
			t.Errorf("%s: %v, want exit %d", c.sig, err, c.exit)
		}
	}
}
