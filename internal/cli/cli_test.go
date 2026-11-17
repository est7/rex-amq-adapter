package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

func file(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// command is a small declaration exercising each value form: a body, a
// handle, a repeatable artifact, and --input.
func command() *Command {
	return &Command{Path: []string{"t"}, Input: true,
		Args: []Arg{{Name: "report", Value: Value{Kind: KindBody}}},
		Flags: []Flag{
			{Name: "body", Value: Value{Kind: KindBody}},
			{Name: "to", Value: Value{Kind: KindHandle}},
			{Name: "artifact", Repeat: true, Value: Value{Kind: KindArtifact}},
			{Name: "limit", Value: Value{Kind: KindInt, Min: 1, Max: 5}},
			{Name: "input", Value: Value{Kind: KindInput}},
		}}
}

// A body given as @FILE is checked as a path before any read, and read
// whole afterwards: what reaches the command is the file's text, never its
// path. - reads stdin, and inline text is itself.
func TestBodyAtFileReadsTheFile(t *testing.T) {
	p := file(t, "CONTENTS\n")
	in, err := command().parse([]string{"@" + p, "--body", "@" + p})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{in.Arg("report"), in.Str("body")} {
		got, err := ReadBody(v, nil)
		if err != nil || got != "CONTENTS\n" {
			t.Errorf("ReadBody(%q) = %q, %v; want the file's text", v, got, err)
		}
	}
	if got, err := ReadBody("-", strings.NewReader("from stdin")); err != nil || got != "from stdin" {
		t.Errorf("- = %q, %v", got, err)
	}
	if got, err := ReadBody("inline", nil); err != nil || got != "inline" {
		t.Errorf("inline = %q, %v", got, err)
	}
	if _, err := command().parse([]string{"--body", "@" + p + ".missing"}); err == nil {
		t.Error("a missing @FILE passed validation")
	}
	bad := file(t, "a\x1b[31mb")
	in, err = command().parse([]string{"--body", "@" + bad})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBody(in.Str("body"), nil); err == nil {
		t.Error("an ANSI escape read from @FILE was accepted")
	}
}

// --input is one JSON object whose keys are the command's args and flags:
// @FILE, -, or inline. Its values meet the same checks as flags; an unknown
// key, a key also on the command line, or a second object is usage.
func TestInputMergesLikeFlags(t *testing.T) {
	c := command()
	merge := func(raw string, args ...string) (Input, error) {
		in, err := c.parse(append(args, "--input", raw))
		if err != nil {
			return in, err
		}
		b, err := readInput(in.Str("input"), strings.NewReader(raw))
		if err != nil {
			return in, err
		}
		return c.mergeInput(in, b)
	}
	in, err := merge(`{"to":"claude-02","body":"hi","limit":3,"artifact":["a.md=@` + file(t, "x") + `"]}`)
	if err != nil || in.Str("to") != "claude-02" || in.Str("body") != "hi" || in.Int("limit") != 3 || len(in.All("artifact")) != 1 {
		t.Fatalf("inline: %+v %v", in, err)
	}
	p := file(t, `{"to":"codex-01"}`)
	if in, err := merge("@" + p); err != nil || in.Str("to") != "codex-01" {
		t.Fatalf("@FILE: %+v %v", in, err)
	}
	in, err = c.parse([]string{"--input", "-"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := readInput(in.Str("input"), strings.NewReader(`{"to":"agy-03"}`))
	if in, err = c.mergeInput(in, b); err != nil || in.Str("to") != "agy-03" {
		t.Fatalf("stdin: %+v %v", in, err)
	}
	for raw, code := range map[string]fault.Code{
		`{"nope":1}`:            fault.Usage,
		`{"to":"a"} {"to":"b"}`: fault.Usage,
		`{"to":"../x"}`:         fault.InvalidInput,
		`{"limit":9}`:           fault.InvalidInput,
		`{"input":"{}"}`:        fault.Usage,
		`[1]`:                   fault.InvalidInput,
	} {
		if _, err := merge(raw); err == nil {
			t.Errorf("%s was merged", raw)
		} else if f, ok := fault.As(err); !ok || f.Code != code {
			t.Errorf("%s: %v, want %s", raw, err, code)
		}
	}
	if _, err := merge(`{"to":"codex-01"}`, "--to", "claude-02"); err == nil {
		t.Error("a key given on the command line and in --input was merged")
	}
}

// Durations take a Go duration or bare seconds, positive and at most a day.
func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"60": time.Minute, "90s": 90 * time.Second, "2m": 2 * time.Minute, "1.5": 1500 * time.Millisecond} {
		if d, err := ParseDuration(in); err != nil || d != want {
			t.Errorf("%q = %v, %v; want %v", in, d, err, want)
		}
	}
	for _, in := range []string{"", "abc", "-5", "0", "-1s", "25h"} {
		if d, err := ParseDuration(in); err == nil {
			t.Errorf("%q accepted as %v", in, d)
		}
	}
}

// A list's snippet is one clean line of at most n runes: control, ANSI,
// bidi, and zero-width characters gone, whitespace folded.
func TestSnippet(t *testing.T) {
	for in, want := range map[string]string{
		"hello\n\nworld":                 "hello world",
		"a\x1b[31mred\x1b[0m b":          "ared b",
		"x‮evil​!":                       "xevil!",
		strings.Repeat("é", 300):         strings.Repeat("é", 200),
		"  lead\ttab ":                   "lead tab",
		"bell\x07 and \x1b]0;title\x07x": "bell and x",
	} {
		if got := Snippet(in, 200); got != want {
			t.Errorf("Snippet(%q) = %q, want %q", in, got, want)
		}
	}
}

// A page is --limit items from the cursor's offset, says whether more
// remain, and its cursor works only for the listing it came from.
func TestPageAndCursor(t *testing.T) {
	c := &Command{Path: []string{"t"}, Flags: []Flag{{Name: "limit", Value: Value{Kind: KindInt, Min: 1, Max: 100}}, {Name: "cursor", Value: Value{Kind: KindCursor}}}}
	items := []int{1, 2, 3, 4, 5}
	in, _ := c.parse([]string{"--limit", "2"})
	p, err := Page(items, in, "a")
	if err != nil || len(p.Items) != 2 || p.Total != 5 || !p.Truncated || p.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", p, err)
	}
	in, _ = c.parse([]string{"--limit", "2", "--cursor", p.NextCursor})
	if p2, err := Page(items, in, "a"); err != nil || p2.Items[0] != 3 {
		t.Fatalf("page 2: %+v %v", p2, err)
	}
	if _, err := Page(items, in, "b"); err == nil {
		t.Error("a cursor of listing a paged listing b")
	}
	in, _ = c.parse(nil)
	if p, err := Page([]int(nil), in, "a"); err != nil || p.Items == nil || p.Total != 0 || p.Truncated {
		t.Errorf("empty: %+v %v", p, err)
	}
}

// --fields keeps the named fields of data or of each item; a field no item
// has is invalid_input only when the caller named it.
func TestProject(t *testing.T) {
	data := List[map[string]any]{Items: []map[string]any{{"a": 1, "b": 2}}, Total: 1}
	got, err := Project(data, []string{"a"}, true)
	var buf bytes.Buffer
	if err != nil || !strings.Contains(sprint(&buf, got), `"items":[{"a":1}]`) {
		t.Fatalf("items: %s %v", buf.String(), err)
	}
	if _, err := Project(data, []string{"zz"}, true); err == nil {
		t.Error("an unknown field the caller named was dropped silently")
	}
	if _, err := Project(data, []string{"zz"}, false); err != nil {
		t.Errorf("a default projection failed: %v", err)
	}
	if got, err := Project(map[string]any{"x": 1, "y": 2}, []string{"y"}, true); err != nil || sprint(&buf, got) != `{"y":2}` {
		t.Errorf("object: %v %v", got, err)
	}
}

func sprint(b *bytes.Buffer, v any) string {
	b.Reset()
	s, _ := marshalCompact(v)
	b.WriteString(s)
	return s
}

func marshalCompact(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// A command a signal cancelled answers with the signal's code, whatever it
// returned while it cleaned up; a stream that already wrote frames prints
// nothing more and exits with the signal's status.
func TestSignalledCommandAnswersWithTheSignal(t *testing.T) {
	for _, c := range []struct {
		code   fault.Code
		exit   int
		stream bool
	}{{fault.Terminated, 143, false}, {fault.Interrupted, 130, false}, {fault.Terminated, 143, true}} {
		cmd := &Command{Path: []string{"wait"}, Tier: TierRead, Examples: []string{"a", "b"}, Stream: c.stream,
			Run: func(ctx *Ctx, in Input) (Result, error) {
				<-ctx.Done()
				// What a command returns as it cleans up: here a finite failure.
				return Result{Streamed: c.stream}, fault.New(fault.NotAdopted, "gave up")
			}}
		r := &Registry{Commands: []*Command{cmd}}
		if err := r.Finish(); err != nil {
			t.Fatal(err)
		}
		var o, e bytes.Buffer
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(fault.New(c.code, "signal"))
		p := out.Printer{Streams: out.Streams{Stdout: &o, Stderr: &e}, Mode: out.Machine}
		code := r.Execute(&Ctx{Context: ctx, Getenv: func(string) string { return "" }, Out: p}, []string{"wait"})
		if code != c.exit || e.Len() != 0 {
			t.Errorf("%s stream=%v: exit %d stderr %q", c.code, c.stream, code, e.String())
		}
		switch {
		case c.stream && o.Len() != 0:
			t.Errorf("a stream printed after its signal: %q", o.String())
		case !c.stream && !strings.Contains(o.String(), `"code":"`+string(c.code)+`"`):
			t.Errorf("%s: envelope %q", c.code, o.String())
		}
	}
}
