package app

import (
	"context"
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
	if exports, err := e.Exports(loc); err != nil || !strings.Contains(exports, "AM_ME='codex'") {
		t.Fatalf("terminal lost AMQ identity after background shutdown: %q %v", exports, err)
	}
	if !wakerLive(t, e, "codex") {
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
