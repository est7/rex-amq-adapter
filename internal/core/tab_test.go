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

func TestWindowLedgerClaimedTabMode(t *testing.T) {
	if ClaimedTabMode("narrator") != WindowNarrator || ClaimedTabMode("ship") != WindowCrew || ClaimedTabMode("ship-topic") != WindowCrew {
		t.Error("a claimed tab is a narrator's only when named narrator")
	}
}

func TestCrewTabAdapterLabelNamesNoCrew(t *testing.T) {
	for label, want := range map[string]bool{
		"claude-85":                    true,
		"implementer(claude-85)":       true,
		"implementer(claude-85) | fix": true,
		"pi-01 | x":                    true,
		"ship":                         false,
		"ship | login":                 false,
		"reviewer | fix login":         false, // a launcher's label
		"Window 4c6fe88d":              false,
	} {
		if got := AdapterLabel(label); got != want {
			t.Errorf("AdapterLabel(%q) = %v, want %v", label, got, want)
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
		blocks int
		next   TabState
		rename bool
	}{
		{"an unknown placeholder tab of one block follows", TabState{}, "Window 4c6fe88d", 1, auto(want), true},
		{"an unknown placeholder tab of two blocks is claimed but kept", TabState{}, "Window 4c6fe88d", 2, auto("Window 4c6fe88d"), false},
		{"an unknown named tab is left alone", TabState{}, "Work", 1, TabState{}, false},
		{"an unknown tab of four hex digits is a name", TabState{}, "Window 1a2b", 1, TabState{}, false},
		{"a follower still showing the summon label follows", auto("implementer | fix"), "implementer | fix", 1, auto(want), true},
		{"a follower showing want only refreshes its entry", auto("implementer | fix"), want, 1, auto(want), false},
		{"a follower Rex reset to its placeholder follows", auto(want), "Window 4c6fe88d", 1, auto(want), true},
		{"a follower someone renamed becomes manual", auto("implementer(claude-85)"), "my tab", 1, TabState{Mode: WindowManual, Last: "implementer(claude-85)"}, false},
		{"a follower of two blocks keeps its label", auto("reviewer(codex-01)"), "reviewer(codex-01)", 2, auto("reviewer(codex-01)"), false},
		{"a manual tab is never renamed", TabState{Mode: WindowManual, Last: "x"}, "Window 4c6fe88d", 1, TabState{Mode: WindowManual, Last: "x"}, false},
		{"a crew tab is never renamed", TabState{Mode: WindowCrew, Last: "ship"}, "ship", 1, TabState{Mode: WindowCrew, Last: "ship"}, false},
		{"a narrator tab is never renamed", TabState{Mode: WindowNarrator, Last: "narrator"}, "narrator", 1, TabState{Mode: WindowNarrator, Last: "narrator"}, false},
	} {
		next, rename := FollowTab(c.t, c.label, c.blocks, want)
		if next != c.next || rename != c.rename {
			t.Errorf("%s: got %+v rename=%v, want %+v rename=%v", c.name, next, rename, c.next, c.rename)
		}
	}
	if next, rename := FollowTab(auto("x"), "x", 1, ""); next != auto("x") || rename {
		t.Errorf("no pane label: got %+v rename=%v", next, rename)
	}
}
