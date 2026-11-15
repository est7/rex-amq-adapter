package main

import (
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/install"
)

// status names each Codex hook still waiting for approval, the fix (/hooks),
// and what waits on it; with none waiting it says so.
func TestStatusHookApprovalNote(t *testing.T) {
	c := codexHooks{File: "/h/.codex/hooks.json", Unapproved: []install.CodexHookReview{{Key: "/h/.codex/hooks.json:pre_tool_use:1:0", Status: install.HookUntrusted}}}
	note := c.note()
	for _, want := range []string{"1 hook(s)", "unapproved_hooks", "/h/.codex/hooks.json:pre_tool_use:1:0 (untrusted)", "/hooks", "Hooks need review", "another family"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if ok := (codexHooks{File: "/h/.codex/hooks.json"}).note(); !strings.Contains(ok, "every hook") || strings.Contains(ok, "/hooks in") {
		t.Errorf("approved note: %s", ok)
	}
}
