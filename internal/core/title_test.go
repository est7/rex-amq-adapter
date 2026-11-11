package core

import (
	"strings"
	"testing"
)

// A session label names an Epic unless it is blank or the name Rex gives a
// session nobody named; a named Epic is made one line with no separator.
func TestEpicTitleLabel(t *testing.T) {
	for label, want := range map[string]string{
		"":                       "",
		"   ":                    "",
		"Session 01a1cad4":       "",
		" Session 01a1cad4 ":     "",
		"Session 01A1CAD4":       "Session 01A1CAD4",
		"Session 01a1cad":        "Session 01a1cad",
		"Session 01a1cad4x":      "Session 01a1cad4x",
		"auth-refresh":           "auth-refresh",
		"  cross  session\namq ": "cross session amq",
		"ship | v2":              "ship / v2",
	} {
		if got := EpicLabel(label); got != want {
			t.Errorf("EpicLabel(%q) = %q, want %q", label, got, want)
		}
		if PlaceholderSession(label) != (want == "") {
			t.Errorf("PlaceholderSession(%q) = %v", label, PlaceholderSession(label))
		}
	}
}

// With a named Epic the conversation title puts it in brackets before the
// task; without one it is exactly the pane label.
func TestConversationTitle(t *testing.T) {
	for _, c := range []struct {
		h                     Handle
		role, epic, task, out string
	}{
		{"claude-01", "reviewer", "auth-refresh", "fix login", "reviewer(claude-01) | [auth-refresh] fix login"},
		{"claude-01", "", "auth-refresh", "fix login", "claude-01 | [auth-refresh] fix login"},
		{"claude-01", "reviewer", "auth-refresh", "", "reviewer(claude-01) | [auth-refresh]"},
		{"pi-02", "", "auth-refresh", "  ", "pi-02 | [auth-refresh]"},
		{"pi-02", "", "ship | v2", "a|b", "pi-02 | [ship / v2] a/b"},
		{"claude-01", "reviewer", "", "fix login", "reviewer(claude-01) | fix login"},
		{"claude-01", "reviewer", "Session 01a1cad4", "fix login", "reviewer(claude-01) | fix login"},
		{"claude-01", "reviewer", "Session 01a1cad4", "", "reviewer(claude-01)"},
		{"codex-03", "", "", "", "codex-03"},
	} {
		if got := ConversationTitle(c.h, c.role, c.epic, c.task); got != c.out {
			t.Errorf("ConversationTitle(%q, %q, %q, %q) = %q, want %q", c.h, c.role, c.epic, c.task, got, c.out)
		}
		if c.epic == "" || PlaceholderSession(c.epic) {
			if pane := PaneLabel(c.h, c.role, c.task); c.out != pane {
				t.Errorf("without an Epic %q is not the pane label %q", c.out, pane)
			}
		}
	}
}

// Every conversation title still names its agent's handle, with or without
// the state glyph a harness draws in front, and never another handle.
func TestConversationTitleNamesHandle(t *testing.T) {
	for _, role := range []string{"", "reviewer"} {
		for _, epic := range []string{"", "Session 01a1cad4", "auth-refresh", "a | b", "[x]"} {
			for _, task := range []string{"", "fix login", "a | b"} {
				title := ConversationTitle("claude-01", role, epic, task)
				for _, shown := range []string{title, "✳ " + title} {
					if !TitleNamesHandle(shown, "claude-01") {
						t.Errorf("%q does not name claude-01", shown)
					}
					if TitleNamesHandle(shown, "claude-02") {
						t.Errorf("%q names claude-02", shown)
					}
				}
			}
		}
	}
}

// Claude Code and Pi are offered their title once a hook carries it, and
// want it again when it changes; Codex and agy take none, whatever their
// terminal shows.
func TestEpicTitleTracking(t *testing.T) {
	const want = "reviewer(claude-01) | [auth-refresh] fix login"
	for _, k := range []Kind{Claude, Pi} {
		if got := TrackTitle(k, want, ""); got != (TitleState{Status: TitleDesired, Desired: want}) {
			t.Errorf("%s never offered: %+v", k, got)
		}
		if got := TrackTitle(k, want, "reviewer(claude-01)"); got != (TitleState{Status: TitleDesired, Desired: want, Offered: "reviewer(claude-01)"}) {
			t.Errorf("%s offered an older title: %+v", k, got)
		}
		if got := TrackTitle(k, want, want); got != (TitleState{Status: TitleOffered, Desired: want, Offered: want}) {
			t.Errorf("%s offered it: %+v", k, got)
		}
	}
	for _, k := range []Kind{Codex, Agy} {
		if got := TrackTitle(k, want, want); got != (TitleState{Status: TitleUnsupported, Desired: want}) {
			t.Errorf("%s: %+v", k, got)
		}
	}
}

// Agents are told the Rex session label is the human's Epic, whether or not
// they have a task yet.
func TestEpicTitleNoticeForbidsSessionRename(t *testing.T) {
	for _, task := range []string{"", "fix login"} {
		n := TaskNotice("claude-06", "reviewer", "x title", task)
		if !strings.Contains(n, "1 Rex session = 1 Epic") || !strings.Contains(n, "never run `rex session rename` for your own task") {
			t.Errorf("task %q: the notice does not keep the session label for the Epic:\n%s", task, n)
		}
	}
}
