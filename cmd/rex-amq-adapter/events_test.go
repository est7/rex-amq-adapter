package main

import "testing"

// events has one output format and one scope at a time.
func TestEventsRejectsBadUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--tab"},
		{"--json", "--tab", "--all"},
		{"--json", "--session", "session:1", "--all"},
		{"--json", "extra"},
	} {
		if code := runEvents(args); code != 2 {
			t.Errorf("events %v: exit %d, want 2", args, code)
		}
	}
}
