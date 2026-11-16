package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// panel has one output format and takes only --session; anything else is
// usage (exit 2), and the command is listed in usage.
func TestPanelRejectsBadUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"--session", "session:1"}, {"--json", "extra"}, {"--json", "--tab"}, {"--json", "--all"}} {
		if code := run(append([]string{"panel"}, args...)); code != 2 {
			t.Errorf("panel %v: exit %d, want 2", args, code)
		}
	}
	if !strings.Contains(usage, "panel --json [--session S]") {
		t.Error("usage does not list panel --json")
	}
}

// An unattended shell, and an agent's tool shell, are refused before the
// panel asks Rex anything: the stub Rex fails every call, so only the gate
// can name the reason.
func TestPanelRefusesAgentsBeforeRex(t *testing.T) {
	isolate(t)
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho stub rex called >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	for _, c := range []struct{ name, value, reason string }{
		{"ME_UNATTENDED", "1", "ME_UNATTENDED=1"},
		{"CODEX_THREAD_ID", "thread-1", "CODEX_THREAD_ID is set"},
	} {
		t.Setenv(c.name, c.value)
		code, out := captureStderr(t, func() int { return run([]string{"panel", "--json", "--session", "session:probe"}) })
		t.Setenv(c.name, "")
		if code != 1 || !strings.Contains(out, c.reason) || strings.Contains(out, "stub rex called") {
			t.Errorf("%s=%s: exit %d\n%s", c.name, c.value, code, out)
		}
	}
}
