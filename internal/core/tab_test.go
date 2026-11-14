package core

import "testing"

func TestWindowLedgerPlaceholderTab(t *testing.T) {
	for label, want := range map[string]bool{
		"Window 4c6fe88d":  true,
		"Window 82b616eb":  true,
		"Window 1a2b":      false, // four digits: a name someone chose
		"Window 4C6FE88D":  false,
		"Window 4c6fe88d ": false,
		"window 4c6fe88d":  false,
		"Window 4c6fe88g":  false,
		"pair":             false,
		"":                 false,
	} {
		if got := PlaceholderTab(label); got != want {
			t.Errorf("PlaceholderTab(%q) = %v, want %v", label, got, want)
		}
	}
}

func TestFollowerTabDecisions(t *testing.T) {
	const want = "implementer(claude-85) | fix"
	auto := func(last string) TabState { return TabState{Mode: WindowAuto, Last: last} }
	for _, c := range []struct {
		name   string
		t      TabState
		label  string
		agents int
		next   TabState
		rename string
	}{
		{"an unknown placeholder tab of one agent follows", TabState{}, "Window 4c6fe88d", 1, auto(want), want},
		{"an unknown placeholder tab of two agents is claimed but kept", TabState{}, "Window 4c6fe88d", 2, auto("Window 4c6fe88d"), ""},
		{"an unknown named tab is left alone", TabState{}, "Work", 1, TabState{}, ""},
		{"an unknown tab of four hex digits is a name", TabState{}, "Window 1a2b", 1, TabState{}, ""},
		{"an unknown tab named after a crew is left alone", TabState{}, "pair", 1, TabState{}, ""},
		{"a follower still showing the summon label follows", auto("implementer | fix"), "implementer | fix", 1, auto(want), want},
		{"a follower showing want only refreshes its entry", auto("implementer | fix"), want, 1, auto(want), ""},
		{"a follower Rex reset to its placeholder follows", auto(want), "Window 4c6fe88d", 1, auto(want), want},
		{"a follower someone renamed becomes manual", auto("implementer(claude-85)"), "my tab", 1, TabState{Mode: WindowManual, Last: "implementer(claude-85)"}, ""},
		{"a follower of two agents keeps a label that names no task", auto("reviewer(codex-01)"), "reviewer(codex-01)", 2, auto("reviewer(codex-01)"), ""},
		{"a follower of several agents drops its agent's label to the task", auto("orchestrator(claude-03) | crew audit goal"), "orchestrator(claude-03) | crew audit goal", 4, auto("crew audit goal"), "crew audit goal"},
		{"a follower whose one agent has a preview pane beside it follows that agent", auto("orchestrator(claude-03) | audit"), "orchestrator(claude-03) | audit", 1, auto(want), want},
		{"a follower with no agent left keeps its label", auto("orchestrator(claude-03) | audit"), "orchestrator(claude-03) | audit", 0, auto("orchestrator(claude-03) | audit"), ""},
		{"a follower of several agents already showing the task keeps it", auto("crew audit goal"), "crew audit goal", 3, auto("crew audit goal"), ""},
		{"a follower of several agents someone renamed becomes manual", auto("orchestrator(claude-03) | audit"), "release", 2, TabState{Mode: WindowManual, Last: "orchestrator(claude-03) | audit"}, ""},
		{"a follower back to one agent follows it again", auto("crew audit goal"), "crew audit goal", 1, auto(want), want},
		{"a manual tab is never renamed", TabState{Mode: WindowManual, Last: "x"}, "Window 4c6fe88d", 1, TabState{Mode: WindowManual, Last: "x"}, ""},
	} {
		next, rename := FollowTab(c.t, c.label, c.agents, want)
		if next != c.next || rename != c.rename {
			t.Errorf("%s: got %+v rename=%q, want %+v rename=%q", c.name, next, rename, c.next, c.rename)
		}
	}
	if next, rename := FollowTab(auto("x"), "x", 1, ""); next != auto("x") || rename != "" {
		t.Errorf("no pane label: got %+v rename=%q", next, rename)
	}
}
