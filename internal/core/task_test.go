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
		"architect | v0.8.2 review":     "v0.8.2 review",
		"| fix login":                   "fix login",
		"Reviewer-2 |  fix   login ":    "fix login",
		"codex | fix login":             "fix login",
		"reviewer":                      "",
		"architect |   ":                "",
		"codex-reviewer-01 | fix login": "",
		"claude-06 | adapter docs":      "",
		"agy-architect-06 | pane label": "",
		"pi-goal-ship-v1-02 | ship":     "",
		"shell":                         "",
		"implementer | a|b":             "a/b",
	} {
		if got := InitialTask(label); got != want {
			t.Errorf("InitialTask(%q) = %q, want %q", label, got, want)
		}
	}
}

// The pane-label request stands apart from the AMQ instructions, so an agent
// that never messages anyone still reads it; an agent that starts with a task
// is told what its pane shows instead of being asked for one.
func TestSessionNoticeAsksForTheTaskOnItsOwn(t *testing.T) {
	asked := SessionNotice("claude-06", "x env", "x title", "", 0)
	if !strings.Contains(asked, "\nPane label:") || !strings.Contains(asked, `x title "<task in a few words>"`) {
		t.Errorf("no separate request for a task:\n%s", asked)
	}
	told := SessionNotice("codex-architect-01", "x env", "x title", "v0.8.2 review", 0)
	if !strings.Contains(told, "`codex-architect-01 | v0.8.2 review`") || strings.Contains(told, "<task in a few words>") {
		t.Errorf("an agent with a task was asked for one:\n%s", told)
	}
}
