package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A personal agent opens an Epic with one command, in a private Rex server:
// the Epic's session gets an orchestrator and a narrator, both adopted, and
// the link between the personal agent and the Epic is recorded.
func TestPersonalAgentOpensAnEpic(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	goal := filepath.Join(w.dir, "goal.md")
	w.write(goal, "GOAL: ship it\n")
	w.typeInto(tb.first, "epic e2e-login -c "+w.dir+" --goal "+goal+" --timeout 40")
	w.until("the epic command to finish", 50*time.Second, func() bool { return strings.Contains(w.joined("me"), "exit=") })
	out := w.joined("me")
	if !strings.Contains(out, "exit=0") {
		screens := ""
		for _, b := range strings.Split(out, `"block":"`)[1:] {
			id, _, _ := strings.Cut(b, `"`)
			s, err := w.rexErr("capture", "-b", id, "--trim", "--unwrap")
			screens += "\n--- screen of " + id + "\n" + s + "\n" + func() string {
				if err != nil {
					return err.Error()
				}
				return ""
			}()
		}
		t.Fatalf("epic:\n%s%s\n%s", out, screens, w.dump())
	}
	var res struct {
		Session      string `json:"session_id"`
		Linked       bool   `json:"linked"`
		Orchestrator struct {
			Block, Status, Handle string
		} `json:"orchestrator"`
		Narrator struct {
			Block, Status, Handle string
		} `json:"narrator"`
	}
	line, _, _ := strings.Cut(out, "\n")
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if !res.Linked || res.Orchestrator.Status != "adopted" || res.Narrator.Status != "adopted" ||
		res.Orchestrator.Handle != w.handleOf("orchestrator") || res.Narrator.Handle != w.handleOf("narrator") {
		t.Fatalf("result %+v\n%s", res, w.dump())
	}
	links, _ := filepath.Glob(filepath.Join(w.root(), "extensions", "rex-amq-adapter", "links", "*.json"))
	if len(links) != 1 {
		t.Fatalf("links %v", links)
	}
	b, err := os.ReadFile(links[0])
	if err != nil || !strings.Contains(string(b), res.Session) {
		t.Errorf("link %s: %v", b, err)
	}
	who, code := w.who(res.Orchestrator.Block, "--all", "--json")
	if code != 0 || !strings.Contains(who, `"role": "orchestrator"`) || !strings.Contains(who, `"role": "narrator"`) || !strings.Contains(who, `"tab": "narrator"`) {
		t.Errorf("who in the Epic: %d\n%s", code, who)
	}
}
