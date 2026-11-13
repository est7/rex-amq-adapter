package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func TestCodexBackgroundSessionCannotRetireTerminalIdentity(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "terminal-session", Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{SessionID: "terminal-session"}) })
	before, _, _ := e.Store.Record(block)
	// An internal Codex thread inherits the same Rex block and foreground PID.
	// Its lifecycle must not replace or retire the terminal's session binding.
	child := HookInput{SessionID: "memory-consolidation", Source: "startup", TranscriptPath: []byte("null")}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, child); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(ctx, EventSessionEnd, core.Codex, loc, child); err != nil {
		t.Fatal(err)
	}
	after, ok, err := e.Store.Record(block)
	if err != nil || !ok || !after.Live() || after.AgentSessionID != before.AgentSessionID || after.AgentPID != before.AgentPID || !after.AdoptedAt.Equal(before.AdoptedAt) {
		t.Fatalf("background lifecycle changed terminal identity: before=%+v after=%+v ok=%v err=%v", before, after, ok, err)
	}
	if exports, err := e.Exports(loc); err != nil || !strings.Contains(exports, "AM_ME='codex-01'") {
		t.Fatalf("terminal lost AMQ identity after background shutdown: %q %v", exports, err)
	}
	if !wakerLive(t, e, "codex-01") {
		t.Fatal("background shutdown retired the terminal waker")
	}
}

func TestForeignCodexHookDoesNotAnnounceTerminalMail(t *testing.T) {
	e := fixture(t, &fakeTerm{state: "working"}, "m1")
	rec, _, _ := e.Store.Record(block)
	rec.AgentSessionID = "terminal-session"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := Location{Block: block}
	r, err := e.Hook(ctx, EventPostTool, core.Codex, loc, HookInput{SessionID: "memory-consolidation", TranscriptPath: []byte("null")})
	if err != nil || r.Notice != "" {
		t.Fatalf("foreign thread received terminal mail: %+v %v", r, err)
	}
	r, err = e.Hook(ctx, EventPostTool, core.Codex, loc, HookInput{SessionID: "terminal-session"})
	if err != nil || !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("terminal mail was consumed by a foreign hook: %+v %v", r, err)
	}
}

func TestCodexBackgroundFirstDoesNotClaimPane(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	ctx := context.Background()
	loc := Location{Block: block}
	child := HookInput{SessionID: "memory-consolidation", Source: "fork", TranscriptPath: []byte("null")}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, child); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.Store.Record(block); err != nil || ok {
		t.Fatalf("ephemeral background start claimed pane: exists=%v err=%v", ok, err)
	}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "terminal-session", Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if _, err := e.Hook(ctx, EventSessionEnd, core.Codex, loc, child); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.Record(block)
	if !rec.Live() || rec.AgentSessionID != "terminal-session" {
		t.Fatalf("background-first lifecycle retired terminal: %+v", rec)
	}
}

func TestCodexMissedInteractiveStartStillAnnouncesMail(t *testing.T) {
	e := fixture(t, &fakeTerm{state: "working"}, "m1")
	rec, _, _ := e.Store.Record(block)
	rec.AgentSessionID = "before-resume"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventPostTool, core.Codex, Location{Block: block}, HookInput{SessionID: "after-resume", TranscriptPath: []byte(`"/tmp/interactive.jsonl"`)})
	if err != nil || !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("missed startup silenced interactive mail: %+v %v", r, err)
	}
}

func TestCodexTranscriptFieldDistinguishesNullFromOmitted(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantNotice    bool
	}{
		{"null", `{"session_id":"worker","transcript_path":null}`, false},
		{"omitted", `{"session_id":"terminal"}`, true},
		{"path", `{"session_id":"terminal","transcript_path":"/tmp/terminal.jsonl"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := fixture(t, &fakeTerm{state: "working"}, "m1")
			in, err := ReadHookInput(strings.NewReader(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			r, err := e.Hook(context.Background(), EventPostTool, core.Codex, Location{Block: block}, in)
			if err != nil || strings.Contains(r.Notice, "1 unread") != tc.wantNotice {
				t.Fatalf("hook notice=%q wantNotice=%v err=%v", r.Notice, tc.wantNotice, err)
			}
		})
	}
}

func TestCodexInteractiveSessionChangesRemainAdoptable(t *testing.T) {
	for _, source := range []string{"startup", "resume", "clear", "fork"} {
		t.Run(source, func(t *testing.T) {
			e := realAmqEnv(t, &fakeTerm{state: "working"})
			ctx := context.Background()
			loc := Location{Block: block}
			if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "first", Source: "startup"}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
			if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "next", Source: source}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Hook(ctx, EventSessionEnd, core.Codex, loc, HookInput{SessionID: "first"}); err != nil {
				t.Fatal(err)
			}
			rec, _, _ := e.Store.Record(block)
			if !rec.Live() || rec.AgentSessionID != "next" {
				t.Fatalf("interactive %s was not preserved: %+v", source, rec)
			}
		})
	}
}

func TestCodexNewProcessCanReplaceSessionInSamePane(t *testing.T) {
	term := &fakeTerm{state: "working"}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	// A previous PID that has exited, with a replacement CLI in the foreground.
	if _, err := e.Adopt(ctx, loc, core.Codex, HookInput{SessionID: "previous", AgentPID: 0x3ffffff0}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "replacement", Source: "startup"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.Record(block)
	if !rec.Live() || rec.AgentSessionID != "replacement" || rec.AgentPID != os.Getpid() {
		t.Fatalf("replacement CLI did not acquire its identity: %+v", rec)
	}
}

// A failed adoption (two agents starting at once can run SessionStart out of
// time) holds back only the hooks that run on every tool call. A hook that
// runs once a turn retries at once: an agent whose first turn ends before the
// back-off would otherwise idle with no mailbox.
func TestOncePerTurnHooksRetryAFailedAdoption(t *testing.T) {
	term := &fakeTerm{state: "idle", gone: true}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1", TranscriptPath: []byte(`"/tmp/t.jsonl"`)}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, in); err == nil {
		t.Fatal("setup: adoption should fail while the block cannot be read")
	}
	term.mu.Lock()
	term.gone = false
	term.mu.Unlock()
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })

	if _, err := e.Hook(ctx, EventPostTool, core.Codex, loc, in); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Store.Record(block); ok {
		t.Fatal("a tool call retried the failed adoption inside the back-off")
	}
	r, err := e.Hook(ctx, EventPrompt, core.Codex, loc, in)
	if rec, ok, _ := e.Store.Record(block); err != nil || !ok || !rec.Live() || !strings.Contains(r.Notice, "you are codex-01") {
		t.Fatalf("the next prompt must adopt: %+v %v %q", rec, err, r.Notice)
	}
}

func TestTurnEndRetriesAFailedAdoption(t *testing.T) {
	term := &fakeTerm{state: "idle", gone: true}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	loc := Location{Block: block}
	in := HookInput{SessionID: "s1", TranscriptPath: []byte(`"/tmp/t.jsonl"`)}
	_, _ = e.Hook(ctx, EventSessionStart, core.Codex, loc, in)
	term.mu.Lock()
	term.gone = false
	term.mu.Unlock()
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if _, err := e.Hook(ctx, EventStop, core.Codex, loc, in); err != nil {
		t.Fatal(err)
	}
	if rec, ok, _ := e.Store.Record(block); !ok || !rec.Live() {
		t.Fatal("the end of the first turn must adopt an agent whose start failed")
	}
}

// Codex without --no-daemon runs every pane's threads in one shared daemon,
// which the first pane's Codex started: the daemon descends from that pane's
// agent and inherited its REX_BLOCK. A thread of another pane fires its hooks
// from there, so they name the first pane's block and descend from its agent.
func sharedDaemon(agent int) func() ([]proc, error) {
	return func() ([]proc, error) {
		return []proc{{0x3ffffff1, "rex-amq-adapter"}, {0x3ffffff2, "zsh"}, {0x3ffffff3, "codex"}, {agent, "codex"}}, nil
	}
}

func TestSharedCodexDaemonThreadDoesNotTakeThePanesIdentity(t *testing.T) {
	e := fixture(t, &fakeTerm{state: "working"}, "m1")
	rec, _, _ := e.Store.Record(block)
	rec.AgentSessionID = "pane-a"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	e.Ancestors = sharedDaemon(rec.AgentPID)
	ctx := context.Background()
	loc := Location{Block: block}
	paneB := HookInput{SessionID: "pane-b", TranscriptPath: []byte(`"/tmp/pane-b.jsonl"`)}
	for _, event := range []string{EventSessionStart, EventPrompt, EventPostTool} {
		r, _ := e.Hook(ctx, event, core.Codex, loc, paneB)
		if r.Notice != "" {
			t.Errorf("%s: another pane's thread was told of this pane's mail: %q", event, r.Notice)
		}
	}
	after, _, _ := e.Store.Record(block)
	if after.Handle != rec.Handle || after.AgentSessionID != "pane-a" || after.AgentPID != rec.AgentPID || !after.Live() {
		t.Errorf("another pane's thread took the record: before %+v, after %+v", rec, after)
	}
	if ledger, err := e.Store.Announced(rec.Handle); err != nil || ledger.IDs["m1"] {
		t.Errorf("another pane's thread marked the mail announced (err %v)", err)
	}
}

func TestSharedCodexDaemonThreadIsNotAdopted(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	e.Ancestors = sharedDaemon(os.Getpid())
	ctx := context.Background()
	loc := Location{Block: block}
	_, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "pane-b", TranscriptPath: []byte(`"/tmp/pane-b.jsonl"`)})
	if !errors.Is(err, ErrSharedHarness) || !strings.Contains(err.Error(), "--no-daemon") {
		t.Errorf("adoption under a shared daemon: %v, want ErrSharedHarness naming --no-daemon", err)
	}
	if rec, ok, _ := e.Store.Record(block); ok {
		t.Errorf("a shared daemon's thread was adopted into the pane: %+v", rec)
	}
}

func TestCodexHookUnderItsShellIsAdopted(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	e.Ancestors = func() ([]proc, error) {
		return []proc{{0x3ffffff1, "rex-amq-adapter"}, {0x3ffffff2, "zsh"}, {os.Getpid(), "codex"}}, nil
	}
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Codex, loc, HookInput{SessionID: "s1", TranscriptPath: []byte(`"/tmp/s1.jsonl"`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{SessionID: "s1"}) })
	rec, ok, _ := e.Store.Record(block)
	if !ok || !rec.Adopted() || rec.AgentPID != os.Getpid() || rec.AgentSessionID != "s1" {
		t.Fatalf("codex --no-daemon was not adopted: %+v", rec)
	}
	r, err := e.Hook(ctx, EventPostTool, core.Codex, loc, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if after, _, _ := e.Store.Record(block); after.AdoptedAt != rec.AdoptedAt || after.Handle != rec.Handle {
		t.Errorf("the lease of codex --no-daemon was not kept: %+v -> %+v (%+v)", rec, after, r)
	}
}

func TestRunsUnder(t *testing.T) {
	const self, sh, agent, mid = 10, 11, 12, 13
	for _, c := range []struct {
		name      string
		kind      core.Kind
		ancestors []proc
		want      error
	}{
		{"codex under its shell", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "zsh"}, {agent, "codex"}, {1, "launchd"}}, nil},
		{"codex under a login shell", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "-zsh"}, {agent, "codex"}}, nil},
		{"codex directly", core.Codex, []proc{{self, "rex-amq-adapter"}, {agent, "codex"}}, nil},
		{"codex shared daemon", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "zsh"}, {mid, "codex"}, {agent, "codex"}}, ErrSharedHarness},
		{"codex shared daemon under a launcher", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "codex"}, {14, "codex"}, {agent, "node"}}, ErrSharedHarness},
		{"codex native under its launcher", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "codex"}, {agent, "node"}}, nil},
		{"codex daemon reparented", core.Codex, []proc{{self, "rex-amq-adapter"}, {sh, "zsh"}, {mid, "codex"}, {1, "launchd"}}, errNotUnder},
		{"agy under its shell", core.Agy, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {agent, "agy"}}, nil},
		{"agy -p started directly by the pane's program", core.Agy, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "agy"}, {agent, "python3"}}, ErrStartedInPane},
		{"agy -p exec'd by sh -c under the pane's claude", core.Agy, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "agy"}, {14, "timeout"}, {agent, "claude"}}, ErrStartedInPane},
		{"agy -p run by the pane's agy", core.Agy, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "agy"}, {14, "bash"}, {agent, "agy"}}, ErrStartedInPane},
		{"agy -p run by the pane's agy without a shell", core.Agy, []proc{{self, "rex-amq-adapter"}, {mid, "agy"}, {agent, "agy"}}, ErrStartedInPane},
		{"agy -p run by the pane's claude", core.Agy, []proc{{self, "rex-amq-adapter"}, {sh, "sh"}, {mid, "agy"}, {14, "timeout"}, {15, "bash"}, {agent, "claude"}}, ErrStartedInPane},
		{"agy elsewhere", core.Agy, []proc{{self, "rex-amq-adapter"}, {mid, "agy"}}, errNotUnder},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := runsUnder(c.kind, c.ancestors, agent); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Errorf("runsUnder = %v, want %v", err, c.want)
			}
		})
	}
}
