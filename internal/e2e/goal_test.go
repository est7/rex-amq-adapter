package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goalHears = 15 * time.Second // the watcher's tick and a doorbell

// goalTab starts a goal agent and a reviewer beside it.
func goalTab(w *world) (tb *tab, reviewer string) {
	w.t.Helper()
	tb = w.newTab("lead", "goal")
	w.ready("lead")
	reviewer = tb.split("rev", "reviewer")
	w.ready("rev")
	return tb, reviewer
}

func (w *world) unread(handle string) int {
	entries, _ := os.ReadDir(filepath.Join(w.root(), "agents", handle, "inbox", "new"))
	return len(entries)
}

func (w *world) doorbell(name string) bool { return strings.Contains(w.got(name), "AMQ doorbell") }

func (w *world) drain(handle string) string {
	w.t.Helper()
	cmd := exec.Command(w.amqBin, "--no-update-check", "drain", "--root", w.root(), "--me", handle, "--include-body")
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("drain: %v\n%s", err, out)
	}
	return string(out)
}

func TestAnIdleGoalAgentIsWokenWhenAPeerExits(t *testing.T) {
	w := newWorld(t)
	_, reviewer := goalTab(w)
	w.typeInto(reviewer, "quit")
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
	mail := w.drain("claude-goal-01")
	for _, want := range []string{"claude-reviewer-01", "left your Rex tab", "session rev"} {
		if !strings.Contains(mail, want) {
			t.Errorf("mail lacks %q:\n%s", want, mail)
		}
	}
}

func TestAnIdleGoalAgentIsWokenWhenAPeerCrashes(t *testing.T) {
	w := newWorld(t)
	_, reviewer := goalTab(w)
	w.typeInto(reviewer, "crash")
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
}

func TestAnIdleGoalAgentIsWokenWhenAPeersPaneCloses(t *testing.T) {
	w := newWorld(t)
	_, reviewer := goalTab(w)
	w.rex("block", "close", reviewer)
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
}

func TestAPeerReportsWhenTheGoalAgentIsGone(t *testing.T) {
	w := newWorld(t)
	tb, _ := goalTab(w)
	w.typeInto(tb.first, "crash")
	w.until("the surviving peer to be told", goalHears, func() bool { return w.doorbell("rev") })
	mail := w.drain("claude-reviewer-01")
	if !strings.Contains(mail, "claude-goal-01") || !strings.Contains(mail, "do not take over") {
		t.Fatalf("mail:\n%s", mail)
	}
}

func TestOnlyTheGoalAgentIsWokenWhenAPeerExits(t *testing.T) {
	w := newWorld(t)
	tb, reviewer := goalTab(w)
	tb.split("qa", "qa")
	w.ready("qa")
	w.typeInto(reviewer, "quit")
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
	time.Sleep(time.Second)
	if n := w.unread("claude-qa-01"); n != 0 || w.doorbell("qa") {
		t.Fatalf("another peer was mailed: %d unread, typed %q", n, w.got("qa"))
	}
}

func TestAGoalAgentInAnotherTabHearsNothing(t *testing.T) {
	w := newWorld(t)
	_, reviewer := goalTab(w)
	w.newTab("other", "goal")
	w.ready("other")
	w.typeInto(reviewer, "quit")
	w.until("the tab's own goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
	time.Sleep(time.Second)
	if n := w.unread("claude-goal-02"); n != 0 {
		t.Fatalf("a goal agent in another tab got %d messages", n)
	}
}

func TestADepartedGoalAgentIsNotMailed(t *testing.T) {
	w := newWorld(t)
	tb, reviewer := goalTab(w)
	w.typeInto(tb.first, "quit")
	w.until("the peer to be told", goalHears, func() bool { return w.doorbell("rev") })
	w.typeInto(reviewer, "quit")
	w.until("the peer to depart", 5*time.Second, func() bool { return strings.Contains(w.got("rev"), "quit") })
	time.Sleep(time.Second)
	if n := w.unread("claude-goal-01"); n != 0 {
		t.Fatalf("a departed goal agent got %d messages", n)
	}
}

func TestWithoutAGoalAgentNobodyIsWoken(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("alice", "implementer")
	w.ready("alice")
	reviewer := tb.split("bob", "reviewer")
	w.ready("bob")
	w.typeInto(reviewer, "quit")
	time.Sleep(3 * time.Second)
	if w.doorbell("alice") {
		t.Fatalf("a tab without a goal agent rang a doorbell: %s", w.got("alice"))
	}
}

func TestTheGoalWatcherEndsWithItsGoalAgent(t *testing.T) {
	w := newWorld(t)
	tb, _ := goalTab(w)
	w.until("a watcher to run", 5*time.Second, w.watcherRuns)
	w.typeInto(tb.first, "quit")
	w.until("the watcher to end", goalHears, func() bool { return !w.watcherRuns() })
}

func TestOnlyAGoalAgentGetsAWatcher(t *testing.T) {
	w := newWorld(t)
	w.newTab("alice", "implementer")
	w.ready("alice")
	time.Sleep(500 * time.Millisecond)
	if w.watcherRuns() {
		t.Fatal("an agent without the goal role started a watcher")
	}
}

func TestAGoalAgentDoesNotHearOfItsOwnTabsAfterItLeaves(t *testing.T) {
	w := newWorld(t)
	tb, reviewer := goalTab(w)
	w.typeInto(tb.first, "quit")
	w.until("the peer to be told", goalHears, func() bool { return w.doorbell("rev") })
	w.typeInto(reviewer, "quit")
	time.Sleep(2 * time.Second)
	if w.doorbell("lead") {
		t.Fatal("a departed goal agent got a doorbell")
	}
}
