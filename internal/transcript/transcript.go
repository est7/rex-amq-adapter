// Package transcript reads the end of an agent's own conversation record,
// as its harness writes it, into core.Rows. Nothing here interprets text:
// only the harness's own fields (an error flag, a tool call id, a
// timestamp) are read, so the main agent can judge the rows itself.
//
// Claude Code: ~/.claude/projects/<slug>/<session>.jsonl. Codex:
// ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread>.jsonl. pi:
// ~/.pi/agent/sessions/<slug>/<ts>_<session>.jsonl. Antigravity:
// ~/.gemini/antigravity-cli/conversations/<conversation>.db, read with the
// sqlite3 command.
package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// tailBytes bounds how much of a file is read: the last rows only.
const tailBytes = 512 * 1024

// Read returns the last k rows of the transcript of session for an agent of
// kind, under home. ok is false when no transcript is found.
func Read(ctx context.Context, home string, kind core.Kind, session string, k int) (core.Tail, bool, error) {
	if session == "" {
		return core.Tail{}, false, nil
	}
	switch kind {
	case core.Claude:
		return readJSONL(filepath.Join(home, ".claude", "projects", "*", session+".jsonl"), k, claudeRow)
	case core.Codex:
		return readJSONL(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+session+".jsonl"), k, codexRow)
	case core.Pi:
		return readJSONL(filepath.Join(home, ".pi", "agent", "sessions", "*", "*_"+session+".jsonl"), k, piRow)
	case core.Agy:
		return readAgy(ctx, filepath.Join(home, ".gemini", "antigravity-cli", "conversations", session+".db"), k)
	}
	return core.Tail{}, false, nil
}

// readJSONL reads the last k rows that parse maps of the newest file
// matching pattern.
func readJSONL(pattern string, k int, parse func(map[string]any) (core.Row, bool)) (core.Tail, bool, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return core.Tail{}, false, err
	}
	path := newest(matches)
	f, err := os.Open(path)
	if err != nil {
		return core.Tail{}, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return core.Tail{}, false, err
	}
	if info.Size() > tailBytes {
		if _, err := f.Seek(info.Size()-tailBytes, io.SeekStart); err != nil {
			return core.Tail{}, false, err
		}
	}
	t := core.Tail{Source: path, LastAt: info.ModTime()}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	first := info.Size() > tailBytes // a partial first line is skipped
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if r, ok := parse(m); ok {
			t.Rows = append(t.Rows, r)
		}
	}
	if len(t.Rows) > k {
		t.Rows = t.Rows[len(t.Rows)-k:]
	}
	if n := len(t.Rows); n > 0 && t.Rows[n-1].At.After(t.LastAt) {
		t.LastAt = t.Rows[n-1].At
	}
	return t, true, nil
}

func newest(paths []string) string {
	best, bestAt := paths[0], time.Time{}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(bestAt) {
			best, bestAt = p, info.ModTime()
		}
	}
	return best
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func when(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// contentText joins the text of a content array or string, and the error
// fields a harness writes beside it.
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch str(m, "type") {
			case "text", "input_text", "output_text":
				parts = append(parts, str(m, "text"))
			case "tool_result":
				parts = append(parts, contentText(m["content"]))
			case "tool_use", "toolCall":
				parts = append(parts, str(m, "name")+"(…)")
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// claudeRow: assistant rows carry text and tool_use blocks (isApiErrorMessage
// marks an API error); user rows carry tool_result blocks (is_error) or the
// user's text.
func claudeRow(m map[string]any) (core.Row, bool) {
	typ := str(m, "type")
	if typ != "assistant" && typ != "user" {
		return core.Row{}, false
	}
	msg, _ := m["message"].(map[string]any)
	r := core.Row{ID: str(m, "uuid"), At: when(str(m, "timestamp")), Kind: "message", Text: contentText(msg["content"])}
	if e, _ := m["isApiErrorMessage"].(bool); e {
		r.Kind, r.Error = "error", true
	}
	if items, ok := msg["content"].([]any); ok {
		for _, item := range items {
			b, _ := item.(map[string]any)
			switch str(b, "type") {
			case "tool_use":
				r.Kind, r.ToolID = "tool_call", str(b, "id")
			case "tool_result":
				r.Kind, r.ToolID = "tool_result", str(b, "tool_use_id")
				if e, _ := b["is_error"].(bool); e {
					r.Error = true
				}
			}
		}
	}
	return r, true
}

// codexRow: response_item rows carry messages, tool calls and their outputs;
// an event_msg of type error is the harness's own error flag.
func codexRow(m map[string]any) (core.Row, bool) {
	p, _ := m["payload"].(map[string]any)
	if p == nil {
		return core.Row{}, false
	}
	r := core.Row{ID: str(m, "ordinal"), At: when(str(m, "timestamp"))}
	if r.ID == "" {
		if n, ok := m["ordinal"].(float64); ok {
			r.ID = strconv.Itoa(int(n))
		}
	}
	switch str(m, "type") {
	case "event_msg":
		if str(p, "type") != "error" {
			return core.Row{}, false
		}
		r.Kind, r.Error, r.Text = "error", true, str(p, "message")
	case "response_item":
		switch str(p, "type") {
		case "message", "agent_message":
			if role := str(p, "role"); role != "" && role != "user" && role != "assistant" {
				return core.Row{}, false // developer and system rows are the prompt, not the conversation
			}
			r.Kind, r.Text = "message", contentText(p["content"])
			if r.Text == "" {
				r.Text = str(p, "text")
			}
		case "function_call", "custom_tool_call":
			r.Kind, r.ToolID, r.Text = "tool_call", str(p, "call_id"), str(p, "name")+"(…)"
		case "function_call_output", "custom_tool_call_output":
			r.Kind, r.ToolID, r.Text = "tool_result", str(p, "call_id"), contentText(p["output"])
		default:
			return core.Row{}, false
		}
	default:
		return core.Row{}, false
	}
	return r, true
}

// piRow: message rows carry the role; an assistant message's stopReason
// "error" and errorMessage, and a toolResult's isError, are the flags.
func piRow(m map[string]any) (core.Row, bool) {
	if str(m, "type") != "message" {
		return core.Row{}, false
	}
	msg, _ := m["message"].(map[string]any)
	r := core.Row{ID: str(m, "id"), At: when(str(m, "timestamp")), Kind: "message", Text: contentText(msg["content"])}
	switch str(msg, "role") {
	case "assistant":
		if str(msg, "stopReason") == "error" {
			r.Kind, r.Error = "error", true
		}
		if e := str(msg, "errorMessage"); e != "" {
			r.Error, r.Text = true, strings.TrimSpace(r.Text+" "+e)
		}
		if items, ok := msg["content"].([]any); ok {
			for _, item := range items {
				if b, _ := item.(map[string]any); str(b, "type") == "toolCall" {
					r.Kind, r.ToolID = "tool_call", str(b, "id")
				}
			}
		}
	case "toolResult":
		r.Kind, r.ToolID = "tool_result", str(msg, "toolCallId")
		if e, _ := msg["isError"].(bool); e {
			r.Error = true
		}
	}
	return r, true
}

// readAgy reads the last k steps of a conversation database: a step with
// error_details is the harness's error flag. Steps carry no timestamp, so
// the database's modification time stands for the last row.
func readAgy(ctx context.Context, path string, k int) (core.Tail, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return core.Tail{}, false, nil
	}
	if err != nil {
		return core.Tail{}, false, err
	}
	q := "select idx, status, coalesce(quote(error_details),'') from steps order by idx desc limit " + strconv.Itoa(k)
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", "-separator", "\t", path, q).Output()
	if err != nil {
		return core.Tail{}, false, err
	}
	t := core.Tail{Source: path, LastAt: info.ModTime()}
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		cells := strings.SplitN(string(lines[i]), "\t", 3)
		if len(cells) < 2 {
			continue
		}
		r := core.Row{ID: cells[0], At: info.ModTime(), Kind: "message", Text: "step status " + cells[1]}
		if len(cells) == 3 && cells[2] != "" && cells[2] != "NULL" && cells[2] != "''" {
			r.Kind, r.Error = "error", true
			r.Text += " " + printable(cells[2])
		}
		t.Rows = append(t.Rows, r)
	}
	return t, true, nil
}

// printable keeps the readable runs of a quoted blob (a protobuf with the
// message text inside), dropping the framing bytes.
func printable(quoted string) string {
	if strings.HasPrefix(quoted, "X'") {
		b, err := hexDecode(strings.TrimSuffix(strings.TrimPrefix(quoted, "X'"), "'"))
		if err == nil {
			quoted = string(b)
		}
	}
	var out []string
	var run []rune
	flush := func() {
		if len(run) >= 4 {
			out = append(out, string(run))
		}
		run = run[:0]
	}
	for _, c := range quoted {
		if c >= 0x20 && c != 0x7f || c == '\n' {
			run = append(run, c)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(out, " ")
}

func hexDecode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		v, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		out = append(out, byte(v))
	}
	return out, nil
}
