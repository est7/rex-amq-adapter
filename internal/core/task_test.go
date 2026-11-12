package core

import (
	"strings"
	"testing"
)

// A launch label "<role> | <task>" gives the new agent its task; a label the
// adapter wrote for an earlier agent ("<handle> | <task>") gives none, since
// that task was the earlier agent's.
func TestInitialTask(t *testing.T) {
	for label, want := range map[string]string{
		"architect | v0.8.2 review":      "v0.8.2 review",
		"| fix login":                    "fix login",
		"Reviewer-2 |  fix   login ":     "fix login",
		"codex | fix login":              "fix login",
		"reviewer":                       "",
		"architect |   ":                 "",
		"reviewer(codex-01) | fix login": "",
		"claude-06 | adapter docs":       "",
		"architect(agy-06) | pane label": "",
		"orchestrator(pi-02) | ship":     "",
		"shell":                          "",
		"implementer | a|b":              "a/b",
	} {
		if got := InitialTask(label); got != want {
			t.Errorf("InitialTask(%q) = %q, want %q", label, got, want)
		}
	}
}

// The pane-label request stands apart from the AMQ instructions, so an agent
// that never messages anyone still reads it; an agent that starts with a task
// is told what its pane shows instead of being asked for one.
func TestTaskNoticeAsksForATaskOrSaysWhatThePaneShows(t *testing.T) {
	if amq := SessionNotice("claude-06", "", "/x/rex-amq-adapter", 0); strings.Contains(amq, "Pane label") || strings.Contains(amq, "title") {
		t.Errorf("the AMQ notice carries the pane-label request:\n%s", amq)
	}
	asked := TaskNotice("claude-06", "", "x title", "")
	if !strings.HasPrefix(asked, "Pane label:") || !strings.Contains(asked, `x title "<task in a few words>"`) {
		t.Errorf("no request for a task:\n%s", asked)
	}
	told := TaskNotice("codex-01", "architect", "x title", "v0.8.2 review")
	if !strings.Contains(told, "`architect(codex-01) | v0.8.2 review`") || strings.Contains(told, "<task in a few words>") {
		t.Errorf("an agent with a task was asked for one:\n%s", told)
	}
}

// The start notice carries what a briefed peer needs to exchange mail, so it
// can work without loading the guide first: the env line, reading, replying,
// sending, ending its turn after a send, and where its result goes. The
// guide stays the place for starting peers.
func TestSessionNoticeCarriesTheMailCommands(t *testing.T) {
	n := SessionNotice("codex-02", "qa", "/x/rex-amq-adapter", 1)
	if bareAmq.MatchString(n) {
		t.Errorf("notice teaches bare amq mail, which skips the gateway:\n%s", n)
	}
	for _, want := range []string{
		"/x/rex-amq-adapter drain --include-body",
		"/x/rex-amq-adapter reply --id <id>",
		"/x/rex-amq-adapter send --to <handle>",
		"end your turn",
		"the agent that briefed you",
		"skill before you start or summon a peer",
	} {
		if !strings.Contains(n, want) {
			t.Errorf("notice lacks %q:\n%s", want, n)
		}
	}
	if strings.Contains(n, "before messaging other agents") {
		t.Errorf("notice still sends a peer to the guide before any mail:\n%s", n)
	}
}

// A narrator or a personal agent is not told to mail whoever it likes: its
// role prompt names the one agent it writes to.
func TestSessionNoticeOfAStealthRoleSendsItToItsRolePrompt(t *testing.T) {
	for _, role := range []string{NarratorRole, PersonalRole} {
		n := SessionNotice("claude-05", role, "/x/rex-amq-adapter", 0)
		if strings.Contains(n, "send --to <handle>") || !strings.Contains(n, "your role prompt names") {
			t.Errorf("%s notice:\n%s", role, n)
		}
	}
}
