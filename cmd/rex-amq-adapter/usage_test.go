package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
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
	for _, args := range [][]string{{"--yaml"}, {"--json", "a", "b"}, {"-x"}, {"show"}, {"show", "pair", "extra"}, {"show", "no-such-crew"}, {"show", "--role", "no-such-role"}} {
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

// capture runs a command with its stdout in a file and returns the exit
// code and what it printed.
func capture(t *testing.T, run func([]string) int, args ...string) (int, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	before := os.Stdout
	os.Stdout = out
	code := run(args)
	os.Stdout = before
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}

// crews --json is the listing as data: each crew's lead, members, and
// layer, each role's layer, and a purpose line for both, so an agent can
// plan a team without parsing the table.
func TestCrewsJSONListsCrewsAndRolesWithTheirPurpose(t *testing.T) {
	isolate(t)
	code, out := capture(t, runCrews, "--json")
	if code != 0 {
		t.Fatalf("crews --json: exit %d: %s", code, out)
	}
	var got struct {
		Crews []struct {
			Name, Lead, Layer, Purpose string
			Members                    []string
		}
		Roles []struct{ Name, Layer, Purpose string }
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	crews := map[string]int{}
	for i, c := range got.Crews {
		crews[c.Name] = i
	}
	pair, ok := crews["pair"]
	if !ok || len(got.Crews) != 5 {
		t.Fatalf("crews: %+v", got.Crews)
	}
	if c := got.Crews[pair]; c.Lead != "implementer" || strings.Join(c.Members, ",") != "implementer,reviewer" || c.Layer != "built-in" || !strings.HasPrefix(c.Purpose, "This tab is a two-agent loop") || strings.Contains(c.Purpose, "\n") {
		t.Errorf("pair: %+v", c)
	}
	roles := map[string]string{}
	for _, r := range got.Roles {
		roles[r.Name] = r.Purpose
	}
	if !strings.HasPrefix(roles["orchestrator"], "You are the orchestrator") || roles["personal"] == "" || len(got.Roles) != 9 {
		t.Errorf("roles: %+v", got.Roles)
	}
}

// --version is version, as most CLIs spell it.
func TestVersionFlagIsTheVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		if code, out := capture(t, run, arg); code != 0 || strings.TrimSpace(out) != version() {
			t.Errorf("%s: exit %d, %q", arg, code, out)
		}
	}
}

// epic needs a name and takes only its own flags; usage errors exit 2 before
// anything is resolved or started.
func TestEpicRejectsBadUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"--goal", "g.md"}, {"x", "--nope"}, {"x", "extra"}, {"x", "--timeout", "soon"}} {
		if code := runEpic(args); code != 2 {
			t.Errorf("epic %v: exit %d, want 2", args, code)
		}
	}
}
