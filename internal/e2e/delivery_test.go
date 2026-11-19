package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnIdleAgentIsToldOfMailByADoorbell(t *testing.T) {
	w := newWorld(t)
	w.newTab("alice", "")
	w.ready("alice")
	w.send("alice", "claude-01", "hello")
	w.until("a doorbell", 20*time.Second, func() bool { return strings.Contains(w.got("alice"), "AMQ doorbell") })
}

// The doorbell's own command drains through the adapter's gateway: the
// agent that runs it gets the body, and amq records it drained (ARCH-1).
func TestADoorbellPromptedAgentDrainsThroughTheGateway(t *testing.T) {
	w := newWorld(t)
	w.newTab("alice", "")
	w.ready("alice")
	w.write(filepath.Join(w.dir, "alice.obey"), "")
	w.send("alice", "claude-01", "hello through the gateway")
	drained := func() string { b, _ := os.ReadFile(filepath.Join(w.dir, "alice.drained")); return string(b) }
	w.until("the doorbell's drain", 20*time.Second, func() bool { return strings.Contains(drained(), "exit=") })
	got := w.got("alice")
	if !strings.Contains(got, w.adapter+" mail drain --include-body") || strings.Contains(got, "amq drain") {
		t.Errorf("doorbell %q does not name the adapter's drain", got)
	}
	if out := drained(); !strings.Contains(out, "exit=0") || !strings.Contains(out, "hello through the gateway") {
		t.Fatalf("drain:\n%s\n%s", out, w.dump())
	}
	if left, _ := os.ReadDir(filepath.Join(w.root(), "agents", "claude-01", "inbox", "new")); len(left) != 0 {
		t.Errorf("%d messages still unread after the drain", len(left))
	}
}

func TestAwaitReportsWhatBecameOfAStartedAgent(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name   string
		script string
		code   int
		want   string
	}{
		{"exited", "exit 0", 7, `"status":"exited"`},
		{"trust", "printf 'Do you trust the contents of this directory?\\n'; sleep 30", 7, `"status":"trust_screen"`},
		{"blocked", "printf '\\033]7501;state=blocked:kind=permission\\033\\\\'; sleep 30", 7, `"status":"blocked"`},
		{"silent", "sleep 30", 7, `"status":"timeout"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := w.adapterRun("agent", "await", w.program(c.script), "--timeout", "2s")
			if code != c.code || !strings.Contains(out, c.want) {
				t.Fatalf("code=%d out=%s, want %d %s", code, out, c.code, c.want)
			}
		})
	}
}

func TestAwaitNamesAnAdoptedAgent(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("alice", "reviewer")
	out, code := w.adapterRun("agent", "await", tb.first, "--timeout", "15s")
	if code != 0 || !strings.Contains(out, `"handle":"claude-01"`) {
		t.Fatalf("code=%d out=%s\n%s", code, out, w.dump())
	}
	w.ready("alice") // the hook goes on to start the waker after the record exists
}

// An idle agent runs no hook, so its dead waker would leave mail unseen:
// `agent collect` brings it back (agent list only reads).
func TestCollectBringsBackADeadWakerSoIdleAgentsHearOfMail(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("alice", "")
	w.ready("alice")
	w.killWaker("claude-01")
	w.send("alice", "claude-01", "hello")
	time.Sleep(time.Second)
	if strings.Contains(w.got("alice"), "AMQ doorbell") {
		t.Fatal("the fault did not take: a doorbell rang with no waker")
	}
	if out, code := w.adapterIn(tb.first, "agent", "list"); code != 0 {
		t.Fatalf("agent list: %d %s", code, out)
	}
	time.Sleep(time.Second)
	if strings.Contains(w.got("alice"), "AMQ doorbell") {
		t.Fatal("agent list revived the waker: it must only read")
	}
	if out, code := w.adapterIn(tb.first, "agent", "collect"); code != 0 {
		t.Fatalf("agent collect: %d %s", code, out)
	}
	w.until("a doorbell from the revived waker", 20*time.Second, func() bool { return strings.Contains(w.got("alice"), "AMQ doorbell") })
}

// The send that needs the doorbell repairs it: a peer's `mail send`
// through the adapter brings back the recipient's dead waker, so an idle
// agent hears of the mail without anyone listing or collecting.
func TestMailSendBringsBackTheRecipientsDeadWaker(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("alice", "")
	w.ready("alice")
	bob := tb.split("bob", "")
	w.ready("bob")
	alice := w.handleOf("alice")
	w.killWaker(alice)
	w.typeInto(bob, "mail send --to "+alice+" --subject e2e --body hello-from-bob")
	w.until("bob's send", 20*time.Second, func() bool { return strings.Contains(w.joined("bob"), "exit=") })
	if out := w.joined("bob"); !strings.Contains(out, "exit=0") {
		t.Fatalf("mail send:\n%s\n%s", out, w.dump())
	}
	w.until("a doorbell from the waker the send revived", 20*time.Second, func() bool { return strings.Contains(w.got("alice"), "AMQ doorbell") })
}
