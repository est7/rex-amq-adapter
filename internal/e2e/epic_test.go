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
// the link between the personal agent and the Epic is recorded. The same
// command run again under its --op-id starts nothing: it reports the same
// session, replayed and opened.
func TestPersonalAgentOpensAnEpic(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	goal := filepath.Join(w.dir, "goal.md")
	w.write(goal, "GOAL: ship it\n")
	open := "epic e2e-login -c " + w.project("app") + " --goal " + goal + " --timeout 40 --op-id e2e-open-1"
	w.typeInto(tb.first, open)
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

	sessions := w.rex("session", "list")
	w.typeInto(tb.first, open)
	w.until("the retried epic command to finish", 50*time.Second, func() bool { return strings.Count(w.joined("me"), "exit=") == 2 })
	lines := strings.Split(strings.TrimSpace(w.joined("me")), "\n")
	var again struct {
		Session  string `json:"session_id"`
		Replayed bool   `json:"replayed"`
		State    string `json:"lifecycle_state"`
	}
	if len(lines) != 4 || lines[3] != "exit=0" || json.Unmarshal([]byte(lines[2]), &again) != nil || again.Session != res.Session || !again.Replayed || again.State != "opened" {
		t.Fatalf("the retry:\n%s\n%s", w.joined("me"), w.dump())
	}
	if after := w.rex("session", "list"); after != sessions {
		t.Errorf("the retry changed the sessions:\n%s\nthen\n%s", sessions, after)
	}
	// The human's cockpit, from outside Rex, names the personal agent and
	// reads its linked Epic; run from the orchestrator's pane it is refused.
	// ME_UNATTENDED is cleared: the agent running the tests may carry it.
	quiet := []string{"ME_UNATTENDED="}
	out, code = w.adapterRunEnv(quiet, "view", "--cockpit", "--personal", w.handleOf("me"))
	var cockpit struct {
		Epics []struct {
			Session   string `json:"session_id"`
			Lifecycle struct {
				State string `json:"state"`
				OpID  string `json:"open_op_id"`
			} `json:"lifecycle"`
		} `json:"epics"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &cockpit) != nil || len(cockpit.Epics) != 1 || cockpit.Epics[0].Session != res.Session ||
		cockpit.Epics[0].Lifecycle.State != "opened" || cockpit.Epics[0].Lifecycle.OpID != "e2e-open-1" {
		t.Errorf("the cockpit outside Rex: exit %d\n%s", code, out)
	}
	if out, code := w.adapterRunEnv(append(quiet, "REX_BLOCK="+res.Orchestrator.Block), "view", "--cockpit", "--personal", w.handleOf("me")); code != 1 || out != "" {
		t.Errorf("the cockpit from the orchestrator's pane: exit %d\n%s", code, out)
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
	w.typeInto(tb.first, "epic e2e-close -c "+w.project("app")+" --asked open-it --timeout 40")
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

	w.typeInto(tb.first, "epic close "+res.Session+" --asked close-it --op-id e2e-close-1")
	w.until("the close to finish", 20*time.Second, func() bool { return exits() == 3 })
	got := w.joined("me")
	if !strings.HasSuffix(strings.TrimSpace(got), "exit=0") || !strings.Contains(got, `"unlinked":true`) || !strings.Contains(got, orch) {
		t.Fatalf("close:\n%s\n%s", got, w.dump())
	}
	// The same close again, under its op id: the session and the link are
	// gone, and it finishes as done, with Rex's own answer to a second kill.
	w.typeInto(tb.first, "epic close "+res.Session+" --asked close-it --op-id e2e-close-1")
	w.until("the retried close to finish", 20*time.Second, func() bool { return exits() == 4 })
	if got := w.joined("me"); !strings.HasSuffix(strings.TrimSpace(got), "exit=0") || strings.Count(got, `"replayed":true`) != 1 || !strings.Contains(got, `"lifecycle_state":"closed"`) {
		t.Fatalf("the retried close:\n%s\n%s", got, w.dump())
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
