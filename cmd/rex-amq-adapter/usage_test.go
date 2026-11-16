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
	// ME_UNATTENDED too: an unattended agent running the tests would make
	// the cockpit refuse every case.
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID", "ME_UNATTENDED"} {
		t.Setenv(v, "")
	}
}

// view --cockpit is a command of its own: its scope flags need it, the
// narrator's verbs do not take it, a shell outside Rex that names no
// personal agent is usage, and one that names a missing one is refused; an
// unattended shell is refused even when it names one.
func TestViewCockpitExitCodes(t *testing.T) {
	isolate(t)
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"view", "--personal", "pi-01"}, 2},
		{[]string{"view", "--hub", "/tmp"}, 2},
		{[]string{"view", "refresh", "--cockpit"}, 2},
		{[]string{"view", "read", "--ids", "x", "--cockpit"}, 2},
		{[]string{"view", "--cockpit"}, 2},
		{[]string{"view", "--cockpit", "--session", "session:a"}, 2},
		{[]string{"view", "--cockpit", "--personal", "pi-01"}, 1},
		{[]string{"view", "--cockpit", "--hub", t.TempDir()}, 1},
	} {
		if code := run(c.args); code != c.want {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.want)
		}
	}
	t.Setenv("ME_UNATTENDED", "1")
	code, out := captureStderr(t, func() int { return run([]string{"view", "--cockpit", "--personal", "pi-01"}) })
	if code != 1 || !strings.Contains(out, "ME_UNATTENDED=1") {
		t.Errorf("an unattended shell: exit %d\n%s", code, out)
	}
}

// epic and epic close take --op-id; one that is not text of at most 256
// bytes is usage, refused before any caller lookup.
func TestEpicOpIDFlag(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"epic", "x", "--asked", "go", "--op-id", strings.Repeat("x", 257)},
		{"epic", "close", "session:a", "--asked", "close", "--op-id", "a\tb"},
	} {
		code, out := captureStderr(t, func() int { return run(args) })
		if code != 2 || !strings.Contains(out, "--op-id is at most 256 bytes") {
			t.Errorf("%v: exit %d\n%s", args[:2], code, out)
		}
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

// The crews table lists the roles that brief agents in a session of no crew
// under "(any session)", since a tab names no crew.
func TestCrewsTableNamesRolesOfAnySession(t *testing.T) {
	isolate(t)
	code, out := capture(t, runCrews)
	if code != 0 || !strings.Contains(out, "(any session)") || strings.Contains(out, "(any tab)") {
		t.Fatalf("crews: exit %d\n%s", code, out)
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

// captureStderr runs f with its stderr in a file and returns the exit code
// and what it printed there.
func captureStderr(t *testing.T, f func() int) (int, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	before := os.Stderr
	os.Stderr = out
	code := f()
	os.Stderr = before
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
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
	if c := got.Crews[pair]; c.Lead != "implementer" || strings.Join(c.Members, ",") != "implementer,reviewer" || c.Layer != "built-in" || !strings.HasPrefix(c.Purpose, "This crew is a two-agent loop") || strings.Contains(c.Purpose, "\n") {
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

// epic needs a name and takes only its own flags, as epic close needs its
// session; usage errors exit 2 before anything is resolved or started.
func TestEpicRejectsBadUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"--goal", "g.md"}, {"x", "--nope"}, {"x", "extra"}, {"x", "--timeout", "soon"},
		{"close"}, {"close", "--asked", "x"}, {"close", "s", "--nope"}, {"close", "s", "extra"}} {
		if code := runEpic(args); code != 2 {
			t.Errorf("epic %v: exit %d, want 2", args, code)
		}
	}
}

// retire takes one handle, then its flags; anything else is usage (exit 2),
// and the command is listed in usage.
func TestRetireUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{nil, {"--force"}, {"codex-01", "extra"}, {"codex-01", "--nope"}} {
		if code := runRetire(args); code != 2 {
			t.Errorf("retire %v: exit %d, want 2", args, code)
		}
	}
	if code := runRetire([]string{"codex-01"}); code == 0 {
		t.Error("retire from no Rex pane: exit 0")
	}
	if !strings.Contains(usage, "retire <handle> [--force]") {
		t.Error("usage does not list retire")
	}
}
