package main

import (
	"strings"
	"testing"
)

// agy stops asking after repeated failures, so the title command always
// answers: outside Rex, for an unknown kind, or with unreadable input it
// names the directory, or prints an empty line.
func TestPaneTitleAlwaysAnswers(t *testing.T) {
	t.Setenv("REX_BLOCK", "")
	if got := paneTitle("agy", strings.NewReader(`{"cwd":"/w/proj","conversation_id":"c"}`)); got != "proj" {
		t.Errorf("outside Rex: %q", got)
	}
	if got := paneTitle("nope", strings.NewReader(`{"cwd":"/w/proj"}`)); got != "proj" {
		t.Errorf("unknown kind: %q", got)
	}
	if got := paneTitle("agy", strings.NewReader(`not json`)); got != "" {
		t.Errorf("unreadable input: %q", got)
	}
}
