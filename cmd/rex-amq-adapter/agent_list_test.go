package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// clearHarnessSessions keeps the session of the agent running these tests,
// which its harness set in this environment, from deciding who the caller is.
func clearHarnessSessions(t *testing.T) {
	t.Helper()
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
		t.Setenv(v, "")
	}
}

// Exercise the public command with two tabs in one session, and records of
// another session. Stored windows deliberately lag the layout for agents moved
// between the tabs.
func whoFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	clearHarnessSessions(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := filepath.Join(dir, "rex")
	script := fmt.Sprintf(`#!/bin/sh
shift
if [ "$1" = "-S" ]; then shift 2; fi
case "$*" in
  'whoami --json') echo '{"instance_id":"test","endpoint":"unix:///test.sock"}' ;;
  'block inspect block:self --json')
    case "$WHO_TEST_MODE" in
      unavailable) exit 4 ;;
      unknown) echo '{"session_id":"session:1","block":{"block_id":"block:self"}}'; exit 0 ;;
    esac
    echo '{"session_id":"session:1","block":{"block_id":"block:self","window_id":"window:1"}}' ;;
  'block ls -s session:1 --json')
    if [ "$WHO_TEST_MODE" = "list-failed" ]; then exit 4; fi
    echo '{"blocks":[{"block_id":"block:self","window_id":"window:1"},{"block_id":"block:mate","window_id":"window:1"},{"block_id":"block:moved-in","window_id":"window:1"},{"block_id":"block:other","window_id":"window:2"},{"block_id":"block:moved-out","window_id":"window:2"}]}' ;;
  'block inspect block:closed --json'|'block inspect block:closed-2 --json') exit 3 ;;
  'block inspect '* ) echo '{"session_id":"session:1","block":{"window_id":"window:1"}}' ;;
  'session inspect session:1 --json') echo '{"session_id":"session:1","label":"Project","windows":[{"window_id":"window:1","label":"Work"}]}' ;;
  'block call -b block:other process')
    if [ "$WHO_TEST_MODE" = "other-gone" ]; then exit 3; fi
    echo '{"foreground":{"pid":%d,"name":"pi"}}' ;;
  'block call -b block:closed process'|'block call -b block:closed-2 process') exit 3 ;;
  'block call -b block:away process') exit 3 ;;
  'block call '* ) echo '{"foreground":{"pid":%d,"name":"pi"}}' ;;
  *) echo "unexpected Rex command: $*" >&2; exit 1 ;;
esac
`, os.Getpid(), os.Getpid())
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", bin)
	t.Setenv("REX_AMQ_AMQ", "/usr/bin/false") // discovery must not need an AMQ subprocess
	t.Setenv("REX_AMQ_ADAPTER_HOME", dir)
	t.Setenv("REX_SERVER", "unix:///test.sock")
	t.Setenv("REX_SESSION", "session:stale") // use the block's current location
	t.Setenv("REX_BLOCK", "block:self")
	t.Setenv("WHO_TEST_MODE", "")
	s := store.Store{P: store.Paths{Home: dir}}
	for _, agent := range []struct{ handle, block, window, server, role string }{
		{"pi-self", "self", "1", "test", ""},
		{"pi-mate", "mate", "1", "test", "reviewer"},
		{"pi-moved-in", "moved-in", "2", "test", ""},
		{"pi-other", "other", "2", "test", ""},
		{"pi-moved-out", "moved-out", "1", "test", ""},
		{"pi-other-server", "mate", "1", "elsewhere", ""},
	} {
		if err := putAgent(s, testAgent{
			Handle: core.Handle(agent.handle), Kind: core.Pi, Role: agent.role, AgentPID: os.Getpid(),
			BlockID: "block:" + agent.block, SessionID: "session:1", WindowID: "window:" + agent.window, Server: "unix:///" + agent.server + ".sock",
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, r := range []testAgent{
		{Handle: "pi-departed-1", SessionID: "session:1", WindowID: "window:1"},
		{Handle: "pi-departed-2", SessionID: "session:1", WindowID: "window:2"},
		{Handle: "pi-departed-elsewhere", SessionID: "session:2", WindowID: "window:9"},
	} {
		r.Kind, r.BlockID, r.Server, r.DepartedAt = core.Pi, "block:"+string(r.Handle), "unix:///test.sock", &now
		if err := putAgent(s, r); err != nil {
			t.Fatal(err)
		}
	}
}

// agentText runs an agent verb in text mode (--text): the table on stdout,
// diagnostics on stderr.
func agentText(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	r := invoke(t, "", append(args, "--text")...)
	return r.code, r.stdout, r.stderr
}

// collect runs `agent collect` in machine mode and returns its exit status
// and its warnings joined: what reconciliation could not settle.
func collect(t *testing.T) (int, string) {
	t.Helper()
	code, env := machine(t, "agent", "collect")
	return code, strings.Join(env.Warnings, "\n")
}

// agent list shows the caller's Rex session only, both tabs of it, and
// with --all its departed agents too; the text table has ROLE and TAB.
func TestAgentListOnlyListsAgentsInCallersSession(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%v", all), func(t *testing.T) {
			whoFixture(t)
			args := []string{"agent", "list"}
			if all {
				args = append(args, "--all")
			}
			code, out, errout := agentText(t, args...)
			if code != 0 {
				t.Fatalf("agent list failed: %d %s", code, errout)
			}
			got := strings.Fields(out)
			want := []string{"pi-self", "pi-mate", "pi-moved-in", "pi-other", "pi-moved-out"}
			departed := []string{"pi-departed-1", "pi-departed-2"}
			if all {
				want = append(want, departed...)
			}
			for _, handle := range want {
				if !slices.Contains(got, handle) {
					t.Errorf("missing agent of this session %s:\n%s", handle, out)
				}
			}
			for _, handle := range []string{"pi-other-server", "pi-departed-elsewhere"} {
				if slices.Contains(got, handle) {
					t.Errorf("exposed agent outside this session %s:\n%s", handle, out)
				}
			}
			for _, handle := range departed {
				if !all && slices.Contains(got, handle) {
					t.Errorf("departed agent %s appeared without --all:\n%s", handle, out)
				}
			}
			rows := map[string][]string{}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				f := strings.Fields(line)
				rows[f[0]] = f
			}
			if h := rows["HANDLE"]; len(h) < 4 || h[2] != "ROLE" || h[3] != "TAB" {
				t.Errorf("no ROLE and TAB columns: %v", h)
			}
			if r := rows["pi-mate"]; len(r) < 3 || r[2] != "reviewer" || rows["pi-self"][2] != "-" {
				t.Errorf("roles: pi-mate %v, pi-self %v", r, rows["pi-self"])
			}
			tabs := map[string]string{"pi-self": "Work", "pi-moved-in": "Work", "pi-other": "window:2", "pi-moved-out": "window:2"}
			if all {
				tabs["pi-departed-1"], tabs["pi-departed-2"] = "Work", "window:2"
			}
			for handle, tab := range tabs {
				if r := rows[handle]; len(r) < 4 || r[3] != tab {
					t.Errorf("%s: row %v, want tab %s", handle, r, tab)
				}
			}
		})
	}
}

// Without its session resolved, agent list fails and lists nothing: it
// never falls back to every record.
func TestAgentListCannotFallBackToGlobalDiscovery(t *testing.T) {
	for _, mode := range []string{"outside-rex", "unavailable", "unknown", "list-failed"} {
		t.Run(mode, func(t *testing.T) {
			whoFixture(t)
			t.Setenv("WHO_TEST_MODE", mode)
			if mode == "outside-rex" {
				t.Setenv("REX_BLOCK", "")
			}
			code, env := machine(t, "agent", "list")
			if code == 0 || env.OK || string(env.Data) != "null" {
				t.Fatalf("unresolved session must fail without listing agents: code=%d data=%s", code, env.Data)
			}
		})
	}
}

// snapshot is every file under dir with its contents.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			files[p+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// agent list is a pure read (CQS): over a session whose agents a
// collection would park (a process that exited, a closed block) and with
// amq set to fail any waker repair, it changes no byte of the state
// directory, warns of nothing, and still shows the stale agents live;
// `agent collect` is what reconciles them.
func TestAgentListPure(t *testing.T) {
	whoFixture(t)
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	whoAgent(t, testAgent{Handle: "pi-mate", BlockID: "block:mate", AgentPID: gone.Process.Pid})
	whoAgent(t, testAgent{Handle: "pi-closed", BlockID: "block:closed"})
	marker := filepath.Join(t.TempDir(), "amq-ran")
	amq := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(amq, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_AMQ", amq)
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	before := snapshot(t, home)
	for _, args := range [][]string{{"agent", "list"}, {"agent", "list", "--all"}, {"agent", "list", "--fields", "handle,lifecycle,delivery"}} {
		code, env := machine(t, args...)
		if code != 0 || len(env.Warnings) != 0 {
			t.Fatalf("%v: exit %d, warnings %v", args, code, env.Warnings)
		}
	}
	if after := snapshot(t, home); !maps.Equal(before, after) {
		for k := range after {
			if before[k] != after[k] {
				t.Errorf("agent list changed %s", k)
			}
		}
		for k := range before {
			if _, ok := after[k]; !ok {
				t.Errorf("agent list removed %s", k)
			}
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("agent list ran amq (a waker check or repair)")
	}
	if !whoRecord(t, "pi-mate").Live() || !whoRecord(t, "pi-closed").Live() {
		t.Fatal("agent list parked an agent")
	}
	// The same state is what agent collect reconciles.
	whoWorkingAmq(t)
	if code, warnings := collect(t); code != 0 {
		t.Fatalf("agent collect: exit %d, warnings %q", code, warnings)
	}
	if whoRecord(t, "pi-mate").Live() {
		t.Error("agent collect did not park an agent whose process exited")
	}
}

// agent list's default fields are handle, kind, role, tab, state, and
// unread; --fields takes any core.Agent field, and the page says whether
// it is complete.
func TestAgentListFieldsAndPage(t *testing.T) {
	whoFixture(t)
	code, env := machine(t, "agent", "list", "--all")
	if code != 0 {
		t.Fatalf("exit %d %+v", code, env.Error)
	}
	var page struct {
		Items     []map[string]any `json:"items"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 7 || len(page.Items) != 7 || page.Truncated {
		t.Fatalf("page %d/%d truncated=%v", len(page.Items), page.Total, page.Truncated)
	}
	for _, it := range page.Items {
		keys := slices.Sorted(maps.Keys(it))
		if !slices.Equal(keys, []string{"handle", "kind", "role", "state", "tab", "unread"}) {
			t.Fatalf("default fields %v", keys)
		}
	}
	code, env = machine(t, "agent", "list", "--fields", "handle,role,tab,lifecycle", "--limit", "2")
	if code != 0 {
		t.Fatalf("exit %d %+v", code, env.Error)
	}
	var proj struct {
		Items      []core.Agent `json:"items"`
		Truncated  bool         `json:"truncated"`
		NextCursor string       `json:"next_cursor"`
	}
	if err := json.Unmarshal(env.Data, &proj); err != nil {
		t.Fatal(err)
	}
	if len(proj.Items) != 2 || !proj.Truncated || proj.NextCursor == "" {
		t.Fatalf("page of 2: %+v", proj)
	}
	for _, a := range proj.Items {
		if a.Handle == "pi-mate" && (a.Role != "reviewer" || a.Tab != "Work" || a.Lifecycle.State != core.LifeLive) {
			t.Errorf("pi-mate: %+v", a)
		}
	}
	if code, env := machine(t, "agent", "list", "--cursor", proj.NextCursor, "--all"); code != 2 || env.code() != "invalid_input" {
		t.Errorf("a cursor of another listing: exit %d %s", code, env.code())
	}
	if code, env := machine(t, "agent", "list", "--fields", "nonesuch"); code != 2 || env.code() != "invalid_input" {
		t.Errorf("an unknown field: exit %d %s", code, env.code())
	}
}

// Reconciliation reports what it could not settle in band and never puts
// another session's records in the caller's view.
func TestAgentCollectDoesNotExposeAnotherSession(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park is a warning
	whoAgent(t, testAgent{Handle: "pi-away", BlockID: "block:away", SessionID: "session:2", WindowID: "window:9"})
	if code, warnings := collect(t); code != 0 || strings.Contains(warnings, "pi-away") {
		t.Fatalf("another session's broken waker affected collection: code=%d warnings=%q", code, warnings)
	}
}

// Another tab of the session is the caller's to reconcile: its agents are on
// the same task.
func TestAgentCollectReconcilesAnotherTabOfTheSession(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park is a warning
	t.Setenv("WHO_TEST_MODE", "other-gone")
	if code, warnings := collect(t); code != 0 || !strings.Contains(warnings, "pi-other") {
		t.Fatalf("another tab's agent was not reconciled: code=%d warnings=%q", code, warnings)
	}
}

func TestAgentCollectParksAClosedBlockOfThisTab(t *testing.T) {
	whoFixture(t)
	s := whoStore()
	if err := putAgent(s, testAgent{
		Handle: "pi-closed", Kind: core.Pi, BlockID: "block:closed",
		Server: "unix:///test.sock", SessionID: "session:1", WindowID: "window:1",
	}); err != nil {
		t.Fatal(err)
	}
	whoWorkingAmq(t)
	if code, warnings := collect(t); code != 0 {
		t.Fatalf("collect: %d %q", code, warnings)
	}
	code, out, errout := agentText(t, "agent", "list", "--all")
	if code != 0 || !slices.Contains(strings.Fields(out), "pi-closed") {
		t.Fatalf("a closed block must remain discoverable as departed: code=%d stdout=%s stderr=%s", code, out, errout)
	}
	p, live, err := placed(s, "pi-closed")
	if err != nil || live || p.BlockID != "block:closed" {
		t.Fatalf("closed block was not collected: %+v live=%v %v", p, live, err)
	}
}

// whoWorkingAmq lets a park succeed, for tests that are not about its failure.
func whoWorkingAmq(t *testing.T) {
	t.Helper()
	bin := filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "amq")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"schema\":2,\"wake\":{\"status\":\"missing\",\"live\":false}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_AMQ", bin)
}

func whoStore() store.Store {
	return store.Store{P: store.Paths{Home: os.Getenv("REX_AMQ_ADAPTER_HOME")}}
}

// whoAgent records a live agent on the fixture's server, recorded in
// session:1, window:1 unless the caller changes it.
func whoAgent(t *testing.T, r testAgent) {
	t.Helper()
	r.Kind = core.Pi
	if r.Server == "" {
		r.Server = "unix:///test.sock"
	}
	if r.WindowID == "" {
		r.WindowID = "window:1"
	}
	if r.SessionID == "" {
		r.SessionID = "session:1"
	}
	if err := putAgent(whoStore(), r); err != nil {
		t.Fatal(err)
	}
}

// whoState is where an agent runs or last ran, and whether it runs now.
type whoState struct {
	store.Place
	live bool
}

func (w whoState) Live() bool { return w.live }

func whoRecord(t *testing.T, handle core.Handle) whoState {
	t.Helper()
	if _, ok, err := whoStore().IdentityOf(handle); err != nil || !ok {
		t.Fatalf("no identity for %s: %v", handle, err)
	}
	p, live, err := placed(whoStore(), handle)
	if err != nil {
		t.Fatal(err)
	}
	return whoState{Place: p, live: live}
}

func TestAgentCollectParksAnAgentWhoseProcessExitedInThisSession(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	whoAgent(t, testAgent{Handle: "pi-mate", BlockID: "block:mate", AgentPID: gone.Process.Pid})
	if code, warnings := collect(t); code != 0 || warnings != "" {
		t.Fatalf("collect failed: code=%d warnings=%q", code, warnings)
	}
	if whoRecord(t, "pi-mate").Live() {
		t.Fatal("an agent whose process exited was not parked")
	}
	_, out, _ := agentText(t, "agent", "list", "--all")
	if !strings.Contains(out, "departed") {
		t.Fatalf("parked agent is not listed as departed:\n%s", out)
	}
}

func TestAgentCollectLeavesAClosedBlockInAnotherSessionAlone(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park shows on stderr
	for _, r := range []testAgent{
		{Handle: "pi-closed", BlockID: "block:closed", SessionID: "session:2", WindowID: "window:9"},
	} {
		whoAgent(t, r)
	}
	if code, warnings := collect(t); code != 0 || strings.Contains(warnings, "pi-closed") {
		t.Fatalf("another session's closed block was collected: code=%d warnings=%q", code, warnings)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("another session's agent was parked")
	}
}

func TestAgentCollectLeavesAClosedBlockOnAnotherServerAlone(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	whoAgent(t, testAgent{Handle: "pi-closed", BlockID: "block:closed", Server: "unix:///elsewhere.sock"})
	if code, warnings := collect(t); code != 0 || warnings != "" {
		t.Fatalf("collect failed: code=%d warnings=%q", code, warnings)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("another server's agent was parked")
	}
}

// An agent moved to another tab of the session stays this session's: who
// reconciles it, and records where it is now.
func TestAgentCollectFollowsAnAgentMovedToAnotherTab(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	whoAgent(t, testAgent{Handle: "pi-moved-out", BlockID: "block:moved-out", AgentPID: gone.Process.Pid})
	if code, warnings := collect(t); code != 0 || warnings != "" {
		t.Fatalf("collect failed: code=%d warnings=%q", code, warnings)
	}
	r := whoRecord(t, "pi-moved-out")
	if r.Live() {
		t.Error("an agent of this session whose process exited was not parked")
	}
	if r.WindowID != "window:2" || r.SessionID != "session:1" {
		t.Errorf("the record does not follow the move: window %q, session %q", r.WindowID, r.SessionID)
	}
}

// A block absent from this session's layout may have moved to another session,
// where it is alive. Only its absence from Rex proves it gone.
func TestAgentCollectLeavesAnAgentMovedToAnotherSessionAlone(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	whoAgent(t, testAgent{Handle: "pi-moved-away", BlockID: "block:elsewhere", AgentPID: os.Getpid()})
	if code, warnings := collect(t); code != 0 || warnings != "" {
		t.Fatalf("collect failed: code=%d warnings=%q", code, warnings)
	}
	_, out, _ := agentText(t, "agent", "list", "--all")
	if !whoRecord(t, "pi-moved-away").Live() {
		t.Fatal("a live agent moved to another session was parked")
	}
	if strings.Contains(out, "pi-moved-away") {
		t.Fatalf("an agent in another session was listed:\n%s", out)
	}
}

func TestAgentCollectReportsAFailedWakerRetire(t *testing.T) {
	whoFixture(t) // amq fails
	whoAgent(t, testAgent{Handle: "pi-closed", BlockID: "block:closed"})
	code, warnings := collect(t)
	if code != 0 {
		t.Fatalf("a failed retire failed the collection: code=%d warnings=%q", code, warnings)
	}
	if !strings.Contains(warnings, "pi-closed") {
		t.Fatalf("the failure was not reported: warnings=%q", warnings)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("an agent whose waker survived was parked")
	}
}

func TestAgentCollectReconcilesEveryRecordPastAFailure(t *testing.T) {
	whoFixture(t) // amq fails
	whoAgent(t, testAgent{Handle: "pi-closed-1", BlockID: "block:closed"})
	whoAgent(t, testAgent{Handle: "pi-closed-2", BlockID: "block:closed-2"}) // one live agent per block (store I3)
	_, warnings := collect(t)
	for _, handle := range []string{"pi-closed-1", "pi-closed-2"} {
		if !strings.Contains(warnings, handle) {
			t.Errorf("collection stopped before %s: warnings=%q", handle, warnings)
		}
	}
}

func TestAgentCollectReportsAnUnusableLifecycleLock(t *testing.T) {
	whoFixture(t)
	lock := filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "lifecycle.lock")
	if err := os.WriteFile(lock, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	if code, warnings := collect(t); code != 0 || !strings.Contains(warnings, "lifecycle.lock") {
		t.Fatalf("the failure was not reported: code=%d warnings=%q", code, warnings)
	}
	code, out, errout := agentText(t, "agent", "list")
	got := strings.Fields(out)
	if code != 0 || !slices.Contains(got, "pi-mate") || !slices.Contains(got, "pi-other") || slices.Contains(got, "pi-other-server") {
		t.Fatalf("listing is not this session's: %d %s\n%s", code, errout, out)
	}
}
