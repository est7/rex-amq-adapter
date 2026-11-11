package transcript

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Rows as Claude Code writes them: an API error row, a tool_use, an
// errored tool_result, and ordinary text; other row types are skipped.
func TestClaudeTail(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude/projects/-Users-x--app/sid-1.jsonl"), `{"type":"attachment","uuid":"a0"}
{"type":"assistant","uuid":"u1","timestamp":"2026-11-10T02:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","uuid":"u2","timestamp":"2026-11-10T02:01:00.000Z","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your 5-hour limit · resets 11:40am"}]}}
{"type":"assistant","uuid":"u3","timestamp":"2026-11-10T02:02:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}
{"type":"user","uuid":"u4","timestamp":"2026-11-10T02:03:00.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"boom"}]}}
{"type":"api-request","uuid":"a1"}
`)
	tail, ok, err := Read(context.Background(), home, core.Claude, "sid-1", 3)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(tail.Rows) != 3 || tail.Rows[0].ID != "u2" || !tail.Rows[0].Error || tail.Rows[0].Kind != "error" {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if tail.Rows[1].Kind != "tool_call" || tail.Rows[1].ToolID != "toolu_1" || tail.Rows[2].Kind != "tool_result" || !tail.Rows[2].Error || tail.Rows[2].Text != "boom" {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if want := time.Date(2026, 11, 10, 2, 3, 0, 0, time.UTC); !tail.LastAt.Equal(want) && tail.LastAt.Before(want) {
		t.Fatalf("last at %v", tail.LastAt)
	}
	if _, ok, _ := Read(context.Background(), home, core.Claude, "nope", 3); ok {
		t.Fatal("found a transcript that does not exist")
	}
}

// Rows as Codex writes them: tool calls and outputs keyed by call_id, agent
// messages, an error event; developer messages are skipped.
func TestCodexTail(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex/sessions/2026/11/10/rollout-2026-11-10T02-00-00-thread-1.jsonl"), `{"timestamp":"2026-11-10T02:00:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":"thread-1"}}
{"timestamp":"2026-11-10T02:00:01.000Z","ordinal":1,"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex"}]}}
{"timestamp":"2026-11-10T02:00:02.000Z","ordinal":2,"type":"response_item","payload":{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"ls"}}
{"timestamp":"2026-11-10T02:00:03.000Z","ordinal":3,"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"done"}]}}
{"timestamp":"2026-11-10T02:00:04.000Z","ordinal":4,"type":"response_item","payload":{"type":"function_call","call_id":"call_2","name":"spawn","arguments":"{}"}}
{"timestamp":"2026-11-10T02:00:05.000Z","ordinal":5,"type":"event_msg","payload":{"type":"error","message":"stream disconnected"}}
`)
	tail, ok, err := Read(context.Background(), home, core.Codex, "thread-1", 10)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(tail.Rows) != 4 {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if tail.Rows[0].Kind != "tool_call" || tail.Rows[0].ToolID != "call_1" || tail.Rows[1].Kind != "tool_result" || tail.Rows[1].Text != "done" {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if tail.Rows[2].Kind != "tool_call" || tail.Rows[2].ToolID != "call_2" || !tail.Rows[3].Error || tail.Rows[3].Text != "stream disconnected" {
		t.Fatalf("rows: %+v", tail.Rows)
	}
}

// Rows as pi writes them: an assistant stopReason error with its message, a
// tool call, an errored toolResult.
func TestPiTail(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".pi/agent/sessions/--Users-x--app--/2026-11-10T02-00-00-000Z_sid-9.jsonl"), `{"type":"session","id":"sid-9"}
{"type":"message","id":"m1","timestamp":"2026-11-10T02:00:00.000Z","message":{"role":"assistant","stopReason":"error","errorMessage":"Upstream stream interrupted","content":[]}}
{"type":"message","id":"m2","timestamp":"2026-11-10T02:00:01.000Z","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"tc_1","name":"bash"}]}}
{"type":"message","id":"m3","timestamp":"2026-11-10T02:00:02.000Z","message":{"role":"toolResult","toolCallId":"tc_1","isError":true,"content":[{"type":"text","text":"exit 1"}]}}
`)
	tail, ok, err := Read(context.Background(), home, core.Pi, "sid-9", 10)
	if err != nil || !ok || len(tail.Rows) != 4 {
		t.Fatalf("%v %v %+v", ok, err, tail.Rows)
	}
	if !tail.Rows[0].Error || tail.Rows[0].Text != "Upstream stream interrupted" || tail.Rows[2].Kind != "tool_call" || tail.Rows[2].ToolID != "tc_1" || !tail.Rows[3].Error || tail.Rows[3].ToolID != "tc_1" {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if len(tail.Pending) != 0 {
		t.Fatalf("pending: %+v", tail.Pending)
	}
}

// Steps as Antigravity stores them: error_details marks an error; the
// database's modification time is the last activity.
func TestAgyTail(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	home := t.TempDir()
	db := filepath.Join(home, ".gemini/antigravity-cli/conversations/conv-1.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	sql := "create table steps (idx integer primary key, step_type integer, status integer, error_details blob);" +
		"insert into steps values (1, 17, 3, null), (2, 132, 7, X'1213" + hexOf("file does not exist") + "'), (3, 15, 3, null);"
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tail, ok, err := Read(context.Background(), home, core.Agy, "conv-1", 2)
	if err != nil || !ok || len(tail.Rows) != 2 {
		t.Fatalf("%v %v %+v", ok, err, tail.Rows)
	}
	if tail.Rows[0].ID != "2" || !tail.Rows[0].Error || tail.Rows[0].Text != "step status 7 file does not exist" || tail.Rows[1].Error {
		t.Fatalf("rows: %+v", tail.Rows)
	}
	if tail.LastAt.IsZero() {
		t.Fatal("no last activity")
	}
}

func hexOf(s string) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, digits[s[i]>>4], digits[s[i]&15])
	}
	return string(out)
}

// Parallel tool calls answered out of order are both complete; a call whose
// result never came stays pending even when more rows follow than a notice
// shows.
func TestPendingCallsKeepTheirPairing(t *testing.T) {
	home := t.TempDir()
	var rows []string
	rows = append(rows, `{"type":"assistant","uuid":"u1","timestamp":"2026-11-10T01:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"A","name":"Bash"},{"type":"tool_use","id":"B","name":"Read"}]}}`)
	rows = append(rows, `{"type":"user","uuid":"u2","timestamp":"2026-11-10T01:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"B","content":"b"},{"type":"tool_result","tool_use_id":"A","content":"a"}]}}`)
	rows = append(rows, `{"type":"assistant","uuid":"u3","timestamp":"2026-11-10T01:00:02.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"C","name":"Bash"}]}}`)
	for i := 0; i < 12; i++ {
		rows = append(rows, `{"type":"assistant","uuid":"m`+string(rune('a'+i))+`","timestamp":"2026-11-10T01:01:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"chatter"}]}}`)
	}
	write(t, filepath.Join(home, ".claude/projects/-Users-x--app/sid-2.jsonl"), strings.Join(rows, "\n")+"\n")
	tail, ok, err := Read(context.Background(), home, core.Claude, "sid-2", 8)
	if err != nil || !ok || len(tail.Rows) != 8 {
		t.Fatalf("%v %v %d rows", ok, err, len(tail.Rows))
	}
	if len(tail.Pending) != 1 || tail.Pending[0].ToolID != "C" {
		t.Fatalf("pending: %+v", tail.Pending)
	}
	h := core.Assess(tail, false, time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC))
	if h.Hung == nil || h.Hung.ToolID != "C" {
		t.Fatalf("hung: %+v", h.Hung)
	}
}

// An Antigravity database in WAL mode shows its activity in the -wal file;
// the main file may stay old.
func TestAgyActivityFollowsTheWAL(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	home := t.TempDir()
	db := filepath.Join(home, ".gemini/antigravity-cli/conversations/conv-2.db")
	os.MkdirAll(filepath.Dir(db), 0o700)
	if out, err := exec.Command("sqlite3", db, "pragma journal_mode=wal; create table steps (idx integer primary key, step_type integer, status integer, error_details blob); insert into steps values (1, 17, 3, null);").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(db, old, old)
	// The sqlite3 command checkpoints on exit; a writer that stays open
	// leaves the -wal file, which is what the reader looks at.
	if _, err := os.Stat(db + "-wal"); err != nil {
		if err := os.WriteFile(db+"-wal", nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tail, ok, err := Read(context.Background(), home, core.Agy, "conv-2", 2)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if time.Since(tail.LastAt) > time.Hour {
		t.Fatalf("activity read from the stale main file: %v", tail.LastAt)
	}
	if !tail.Rows[0].At.IsZero() {
		t.Fatal("a step row claimed a time it does not have")
	}
}

// A1: Codex writes turn-abort errors (e.g. stream disconnected: Bad Request) as
// event_msg rows of type task_complete with a non-null error object; a normal
// task_complete has error: null and yields no row.
func TestCodexTaskCompleteErrorRow(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions/2026/11/10/rollout-2026-11-10T02-00-00-thread-tc.jsonl")
	write(t, path, `{"timestamp":"2026-11-10T02:00:00.000Z","ordinal":1,"type":"event_msg","payload":{"type":"task_complete","turn_id":"t0","error":null}}
{"timestamp":"2026-11-10T02:00:05.000Z","ordinal":2,"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","error":{"message":"stream disconnected before completion: Codex: Bad Request"}}}
`)
	tail, ok, err := Read(context.Background(), home, core.Codex, "thread-tc", 10)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(tail.Rows) != 1 {
		t.Fatalf("want 1 row (normal task_complete skipped), got %+v", tail.Rows)
	}
	r := tail.Rows[0]
	if r.Kind != "error" || !r.Error || r.ToolID != "t1" || r.Text != "stream disconnected before completion: Codex: Bad Request" {
		t.Fatalf("unexpected row: %+v", r)
	}
}

// A2: Assess counts only turn-level harness error rows (Kind == "error") in
// h.Errors; tool_result rows with Error == true stay out of h.Errors so routine
// non-zero exit codes do not page, while still rendering with [error] in notices.
func TestAssessIgnoresToolResultErrorsButNoticesStillMarkThem(t *testing.T) {
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	toolOnly := core.Tail{
		Source: "/tmp/t.jsonl",
		Rows: []core.Row{
			{ID: "1", At: now.Add(-2 * time.Second), Kind: "tool_call", ToolID: "c1", Text: "Bash(…)"},
			{ID: "2", At: now.Add(-time.Second), Kind: "tool_result", ToolID: "c1", Error: true, Text: "exit status 1"},
		},
		LastAt: now.Add(-time.Second),
	}
	if h := core.Assess(toolOnly, false, now); len(h.Errors) != 0 {
		t.Fatalf("tool_result error must not count in Assess.Errors: %+v", h.Errors)
	}
	withTurnErr := toolOnly
	withTurnErr.Rows = append(withTurnErr.Rows, core.Row{
		ID: "3", At: now, Kind: "error", Error: true, Text: "5-hour limit",
	})
	h := core.Assess(withTurnErr, false, now)
	if len(h.Errors) != 1 || h.Errors[0].ID != "3" {
		t.Fatalf("want 1 turn-level error row, got %+v", h.Errors)
	}
	peer := core.Peer{Handle: "claude-01", Kind: core.Claude}
	_, hBody := core.HealthNotice(peer, core.NoticeStalled, withTurnErr, h, nil, now)
	_, eBody := core.ErrorNotice(peer, core.NoticeTurnError, "5-hour limit", "its summoner", withTurnErr)
	for name, body := range map[string]string{"HealthNotice": hBody, "ErrorNotice": eBody} {
		if !strings.Contains(body, "tool_result [error]: exit status 1") || !strings.Contains(body, "error [error]: 5-hour limit") {
			t.Fatalf("%s missing [error] marks:\n%s", name, body)
		}
	}
}

// A2b (S2b): readAgy marks a step with error_details (e.g. step_type 132,
// status 7 "file does not exist") as Kind "tool_result" with Error=true.
// Assess returns no Errors on it, and HealthNotice still prints it with [error].
func TestAgyStepErrorIsToolResultNotTurnError(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	home := t.TempDir()
	db := filepath.Join(home, ".gemini/antigravity-cli/conversations/conv-s2b.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	sql := "create table steps (idx integer primary key, step_type integer, status integer, error_details blob);" +
		"insert into steps values (1, 17, 3, null), (2, 132, 7, X'1213" + hexOf("file does not exist") + "'), (3, 15, 3, null);"
	if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tail, ok, err := Read(context.Background(), home, core.Agy, "conv-s2b", 3)
	if err != nil || !ok || len(tail.Rows) != 3 {
		t.Fatalf("ok=%v err=%v rows=%+v", ok, err, tail.Rows)
	}
	if tail.Rows[1].Kind != "tool_result" || !tail.Rows[1].Error || tail.Rows[1].Text != "step status 7 file does not exist" {
		t.Fatalf("want tool_result with Error=true, got %+v", tail.Rows[1])
	}
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	if h := core.Assess(tail, false, now); len(h.Errors) != 0 {
		t.Fatalf("Assess must not count agy step tool_result as turn error: %+v", h.Errors)
	}
	withTurn := tail
	withTurn.Rows = append(withTurn.Rows, core.Row{ID: "99", At: now, Kind: "error", Error: true, Text: "turn failed"})
	h := core.Assess(withTurn, false, now)
	_, body := core.HealthNotice(core.Peer{Handle: "agy-01", Kind: core.Agy}, core.NoticeStalled, withTurn, h, nil, now)
	if !strings.Contains(body, "tool_result [error]: step status 7 file does not exist") {
		t.Fatalf("HealthNotice missing [error] mark for agy step:\n%s", body)
	}
}
