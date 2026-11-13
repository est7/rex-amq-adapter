package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func TestManagedMailRejectsHiddenRoutingOverrides(t *testing.T) {
	for _, verb := range []string{"send", "reply"} {
		t.Run(verb, func(t *testing.T) {
			e, ctx := managedWorld(t)
			put(t, e, store.Record{Handle: "claude-10", SessionID: "session:b"})
			if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "claude-10"); err != nil {
				t.Fatal(err)
			}
			args := []string{"--to", "codex-01"}
			if verb == "reply" {
				for _, pair := range [][2]string{{"codex-01", "claude-02"}, {"claude-09", "claude-10"}} {
					if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), pair[0], pair[1], "request", "body"); err != nil {
						t.Fatal(err)
					}
				}
				local, err := amq.Unread(e.Paths.Root(), "claude-02")
				if err != nil || len(local) != 1 {
					t.Fatalf("local request: %v %v", local, err)
				}
				foreign, err := amq.Unread(e.Paths.Root(), "claude-10")
				if err != nil || len(foreign) != 1 {
					t.Fatalf("foreign request: %v %v", foreign, err)
				}
				args = []string{"--id", local[0]}
				args = append(args, "--body", "override-regression", "--subject", "--", "--me", "claude-10", "--id", foreign[0], "--context", "{}", "--json=false")
			} else {
				args = append(args, "--body", "override-regression", "--subject", "--", "--me", "claude-10", "--to", "claude-09", "--context", "{}", "--json=false")
			}
			out, code, err := run(t, e, ctx, "claude-02", verb, args...)
			if code != 2 || !errors.Is(err, ErrUsage) {
				t.Errorf("routing override must be usage error: code=%d err=%v out=%q", code, err, out)
			}
			for _, h := range []string{"codex-01", "claude-09"} {
				ids, err := amq.Unread(e.Paths.Root(), h)
				if err != nil || len(ids) != 0 {
					t.Errorf("invalid %s delivered to %s: %v %v", verb, h, ids, err)
				}
			}
		})
	}
}

func TestManagedMailRejectsUnknownFlagsBeforeEffects(t *testing.T) {
	e, ctx := managedWorld(t)
	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"send", []string{"--to", "codex-01", "--body", "body"}},
		{"reply", []string{"--id", "missing", "--body", "body"}},
		{"read", []string{"--id", "missing"}},
		{"drain", nil}, {"thread", []string{"--id", "thread"}},
		{"trace", []string{"missing"}}, {"list", nil}, {"dlq", []string{"list"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "--not-an-amq-flag")
			if _, code, err := run(t, e, ctx, "claude-02", tc.verb, args...); code != 2 || !errors.Is(err, ErrUsage) {
				t.Errorf("unknown flag: %d %v", code, err)
			}
		})
	}
	if ids, err := amq.Unread(e.Paths.Root(), "codex-01"); err != nil || len(ids) != 0 {
		t.Fatalf("invalid command delivered mail: %v %v", ids, err)
	}
}

type observedInput struct {
	read   bool
	fd     uintptr
	source io.Reader
}

func (r *observedInput) Read(p []byte) (int, error) {
	r.read = true
	if r.source != nil {
		return r.source.Read(p)
	}
	return 0, errors.New("stdin must not be read")
}
func (r *observedInput) Fd() uintptr { return r.fd }

func TestManagedMailHelpNeverReadsStdin(t *testing.T) {
	e, ctx := managedWorld(t)
	for _, verb := range []string{"send", "reply", "read", "drain", "thread", "trace", "dlq", "list"} {
		for _, help := range []string{"-h", "--help"} {
			t.Run(verb+help, func(t *testing.T) {
				in := &observedInput{}
				var out bytes.Buffer
				code, err := e.ManagedMail(ctx, Location{Block: "block:claude-02"}, verb, []string{help}, in, &out, &out)
				if code != 0 || err != nil || in.read || !strings.Contains(out.String(), "Usage") {
					t.Fatalf("help: code=%d err=%v read=%v out=%q", code, err, in.read, out.String())
				}
			})
		}
	}
}

func TestManagedMailImplicitTerminalBodyIsUsageError(t *testing.T) {
	e, ctx := managedWorld(t)
	terminal := managedTerminal(t)
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-01", "claude-02", "request", "body"); err != nil {
		t.Fatal(err)
	}
	ids, err := amq.Unread(e.Paths.Root(), "claude-02")
	if err != nil || len(ids) != 1 {
		t.Fatalf("request: %v %v", ids, err)
	}
	for _, tc := range []struct {
		verb string
		args []string
	}{{"send", []string{"--to", "codex-01"}}, {"reply", []string{"--id", ids[0]}}} {
		t.Run(tc.verb, func(t *testing.T) {
			in := &observedInput{fd: terminal.Fd()}
			var out bytes.Buffer
			code, err := e.ManagedMail(ctx, Location{Block: "block:claude-02"}, tc.verb, tc.args, in, &out, &out)
			if code != 2 || !errors.Is(err, ErrUsage) || in.read {
				t.Fatalf("implicit terminal body: code=%d err=%v read=%v", code, err, in.read)
			}
		})
	}
}

func TestManagedMailExplicitStdinBodyStillReads(t *testing.T) {
	e, ctx := managedWorld(t)
	terminal := managedTerminal(t)
	for _, body := range []string{"-", ""} {
		in := &observedInput{fd: terminal.Fd(), source: strings.NewReader("explicit stdin")}
		var out bytes.Buffer
		code, err := e.ManagedMail(ctx, Location{Block: "block:claude-02"}, "send", []string{"--to", "codex-01", "--body", body}, in, &out, &out)
		if code != 0 || err != nil || !in.read {
			t.Fatalf("explicit stdin body %q: %d %v %s read=%v", body, code, err, out.String(), in.read)
		}
	}
}

func TestManagedMailRejectsRoutingFlagsAndOperands(t *testing.T) {
	e, ctx := managedWorld(t)
	for _, tc := range []struct {
		verb string
		args []string
	}{
		{"send", []string{"--to", "codex-01", "--body", "body", "--subject", "--"}},
		{"reply", []string{"--id", "missing", "--body", "body", "--subject", "--"}},
		{"read", []string{"--id", "--"}}, {"drain", nil},
		{"thread", []string{"--id", "--"}}, {"trace", []string{"missing", "--from", "--"}},
		{"list", []string{"--from", "--"}}, {"dlq", []string{"list"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			for _, bad := range [][]string{
				{"--me=claude-09"}, {"--root=/nonexistent"}, {"--session=foreign"}, {"--project=foreign"},
				{"--from-session=foreign"}, {"--ignore-session-pin"}, {"--", "--me=claude-09"}, {"unparsed-operand"},
			} {
				args := append(append([]string{}, tc.args...), bad...)
				if _, code, err := run(t, e, ctx, "claude-02", tc.verb, args...); code != 2 || !errors.Is(err, ErrUsage) {
					t.Errorf("%q: code=%d err=%v", args, code, err)
				}
			}
		})
	}
	if ids, err := amq.Unread(e.Paths.Root(), "codex-01"); err != nil || len(ids) != 0 {
		t.Fatalf("invalid syntax sent mail: %v %v", ids, err)
	}
}

func TestManagedMailFlagValuesRemainLiteral(t *testing.T) {
	e, ctx := managedWorld(t)
	needsAmq(t, e, "amq's read verb")
	out, code, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--subject", "--", "--body", "--me=claude-09", "--json=false", "--context", `{"note":"--root=/foreign"}`)
	if code != 0 || err != nil || !strings.HasPrefix(out, "Sent ") {
		t.Fatalf("literal values: %d %v %q", code, err, out)
	}
	ids, err := amq.Unread(e.Paths.Root(), "codex-01")
	if err != nil || len(ids) != 1 {
		t.Fatalf("local delivery: %v %v", ids, err)
	}
	h, err := amq.ReadHeader(e.Paths.Root(), "codex-01", amq.BoxUnread, ids[0])
	if err != nil || h.From != "claude-02" || h.Subject != "--" {
		t.Fatalf("header changed: %+v %v", h, err)
	}
	var note string
	if !h.ContextOf("note", &note) || note != "--root=/foreign" {
		t.Fatalf("context changed: %v", h.Context)
	}
	var read bytes.Buffer
	code, err = e.AMQ.Run(ctx, nil, &read, &read, "read", "--root", e.Paths.Root(), "--me", "codex-01", "--id", ids[0], "--json")
	if code != 0 || err != nil || !strings.Contains(read.String(), "--me=claude-09") {
		t.Fatalf("body changed: %d %v %s", code, err, read.String())
	}
	if p, ok, err := e.Store.ProvenanceOf(ids[0], "", "", nil); err != nil || !ok || p.From.Handle != "claude-02" || len(p.To) != 1 || p.To[0].Handle != "codex-01" {
		t.Fatalf("provenance changed: %+v %v %v", p, ok, err)
	}
}

func TestManagedMailRejectsEmptyRecipientListBeforeProvenance(t *testing.T) {
	for _, to := range []string{"", ",", " ", " , \t , "} {
		t.Run(to, func(t *testing.T) {
			e, ctx := managedWorld(t)
			out, code, err := run(t, e, ctx, "claude-02", "send", "--to", to, "--body", "must not be sent")
			if code != 2 || !errors.Is(err, ErrUsage) {
				t.Errorf("empty recipients: code=%d err=%v out=%q", code, err, out)
			}
			pending := filepath.Join(e.Paths.Extensions(), "provenance", "pending")
			entries, err := os.ReadDir(pending)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("invalid recipients created pending provenance: %v", entries)
			}
		})
	}
}
