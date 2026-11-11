package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// healthWorld is stuckWorld without stuck mail: the peer codex has a Codex
// rollout under TranscriptHome, the goal orch-01 a Claude transcript, both
// written by the test. Notifications to the desktop are captured.
func healthWorld(t *testing.T) (Env, *fakeTerm, store.Record, Session, *time.Time, *[]string) {
	t.Helper()
	e, term, orch, s, now := stuckWorld(t, "", "idle", false)
	e.TranscriptHome = t.TempDir()
	var desktop []string
	e.Notify = func(title, text string) { desktop = append(desktop, text) }
	for _, r := range []struct{ h, sid string }{{"codex", "thread-1"}, {"orch-01", "sid-g"}} {
		rec, _, _ := e.Store.Record(map[string]string{"codex": block, "orch-01": "block:g"}[r.h])
		rec.AgentSessionID = r.sid
		if err := e.Store.PutRecord(rec); err != nil {
			t.Fatal(err)
		}
		if r.h == "orch-01" {
			orch = rec // the watcher passes the live record, native session id included
		}
	}
	// Drain the brief stuckWorld sent, so the orchestrator's inbox counts notices only.
	inbox := filepath.Join(e.Paths.Root(), "agents", "codex", "inbox", "new")
	entries, _ := os.ReadDir(inbox)
	for _, en := range entries {
		os.Remove(filepath.Join(inbox, en.Name()))
	}
	return e, term, orch, s, now, &desktop
}

func writeRollout(t *testing.T, home string, rows ...string) string {
	t.Helper()
	p := filepath.Join(home, ".codex/sessions/2026/11/10/rollout-2026-11-10T02-00-00-thread-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeClaude(t *testing.T, home string, rows ...string) {
	t.Helper()
	p := filepath.Join(home, ".claude/projects/-Users-x--app/sid-g.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mailBody reads one unread message of h.
func mailBody(t *testing.T, e Env, h, id string) string {
	t.Helper()
	dir := filepath.Join(e.Paths.Root(), "agents", h, "inbox", "new")
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), id) {
			b, err := os.ReadFile(filepath.Join(dir, en.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("no file for %s in %s", id, dir)
	return ""
}

func dumpLog(t *testing.T, e Env) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(e.Paths.Logs(), "watchdog.log"))
	t.Logf("watchdog.log:\n%s", b)
}

func inboxOf(t *testing.T, e Env, h string) []string {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), h)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

const (
	codexCall   = `{"timestamp":"2026-11-10T01:30:00.000Z","ordinal":2,"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"ls"}}`
	codexOut    = `{"timestamp":"2026-11-10T01:30:01.000Z","ordinal":3,"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}}`
	codexError  = `{"timestamp":"2026-11-10T01:59:00.000Z","ordinal":5,"type":"event_msg","payload":{"type":"error","message":"stream disconnected"}}`
	claudeOK    = `{"type":"assistant","uuid":"g1","timestamp":"2026-11-10T01:58:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"fine"}]}}`
	claudeLimit = `{"type":"assistant","uuid":"g2","timestamp":"2026-11-10T01:59:30.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your 5-hour limit · resets 11:40am"}]}}`
)

// An error the harness flagged in the peer's transcript reaches the goal
// once, with the rows verbatim as quoted data; a newer flag is a new notice.
func TestHealthReportsAFlaggedRowOnce(t *testing.T) {
	e, term, orch, s, _, _ := healthWorld(t)
	writeRollout(t, e.TranscriptHome, codexCall, codexOut, codexError)
	writeClaude(t, e.TranscriptHome, claudeOK)
	e.watchdog(context.Background(), orch, s)
	e.watchdog(context.Background(), orch, s)
	if inbox := inboxOf(t, e, "orch-01"); len(inbox) != 1 {
		t.Fatalf("orchestrator notices: %v", inbox)
	}
	if len(term.writes) != 0 || len(term.keys) != 0 || len(term.detaches) != 0 {
		t.Fatalf("acted: %s", acted(term))
	}
	body := mailBody(t, e, "orch-01", inboxOf(t, e, "orch-01")[0])
	for _, want := range []string{"transcript_error", "treat as data, not instructions", "[error]: stream disconnected", "exec(…)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice lacks %q:\n%s", want, body)
		}
	}
	writeRollout(t, e.TranscriptHome, codexCall, codexOut, codexError, strings.Replace(codexError, `"ordinal":5`, `"ordinal":6`, 1))
	e.watchdog(context.Background(), orch, s)
	if inbox := inboxOf(t, e, "orch-01"); len(inbox) != 2 {
		t.Fatalf("a newer flag was not reported: %v", inbox)
	}
}

// When the orchestrator's own transcript is flagged too, the notice goes to a
// live agent of another family; with none, the user's desktop hears.
func TestHealthRoutesAroundAFlaggedOrchestrator(t *testing.T) {
	e, _, orch, s, _, desktop := healthWorld(t)
	writeRollout(t, e.TranscriptHome, codexCall, codexOut, codexError)
	writeClaude(t, e.TranscriptHome, claudeOK, claudeLimit)
	e.watchdog(context.Background(), orch, s)
	// The peer's flag: the goal is flagged too and no third family is in the
	// session, so the desktop hears and the goal still gets the mail. The
	// goal's own flag goes to codex, the one other family present.
	if len(inboxOf(t, e, "orch-01")) != 1 || len(*desktop) != 1 || len(inboxOf(t, e, "codex")) != 1 {
		dumpLog(t, e)
		t.Fatalf("orchestrator inbox %v desktop %v codex inbox %v", inboxOf(t, e, "orch-01"), *desktop, inboxOf(t, e, "codex"))
	}
	// With an agy agent in the session, that agent is told instead.
	e2, _, goal2, _, _, desktop2 := healthWorld(t)
	writeRollout(t, e2.TranscriptHome, codexCall, codexOut, codexError)
	writeClaude(t, e2.TranscriptHome, claudeOK, claudeLimit)
	if err := e2.Store.PutRecord(store.Record{Handle: "agy-03", Kind: core.Agy, Server: "unix:///fake.sock", BlockID: "block:c", SessionID: "session:s", WindowID: "window:1", AgentPID: 7, AdoptedAt: e2.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e2.AmqBin, e2.Paths.Root(), "agy-03"); err != nil {
		t.Fatal(err)
	}
	e2.Rex.(*fakeTerm).windows["block:c"] = "window:1"
	s2, err := e2.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	e2.watchdog(context.Background(), goal2, s2)
	if len(inboxOf(t, e2, "agy-03")) != 2 || len(inboxOf(t, e2, "orch-01")) != 0 || len(*desktop2) != 0 {
		t.Fatalf("agy %v orchestrator %v desktop %v", inboxOf(t, e2, "agy-03"), inboxOf(t, e2, "orch-01"), *desktop2)
	}
}

// A tool call without a result, and a working agent whose transcript stops
// growing, are each told once.
func TestHealthReportsAHungToolAndAStall(t *testing.T) {
	e, term, orch, s, now, _ := healthWorld(t)
	p := writeRollout(t, e.TranscriptHome, codexCall) // call_1 at 01:30, no output; now is 02:00
	writeClaude(t, e.TranscriptHome, claudeOK)
	old := now.Add(-20 * time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	term.state = "working"
	e.watchdog(context.Background(), orch, s)
	e.watchdog(context.Background(), orch, s)
	inbox := inboxOf(t, e, "orch-01")
	if len(inbox) != 2 {
		t.Fatalf("orchestrator notices: %v", inbox)
	}
	var subjects []string
	for _, id := range inbox {
		subjects = append(subjects, mailBody(t, e, "orch-01", id))
	}
	all := strings.Join(subjects, "\n")
	if !strings.Contains(all, "tool_hung") || !strings.Contains(all, "stalled") || !strings.Contains(all, "Screen, last lines") {
		t.Fatalf("notices:\n%s", all)
	}
	// Activity resumes: no further notice.
	writeRollout(t, e.TranscriptHome, codexCall, codexOut)
	e.watchdog(context.Background(), orch, s)
	if len(inboxOf(t, e, "orch-01")) != 2 {
		t.Fatal("reported again after activity resumed")
	}
}

// A3 (fast path): Claude StopFailure hook reports turn_error to the recipient,
// returns an empty Reply{}, and when the transcript already holds a fresh error
// row E (within 2*watchEvery), sets LastErrorID = E.ID (HookToldAt zero) so the
// next health() tick sends no duplicate message.
func TestClaudeStopFailureAndFlushedTranscriptDedup(t *testing.T) {
	e, _, goal, s, _, _ := healthWorld(t)
	e.Rex.(*fakeTerm).windows["block:c"] = "window:1"
	claudeRec := store.Record{
		Handle: "claude-04", Kind: core.Claude, Server: "unix:///fake.sock",
		BlockID: "block:c", SessionID: "session:s", WindowID: "window:1",
		AgentPID: 42, AgentSessionID: "sid-c4", AdoptedAt: e.Now(), FirstAdoptedAt: e.Now(),
	}
	if err := e.Store.PutRecord(claudeRec); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), "claude-04"); err != nil {
		t.Fatal(err)
	}
	// Write a fresh transcript error row (5s before now) before StopFailure runs.
	p := filepath.Join(e.TranscriptHome, ".claude/projects/-Users-x--app/sid-c4.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	freshLimit := strings.Replace(claudeLimit, "2026-11-10T01:59:30.000Z", "2026-11-10T01:59:55.000Z", 1)
	if err := os.WriteFile(p, []byte(claudeOK+"\n"+freshLimit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"}, HookInput{
		SessionID:    "sid-c4",
		AgentPID:     42,
		Error:        "rate_limit",
		ErrorDetails: "5h limit",
	})
	if err != nil || r.Notice != "" {
		t.Fatalf("StopFailure reply=%+v err=%v", r, err)
	}
	if out, err := Render(core.Claude, EventStopFailure, r); err != nil || out != "" {
		t.Fatalf("Render StopFailure=%q err=%v", out, err)
	}
	inbox := inboxOf(t, e, "orch-01")
	if len(inbox) != 1 {
		t.Fatalf("want 1 message to orch-01, got %v", inbox)
	}
	body := mailBody(t, e, "orch-01", inbox[0])
	for _, want := range []string{"error: claude-04 turn_error", "rate_limit 5h limit", "the session's orchestrator", "idle at its prompt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice missing %q:\n%s", want, body)
		}
	}
	mark, err := e.Store.HealthMark("block:c")
	if err != nil || mark.LastErrorID != "g2" || !mark.HookToldAt.IsZero() || mark.TurnErroredAt.IsZero() {
		t.Fatalf("unexpected HealthMark after flushed StopFailure: %+v err=%v", mark, err)
	}
	// Subsequent health() tick must not double-send.
	writeClaude(t, e.TranscriptHome, claudeOK)
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("watchdog sent duplicate notice after StopFailure: %v", inboxOf(t, e, "orch-01"))
	}
}

// A3b: When StopFailure runs before the error row E is flushed to the
// transcript, the hook mails immediately and sets HookToldAt = now; once E is
// written with E.At >= now - 2s, the next health() tick advances LastErrorID = E.ID,
// clears HookToldAt, and sends no duplicate. A subsequent error row E2 on a
// later turn is still reported.
func TestClaudeStopFailureBeforeTranscriptFlushDedup(t *testing.T) {
	e, _, goal, _, now, _ := healthWorld(t)
	e.Rex.(*fakeTerm).windows["block:c"] = "window:1"
	claudeRec := store.Record{
		Handle: "claude-05", Kind: core.Claude, Server: "unix:///fake.sock",
		BlockID: "block:c", SessionID: "session:s", WindowID: "window:1",
		AgentPID: 43, AgentSessionID: "sid-c5", AdoptedAt: *now, FirstAdoptedAt: *now,
	}
	if err := e.Store.PutRecord(claudeRec); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), "claude-05"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.TranscriptHome, ".claude/projects/-Users-x--app/sid-c5.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// Only claudeOK is on disk when StopFailure fires.
	if err := os.WriteFile(p, []byte(claudeOK+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaude(t, e.TranscriptHome, claudeOK)
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(context.Background(), EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"}, HookInput{
		SessionID:    "sid-c5",
		AgentPID:     43,
		Error:        "rate_limit",
		ErrorDetails: "5h limit",
	}); err != nil {
		t.Fatal(err)
	}
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("want immediate hook mail, got %v", inboxOf(t, e, "orch-01"))
	}
	mark, err := e.Store.HealthMark("block:c")
	if err != nil || mark.HookToldAt.IsZero() || mark.LastErrorID != "" {
		t.Fatalf("want HookToldAt set and LastErrorID empty before flush, got %+v", mark)
	}
	// Now the harness flushes E with At = now (within >= HookToldAt - 2s).
	flushedErr := `{"type":"assistant","uuid":"e-flushed","timestamp":"2026-11-10T02:00:00.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"5h limit"}]}}`
	if err := os.WriteFile(p, []byte(claudeOK+"\n"+flushedErr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("watchdog must not duplicate pre-flush hook mail: %v", inboxOf(t, e, "orch-01"))
	}
	mark, err = e.Store.HealthMark("block:c")
	if err != nil || mark.LastErrorID != "e-flushed" || !mark.HookToldAt.IsZero() {
		t.Fatalf("want LastErrorID=e-flushed and HookToldAt cleared, got %+v", mark)
	}
	// A subsequent new error row E2 on a later turn is reported.
	secondErr := `{"type":"assistant","uuid":"e-second","timestamp":"2026-11-10T02:01:00.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"second error"}]}}`
	if err := os.WriteFile(p, []byte(claudeOK+"\n"+flushedErr+"\n"+secondErr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 2 {
		t.Fatalf("subsequent error row E2 was not reported: %v", inboxOf(t, e, "orch-01"))
	}
}

// A3c (F2 + F3): When an older untold error row E0 (older than 2*watchEvery) is
// newest on disk at the moment StopFailure fires for a new un-flushed error E,
// the hook leaves LastErrorID unset and sets HookToldAt = now. On tick 1 (before
// E flushes), health() mails E0 and keeps HookToldAt intact. On tick 2 (after E
// flushes), health() consumes HookToldAt and does not mail E a second time.
// Also verifies F1: if amq.Send fails during the hook, neither LastErrorID nor
// HookToldAt is persisted, so the watchdog backstop still reports the row.
func TestClaudeStopFailureOlderUntoldRowDoesNotClearHookToldAt(t *testing.T) {
	e, _, goal, _, now, _ := healthWorld(t)
	e.Rex.(*fakeTerm).windows["block:c"] = "window:1"
	claudeRec := store.Record{
		Handle: "claude-06", Kind: core.Claude, Server: "unix:///fake.sock",
		BlockID: "block:c", SessionID: "session:s", WindowID: "window:1",
		AgentPID: 44, AgentSessionID: "sid-c6", AdoptedAt: *now, FirstAdoptedAt: *now,
	}
	if err := e.Store.PutRecord(claudeRec); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), "claude-06"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.TranscriptHome, ".claude/projects/-Users-x--app/sid-c6.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// E0 is 1 minute old (older than 2*watchEvery = 10s), untold.
	e0 := `{"type":"assistant","uuid":"e0-old","timestamp":"2026-11-10T01:59:00.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"earlier error E0"}]}}`
	if err := os.WriteFile(p, []byte(claudeOK+"\n"+e0+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaude(t, e.TranscriptHome, claudeOK)
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}

	// F1 negative check: if amq.Send fails, HookToldAt and LastErrorID stay unset.
	badEnv := e
	badEnv.AmqBin = "/nonexistent/amq"
	_, _ = badEnv.Hook(context.Background(), EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"}, HookInput{
		SessionID: "sid-c6", AgentPID: 44, Error: "rate_limit", ErrorDetails: "new error E",
	})
	if m, _ := e.Store.HealthMark("block:c"); !m.HookToldAt.IsZero() || m.LastErrorID != "" {
		t.Fatalf("failed send must not set HookToldAt or LastErrorID: %+v", m)
	}

	// Now StopFailure succeeds for E (while E is still not flushed; E0 is on disk).
	if _, err := e.Hook(context.Background(), EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"}, HookInput{
		SessionID: "sid-c6", AgentPID: 44, Error: "rate_limit", ErrorDetails: "new error E",
	}); err != nil {
		t.Fatal(err)
	}
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("want 1 mail (for E from hook), got %v", inboxOf(t, e, "orch-01"))
	}
	mark, _ := e.Store.HealthMark("block:c")
	if mark.LastErrorID != "" || mark.HookToldAt.IsZero() {
		t.Fatalf("hook must not adopt stale E0 as LastErrorID: %+v", mark)
	}

	// Tick 1 (E still not flushed; E0 is newest): watchdog mails E0 and keeps HookToldAt.
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 2 {
		t.Fatalf("want 2 mails (hook E + watchdog E0), got %v", inboxOf(t, e, "orch-01"))
	}
	mark, _ = e.Store.HealthMark("block:c")
	if mark.LastErrorID != "e0-old" || mark.HookToldAt.IsZero() {
		t.Fatalf("tick 1 must advance LastErrorID=e0-old and preserve HookToldAt: %+v", mark)
	}

	// Tick 2: E flushes with timestamp = now; watchdog consumes HookToldAt and sends nothing.
	eNew := `{"type":"assistant","uuid":"e-new","timestamp":"2026-11-10T02:00:00.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"new error E"}]}}`
	if err := os.WriteFile(p, []byte(claudeOK+"\n"+e0+"\n"+eNew+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 2 {
		t.Fatalf("tick 2 must not duplicate E: %v", inboxOf(t, e, "orch-01"))
	}
	mark, _ = e.Store.HealthMark("block:c")
	if mark.LastErrorID != "e-new" || !mark.HookToldAt.IsZero() {
		t.Fatalf("tick 2 must advance LastErrorID=e-new and clear HookToldAt: %+v", mark)
	}
}

// A4: Codex Stop hook when the rollout ends with a task_complete error for
// TurnID sends turn_error, returns Reply{} (never blocking the stop even when
// unread mail waits), and updates LastErrorID.
func TestCodexStopReportsTaskCompleteErrorWithoutBlocking(t *testing.T) {
	e, _, goal, s, _, _ := healthWorld(t)
	writeClaude(t, e.TranscriptHome, claudeOK)
	rollout := writeRollout(t, e.TranscriptHome,
		codexCall,
		codexOut,
		`{"timestamp":"2026-11-10T01:59:59.000Z","ordinal":7,"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-bad","error":{"message":"stream disconnected before completion: Codex: Bad Request"}}}`,
	)
	// Put unread mail in codex's inbox: an errored Stop must still return Reply{}
	// rather than blocking to announce mail.
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "orch-01", "codex", "ping", "unread"); err != nil {
		t.Fatal(err)
	}
	rawPath := `"` + rollout + `"`
	r, err := e.Hook(context.Background(), EventStop, core.Codex, Location{Server: "unix:///fake.sock", Block: block}, HookInput{
		SessionID:      "thread-1",
		TurnID:         "turn-bad",
		TranscriptPath: []byte(rawPath),
	})
	if err != nil || r.Notice != "" {
		t.Fatalf("Codex errored Stop must not block: r=%+v err=%v", r, err)
	}
	inbox := inboxOf(t, e, "orch-01")
	if len(inbox) != 1 {
		t.Fatalf("want 1 turn_error message to orch-01, got %v", inbox)
	}
	body := mailBody(t, e, "orch-01", inbox[0])
	if !strings.Contains(body, "error: codex turn_error") || !strings.Contains(body, "stream disconnected before completion: Codex: Bad Request") {
		t.Fatalf("unexpected Codex error notice:\n%s", body)
	}
	mark, err := e.Store.HealthMark(block)
	if err != nil || mark.LastErrorID != "7" {
		t.Fatalf("want LastErrorID=7, got %+v err=%v", mark, err)
	}
	e.watchdog(context.Background(), goal, s)
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("watchdog duplicated Codex Stop error: %v", inboxOf(t, e, "orch-01"))
	}
}

// A5: Antigravity Stop hook reports turn_error on ERROR, turn_limit on
// MAX_INVOCATIONS / MAX_TOKEN_BUDGET_EXCEEDED / MAX_FORCED_INVOCATIONS, sends
// nothing on USER_CANCELED, and runs the normal announce path on NO_TOOL_CALL.
func TestAgyStopClassifiesTerminationReasons(t *testing.T) {
	e, _, _, _, now, _ := healthWorld(t)
	writeClaude(t, e.TranscriptHome, claudeOK)
	e.Rex.(*fakeTerm).windows["block:agy"] = "window:1"
	agyRec := store.Record{
		Handle: "agy-09", Kind: core.Agy, Server: "unix:///fake.sock",
		BlockID: "block:agy", SessionID: "session:s", WindowID: "window:1",
		AgentPID: os.Getpid(), AgentSessionID: "conv-9", AdoptedAt: *now, FirstAdoptedAt: *now,
	}
	if err := e.Store.PutRecord(agyRec); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), "agy-09"); err != nil {
		t.Fatal(err)
	}
	loc := Location{Server: "unix:///fake.sock", Block: "block:agy"}

	// 1. ERROR -> turn_error
	if r, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{
		ConversationID:    "conv-9",
		TerminationReason: "ERROR",
		Error:             "DEADLINE_EXCEEDED",
	}); err != nil || r.Notice != "" {
		t.Fatalf("ERROR: r=%+v err=%v", r, err)
	}
	inbox := inboxOf(t, e, "orch-01")
	if len(inbox) != 1 || !strings.Contains(mailBody(t, e, "orch-01", inbox[0]), "error: agy-09 turn_error") || !strings.Contains(mailBody(t, e, "orch-01", inbox[0]), "DEADLINE_EXCEEDED") {
		t.Fatalf("ERROR notice: %v", inbox)
	}

	// 2. MAX_INVOCATIONS -> turn_limit
	if r, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{
		ConversationID:    "conv-9",
		TerminationReason: "MAX_INVOCATIONS",
	}); err != nil || r.Notice != "" {
		t.Fatalf("MAX_INVOCATIONS: r=%+v err=%v", r, err)
	}
	inbox = inboxOf(t, e, "orch-01")
	if len(inbox) != 2 {
		t.Fatalf("want 2 notices after MAX_INVOCATIONS, got %v", inbox)
	}

	// 3. USER_CANCELED -> no mail, empty reply
	if r, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{
		ConversationID:    "conv-9",
		TerminationReason: "USER_CANCELED",
	}); err != nil || r.Notice != "" {
		t.Fatalf("USER_CANCELED: r=%+v err=%v", r, err)
	}
	if len(inboxOf(t, e, "orch-01")) != 2 {
		t.Fatalf("USER_CANCELED must not mail goal-01: %v", inboxOf(t, e, "orch-01"))
	}

	// 4. NO_TOOL_CALL -> normal stop (clears TurnErroredAt and announces unread mail)
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "orch-01", "agy-09", "task", "hello"); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{
		ConversationID:    "conv-9",
		TerminationReason: "NO_TOOL_CALL",
	})
	if err != nil || r.Notice == "" {
		t.Fatalf("NO_TOOL_CALL with unread mail must announce: r=%+v err=%v", r, err)
	}
	if m, err := e.Store.HealthMark("block:agy"); err != nil || !m.TurnErroredAt.IsZero() {
		t.Fatalf("normal stop must clear TurnErroredAt: %+v err=%v", m, err)
	}

	// 5. MAX_INVOCATIONS after a clean stop reports turn_limit without setting TurnErroredAt (F5).
	if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{
		ConversationID:    "conv-9",
		TerminationReason: "MAX_INVOCATIONS",
	}); err != nil {
		t.Fatal(err)
	}
	if m, err := e.Store.HealthMark("block:agy"); err != nil || !m.TurnErroredAt.IsZero() {
		t.Fatalf("turn_limit must not set TurnErroredAt: %+v err=%v", m, err)
	}
}

// A6: Pi settle hook reports turn_error on Outcome=="error", sends nothing on
// Outcome=="aborted", and runs the normal announce path on Outcome=="completed"
// (or any unknown future outcome value).
func TestPiSettleClassifiesOutcomes(t *testing.T) {
	e, _, _, _, now, _ := healthWorld(t)
	writeClaude(t, e.TranscriptHome, claudeOK)
	e.Rex.(*fakeTerm).windows["block:pi"] = "window:1"
	piRec := store.Record{
		Handle: "pi-06", Kind: core.Pi, Server: "unix:///fake.sock",
		BlockID: "block:pi", SessionID: "session:s", WindowID: "window:1",
		AgentPID: os.Getpid(), AgentSessionID: "sid-pi6", AdoptedAt: *now, FirstAdoptedAt: *now,
	}
	if err := e.Store.PutRecord(piRec); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), "pi-06"); err != nil {
		t.Fatal(err)
	}
	loc := Location{Server: "unix:///fake.sock", Block: "block:pi"}

	// 1. Outcome: "error" -> turn_error
	if r, err := e.Hook(context.Background(), EventSettle, core.Pi, loc, HookInput{
		SessionID:    "sid-pi6",
		AgentPID:     os.Getpid(),
		Outcome:      "error",
		ErrorMessage: "Upstream stream interrupted",
	}); err != nil || r.Notice != "" {
		t.Fatalf("Pi error settle: r=%+v err=%v", r, err)
	}
	inbox := inboxOf(t, e, "orch-01")
	if len(inbox) != 1 || !strings.Contains(mailBody(t, e, "orch-01", inbox[0]), "Upstream stream interrupted") {
		t.Fatalf("Pi error notice: %v", inbox)
	}

	// 2. Outcome: "aborted" -> no mail, empty reply
	if r, err := e.Hook(context.Background(), EventSettle, core.Pi, loc, HookInput{
		SessionID: "sid-pi6",
		AgentPID:  os.Getpid(),
		Outcome:   "aborted",
	}); err != nil || r.Notice != "" {
		t.Fatalf("Pi aborted settle: r=%+v err=%v", r, err)
	}
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("Pi aborted settle must not mail: %v", inboxOf(t, e, "orch-01"))
	}

	// 3. Outcome: "completed" -> normal announce path (clears TurnErroredAt)
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "orch-01", "pi-06", "task", "next"); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventSettle, core.Pi, loc, HookInput{
		SessionID: "sid-pi6",
		AgentPID:  os.Getpid(),
		Outcome:   "completed",
	})
	if err != nil || r.Notice == "" {
		t.Fatalf("Pi completed settle with unread mail must announce: r=%+v err=%v", r, err)
	}
	if m, err := e.Store.HealthMark("block:pi"); err != nil || !m.TurnErroredAt.IsZero() {
		t.Fatalf("completed settle must clear TurnErroredAt: %+v err=%v", m, err)
	}
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("Pi completed settle must not send error mail: %v", inboxOf(t, e, "orch-01"))
	}

	// 4. Unknown future outcome -> treated as normal path (no error mail)
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "orch-01", "pi-06", "task", "after-unknown"); err != nil {
		t.Fatal(err)
	}
	r, err = e.Hook(context.Background(), EventSettle, core.Pi, loc, HookInput{
		SessionID: "sid-pi6",
		AgentPID:  os.Getpid(),
		Outcome:   "future_outcome",
	})
	if err != nil || r.Notice == "" {
		t.Fatalf("Pi unknown outcome settle with unread mail must announce: r=%+v err=%v", r, err)
	}
	if len(inboxOf(t, e, "orch-01")) != 1 {
		t.Fatalf("Pi unknown outcome settle must not send error mail: %v", inboxOf(t, e, "orch-01"))
	}
}

// A7: ErrorRecipient table test covering the 6-step ownership hierarchy,
// flagged-candidate skipping, cross-family fallback, and turn_limit exclusion.
func TestErrorRecipientHierarchyTable(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	parent := core.Peer{Handle: "claude-01", Kind: core.Claude, Window: "w1", AdoptedAt: t0}
	lead := core.Peer{Handle: "claude-02", Kind: core.Claude, Role: "implementer", Window: "w1", AdoptedAt: t2}
	earlierMate := core.Peer{Handle: "claude-03", Kind: core.Claude, Window: "w1", AdoptedAt: t0}
	goalOtherTab := core.Peer{Handle: "claude-09", Kind: core.Claude, Role: core.OrchestratorRole, Window: "w2", AdoptedAt: t2}
	crossCodex := core.Peer{Handle: "codex-01", Kind: core.Codex, Window: "w2", AdoptedAt: t2}
	worker := core.Peer{Handle: "agy-01", Kind: core.Agy, Role: "reviewer", Window: "w1", AdoptedAt: t1}

	cases := []struct {
		name      string
		rec       core.Peer
		peers     []core.Peer
		crewLead  string
		code      string
		flagged   map[core.Handle]bool
		want      core.Handle
		wantWhy   string
		wantFound bool
	}{
		{
			name:      "parent live -> parent",
			rec:       core.Peer{Handle: "agy-01", Kind: core.Agy, Window: "w1", Parent: parent.Handle, AdoptedAt: t1},
			peers:     []core.Peer{parent, lead, goalOtherTab},
			crewLead:  "implementer",
			code:      core.NoticeTurnError,
			want:      parent.Handle,
			wantWhy:   "its summoner",
			wantFound: true,
		},
		{
			name:      "no parent, lead live and rec not lead -> lead",
			rec:       worker,
			peers:     []core.Peer{lead, goalOtherTab},
			crewLead:  "implementer",
			code:      core.NoticeTurnError,
			want:      lead.Handle,
			wantWhy:   "its crew lead",
			wantFound: true,
		},
		{
			name:      "no parent, no crew, tab-mate adopted earlier -> tab-mate",
			rec:       worker,
			peers:     []core.Peer{earlierMate, goalOtherTab},
			code:      core.NoticeTurnError,
			want:      earlierMate.Handle,
			wantWhy:   "its tab owner",
			wantFound: true,
		},
		{
			name:      "rec earliest in its tab, goal in another tab -> goal",
			rec:       core.Peer{Handle: "agy-01", Kind: core.Agy, Window: "w1", AdoptedAt: t0},
			peers:     []core.Peer{lead, goalOtherTab},
			code:      core.NoticeTurnError,
			want:      goalOtherTab.Handle,
			wantWhy:   "the session's orchestrator",
			wantFound: true,
		},
		{
			name:      "rec earliest in its tab, no goal -> desktop (ok=false)",
			rec:       core.Peer{Handle: "agy-01", Kind: core.Agy, Window: "w1", AdoptedAt: t0},
			peers:     []core.Peer{lead, crossCodex},
			code:      core.NoticeTurnError,
			wantFound: false,
		},
		{
			name:      "rec is the goal -> cross-family unflagged agent",
			rec:       goalOtherTab,
			peers:     []core.Peer{crossCodex},
			code:      core.NoticeTurnError,
			want:      crossCodex.Handle,
			wantWhy:   "another model family because claude-09 is flagged",
			wantFound: true,
		},
		{
			name:      "candidate flagged -> skipped to cross-family when owner/goal flagged",
			rec:       worker,
			peers:     []core.Peer{earlierMate, goalOtherTab, crossCodex},
			code:      core.NoticeTurnError,
			flagged:   map[core.Handle]bool{earlierMate.Handle: true, goalOtherTab.Handle: true},
			want:      crossCodex.Handle,
			wantWhy:   "another model family because claude-03 is flagged",
			wantFound: true,
		},
		{
			name:      "turn_limit never takes cross-family branch",
			rec:       goalOtherTab,
			peers:     []core.Peer{crossCodex},
			code:      core.NoticeTurnLimit,
			wantFound: false,
		},
		{
			name:      "nobody unflagged -> ok=false",
			rec:       goalOtherTab,
			peers:     []core.Peer{crossCodex},
			code:      core.NoticeTurnError,
			flagged:   map[core.Handle]bool{crossCodex.Handle: true},
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why, ok := core.ErrorRecipient(tc.rec, tc.peers, tc.crewLead, tc.code, func(h core.Handle) bool {
				return tc.flagged[h]
			})
			if ok != tc.wantFound || got.Handle != tc.want || (tc.wantWhy != "" && why != tc.wantWhy) {
				t.Fatalf("got (%s, %q, %v), want (%s, %q, %v)", got.Handle, why, ok, tc.want, tc.wantWhy, tc.wantFound)
			}
		})
	}
}

// A9: WatchGoal releases LockWatch and calls e.Exec when SelfBin's stat
// identity changes on disk.
func TestWatchGoalHotRestartsWhenBinaryChanges(t *testing.T) {
	e, _, goal, _, _, _ := healthWorld(t)
	selfBin := filepath.Join(t.TempDir(), "rex-amq-adapter")
	if err := os.WriteFile(selfBin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = selfBin
	var execCalled string
	var lockFreeDuringExec bool
	e.Exec = func(argv0 string, argv []string, envv []string) error {
		execCalled = argv0
		// Verify LockWatch was released before Exec was called.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if unlock, err := e.Store.LockWatch(ctx, goal.Handle); err == nil {
			lockFreeDuringExec = true
			unlock()
		}
		return nil
	}
	// Replace SelfBin atomically while the first watchOnce runs.
	origRex := e.Rex
	e.Rex = &binSwapTerm{Terminal: origRex, path: selfBin}
	if err := e.WatchOrchestrator(context.Background(), goal.Handle); err != nil {
		t.Fatalf("WatchGoal: %v", err)
	}
	if execCalled != selfBin || !lockFreeDuringExec {
		t.Fatalf("execCalled=%q lockFreeDuringExec=%v", execCalled, lockFreeDuringExec)
	}
}

type binSwapTerm struct {
	Terminal
	path    string
	swapped bool
}

func (b *binSwapTerm) Whoami(ctx context.Context) (rex.Server, error) {
	if !b.swapped {
		b.swapped = true
		tmp := b.path + ".new"
		_ = os.WriteFile(tmp, []byte("v2-updated"), 0o755)
		_ = os.Rename(tmp, b.path)
	}
	return b.Terminal.Whoami(ctx)
}

// A11: Record.FirstAdoptedAt is set on initial adoption and preserved across
// resume; Record.AdoptedAt updates on resume so resumable() and Summon keep
// their recency ordering, while tab-ownership order stays monotonic.
func TestFirstAdoptedAtPreservedAcrossResumeWhileAdoptedAtUpdates(t *testing.T) {
	term := &fakeTerm{
		fgPID:   100,
		windows: map[string]string{"block:owner": "window:1", "block:peer": "window:1"},
		tabs:    map[string]string{"window:1": "Work"},
		fgs: map[string]rex.Process{
			"block:owner": {PID: 100, Name: "claude", Cwd: "/tmp"},
			"block:peer":  {PID: 200, Name: "codex", Cwd: "/tmp"},
		},
	}
	e := realAmqEnv(t, term)
	e.Crews = crew.Load
	now := time.Date(2026, 11, 10, 1, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }

	// 1. Adopt owner in block:owner at t=1:00.
	a1, err := e.Adopt(context.Background(), Location{Server: "unix:///fake.sock", Block: "block:owner"}, core.Claude, HookInput{
		SessionID: "sess-owner",
		AgentPID:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	tOwnerInit := now

	// 2. Adopt peer in block:peer at t=1:05.
	now = now.Add(5 * time.Minute)
	a2, err := e.Adopt(context.Background(), Location{Server: "unix:///fake.sock", Block: "block:peer"}, core.Codex, HookInput{
		SessionID: "sess-peer",
		AgentPID:  200,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Owner departs at t=1:10 and resumes at t=1:15 with a new PID.
	now = now.Add(5 * time.Minute)
	if err := e.Depart(context.Background(), Location{Server: "unix:///fake.sock", Block: "block:owner"}, HookInput{SessionID: "sess-owner"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	tOwnerResume := now
	term.fgs["block:owner"] = rex.Process{PID: 101, Name: "claude", Cwd: "/tmp"}
	a1Resumed, err := e.Adopt(context.Background(), Location{Server: "unix:///fake.sock", Block: "block:owner"}, core.Claude, HookInput{
		SessionID: "sess-owner",
		AgentPID:  101,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a1Resumed.Record.Handle != a1.Record.Handle {
		t.Fatalf("resumed handle changed: %s != %s", a1Resumed.Record.Handle, a1.Record.Handle)
	}
	if !a1Resumed.Record.FirstAdoptedAt.Equal(tOwnerInit) {
		t.Fatalf("FirstAdoptedAt not preserved: got %v, want %v", a1Resumed.Record.FirstAdoptedAt, tOwnerInit)
	}
	if !a1Resumed.Record.AdoptedAt.Equal(tOwnerResume) {
		t.Fatalf("AdoptedAt did not update on resume: got %v, want %v", a1Resumed.Record.AdoptedAt, tOwnerResume)
	}

	// 4. When peer (adopted at 1:05) errors at 1:16, owner (FirstAdoptedAt 1:00,
	// resumed at 1:15) is still recognized as the earlier tab owner!
	got, why, ok := core.ErrorRecipient(peerOf(a2.Record), []core.Peer{peerOf(a1Resumed.Record)}, "", core.NoticeTurnError, nil)
	if !ok || got.Handle != a1Resumed.Record.Handle || why != "its tab owner" {
		t.Fatalf("after owner resume, ErrorRecipient=(%s, %q, %v), want (%s, \"its tab owner\", true)", got.Handle, why, ok, a1Resumed.Record.Handle)
	}
	_ = errors.New
}
