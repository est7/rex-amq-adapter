package transcript

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	if err != nil || !ok || len(tail.Rows) != 3 {
		t.Fatalf("%v %v %+v", ok, err, tail.Rows)
	}
	if !tail.Rows[0].Error || tail.Rows[0].Text != "Upstream stream interrupted" || tail.Rows[1].ToolID != "tc_1" || !tail.Rows[2].Error || tail.Rows[2].ToolID != "tc_1" {
		t.Fatalf("rows: %+v", tail.Rows)
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
