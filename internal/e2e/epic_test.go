package e2e

import (
	"errors"
	"io/fs"
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
	open := "epic open e2e-login --cwd " + w.project("app") + " --goal " + goal + " --timeout 40 --op-id e2e-open-1"
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
		Session string `json:"session_id"`
		Linked  bool   `json:"linked"`
		Lead    struct {
			Role, Block, Status, Handle string
		} `json:"lead"`
		Narrator struct {
			Block, Status, Handle string
		} `json:"narrator"`
	}
	line, _, _ := strings.Cut(out, "\n")
	if err := envelopeData(line, &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if !res.Linked || res.Lead.Role != "orchestrator" || res.Lead.Status != "adopted" || res.Narrator.Status != "adopted" ||
		res.Lead.Handle != w.handleOf("orchestrator") || res.Narrator.Handle != w.handleOf("narrator") {
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
	who, code := w.adapterIn(res.Lead.Block, "agent", "list", "--all")
	if code != 0 || !strings.Contains(who, `"role":"orchestrator"`) || !strings.Contains(who, `"role":"narrator"`) || !strings.Contains(who, `"tab":"narrator(`+res.Narrator.Handle+`) | e2e-login"`) {
		t.Errorf("agent list in the Epic: %d\n%s", code, who)
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
	if len(lines) != 4 || lines[3] != "exit=0" || envelopeData(lines[2], &again) != nil || again.Session != res.Session || !again.Replayed || again.State != "opened" {
		t.Fatalf("the retry:\n%s\n%s", w.joined("me"), w.dump())
	}
	if after := w.rex("session", "list"); after != sessions {
		t.Errorf("the retry changed the sessions:\n%s\nthen\n%s", sessions, after)
	}
	// The human's cockpit, from outside Rex, names the personal agent and
	// reads its linked Epic; run from the orchestrator's pane it is refused.
	// ME_UNATTENDED is cleared: the agent running the tests may carry it.
	quiet := []string{"ME_UNATTENDED="}
	out, code = w.adapterRunEnv(quiet, "epic", "list", "--personal", w.handleOf("me"))
	var cockpit struct {
		Epics []struct {
			Session   string `json:"session_id"`
			Lifecycle struct {
				State string `json:"state"`
				OpID  string `json:"open_op_id"`
			} `json:"lifecycle"`
		} `json:"items"`
	}
	if code != 0 || envelopeData(out, &cockpit) != nil || len(cockpit.Epics) != 1 || cockpit.Epics[0].Session != res.Session ||
		cockpit.Epics[0].Lifecycle.State != "opened" || cockpit.Epics[0].Lifecycle.OpID != "e2e-open-1" {
		t.Errorf("the cockpit outside Rex: exit %d\n%s", code, out)
	}
	if out, code := w.adapterRunEnv(append(quiet, "REX_BLOCK="+res.Lead.Block), "epic", "list", "--personal", w.handleOf("me")); code != 6 || !strings.Contains(out, `"code":"guard_denied"`) {
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
	w.typeInto(tb.first, "epic open e2e-close --cwd "+w.project("app")+" --asked open-it --timeout 40")
	w.until("the epic command to finish", 50*time.Second, func() bool { return exits() == 1 })
	out := w.joined("me")
	if !strings.Contains(out, "exit=0") {
		t.Fatalf("epic:\n%s\n%s", out, w.dump())
	}
	var res struct {
		Session string `json:"session_id"`
	}
	line, _, _ := strings.Cut(out, "\n")
	if err := envelopeData(line, &res); err != nil || res.Session == "" {
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
	// A closed Epic is never revived: its agents' identities go, and their
	// handles leave the amq roster.
	if _, err := os.Stat(filepath.Join(w.home, "identities", orch+".json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the closed Epic's orchestrator kept its identity: %v", err)
	}
	if cfg, err := os.ReadFile(filepath.Join(w.root(), "meta", "config.json")); err != nil || strings.Contains(string(cfg), `"`+orch+`"`) {
		t.Errorf("the closed Epic's orchestrator is still in the roster (%v): %s", err, cfg)
	}
	if links, _ := filepath.Glob(filepath.Join(w.root(), "extensions", "rex-amq-adapter", "links", "*.json")); len(links) != 0 {
		t.Errorf("links left: %v", links)
	}
}

// The user's Desktop agent, outside Rex (a harness session no hook adopted,
// with no personal agent live anywhere), operates an Epic: it opens one
// linked to the Hub --hub names, with a grant that says an interactive
// agent relayed the words, replays it under its op id, sees it in the Hub's
// cockpit, and closes it on the user's words. An agent of the Epic, from
// its own pane, is refused every one of those commands.
func TestExternalOperatorOpensAndClosesAnEpic(t *testing.T) {
	w := newWorld(t)
	hub, err := filepath.EvalSymlinks(w.project("hub"))
	if err != nil {
		t.Fatal(err)
	}
	goal := filepath.Join(w.dir, "goal.md")
	w.write(goal, "GOAL: ship from the desktop\n")
	desktop := []string{"ANTIGRAVITY_CONVERSATION_ID=desktop-e2e-1"}
	open := []string{"epic", "open", "e2e-external", "--cwd", w.project("app"), "--goal", goal, "--asked", "run it from the desktop",
		"--hub", hub, "--timeout", "40", "--op-id", "e2e-ext-open-1"}
	out, code := w.adapterRunEnv(desktop, open...)
	if code != 0 {
		t.Fatalf("epic open from outside Rex: exit %d\n%s\n%s", code, out, w.dump())
	}
	var res struct {
		Session   string                         `json:"session_id"`
		Linked    bool                           `json:"linked"`
		Hub       string                         `json:"hub"`
		GrantPath string                         `json:"grant_path"`
		Lead      struct{ Block, Status string } `json:"lead"`
		Narrator  struct{ Status string }        `json:"narrator"`
	}
	if err := envelopeData(out, &res); err != nil || !res.Linked || res.Hub != hub || res.Lead.Status != "adopted" || res.Narrator.Status != "adopted" {
		t.Fatalf("open result %+v (%v)\n%s", res, err, w.dump())
	}
	if b, err := os.ReadFile(res.GrantPath); err != nil || !strings.Contains(string(b), "- relayed_by: agy (interactive agent)\n") {
		t.Errorf("grant %s (%v):\n%s", res.GrantPath, err, b)
	}
	again, code := w.adapterRunEnv(desktop, open...)
	var replay struct {
		Session  string `json:"session_id"`
		Replayed bool   `json:"replayed"`
	}
	if code != 0 || envelopeData(again, &replay) != nil || !replay.Replayed || replay.Session != res.Session {
		t.Fatalf("the replay: exit %d\n%s", code, again)
	}
	if list, code := w.adapterRunEnv(desktop, "epic", "list", "--hub", hub); code != 0 || !strings.Contains(list, res.Session) {
		t.Errorf("the Hub's cockpit: exit %d\n%s", code, list)
	}
	member := []string{"REX_BLOCK=" + res.Lead.Block}
	for _, args := range [][]string{{"epic", "list", "--hub", hub}, {"epic", "close", res.Session, "--asked", "close it"}, {"epic", "open", "x", "--cwd", hub, "--asked", "go"}} {
		if out, code := w.adapterRunEnv(member, args...); code != 6 || !strings.Contains(out, `"code":"guard_denied"`) {
			t.Errorf("%v from the lead's pane: exit %d\n%s", args, code, out)
		}
	}
	closeOut, code := w.adapterRunEnv(desktop, "epic", "close", res.Session, "--asked", "close it from the desktop", "--hub", hub, "--force", "--op-id", "e2e-ext-close-1")
	if code != 0 || !strings.Contains(closeOut, `"unlinked":true`) || !strings.Contains(closeOut, `"lifecycle_state":"closed"`) {
		t.Fatalf("epic close from outside Rex: exit %d\n%s\n%s", code, closeOut, w.dump())
	}
	if _, err := w.rexErr("session", "inspect", res.Session); err == nil {
		t.Errorf("the Epic's session %s is still there", res.Session)
	}
	if links, _ := filepath.Glob(filepath.Join(w.root(), "extensions", "rex-amq-adapter", "links", "*.json")); len(links) != 0 {
		t.Errorf("links left: %v", links)
	}
}

// The Operator outside Rex, with no personal agent live, runs an Epic
// through its Hub's inbox: the lead's hand-back and the narrator's mail to
// `personal` are kept there (routed_to hub), `hub inbox --wait` reads them
// with no Rex call of its own, and `hub reply` and `hub send` keep the
// Operator's words and mail them to the narrator, which takes them as the
// gateway's adapter_operator_relay.
func TestExternalOperatorRunsAnEpicThroughTheHubInbox(t *testing.T) {
	w := newWorld(t)
	hub, err := filepath.EvalSymlinks(w.project("hub"))
	if err != nil {
		t.Fatal(err)
	}
	desktop := []string{"CODEX_THREAD_ID=019a8e2c-5c3d-7f10-9e6a-0c2b5d4e7a11"}
	out, code := w.adapterRunEnv(desktop, "epic", "open", "e2e-inbox", "--cwd", w.project("app"), "--asked", "run it from the desktop", "--hub", hub, "--timeout", "40")
	var res struct {
		Session  string                                 `json:"session_id"`
		Lead     struct{ Block, Status, Handle string } `json:"lead"`
		Narrator struct{ Block, Status, Handle string } `json:"narrator"`
	}
	if code != 0 || envelopeData(out, &res) != nil || res.Lead.Status != "adopted" || res.Narrator.Status != "adopted" {
		t.Fatalf("epic open: exit %d\n%s\n%s", code, out, w.dump())
	}
	type page struct {
		Items []struct {
			Seq       int64  `json:"seq"`
			Kind      string `json:"kind"`
			Epic      string `json:"epic"`
			MsgID     string `json:"msg_id"`
			Body      string `json:"body"`
			InReplyTo int64  `json:"in_reply_to"`
		} `json:"items"`
		NextSince int64 `json:"next_since"`
	}
	inbox := func(args ...string) page {
		t.Helper()
		out, code := w.adapterRunEnv(desktop, append([]string{"hub", "inbox", "--hub", hub}, args...)...)
		var p page
		if code != 0 || envelopeData(out, &p) != nil {
			t.Fatalf("hub inbox %v: exit %d\n%s", args, code, out)
		}
		return p
	}
	// A wait started before anything is kept returns with the hand-back.
	waited := make(chan page, 1)
	go func() { waited <- inbox("--wait", "30s") }()
	time.Sleep(500 * time.Millisecond)
	if out, code := w.adapterIn(res.Lead.Block, "epic", "handback", "## Review queue\nPR #5 waits on the user"); code != 0 {
		t.Fatalf("the lead's hand-back: exit %d\n%s", code, out)
	}
	if p := <-waited; len(p.Items) != 1 || p.Items[0].Kind != "handback" || p.Items[0].Epic != res.Session {
		t.Fatalf("hub inbox --wait: %+v", p)
	}
	if out, code := w.adapterIn(res.Narrator.Block, "mail", "send", "--to", "personal", "--kind", "question", "--subject", "hand-back ready", "--body", "PR #5: merge it?", "--json"); code != 0 || !strings.Contains(out, `"routed_to":"hub"`) {
		t.Fatalf("the narrator's mail with no personal agent live: exit %d\n%s", code, out)
	}
	p := inbox("--since", "1")
	if len(p.Items) != 1 || p.Items[0].Kind != "narrator" || p.Items[0].Body != "PR #5: merge it?" || p.NextSince != 2 {
		t.Fatalf("the narrator's entry: %+v", p)
	}
	if out, code := w.adapterRunEnv(desktop, "hub", "reply", "2", "--body", "yes, merge it", "--hub", hub, "--op-id", "e2e-reply-1"); code != 0 || !strings.Contains(out, `"control":"hub:3"`) {
		t.Fatalf("hub reply: exit %d\n%s", code, out)
	}
	if out, code := w.adapterRunEnv(desktop, "hub", "send", res.Session, "--body", "also update the changelog", "--hub", hub); code != 0 || !strings.Contains(out, `"control":"hub:4"`) {
		t.Fatalf("hub send: exit %d\n%s", code, out)
	}
	p = inbox("--since", "2", "--kind", "operator")
	if len(p.Items) != 2 || p.Items[0].InReplyTo != 2 || p.Items[1].InReplyTo != 0 || p.Items[1].Body != "also update the changelog" {
		t.Fatalf("the Operator's entries: %+v", p)
	}
	// The narrator takes both: its drain reads them (the gateway kept them).
	drained, code := w.adapterIn(res.Narrator.Block, "mail", "drain", "--include-body", "--json")
	if code != 0 || !strings.Contains(drained, "operator hub:3: yes, merge it") || !strings.Contains(drained, "operator hub:4: also update the changelog") {
		t.Fatalf("the narrator's drain: exit %d\n%s", code, drained)
	}
	// It relays the user's words to the lead under the entry's control id.
	if out, code := w.adapterIn(res.Narrator.Block, "mail", "send", "--to", res.Lead.Handle, "--control", "hub:3", "--subject", "the user: merge PR #5", "--body", "yes, merge it"); code != 0 {
		t.Fatalf("the narrator's relay to the lead: exit %d\n%s", code, out)
	}
	if out, code := w.adapterRunEnv(desktop, "epic", "close", res.Session, "--asked", "close it", "--hub", hub, "--force"); code != 0 || !strings.Contains(out, `"lifecycle_state":"closed"`) {
		t.Fatalf("epic close: exit %d\n%s", code, out)
	}
	// Closed: the inbox stays readable for the Epic, and nothing more is relayed to it.
	if p := inbox("--epic", res.Session); len(p.Items) != 4 {
		t.Errorf("the closed Epic's entries: %+v", p)
	}
	if out, code := w.adapterRunEnv(desktop, "hub", "send", res.Session, "--body", "too late", "--hub", hub); code == 0 {
		t.Errorf("hub send to a closed Epic: exit 0\n%s", out)
	}
}
