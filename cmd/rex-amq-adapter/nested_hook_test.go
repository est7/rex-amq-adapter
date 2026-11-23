package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/out"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// F14 through the public command: a Codex hook whose process descends from
// the pane's live Claude agent (here the process running this test's
// binary, by its real ancestry) is a `codex exec` that agent's tool shell
// started. Every hook of it exits 0, asks Rex nothing, and leaves the
// agent's record as it was.
func TestNestedHarnessHookExitsZeroAndLeavesTheOwner(t *testing.T) {
	dir := t.TempDir()
	clearHarnessSessions(t)
	calls := filepath.Join(dir, "rex.calls")
	rexBin := filepath.Join(dir, "rex")
	if err := os.WriteFile(rexBin, []byte("#!/bin/sh\necho \"$*\" >>'"+calls+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", rexBin)
	t.Setenv("REX_AMQ_AMQ", "/usr/bin/false")
	t.Setenv("REX_AMQ_ADAPTER_HOME", dir)
	t.Setenv("REX_SERVER", "unix:///test.sock")
	t.Setenv("REX_SESSION", "session:1")
	t.Setenv("REX_BLOCK", "block:pane")
	t.Cleanup(func() { app.Diagnostics = os.Stderr })

	s := store.Store{P: store.Paths{Home: dir}}
	now := time.Now().UTC()
	owner := testAgent{Handle: "claude-07", Kind: core.Claude, Role: core.OrchestratorRole, Server: "unix:///test.sock", BlockID: "block:pane",
		SessionID: "session:1", WindowID: "window:1", AgentPID: os.Getppid(), AgentSessionID: "owner", AdoptedAt: now, FirstAdoptedAt: now}
	if err := putAgent(s, owner); err != nil {
		t.Fatal(err)
	}
	snapshot := func() ([]store.Identity, []store.Lease) {
		ids, _ := s.Identities()
		leases, _ := s.Leases()
		return ids, leases
	}
	beforeIDs, beforeLeases := snapshot()
	for _, event := range []string{"session-start", "post-tool-use", "stop", "session-end"} {
		stdin, err := os.CreateTemp(dir, "stdin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.WriteString(`{"session_id":"nested","transcript_path":"/tmp/nested.jsonl"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		var reply bytes.Buffer
		code := runHook([]string{event, "--agent", "codex"}, out.Streams{Stdout: &reply, Stderr: &reply}, os.Getenv, stdin)
		stdin.Close()
		if code != 0 {
			log, _ := os.ReadFile(filepath.Join(dir, "logs", "hooks.log"))
			t.Errorf("hook %s exited %d, want 0\n%s", event, code, log)
		}
	}
	if ids, leases := snapshot(); !reflect.DeepEqual(ids, beforeIDs) || !reflect.DeepEqual(leases, beforeLeases) || len(ids) != 1 || len(leases) != 1 {
		t.Errorf("the owner's identity or lease changed, or another appeared:\nbefore %+v %+v\nafter  %+v %+v", beforeIDs, beforeLeases, ids, leases)
	}
	if b, err := os.ReadFile(calls); err == nil && strings.TrimSpace(string(b)) != "" {
		t.Errorf("the nested hook asked Rex:\n%s", b)
	}
}
