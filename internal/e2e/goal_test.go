package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	goalHears   = 15 * time.Second // the watcher's tick and a doorbell
	watcherTick = 6 * time.Second
)

// goalTab starts a goal agent and a reviewer beside it.
func goalTab(w *world) (tb *tab, reviewer string) {
	w.t.Helper()
	tb = w.newTab("lead", "goal")
	w.ready("lead")
	reviewer = tb.split("rev", "reviewer")
	w.ready("rev")
	return tb, reviewer
}

// handleOf is the handle the scripted agent called name holds: its session
// id is its name. Handles are numbered by adoption order, so tests ask.
func (w *world) handleOf(name string) string {
	w.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(w.home, "records"))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(w.home, "records", e.Name()))
		if err != nil {
			continue
		}
		var r struct {
			Handle  string `json:"handle"`
			Session string `json:"agent_session_id"`
		}
		if json.Unmarshal(b, &r) == nil && r.Session == name {
			return r.Handle
		}
	}
	w.t.Fatalf("no record for agent %s\n%s", name, w.dump())
	return ""
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
	mail := w.drain(w.handleOf("lead"))
	for _, want := range []string{w.handleOf("rev") + " (claude, reviewer)", "left your Rex session", "session rev"} {
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
	mail := w.drain(w.handleOf("rev"))
	if !strings.Contains(mail, w.handleOf("lead")) || !strings.Contains(mail, "do not take over") {
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
	if n := w.unread(w.handleOf("qa")); n != 0 || w.doorbell("qa") {
		t.Fatalf("another peer was mailed: %d unread, typed %q", n, w.got("qa"))
	}
}

func TestAGoalAgentInAnotherSessionHearsNothing(t *testing.T) {
	w := newWorld(t)
	_, reviewer := goalTab(w)
	w.newTab("other", "goal") // a new session
	w.ready("other")
	w.typeInto(reviewer, "quit")
	w.until("the session's own goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
	time.Sleep(time.Second)
	if n := w.unread(w.handleOf("other")); n != 0 {
		t.Fatalf("a goal agent in another session got %d messages", n)
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
	if n := w.unread(w.handleOf("lead")); n != 0 {
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

// recordedWindow is the window handle's record names.
func (w *world) recordedWindow(handle string) string {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.home, "records", handle+".json"))
	if err != nil {
		w.t.Fatal(err)
	}
	var rec struct {
		Window string `json:"window_id"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		w.t.Fatal(err)
	}
	return rec.Window
}

func TestAGoalAgentHearsAPeerMovedIntoItsTab(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "goal")
	w.ready("lead")
	peer := tb.otherTab("rev", "reviewer")
	w.ready("rev")
	tb.move(peer, tb.first)
	w.until("the watcher to see the move", goalHears, func() bool {
		return w.recordedWindow(w.handleOf("rev")) == w.recordedWindow(w.handleOf("lead"))
	})
	w.typeInto(peer, "crash")
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
}

// A peer moved to another tab of the session is still on the goal agent's
// task: its departure is the goal agent's news.
func TestAGoalAgentHearsAPeerMovedToAnotherTabOfItsSession(t *testing.T) {
	w := newWorld(t)
	tb, reviewer := goalTab(w)
	away := tb.otherTab("away", "implementer")
	w.ready("away")
	tb.move(reviewer, away)
	w.until("the watcher to see the move", goalHears, func() bool {
		return w.recordedWindow(w.handleOf("rev")) == w.recordedWindow(w.handleOf("away"))
	})
	w.typeInto(reviewer, "quit")
	w.until("the goal agent to be woken", goalHears, func() bool { return w.doorbell("lead") })
}

// Without a watcher, the agent who looks brings the records up to date.
func TestWhoFollowsAPeerMovedIntoItsTab(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "implementer")
	w.ready("lead")
	peer := tb.otherTab("rev", "reviewer")
	w.ready("rev")
	tb.move(peer, tb.first)
	if out, code := w.who(tb.first); code != 0 || !strings.Contains(out, w.handleOf("rev")) {
		t.Fatalf("who: %d\n%s", code, out)
	}
	if got, want := w.recordedWindow(w.handleOf("rev")), w.recordedWindow(w.handleOf("lead")); got != want {
		t.Fatalf("the record stayed in %s, the agent is in %s", got, want)
	}
}
