// Package out is the only writer of the process's stdout. Every public
// command's result leaves through Printer.Emit as one v:2 envelope (machine
// mode) or as text for a person (human mode); a stream's frames through
// Printer.Frames; and the four internal callbacks' protocol bytes through
// HookReply, PaneTitle, InjectProgress, and WatchFailed. TestStdoutSingleWriter
// fails the build of any other package that writes stdout.
package out

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/fault"
)

// Version is the envelope's "v".
const Version = 2

// Streams is the process's stdio as the CLI uses it.
type Streams struct {
	Stdout, Stderr io.Writer
	// StdoutTTY is a real isatty(stdout): /dev/null and pipes are not.
	StdoutTTY bool
}

// OS is the process's own stdio: the one place os.Stdout is named.
func OS() Streams {
	return Streams{Stdout: os.Stdout, Stderr: os.Stderr, StdoutTTY: isTerminal(os.Stdout.Fd())}
}

// Mode is how a command's result is printed.
type Mode int

const (
	// Machine prints exactly one JSON envelope line on stdout and nothing on
	// stderr, failures and panics included.
	Machine Mode = iota
	// Human prints data on stdout and diagnostics on stderr, without color.
	Human
)

// SelectMode picks the mode: --json is machine; --text, or a terminal on
// stdout without --json, is human; anything else (a pipe, a file,
// /dev/null) is machine.
func SelectMode(asJSON, asText, stdoutTTY bool) Mode {
	switch {
	case asJSON:
		return Machine
	case asText, stdoutTTY:
		return Human
	}
	return Machine
}

// Meta is the envelope's meta object.
type Meta struct {
	Verb     string `json:"verb"`
	TraceID  string `json:"trace_id,omitempty"`
	Complete *bool  `json:"complete,omitempty"`
}

type envelope struct {
	OK       bool         `json:"ok"`
	V        int          `json:"v"`
	Data     any          `json:"data"`
	Error    *fault.Error `json:"error,omitempty"`
	Meta     Meta         `json:"meta"`
	Warnings []string     `json:"warnings"`
}

// Response is one command's outcome. Err with Data set is a partial result
// (exit 7): the envelope says ok:false and still carries the data.
type Response struct {
	Data     any
	Err      *fault.Error
	Warnings []string
	// Incomplete marks a non-atomic batch some of whose items failed
	// (meta.complete=false).
	Incomplete bool
	// Human renders Data for a person; nil prints it as indented JSON.
	Human func(w io.Writer) error
}

// Printer prints the results of one invocation.
type Printer struct {
	Streams
	Mode    Mode
	Verb    string
	TraceID string
}

// TraceID is the caller's trace id, from the environment only.
func TraceID(getenv func(string) string) string {
	if v := getenv("REX_AMQ_ADAPTER_TRACE_ID"); v != "" {
		return v
	}
	return getenv("AGENT_TRACE_ID")
}

// Emit prints r and returns the process exit status.
func (p Printer) Emit(r Response) int {
	code := 0
	if r.Err != nil {
		code = r.Err.Exit()
	}
	if p.Mode == Machine {
		warnings := r.Warnings
		if warnings == nil {
			warnings = []string{}
		}
		meta := Meta{Verb: p.Verb, TraceID: p.TraceID}
		if r.Incomplete {
			f := false
			meta.Complete = &f
		}
		env := envelope{OK: r.Err == nil, V: Version, Data: r.Data, Error: r.Err, Meta: meta, Warnings: warnings}
		b, err := marshalLine(env)
		if err != nil {
			b, _ = marshalLine(envelope{V: Version, Error: fault.New(fault.Internal, "encode the result: %v", err), Meta: meta, Warnings: warnings})
			code = 1
		}
		_, _ = p.Stdout.Write(b)
		return code
	}
	if r.Data != nil {
		if r.Human != nil {
			if err := r.Human(p.Stdout); err != nil {
				p.diag("print: %v", err)
			}
		} else {
			b, _ := json.MarshalIndent(r.Data, "", "  ")
			fmt.Fprintf(p.Stdout, "%s\n", b)
		}
	}
	for _, w := range r.Warnings {
		p.diag("warning: %s", w)
	}
	if r.Err != nil {
		p.diag("%s", r.Err.Message)
		if r.Err.Hint != "" {
			p.diag("hint: %s", r.Err.Hint)
		}
		if r.Err.NextAction != "" {
			p.diag("next: %s", r.Err.NextAction)
		}
	}
	return code
}

// Fail prints err as the invocation's failure.
func (p Printer) Fail(err *fault.Error) int { return p.Emit(Response{Err: err}) }

// diag writes one human diagnostic line to stderr, its control characters
// made visible so mail text cannot drive the terminal.
func (p Printer) diag(format string, a ...any) {
	fmt.Fprintf(p.Stderr, "rex-amq-adapter: %s\n", Visible(fmt.Sprintf(format, a...)))
}

// Frames is where a stream writes its NDJSON frames once connected.
func (p Printer) Frames() io.Writer { return p.Stdout }

// Raw writes a document command's markdown in human mode.
func (p Printer) Raw(markdown string) {
	if !strings.HasSuffix(markdown, "\n") {
		markdown += "\n"
	}
	_, _ = io.WriteString(p.Stdout, markdown)
}

// Warnings collects lines written to it as envelope warnings (machine mode),
// or forwards them to stderr (human mode). The adapter's Diagnostics goes
// here, so housekeeping never puts bytes on a machine caller's stderr.
type Warnings struct {
	p     Printer
	lines []string
	part  string
}

// NewWarnings is a collector for p.
func NewWarnings(p Printer) *Warnings { return &Warnings{p: p} }

func (w *Warnings) Write(b []byte) (int, error) {
	s := w.part + string(b)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		w.add(s[:i])
		s = s[i+1:]
	}
	w.part = s
	return len(b), nil
}

// Add records one warning.
func (w *Warnings) Add(line string) { w.add(line) }

func (w *Warnings) add(line string) {
	if line = strings.TrimSpace(line); line == "" {
		return
	}
	if w.p.Mode == Human {
		w.p.diag("%s", line)
		return
	}
	w.lines = append(w.lines, line)
}

// Lines are the warnings collected so far (machine mode).
func (w *Warnings) Lines() []string {
	if w.part != "" {
		w.add(w.part)
		w.part = ""
	}
	return w.lines
}

func marshalLine(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// HookReply writes a harness hook's reply, in that harness's own format.
func HookReply(s Streams, reply string) { _, _ = io.WriteString(s.Stdout, reply) }

// PaneTitle answers a harness that asks for its window title: one line.
func PaneTitle(s Streams, title string) { fmt.Fprintln(s.Stdout, title) }

// InjectProgress is amq wake's injector protocol: one AMQ_INJECT_PROGRESS
// line on stderr, and the reason on a second when there is one. stdout
// stays empty.
func InjectProgress(s Streams, progress, code, note string) {
	fmt.Fprintf(s.Stderr, "AMQ_INJECT_PROGRESS=%s\n", progress)
	if code != "" {
		fmt.Fprintf(s.Stderr, "rex-amq-adapter: %s: %s\n", code, note)
	}
}

// WatchFailed reports a session watcher's failure: the watcher runs
// detached, so its stdout carries nothing.
func WatchFailed(s Streams, err error) {
	fmt.Fprintf(s.Stderr, "rex-amq-adapter: %v\n", err)
}

// Visible is s with every C0/C1 control character but tab and newline, and
// every bidi control, shown as an escape, so printing it cannot move the
// cursor, color the terminal, or reorder text.
func Visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || bidi(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func bidi(r rune) bool { return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 }

var secretRe = regexp.MustCompile(`(?i)((?:token|secret|password|passwd|api[_-]?key|authorization|REX_AMQ_SUMMON)\s*[=:]\s*)("[^"]*"|\S+)`)

// Redact hides values that look like secrets (token=…, password: …) in s,
// for crash dumps.
func Redact(s string) string { return secretRe.ReplaceAllString(s, "${1}<redacted>") }
