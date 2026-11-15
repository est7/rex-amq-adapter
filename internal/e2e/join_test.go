package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func (w *world) joined(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.dir, name+".join"))
	return string(b)
}

// tabLabel is the label of the tab block is in.
func (w *world) tabLabel(tb *tab, block string) string {
	w.t.Helper()
	var blk struct {
		Block struct {
			Window string `json:"window_id"`
		} `json:"block"`
	}
	if err := json.Unmarshal([]byte(w.rex("block", "inspect", block, "--json")), &blk); err != nil {
		w.t.Fatal(err)
	}
	var s struct {
		Windows []struct {
			ID    string `json:"window_id"`
			Label string `json:"label"`
		} `json:"windows"`
	}
	if err := json.Unmarshal([]byte(w.rex("session", "inspect", tb.session, "--json")), &s); err != nil {
		w.t.Fatal(err)
	}
	for _, win := range s.Windows {
		if win.ID == blk.Block.Window {
			return win.Label
		}
	}
	return ""
}

// The agent the user talks to leads the crew where it stands: alone in its
// tab, `join pair` makes it the implementer and claims pair for its Rex
// session, and its tab keeps following its pane label.
func TestAnAgentAloneInItsTabJoinsACrew(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "")
	w.ready("lead")
	w.typeInto(tb.first, "join pair")
	w.until("the join to finish", 10*time.Second, func() bool { return strings.Contains(w.joined("lead"), "exit=") })
	out := w.joined("lead")
	h := w.handleOf("lead")
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "implementer("+h+")") || !strings.Contains(out, "this Rex session is crew pair") || !strings.Contains(out, "still needs reviewer") {
		t.Fatalf("join:\n%s\n%s", out, w.dump())
	}
	w.until("the tab to follow its agent", 5*time.Second, func() bool { return w.tabLabel(tb, tb.first) == "implementer("+h+")" })
	if out, code := w.who(tb.first); code != 0 || !strings.Contains(out, "implementer") {
		t.Errorf("who after join: %d\n%s", code, out)
	}
}

// A crew is the session's: beside another agent `join pair` claims it
// without renaming the tab, and a mate that then asks for another crew is
// refused, naming the crew and who works in it.
func TestJoinClaimsTheSessionAndRefusesASecondCrew(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "")
	w.ready("lead")
	mate := tb.split("mate", "")
	w.ready("mate")
	before := w.tabLabel(tb, tb.first)
	w.typeInto(tb.first, "join pair")
	w.until("the join to finish", 10*time.Second, func() bool { return strings.Contains(w.joined("lead"), "exit=") })
	if out := w.joined("lead"); !strings.Contains(out, "exit=0") {
		t.Fatalf("join beside another agent:\n%s\n%s", out, w.dump())
	}
	if got := w.tabLabel(tb, tb.first); got == "pair" || got != before {
		t.Errorf("tab renamed to %q, was %q", got, before)
	}
	w.typeInto(mate, "join ship")
	w.until("the mate's join to finish", 10*time.Second, func() bool { return strings.Contains(w.joined("mate"), "exit=") })
	out := w.joined("mate")
	if strings.Contains(out, "exit=0") || !strings.Contains(out, "another crew (pair") || !strings.Contains(out, w.handleOf("lead")) {
		t.Fatalf("a second crew in the session:\n%s", out)
	}
}

// A summoned peer starts in a tab of its own, in the background, and the
// tab follows its pane label once it is adopted; the caller's tab stays as
// it was.
func TestASummonedPeerGetsATabOfItsOwn(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "")
	w.ready("lead")
	labels := func() []string {
		var s struct {
			Windows []struct {
				Label string `json:"label"`
			} `json:"windows"`
		}
		if err := json.Unmarshal([]byte(w.rex("session", "inspect", tb.session, "--json")), &s); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, win := range s.Windows {
			out = append(out, win.Label)
		}
		return out
	}
	before, tabs := w.tabLabel(tb, tb.first), len(labels())
	w.typeInto(tb.first, "summon implementer --subject fix --body do-it --kind todo --asked go --harness claude --cwd "+w.project("app"))
	w.until("the summon to finish", 40*time.Second, func() bool { return strings.Contains(w.joined("lead"), "exit=") })
	if out := w.joined("lead"); !strings.Contains(out, "exit=0") {
		t.Fatalf("summon:\n%s\n%s", out, w.dump())
	}
	want := "implementer(" + w.handleOf("orchestrator") + ") | fix" // a launched claude is a scripted agent of that name
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		got := labels()
		if len(got) == tabs+1 && slices.Contains(got, want) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("tabs %q, want %d with one showing %q", got, tabs+1, want)
		}
	}
	if got := w.tabLabel(tb, tb.first); got != before {
		t.Errorf("the caller's tab became %q, was %q", got, before)
	}
}
