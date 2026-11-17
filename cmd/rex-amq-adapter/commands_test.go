package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// want is one invocation and the exit status and error code it must give.
type want struct {
	args []string
	code int
	err  string
}

func check(t *testing.T, cases []want) {
	t.Helper()
	for _, c := range cases {
		code, env := machine(t, c.args...)
		if code != c.code || env.code() != c.err {
			msg := ""
			if env.Error != nil {
				msg = env.Error.Message
			}
			t.Errorf("%q: exit %d %q (%s), want %d %q", c.args, code, env.code(), msg, c.code, c.err)
		}
	}
}

// agent summon --council is a review role's: a writer role, a handle, or an
// explicit --harness is usage, settled before the caller is looked up, so
// it holds outside Rex too; a review role passes usage and fails on the
// missing Rex pane instead.
func TestSummonCouncilUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	for _, args := range [][]string{
		{"agent", "summon", "implementer", "--council", "--subject", "s", "--body", "b"},
		{"agent", "summon", "codex-02", "--council", "--subject", "s", "--body", "b"},
		{"agent", "summon", "reviewer", "--council", "--harness", "codex", "--subject", "s", "--body", "b"},
	} {
		code, env := machine(t, args...)
		if code != 2 || env.code() != "usage" || !strings.Contains(env.Error.Message, "council") {
			t.Errorf("%v: exit %d %+v", args, code, env.Error)
		}
	}
	check(t, []want{{[]string{"agent", "summon", "reviewer", "--council", "--subject", "s", "--body", "b"}, 5, "failed_precondition"}})
	r := invoke(t, "", "agent", "summon", "--help", "--text")
	if r.code != 0 || !strings.Contains(r.stdout, "--council") || !strings.Contains(r.stdout, "SCORE") {
		t.Errorf("agent summon --help: exit %d\n%s", r.code, r.stdout)
	}
}

// stream events watches one scope at a time and takes nothing else.
func TestStreamEventsRejectsBadUsage(t *testing.T) {
	isolate(t)
	check(t, []want{
		{[]string{"stream", "events", "--tab", "--all"}, 2, "usage"},
		{[]string{"stream", "events", "--session", "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3", "--all"}, 2, "usage"},
		{[]string{"stream", "events", "extra"}, 2, "usage"},
		{[]string{"stream", "events", "--json", "--follow"}, 2, "usage"},
		{[]string{"stream", "events", "--session", "session:1"}, 2, "invalid_input"},
	})
}

// epic handback takes the report as its one argument: text, @FILE, or -;
// help exits 0, usage 2, and a report reaches the caller lookup (no Rex
// pane here: failed_precondition) instead of being usage.
func TestEpicHandbackArgs(t *testing.T) {
	isolate(t)
	stubTools(t)
	file := filepath.Join(t.TempDir(), "hand-back.md")
	if err := os.WriteFile(file, []byte("## Review queue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := invoke(t, "", "epic", "handback", "--help"); r.code != 0 {
		t.Errorf("--help: exit %d", r.code)
	}
	check(t, []want{
		{[]string{"epic", "handback", "--bogus"}, 2, "usage"},
		{[]string{"epic", "handback"}, 2, "usage"},
		{[]string{"epic", "handback", "a", "b"}, 2, "usage"},
		{[]string{"epic", "handback", "@" + file + ".missing"}, 2, "invalid_input"},
		{[]string{"epic", "handback", "report"}, 5, "failed_precondition"},
		{[]string{"epic", "handback", "@" + file}, 5, "failed_precondition"},
		{[]string{"epic", "handback", "--", "- PR #18"}, 5, "failed_precondition"},
		{[]string{"epic", "handback", "report", "--artifact", "a.md=@" + file}, 5, "failed_precondition"},
		{[]string{"epic", "handback", "report", "--artifact", "../a.md=@" + file}, 2, "invalid_input"},
	})
	if r := invoke(t, "x", "epic", "handback", "-"); r.code != 5 {
		t.Errorf("- from stdin: exit %d %s", r.code, r.stdout)
	}
}

// Outside Rex with no personal agent recorded, hub notify reports
// no_live_personal and exits 0: a launchd job's tick is not a failure, and
// nothing is started or recorded.
func TestHubNotifyWithNoLivePersonalExitsZero(t *testing.T) {
	isolate(t)
	stubTools(t)
	for _, dry := range []bool{true, false} {
		args := []string{"hub", "notify", "--ref", "timer:7", "--event-id", "tick-7", "--summary", "stand-up"}
		if dry {
			args = append(args, "--dry-run")
		}
		code, env := machine(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %+v", args, code, env.Error)
		}
		var res map[string]any
		if err := json.Unmarshal(env.Data, &res); err != nil {
			t.Fatal(err)
		}
		_, marked := res["dry_run"]
		if res["status"] != "no_live_personal" || res["ref"] != "timer:7" || res["event_id"] != "tick-7" || marked != dry {
			t.Fatalf("result %v", res)
		}
	}
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	if n, _ := filepath.Glob(filepath.Join(home, "amq-root", "extensions", "rex-amq-adapter", "notifications", "*.json")); len(n) != 0 {
		t.Errorf("an unsent event was recorded: %v", n)
	}
}

// hub notify needs a typed --ref and an --event-id and takes nothing else;
// a malformed call exits 2 before anything is resolved.
func TestHubNotifyUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	var cases []want
	for _, args := range [][]string{nil, {"--ref", "inbox:1"}, {"--event-id", "e"}, {"--ref", "inbox:1", "--event-id", "e", "extra"},
		{"--ref", "inbox:1", "--event-id", "e", "--nope"}} {
		cases = append(cases, want{append([]string{"hub", "notify"}, args...), 2, "usage"})
	}
	for _, args := range [][]string{{"--ref", "todo:1", "--event-id", "e"}, {"--ref", "inbox:", "--event-id", "e"},
		{"--ref", "inbox:1", "--event-id", "a\nb"}, {"--ref", "inbox:1", "--event-id", "e", "--personal", "Bad Handle"},
		{"--ref", "inbox:1", "--event-id", "e", "--hub", "/nonexistent/hub"}} {
		cases = append(cases, want{append([]string{"hub", "notify"}, args...), 2, "invalid_input"})
	}
	check(t, cases)
}

// agent title takes its task as words: a flag-shaped first word is help or
// a mistake, never a task; `--` lets a task start with a dash.
func TestTitleTreatsFlagsAsUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	for _, h := range []string{"--help", "-h"} {
		if r := invoke(t, "", "agent", "title", h, "fix"); r.code != 0 {
			t.Errorf("title %s fix: exit %d", h, r.code)
		}
	}
	check(t, []want{
		{[]string{"agent", "title", "-x", "fix"}, 2, "usage"},
		{[]string{"agent", "title", "--nope"}, 2, "usage"},
		{[]string{"agent", "title", "a\x1b[31mb"}, 2, "invalid_input"},
		{[]string{"agent", "title", "--", "-x", "fix"}, 5, "failed_precondition"},
	})
}

// crew list and crew show read the crews of --cwd (or the working
// directory); an unknown crew or role is not_found, a flag they do not
// take usage.
func TestCrewCommands(t *testing.T) {
	isolate(t)
	check(t, []want{
		{[]string{"crew", "list", "--yaml"}, 2, "usage"},
		{[]string{"crew", "list", "a"}, 2, "usage"},
		{[]string{"crew", "list", "-c", "."}, 2, "usage"},
		{[]string{"crew", "show"}, 2, "usage"},
		{[]string{"crew", "show", "pair", "extra"}, 2, "usage"},
		{[]string{"crew", "show", "Pair"}, 2, "invalid_input"},
		{[]string{"crew", "show", "no-such-crew"}, 4, "not_found"},
		{[]string{"crew", "show", "--role", "no-such-role"}, 4, "not_found"},
		{[]string{"crew", "list"}, 0, ""},
		{[]string{"crew", "list", "--cwd", t.TempDir()}, 0, ""},
		{[]string{"crew", "show", "pair"}, 0, ""},
		{[]string{"crew", "show", "pair", "--role", "reviewer"}, 0, ""},
		{[]string{"crew", "show", "--role", "orchestrator"}, 0, ""},
	})
	_, env := machine(t, "crew", "show", "pair")
	var show struct {
		Role, Markdown string
	}
	if err := json.Unmarshal(env.Data, &show); err != nil || show.Role != "implementer" || !strings.Contains(show.Markdown, "## Team conventions of crew pair") {
		t.Errorf("crew show pair: %v %+v", err, show)
	}
	r := invoke(t, "", "crew", "show", "pair", "--text")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "crew pair (built-in)") || r.stdout != show.Markdown {
		t.Errorf("crew show --text is not the raw markdown:\n%s", r.stdout)
	}
}

// crew list is the listing as data: each crew's lead, members, layer, and a
// purpose line, and the roles a session of no crew can take.
func TestCrewListListsCrewsAndRolesWithTheirPurpose(t *testing.T) {
	isolate(t)
	code, env := machine(t, "crew", "list")
	if code != 0 {
		t.Fatalf("crew list: exit %d", code)
	}
	var got struct {
		Items []struct {
			Name, Lead, Layer, Purpose string
			Members                    []string
		}
		Total int
		Roles []struct{ Name, Layer, Purpose string }
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, env.Data)
	}
	crews := map[string]int{}
	for i, c := range got.Items {
		crews[c.Name] = i
	}
	pair, ok := crews["pair"]
	if !ok || len(got.Items) != 5 || got.Total != 5 {
		t.Fatalf("crews: %+v", got.Items)
	}
	if c := got.Items[pair]; c.Lead != "implementer" || strings.Join(c.Members, ",") != "implementer,reviewer" || c.Layer != "built-in" || !strings.HasPrefix(c.Purpose, "This crew is a two-agent loop") || strings.Contains(c.Purpose, "\n") {
		t.Errorf("pair: %+v", c)
	}
	roles := map[string]string{}
	for _, r := range got.Roles {
		roles[r.Name] = r.Purpose
	}
	if !strings.HasPrefix(roles["orchestrator"], "You are the orchestrator") || roles["personal"] == "" || len(got.Roles) != 9 {
		t.Errorf("roles: %+v", got.Roles)
	}
	r := invoke(t, "", "crew", "list", "--text")
	if r.code != 0 || !strings.Contains(r.stdout, "(any session)") {
		t.Errorf("crew list --text:\n%s", r.stdout)
	}
}

// version is the version command; there is no --version flag.
func TestVersion(t *testing.T) {
	isolate(t)
	code, env := machine(t, "version")
	var v struct{ Version string }
	if err := json.Unmarshal(env.Data, &v); code != 0 || err != nil || v.Version != version() {
		t.Errorf("version: exit %d %s", code, env.Data)
	}
	if r := invoke(t, "", "version", "--text"); r.code != 0 || strings.TrimSpace(r.stdout) != version() {
		t.Errorf("version --text: %d %q", r.code, r.stdout)
	}
	check(t, []want{{[]string{"--version"}, 2, "usage"}})
}

// epic open and epic close are usage errors (exit 2) before any caller
// lookup when their own words are malformed; a missing --asked on close is
// the named confirmation.
func TestEpicRejectsBadUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	dir := t.TempDir()
	s := "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"
	check(t, []want{
		{[]string{"epic", "open"}, 2, "usage"},
		{[]string{"epic", "open", "--goal", "g.md"}, 2, "usage"},
		{[]string{"epic", "open", "x", "--asked", "go"}, 2, "usage"},
		{[]string{"epic", "open", "x", "--cwd", dir}, 2, "usage"},
		{[]string{"epic", "open", "x", "--cwd", dir, "--asked", "go", "--nope"}, 2, "usage"},
		{[]string{"epic", "open", "x", "extra", "--cwd", dir, "--asked", "go"}, 2, "usage"},
		{[]string{"epic", "open", "x", "--cwd", dir, "--asked", "go", "--timeout", "soon"}, 2, "invalid_input"},
		{[]string{"epic", "open", "x", "-c", dir, "--asked", "go"}, 2, "usage"},
		{[]string{"epic", "open", "x", "--cwd", dir, "--asked", "go", "--op-id", strings.Repeat("x", 257)}, 2, "invalid_input"},
		{[]string{"epic", "open", "x", "--cwd", dir, "--asked", "go", "--op-id", "a‮b"}, 2, "invalid_input"},
		{[]string{"epic", "open", "x", "--cwd", dir, "--asked", "go", "--op-id", "me:board:epic-7#open"}, 5, "failed_precondition"},
		{[]string{"epic", "close"}, 2, "usage"},
		{[]string{"epic", "close", "--asked", "x"}, 2, "usage"},
		{[]string{"epic", "close", s, "--nope"}, 2, "usage"},
		{[]string{"epic", "close", s, "extra"}, 2, "usage"},
		{[]string{"epic", "close", "s"}, 2, "invalid_input"},
		{[]string{"epic", "close", s}, 2, "confirmation_required"},
		{[]string{"epic", "close", s, "--asked", "close", "--op-id", "a\tb"}, 2, "invalid_input"},
		{[]string{"epic", "close", s, "--asked", "close it"}, 5, "failed_precondition"},
	})
}

// epic open --goal - pipes the contract in on stdin, whole up to one byte
// past the limit (so the open refuses an oversized one rather than a cut
// one), as text, never a path; empty stdin is usage. A file stays a path.
func TestEpicGoalStdinFeedsTheGrant(t *testing.T) {
	isolate(t)
	stubTools(t)
	if file, text, err := epicGoal("-", strings.NewReader("GOAL: x\n")); err != nil || file != "" || text != "GOAL: x\n" {
		t.Errorf("piped: %q %q %v", file, text, err)
	}
	if _, text, err := epicGoal("-", strings.NewReader(strings.Repeat("x", app.EpicGoalMax+10))); err != nil || len(text) != app.EpicGoalMax+1 {
		t.Errorf("oversized: %d bytes, %v", len(text), err)
	}
	if file, text, err := epicGoal("g.md", strings.NewReader("ignored")); err != nil || file != "g.md" || text != "" {
		t.Errorf("a file: %q %q %v", file, text, err)
	}
	r := invoke(t, "", "epic", "open", "x", "--cwd", t.TempDir(), "--goal", "-", "--asked", "go")
	if env := decode(t, nil, r); r.code != 2 || env.code() != "usage" {
		t.Errorf("empty stdin: exit %d %s", r.code, env.code())
	}
}

// agent retire takes one handle, then its flags; anything else is usage,
// a hostile handle invalid input, both before any caller lookup.
func TestRetireUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	check(t, []want{
		{[]string{"agent", "retire"}, 2, "usage"},
		{[]string{"agent", "retire", "--force"}, 2, "usage"},
		{[]string{"agent", "retire", "codex-01", "extra"}, 2, "usage"},
		{[]string{"agent", "retire", "codex-01", "--nope"}, 2, "usage"},
		{[]string{"agent", "retire", "../../etc/passwd"}, 2, "invalid_input"},
		{[]string{"agent", "retire", "codex-01"}, 5, "failed_precondition"},
	})
}

// trust grant writes each project's key into Claude's and Codex's stores
// under HOME and reports per directory; --dry-run writes nothing; trust
// root adds, lists, and removes; trust check reads every harness's verdict.
func TestTrustGrantAndRootCommands(t *testing.T) {
	home := isolate(t)
	repo := filepath.Join(home, "projects", "app")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	type grant struct {
		Items []struct{ Dir, Root, Status, Claude, Codex, Source string }
	}
	code, env := machine(t, "trust", "grant", repo, "--dry-run")
	var g grant
	if err := json.Unmarshal(env.Data, &g); code != 0 || err != nil || len(g.Items) != 1 || g.Items[0].Status != "would_grant" || g.Items[0].Root != repo {
		t.Fatalf("grant --dry-run: %d %s", code, env.Data)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Fatal("grant --dry-run wrote .claude.json")
	}
	code, env = machine(t, "trust", "grant", repo)
	g = grant{}
	if err := json.Unmarshal(env.Data, &g); code != 0 || err != nil || len(g.Items) != 1 {
		t.Fatalf("grant: %d %s", code, env.Data)
	}
	if r := g.Items[0]; r.Status != "granted" || r.Root != repo || r.Claude != "set" || r.Codex != "set" || r.Source != "explicit" {
		t.Fatalf("grant: %+v", r)
	}
	claude, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	codex, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(string(claude), repo) || !strings.Contains(string(codex), repo) {
		t.Fatalf("stores under HOME:\n%s\n%s", claude, codex)
	}
	if log, _ := os.ReadFile(filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "logs", "trust.log")); !strings.Contains(string(log), repo) {
		t.Fatalf("trust.log %q", log)
	}
	code, env = machine(t, "trust", "check", repo)
	var verdicts struct {
		Verdicts []struct {
			Harness string
			Trusted bool
		}
	}
	if err := json.Unmarshal(env.Data, &verdicts); code != 0 || err != nil || len(verdicts.Verdicts) != 4 || !verdicts.Verdicts[0].Trusted {
		t.Errorf("trust check after a grant: %d %s", code, env.Data)
	}
	root := filepath.Dir(repo)
	roots := func(args ...string) []string {
		t.Helper()
		code, env := machine(t, args...)
		var r struct{ Items []struct{ Path string } }
		if err := json.Unmarshal(env.Data, &r); code != 0 || err != nil {
			t.Fatalf("%v: %d %s", args, code, env.Data)
		}
		var paths []string
		for _, it := range r.Items {
			paths = append(paths, it.Path)
		}
		return paths
	}
	if got := roots("trust", "root", "add", root, "--dry-run"); len(got) != 1 || got[0] != root {
		t.Fatalf("root add --dry-run: %v", got)
	}
	if got := roots("trust", "root", "list"); len(got) != 0 {
		t.Fatalf("root add --dry-run added: %v", got)
	}
	if got := roots("trust", "root", "add", root); len(got) != 1 || got[0] != root {
		t.Fatalf("root add: %v", got)
	}
	if got := roots("trust", "root", "list"); len(got) != 1 || got[0] != root {
		t.Fatalf("root list: %v", got)
	}
	check(t, []want{{[]string{"trust", "root", "add", home}, 1, "internal"}, {[]string{"trust", "grant", home}, 1, "internal"}})
	code, env = machine(t, "trust", "grant", home, repo)
	if err := json.Unmarshal(env.Data, &g); code != 0 || err != nil || len(g.Items) != 2 || g.Items[0].Status != "failed" || g.Items[1].Status != "granted" {
		t.Errorf("a batch with one failure: %d %s", code, env.Data)
	}
	if got := roots("trust", "root", "remove", root); len(got) != 0 {
		t.Fatalf("root remove: %v", got)
	}
	check(t, []want{
		{[]string{"trust", "grant"}, 2, "usage"},
		{[]string{"trust", "grant", "-x"}, 2, "usage"},
		{[]string{"trust", "grant", filepath.Join(home, "missing")}, 2, "invalid_input"},
		{[]string{"trust", "root"}, 2, "usage"},
		{[]string{"trust", "root", "list", "x"}, 2, "usage"},
		{[]string{"trust", "root", "add"}, 2, "usage"},
		{[]string{"trust", "root", "ls"}, 2, "usage"},
		{[]string{"trust", "root", "rm", root}, 2, "usage"},
		{[]string{"trust", "a", "b"}, 2, "usage"},
	})
}

// The managed mail verbs run amq as the caller after the gateway allows
// them: amq's flags that are the adapter's to set are usage, and what the
// gateway refuses is a permission_denied naming its reason.
func TestManagedMailCodes(t *testing.T) {
	bin, err := amq.ResolveBin()
	if err != nil {
		t.Skip(err)
	}
	paths := store.Paths{Home: t.TempDir()}
	env := app.Env{Paths: paths, Store: store.Store{P: paths}, AMQ: amq.CLI{Bin: bin}, Now: time.Now}
	// A delivered send starts its recipient's waker: stop every one this
	// test's root started, so no amq wake outlives it.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(filepath.Join(paths.Root(), "agents"))
		for _, en := range entries {
			_, _ = env.AMQ.RetireHandle(context.Background(), paths.Root(), en.Name())
			if st, err := amq.WakeCheck(context.Background(), bin, paths.Root(), en.Name()); err == nil && st.PID > 0 {
				_ = syscall.Kill(st.PID, syscall.SIGKILL)
			}
		}
	})
	for _, r := range []testAgent{
		{Handle: "codex-01", Server: "unix:///t.sock", BlockID: "block:a", SessionID: "session:a"},
		{Handle: "claude-02", Server: "unix:///t.sock", BlockID: "block:b", SessionID: "session:a"},
		{Handle: "claude-09", Server: "unix:///t.sock", BlockID: "block:c", SessionID: "session:b"},
	} {
		if err := putAgent(env.Store, r); err != nil {
			t.Fatal(err)
		}
		if err := amq.EnsureMailbox(context.Background(), bin, paths.Root(), string(r.Handle)); err != nil {
			t.Fatal(err)
		}
	}
	at := app.Location{Block: "block:a"}
	for _, c := range []struct {
		verb   string
		args   []string
		code   string
		reason string
	}{
		{"send", []string{"--to", "claude-02", "--subject", "s", "--body", "b", "--json"}, "", ""},
		{"send", []string{"--to", "claude-09", "--subject", "s", "--body", "b"}, "permission_denied", "cross_epic"},
		{"send", []string{"--me", "claude-02", "--to", "claude-02", "--body", "b"}, "usage", ""},
		{"drain", []string{"--root", "/tmp/x"}, "usage", ""},
		{"read", nil, "usage", ""},
		{"tap", nil, "usage", ""},
		{"dlq", []string{"list", "--json"}, "", ""},
	} {
		var out bytes.Buffer
		code, err := env.ManagedMail(context.Background(), at, c.verb, c.args, strings.NewReader(""), &out, &out)
		got, reason := "", ""
		if err != nil {
			f := app.Classify(err)
			got = string(f.Code)
			reason, _ = f.Details["reason"].(string)
		} else if code != 0 {
			got = "exit"
		}
		if got != c.code || c.reason != "" && reason != c.reason {
			t.Errorf("%s %v: exit %d %q %q (%v), want %q %q", c.verb, c.args, code, got, reason, err, c.code, c.reason)
		}
	}
	if ids, _ := amq.Unread(paths.Root(), "claude-09"); len(ids) != 0 {
		t.Fatalf("delivered across Epics: %v", ids)
	}
}

// A stream that fails before it connects answers with one envelope, like
// any command; nothing else reaches stdout.
func TestStreamFailsBeforeConnectingWithAnEnvelope(t *testing.T) {
	isolate(t)
	t.Setenv("REX_AMQ_REX", filepath.Join(t.TempDir(), "no-rex"))
	check(t, []want{
		{[]string{"stream", "events", "--session", "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"}, 1, "rex_unavailable"},
		{[]string{"stream", "events", "--all"}, 1, "rex_unavailable"},
		{[]string{"stream", "events"}, 5, "failed_precondition"},
	})
}

// From outside Rex, epic list and epic show are the human's cockpit: they
// name the personal agent, refuse one that is not live, and refuse an
// unattended or agent shell.
func TestEpicListAndShowOutsideRex(t *testing.T) {
	isolate(t)
	stubTools(t)
	s := "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"
	check(t, []want{
		{[]string{"epic", "list"}, 2, "usage"},
		{[]string{"epic", "list", "--personal", "pi-01"}, 3, "permission_denied"},
		{[]string{"epic", "list", "--hub", t.TempDir()}, 3, "permission_denied"},
		{[]string{"epic", "show"}, 2, "usage"},
		{[]string{"epic", "show", s}, 2, "usage"},
	})
	t.Setenv("ME_UNATTENDED", "1")
	check(t, []want{
		{[]string{"epic", "list", "--personal", "pi-01"}, 6, "guard_denied"},
		{[]string{"epic", "show", s}, 6, "guard_denied"},
	})
}

// Every list command takes --limit, --cursor, and --fields, declared once
// by the registry: mail dlq list included.
func TestListCommandsAreBounded(t *testing.T) {
	isolate(t)
	stubTools(t)
	for _, cmd := range registry().Commands {
		if !cmd.List {
			continue
		}
		info := cmd.Info()
		for _, f := range []string{"limit", "cursor", "fields"} {
			if !slices.ContainsFunc(info.Flags, func(fl cli.Flag) bool { return fl.Name == f }) {
				t.Errorf("%s lacks --%s", cmd.Name(), f)
			}
		}
		if info.MaxLimit != 100 {
			t.Errorf("%s: max limit %d", cmd.Name(), info.MaxLimit)
		}
	}
	if c := registry().Find("mail", "dlq", "list"); c == nil || !c.List || c.DefaultLimit != 20 {
		t.Fatalf("mail dlq list is not a bounded list: %+v", c)
	}
	check(t, []want{
		{[]string{"mail", "dlq", "list", "--limit", "101"}, 2, "invalid_input"},
		{[]string{"mail", "dlq", "list", "--limit", "5", "--fields", "id"}, 5, "failed_precondition"},
	})
}
