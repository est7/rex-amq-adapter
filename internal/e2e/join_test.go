package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// tab, `join pair` names the tab and makes it the implementer.
func TestAnAgentAloneInItsTabJoinsACrew(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "")
	w.ready("lead")
	w.typeInto(tb.first, "join pair")
	w.until("the join to finish", 10*time.Second, func() bool { return strings.Contains(w.joined("lead"), "exit=") })
	out := w.joined("lead")
	h := w.handleOf("lead")
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "implementer("+h+")") || !strings.Contains(out, "still needs reviewer") {
		t.Fatalf("join:\n%s\n%s", out, w.dump())
	}
	if got := w.tabLabel(tb, tb.first); got != "pair" {
		t.Errorf("tab label %q, want pair", got)
	}
	if out, code := w.who(tb.first); code != 0 || !strings.Contains(out, "implementer") {
		t.Errorf("who after join: %d\n%s", code, out)
	}
}

func TestJoinLeavesATabWithAnotherAgentAlone(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("lead", "")
	w.ready("lead")
	tb.split("mate", "")
	w.ready("mate")
	before := w.tabLabel(tb, tb.first)
	w.typeInto(tb.first, "join pair")
	w.until("the join to finish", 10*time.Second, func() bool { return strings.Contains(w.joined("lead"), "exit=") })
	out := w.joined("lead")
	if strings.Contains(out, "exit=0") || !strings.Contains(out, w.handleOf("mate")) || !strings.Contains(out, "rex-amq-adapter tab pair") {
		t.Fatalf("join beside another agent:\n%s", out)
	}
	if got := w.tabLabel(tb, tb.first); got != before {
		t.Errorf("tab renamed to %q", got)
	}
}
