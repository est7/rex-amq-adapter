package main

import (
	"encoding/json"
	"fmt"
	"io"
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
  'block inspect block:closed --json') exit 3 ;;
  'block inspect '* ) echo '{"session_id":"session:1","block":{"window_id":"window:1"}}' ;;
  'session inspect session:1 --json') echo '{"session_id":"session:1","label":"Project","windows":[{"window_id":"window:1","label":"Work"}]}' ;;
  'block call -b block:other process')
    if [ "$WHO_TEST_MODE" = "other-gone" ]; then exit 3; fi
    echo '{"foreground":{"pid":%d,"name":"pi"}}' ;;
  'block call -b block:closed process') exit 3 ;;
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
		if err := s.PutRecord(store.Record{
			Handle: core.Handle(agent.handle), Kind: core.Pi, Role: agent.role, AgentPID: os.Getpid(),
			BlockID: "block:" + agent.block, SessionID: "session:1", WindowID: "window:" + agent.window, Server: "unix:///" + agent.server + ".sock",
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, r := range []store.Record{
		{Handle: "pi-departed-1", SessionID: "session:1", WindowID: "window:1"},
		{Handle: "pi-departed-2", SessionID: "session:1", WindowID: "window:2"},
		{Handle: "pi-departed-elsewhere", SessionID: "session:2", WindowID: "window:9"},
	} {
		r.Kind, r.BlockID, r.Server, r.DepartedAt = core.Pi, "block:"+string(r.Handle), "unix:///test.sock", &now
		if err := s.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
}

func captureWho(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	errout, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errout.Close()
	beforeOut, beforeErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errout
	defer func() { os.Stdout, os.Stderr = beforeOut, beforeErr }()
	code := runWho(args)
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := errout.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	e, err := io.ReadAll(errout)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(b), string(e)
}

func TestWhoOnlyListsAgentsInCallersSession(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%v", all), func(t *testing.T) {
			whoFixture(t)
			var args []string
			if all {
				args = []string{"--all"}
			}
			code, out, errout := captureWho(t, args...)
			if code != 0 {
				t.Fatalf("who failed: %d %s", code, errout)
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
			// The tab is where the layout places a live agent, by its label
			// (window:2 has none), and where a departed one was recorded.
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

func TestWhoCannotFallBackToGlobalDiscovery(t *testing.T) {
	for _, mode := range []string{"outside-rex", "unavailable", "unknown", "list-failed"} {
		t.Run(mode, func(t *testing.T) {
			whoFixture(t)
			t.Setenv("WHO_TEST_MODE", mode)
			if mode == "outside-rex" {
				t.Setenv("REX_BLOCK", "")
			}
			code, out, _ := captureWho(t)
			if code == 0 || out != "" {
				t.Fatalf("unresolved session must fail without listing agents: code=%d output=%q", code, out)
			}
		})
	}
}

func TestWhoDoesNotExposeCollectionErrorsFromAnotherSession(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park shows on stderr
	whoAgent(t, store.Record{Handle: "pi-away", BlockID: "block:away", SessionID: "session:2", WindowID: "window:9"})
	code, out, errout := captureWho(t)
	if code != 0 || errout != "" {
		t.Fatalf("another session's broken waker affected discovery: code=%d stderr=%q", code, errout)
	}
	if strings.Contains(out, "pi-away") {
		t.Fatalf("another session's agent was exposed: %s", out)
	}
}

// Another tab of the session is the caller's to reconcile: its agents are on
// the same task.
func TestWhoCollectsAnotherTabOfTheSession(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park shows on stderr
	t.Setenv("WHO_TEST_MODE", "other-gone")
	code, _, errout := captureWho(t)
	if code != 0 || !strings.Contains(errout, "pi-other") {
		t.Fatalf("another tab's agent was not reconciled: code=%d stderr=%q", code, errout)
	}
}

func TestWhoStillCollectsAClosedBlockFromThisTab(t *testing.T) {
	whoFixture(t)
	dir := os.Getenv("REX_AMQ_ADAPTER_HOME")
	s := store.Store{P: store.Paths{Home: dir}}
	if err := s.PutRecord(store.Record{
		Handle: "pi-closed", Kind: core.Pi, BlockID: "block:closed",
		Server: "unix:///test.sock", SessionID: "session:1", WindowID: "window:1",
	}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "amq")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"schema\":2,\"wake\":{\"status\":\"missing\",\"live\":false}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_AMQ", bin)
	code, out, errout := captureWho(t, "--all")
	if code != 0 || !slices.Contains(strings.Fields(out), "pi-closed") {
		t.Fatalf("closed block must remain discoverable as departed: code=%d stdout=%s stderr=%s", code, out, errout)
	}
	rec, ok, err := s.Record("block:closed")
	if err != nil || !ok || rec.Live() {
		t.Fatalf("closed block was not collected: %+v %v %v", rec, ok, err)
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
func whoAgent(t *testing.T, r store.Record) {
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
	if err := whoStore().PutRecord(r); err != nil {
		t.Fatal(err)
	}
}

func whoRecord(t *testing.T, handle core.Handle) store.Record {
	t.Helper()
	recs, err := whoStore().Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Handle == handle {
			return r
		}
	}
	t.Fatalf("no record for %s", handle)
	return store.Record{}
}

func TestWhoParksAnAgentWhoseProcessExitedInThisSession(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	whoAgent(t, store.Record{Handle: "pi-mate", BlockID: "block:mate", AgentPID: gone.Process.Pid})
	code, out, errout := captureWho(t, "--all")
	if code != 0 || errout != "" {
		t.Fatalf("who failed: code=%d stderr=%q", code, errout)
	}
	if whoRecord(t, "pi-mate").Live() {
		t.Fatal("an agent whose process exited was not parked")
	}
	if !strings.Contains(out, "departed") {
		t.Fatalf("parked agent is not listed as departed:\n%s", out)
	}
}

func TestWhoLeavesAClosedBlockInAnotherSessionAlone(t *testing.T) {
	whoFixture(t) // amq fails, so an attempt to park shows on stderr
	for _, r := range []store.Record{
		{Handle: "pi-closed", BlockID: "block:closed", SessionID: "session:2", WindowID: "window:9"},
	} {
		whoAgent(t, r)
	}
	code, _, errout := captureWho(t)
	if code != 0 || strings.Contains(errout, "pi-closed") {
		t.Fatalf("another session's closed block was collected: code=%d stderr=%q", code, errout)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("another session's agent was parked")
	}
}

func TestWhoLeavesAClosedBlockOnAnotherServerAlone(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	whoAgent(t, store.Record{Handle: "pi-closed", BlockID: "block:closed", Server: "unix:///elsewhere.sock"})
	if code, _, errout := captureWho(t); code != 0 || errout != "" {
		t.Fatalf("who failed: code=%d stderr=%q", code, errout)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("another server's agent was parked")
	}
}

// An agent moved to another tab of the session stays this session's: who
// reconciles it, and records where it is now.
func TestWhoFollowsAnAgentMovedToAnotherTab(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	whoAgent(t, store.Record{Handle: "pi-moved-out", BlockID: "block:moved-out", AgentPID: gone.Process.Pid})
	if code, _, errout := captureWho(t); code != 0 || errout != "" {
		t.Fatalf("who failed: code=%d stderr=%q", code, errout)
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
func TestWhoLeavesAnAgentMovedToAnotherSessionAlone(t *testing.T) {
	whoFixture(t)
	whoWorkingAmq(t)
	whoAgent(t, store.Record{Handle: "pi-moved-away", BlockID: "block:elsewhere", AgentPID: os.Getpid()})
	code, out, errout := captureWho(t, "--all")
	if code != 0 || errout != "" {
		t.Fatalf("who failed: code=%d stderr=%q", code, errout)
	}
	if !whoRecord(t, "pi-moved-away").Live() {
		t.Fatal("a live agent moved to another session was parked")
	}
	if strings.Contains(out, "pi-moved-away") {
		t.Fatalf("an agent in another session was listed:\n%s", out)
	}
}

func TestWhoListsThisSessionWhenRetiringAWakerFails(t *testing.T) {
	whoFixture(t) // amq fails
	whoAgent(t, store.Record{Handle: "pi-closed", BlockID: "block:closed"})
	code, out, errout := captureWho(t)
	if code != 0 {
		t.Fatalf("a failed retire cost the listing: code=%d stderr=%q", code, errout)
	}
	if !slices.Contains(strings.Fields(out), "pi-mate") {
		t.Fatalf("this session's agents were not listed:\n%s", out)
	}
	if !strings.Contains(errout, "pi-closed") {
		t.Fatalf("the failure was not reported: stderr=%q", errout)
	}
	if !whoRecord(t, "pi-closed").Live() {
		t.Fatal("an agent whose waker survived was parked")
	}
}

func TestWhoReconcilesEveryRecordPastAFailure(t *testing.T) {
	whoFixture(t) // amq fails
	whoAgent(t, store.Record{Handle: "pi-closed-1", BlockID: "block:closed"})
	whoAgent(t, store.Record{Handle: "pi-closed-2", BlockID: "block:closed"})
	_, _, errout := captureWho(t)
	for _, handle := range []string{"pi-closed-1", "pi-closed-2"} {
		if !strings.Contains(errout, handle) {
			t.Errorf("collection stopped before %s: stderr=%q", handle, errout)
		}
	}
}

func TestWhoListsThisSessionWhenTheLifecycleLockCannotBeTaken(t *testing.T) {
	whoFixture(t)
	lock := filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "lifecycle.lock")
	if err := os.WriteFile(lock, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	code, out, errout := captureWho(t)
	if code != 0 {
		t.Fatalf("an unusable lock cost the listing: code=%d stderr=%q", code, errout)
	}
	got := strings.Fields(out)
	if !slices.Contains(got, "pi-mate") || !slices.Contains(got, "pi-other") || slices.Contains(got, "pi-other-server") {
		t.Fatalf("listing is not this session's:\n%s", out)
	}
	if !strings.Contains(errout, "lifecycle.lock") {
		t.Fatalf("the failure was not reported: stderr=%q", errout)
	}
}

// who --json lists the same agents as the table, as core.Agent objects (the
// shape events' snapshot uses), so an agent reads roles and tabs without
// parsing columns.
func TestWhoJSONListsTheSameAgentsAsTheTable(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%v", all), func(t *testing.T) {
			whoFixture(t)
			args := []string{"--json"}
			if all {
				args = append(args, "--all")
			}
			code, out, errout := captureWho(t, args...)
			if code != 0 {
				t.Fatalf("who --json: %d %s", code, errout)
			}
			var got struct {
				Agents []core.Agent `json:"agents"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			byHandle := map[core.Handle]core.Agent{}
			for _, a := range got.Agents {
				byHandle[a.Handle] = a
			}
			want := []core.Handle{"pi-self", "pi-mate", "pi-moved-in", "pi-other", "pi-moved-out"}
			if all {
				want = append(want, "pi-departed-1", "pi-departed-2")
			}
			if len(got.Agents) != len(want) {
				t.Errorf("%d agents, want %d: %+v", len(got.Agents), len(want), got.Agents)
			}
			for _, h := range want {
				if _, ok := byHandle[h]; !ok {
					t.Errorf("missing %s", h)
				}
			}
			if m := byHandle["pi-mate"]; m.Role != "reviewer" || m.Tab != "Work" || m.Lifecycle.State != core.LifeLive {
				t.Errorf("pi-mate: %+v", m)
			}
		})
	}
}
