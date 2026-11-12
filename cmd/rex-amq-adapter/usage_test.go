package main

import (
	"testing"
	"time"
)

// isolate points the adapter at an empty state directory and no Rex pane,
// so a command that does resolve its caller fails instead of acting on the
// pane running the tests.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("REX_AMQ_ADAPTER_HOME", t.TempDir())
	t.Setenv("REX_SERVER", "unix:///nonexistent/rex.sock")
	t.Setenv("REX_BLOCK", "")
	t.Setenv("REX_SESSION", "")
	// Crews come from the working directory's parents and the user's
	// config: neither may be the host's.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
		t.Setenv(v, "")
	}
}

// title takes its task as words, so a flag-shaped first word is a request
// for help or a mistake, never a task (`title --help` once set the task to
// "--help"). `--` lets a task start with a dash.
func TestTitleTreatsFlagsAsUsage(t *testing.T) {
	isolate(t)
	for args, want := range map[string]int{"--help": 0, "-h": 0, "--json": 2, "-x": 2} {
		if code := runTitle([]string{args, "fix"}); code != want {
			t.Errorf("title %s fix: exit %d, want %d", args, code, want)
		}
	}
}

// crews lists, or shows one briefing; a flag it does not take is an error,
// not a directory to look in.
func TestCrewsRejectsUnknownFlags(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"--json"}, {"-x"}, {"show"}, {"show", "pair", "extra"}, {"show", "no-such-crew"}, {"show", "--role", "no-such-role"}} {
		if code := runCrews(args); code == 0 {
			t.Errorf("crews %v: exit 0, want an error", args)
		}
	}
	for _, args := range [][]string{nil, {"show", "pair"}, {"show", "pair", "--role", "reviewer"}, {"show", "--role", "orchestrator"}} {
		if code := runCrews(args); code != 0 {
			t.Errorf("crews %v: exit %d, want 0", args, code)
		}
	}
}

// await's timeout is a duration; a bare number is seconds, as the guide's
// "--timeout 60" reads.
func TestAwaitTimeoutTakesSecondsOrADuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"60": time.Minute, "90s": 90 * time.Second, "2m": 2 * time.Minute, "1.5": 1500 * time.Millisecond} {
		var d timeoutFlag
		if err := d.Set(in); err != nil || time.Duration(d) != want {
			t.Errorf("Set(%q) = %v, %v; want %v", in, time.Duration(d), err, want)
		}
	}
	for _, in := range []string{"", "abc", "-5", "0", "-1s"} {
		var d timeoutFlag
		if err := d.Set(in); err == nil {
			t.Errorf("Set(%q) accepted %v", in, time.Duration(d))
		}
	}
}
