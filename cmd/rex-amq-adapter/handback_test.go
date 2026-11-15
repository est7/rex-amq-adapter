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
}

// hand-back is a command: help exits 0, bad usage 2, and a report reaches the
// caller lookup (which fails here, with no Rex) instead of being usage.
func TestHandBackIsDispatched(t *testing.T) {
	isolate(t)
	for args, want := range map[string]int{"--help": 0, "--bogus": 2, "": 2, "report": 1} {
		argv := []string{"hand-back"}
		if args != "" {
			argv = append(argv, args)
		}
		if code := run(argv); code != want {
			t.Errorf("hand-back %q: exit %d, want %d", args, code, want)
		}
	}
	if got, err := handBackBody([]string{"--", "- PR #18"}, strings.NewReader("")); err != nil || got != "- PR #18" {
		t.Errorf("a report after --: %q %v", got, err)
	}
	big, err := handBackBody([]string{"-"}, strings.NewReader(strings.Repeat("x", 1<<20)))
	if err != nil || len(big) != handBackReadMax {
		t.Errorf("stdin is read to %d bytes, want %d (%v)", len(big), handBackReadMax, err)
	}
}
