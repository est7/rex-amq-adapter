package main

import (
	"strings"
	"testing"
)

// summon --council is a review role's: a writer role, a handle, or an
// explicit --harness is usage (exit 2), settled before the caller is looked
// up, so it holds outside Rex too; --help lists the flag.
func TestSummonCouncilUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"summon", "implementer", "--council", "--subject", "s", "--body", "b"},
		{"summon", "codex-02", "--council", "--subject", "s", "--body", "b"},
		{"summon", "reviewer", "--council", "--harness", "codex", "--subject", "s", "--body", "b"},
	} {
		code, out := captureStderr(t, func() int { return run(args) })
		if code != 2 || !strings.Contains(out, "--council") {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
	// A review role passes usage and fails on the missing Rex pane instead.
	if code := run([]string{"summon", "reviewer", "--council", "--subject", "s", "--body", "b"}); code != 1 {
		t.Errorf("summon reviewer --council outside Rex: exit %d, want 1", code)
	}
	code, out := captureStderr(t, func() int { return run([]string{"summon", "--help"}) })
	if code != 2 || !strings.Contains(out, "-council") || !strings.Contains(out, "score") {
		t.Errorf("summon --help: exit %d\n%s", code, out)
	}
}
