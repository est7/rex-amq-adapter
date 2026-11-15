package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hand-back takes its report as the one argument: the text, @FILE, or - for
// stdin, unchanged; anything else is usage.
func TestHandBackBodyArgs(t *testing.T) {
	report := "## Review queue\n- PR #18\n## Blocked on me\n- merge #18\n"
	file := filepath.Join(t.TempDir(), "hand-back.md")
	if err := os.WriteFile(file, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{report}, report},
		{[]string{"@" + file}, report},
		{[]string{"-"}, "from stdin\n"},
	} {
		got, err := handBackBody(c.args, strings.NewReader("from stdin\n"))
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v", c.args, got, err)
		}
	}
	for _, args := range [][]string{nil, {"a", "b"}, {"--summary"}, {"@" + file + ".missing"}} {
		if _, err := handBackBody(args, strings.NewReader("")); err == nil {
			t.Errorf("%q was taken", args)
		}
	}
	if code := runHandBack([]string{"--bogus"}, strings.NewReader("")); code != 2 {
		t.Errorf("a bad flag exits %d", code)
	}
}
