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
	if code != 0 || !strings.Contains(who, `"role": "orchestrator"`) || !strings.Contains(who, `"role": "narrator"`) || !strings.Contains(who, `"tab": "narrator(`+res.Narrator.Handle+`) | e2e-login"`) {
		t.Errorf("who in the Epic: %d\n%s", code, who)
	}
}

// The personal agent closes the Epic it opened, on the user's words: a
// close without them is refused and kills nothing; with them the Epic's Rex
// session is gone, its agents are parked for epic_closed with their records
// kept, and the link is removed.
func TestPersonalAgentClosesAnEpic(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	exits := func() int { return strings.Count(w.joined("me"), "exit=") }
	w.typeInto(tb.first, "epic e2e-close -c "+w.dir+" --asked open-it --timeout 40")
	w.until("the epic command to finish", 50*time.Second, func() bool { return exits() == 1 })
	out := w.joined("me")
	if !strings.Contains(out, "exit=0") {
		t.Fatalf("epic:\n%s\n%s", out, w.dump())
	}
	var res struct {
		Session string `json:"session_id"`
	}
	line, _, _ := strings.Cut(out, "\n")
	if err := json.Unmarshal([]byte(line), &res); err != nil || res.Session == "" {
		t.Fatalf("output %q: %v", out, err)
	}
	orch := w.handleOf("orchestrator")

	w.typeInto(tb.first, "epic close "+res.Session)
	w.until("the refused close to finish", 10*time.Second, func() bool { return exits() == 2 })
	if got := w.joined("me"); strings.HasSuffix(strings.TrimSpace(got), "exit=0") || !strings.Contains(got, "--asked") {
		t.Fatalf("a close without the user's words:\n%s", got)
	}
	if _, err := w.rexErr("session", "inspect", res.Session); err != nil {
		t.Fatalf("a refused close killed the session: %v", err)
	}

	w.typeInto(tb.first, "epic close "+res.Session+" --asked close-it")
	w.until("the close to finish", 20*time.Second, func() bool { return exits() == 3 })
	got := w.joined("me")
	if !strings.HasSuffix(strings.TrimSpace(got), "exit=0") || !strings.Contains(got, `"unlinked":true`) || !strings.Contains(got, orch) {
		t.Fatalf("close:\n%s\n%s", got, w.dump())
	}
	if _, err := w.rexErr("session", "inspect", res.Session); err == nil {
		t.Errorf("the Epic's session %s is still there", res.Session)
	}
	b, err := os.ReadFile(filepath.Join(w.home, "records", orch+".json"))
	if err != nil || !strings.Contains(string(b), `"depart_reason": "epic_closed"`) {
		t.Errorf("the orchestrator's record: %s %v", b, err)
	}
	if links, _ := filepath.Glob(filepath.Join(w.root(), "extensions", "rex-amq-adapter", "links", "*.json")); len(links) != 0 {
		t.Errorf("links left: %v", links)
	}
}
