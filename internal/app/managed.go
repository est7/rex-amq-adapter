package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// ManagedVerbs are the amq commands the adapter runs for an agent, gateway
// first. Their flags are amq's, without the ones that name an identity or a
// root: the adapter supplies those from the caller's record.
var ManagedVerbs = []string{"send", "reply", "read", "drain", "thread", "trace", "dlq", "list"}

// ErrUsage is a managed command given flags it does not take.
var ErrUsage = errors.New("usage")

// ManagedMail runs amq verb with args as the agent in loc, on the adapter's
// root, after the gateway allowed it. The exit status is amq's.
func (e Env) ManagedMail(ctx context.Context, loc Location, verb string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return 1, err
	}
	if !ok || !rec.Live() {
		return 1, errors.New("no AMQ identity for this pane")
	}
	m := managed{e: e, rec: rec, root: e.Paths.Root(), stdin: stdin, stdout: stdout, stderr: stderr}
	switch verb {
	case "send", "reply":
		return m.post(ctx, verb, args)
	case "read":
		return m.read(ctx, args)
	case "drain":
		return m.drain(ctx, args)
	case "thread":
		return m.thread(ctx, args)
	case "trace":
		return m.trace(ctx, args)
	case "dlq":
		return m.dlq(ctx, args)
	case "list":
		return m.list(ctx, args)
	}
	return 2, fmt.Errorf("%w: %q is not a managed mail command", ErrUsage, verb)
}

type managed struct {
	e              Env
	rec            store.Record
	root           string
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (m managed) me() string { return string(m.rec.Handle) }

// run runs amq verb on the adapter's root with args.
func (m managed) run(ctx context.Context, verb string, args ...any) (int, error) {
	argv := []string{verb, "--root", m.root}
	for _, a := range args {
		switch a := a.(type) {
		case string:
			argv = append(argv, a)
		case []string:
			argv = append(argv, a...)
		}
	}
	return m.e.AMQ.Run(ctx, m.stdin, m.stdout, m.stderr, argv...)
}

func (m managed) post(ctx context.Context, verb string, args []string) (int, error) {
	fs := newMailFlags(verb, m.stdout)
	var to, replyTo, control string
	if verb == "send" {
		fs.StringVar(&to, "to", "", "Receiver handles, comma-separated")
		fs.StringVar(&control, "control", "", "Narrator user-control request ID")
		fs.Bool("allow-self", false, "Allow an intentional send to yourself")
		fs.String("thread", "", "Thread ID")
		fs.String("refs", "", "Related message IDs, comma-separated")
	} else {
		fs.StringVar(&replyTo, "id", "", "Message ID to reply to")
	}
	body := fs.String("body", "", "Body string, @file, or - to read stdin")
	ctxObj := fs.String("context", "", "User JSON context object or @file")
	asJSON := fs.Bool("json", false, "Emit JSON output")
	fs.String("subject", "", "Message subject")
	fs.String("kind", "", "Message kind")
	fs.String("priority", "", "Message priority")
	fs.String("labels", "", "Labels, comma-separated")
	fs.Bool("allow-empty", false, "Allow a blank body")
	fs.Bool("strict", false, "Error on unknown handles")
	fs.String("wait-for", "", "Wait for a receipt stage")
	fs.Duration("wait-timeout", 2*time.Minute, "Receipt wait timeout")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	o := Outgoing{From: m.rec.Handle, Check: true, Op: core.Operation{Kind: core.OpSend}}
	if verb == "reply" {
		o.Op.Kind = core.OpReply
		if replyTo == "" {
			return 2, fmt.Errorf("%w: reply needs --id", ErrUsage)
		}
		o.ReplyTo = replyTo
	} else {
		if to == "" {
			return 2, fmt.Errorf("%w: send needs --to", ErrUsage)
		}
		if mailFlagPresent(fs, "control") {
			if control == "" {
				return 2, fmt.Errorf("%w: --control needs the directive's request id", ErrUsage)
			}
			if m.rec.Role != core.NarratorRole {
				return 1, fmt.Errorf("%w: only a narrator relays a control directive, and %s is %q", ErrRefused, m.rec.Handle, m.rec.Role)
			}
			o.Op = core.Operation{Kind: core.OpControl, ControlRequestID: control}
		}
		for _, h := range strings.Split(to, ",") {
			if h = strings.TrimSpace(h); h != "" {
				o.To = append(o.To, core.Handle(h))
			}
		}
		if len(o.To) == 0 {
			return 2, fmt.Errorf("%w: send needs at least one recipient in --to", ErrUsage)
		}
	}
	if mailFlagPresent(fs, "context") {
		if path, ok := strings.CutPrefix(*ctxObj, "@"); ok {
			b, err := os.ReadFile(path)
			if err != nil {
				return 1, err
			}
			*ctxObj = string(b)
		}
		o.Context = json.RawMessage(*ctxObj)
	}
	bodySet := mailFlagPresent(fs, "body")
	if !bodySet && terminalInput(m.stdin) {
		return 2, fmt.Errorf("%w: %s needs --body on a terminal; use --body - to read stdin explicitly", ErrUsage, verb)
	}
	if !bodySet || *body == "-" || *body == "" {
		var b []byte
		if m.stdin != nil {
			var err error
			b, err = io.ReadAll(m.stdin)
			if err != nil {
				return 1, err
			}
		}
		*body = string(b)
	}
	o.Args = append(mailFlagArgs(fs, "subject", "kind", "priority", "labels", "allow-empty", "allow-self", "strict", "thread", "refs", "wait-for", "wait-timeout"), "--body="+*body)
	sent, out, err := m.e.post(ctx, o)
	if err != nil {
		if len(out) > 0 {
			_, _ = m.stdout.Write(out)
		}
		return 1, err
	}
	if *asJSON {
		_, err = m.stdout.Write(out)
		return 0, err
	}
	past := map[string]string{"send": "Sent", "reply": "Replied"}[verb]
	_, err = fmt.Fprintf(m.stdout, "%s %s to %s\n", past, sent.ID, strings.Join(sent.To, ","))
	return 0, err
}

// readable finds message id in the caller's mailbox and lets the gateway
// decide on it. Mail still unread meets the arrival gate first.
func (m managed) readable(id string) (located, error) {
	if _, err := m.e.admit(m.rec); err != nil {
		return located{}, err
	}
	msg, ok, err := m.e.findMessage(m.rec.Handle, id)
	if err != nil {
		return located{}, err
	}
	if !ok {
		return located{}, fmt.Errorf("no message %s in the mailbox of %s (quarantined mail is not readable)", id, m.me())
	}
	return msg, m.e.exposed(m.rec.Handle, msg)
}

func (m managed) read(ctx context.Context, args []string) (int, error) {
	fs := newMailFlags("read", m.stdout)
	id := fs.String("id", "", "Message ID")
	fs.Bool("json", false, "Emit JSON output")
	fs.Bool("strict", false, "Error on unknown handles")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	if *id == "" {
		return 2, fmt.Errorf("%w: read needs --id", ErrUsage)
	}
	if _, err := m.readable(*id); err != nil {
		return 1, err
	}
	return m.run(ctx, "read", "--me", m.me(), "--id", *id, mailFlagArgs(fs, "json", "strict"))
}

// drain takes the caller's unread mail the arrival gate admitted, and only
// that: each kept message is read by its exact id (amq read moves it to
// inbox/cur and writes its drained receipt), so mail that arrives after the
// gate waits for the next check instead of being drained unjudged. Managed
// drains of one agent take turns (LockConsume). The output is amq drain's.
func (m managed) drain(ctx context.Context, args []string) (int, error) {
	fs := newMailFlags("drain", m.stdout)
	limit := fs.Int("limit", 20, "Max messages; 0 means no limit")
	withBody := fs.Bool("include-body", false, "Include message bodies")
	asJSON := fs.Bool("json", false, "Emit JSON output")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	if *limit < 0 {
		return 2, fmt.Errorf("%w: --limit must not be negative", ErrUsage)
	}
	unlock, err := m.e.Store.LockConsume(ctx, m.rec.Handle)
	if err != nil {
		return 1, err
	}
	defer unlock()
	kept, err := m.e.admit(m.rec)
	if err != nil {
		return 1, err
	}
	afterAdmit()
	// A read that fails part way leaves the messages before it drained
	// (moved to inbox/cur, receipted): they are reported before the error,
	// as amq drain reports a batch it committed.
	drained, code, err := m.consume(ctx, kept, *limit, *withBody)
	if perr := m.printDrained(drained, *withBody, *asJSON); err == nil {
		err = perr
	}
	return code, err
}

// afterAdmit runs between a managed drain's gate and its consumption; tests
// deliver mail there.
var afterAdmit = func() {}

// consume reads the messages ids (up to limit; 0 is no limit) still in the
// caller's inbox/new, and no other. On a failure it returns what it drained
// before it.
func (m managed) consume(ctx context.Context, ids []string, limit int, withBody bool) ([]map[string]any, int, error) {
	drained := []map[string]any{}
	for _, id := range ids {
		if limit > 0 && len(drained) >= limit {
			break
		}
		// Still unread: amq read also finds a copy a raw drain moved to
		// inbox/cur, which is not this drain's to report.
		if _, err := os.Stat(filepath.Join(amq.Inbox(m.root, m.me()), id+".md")); err != nil {
			continue
		}
		var out, errb bytes.Buffer
		code, err := m.e.AMQ.Run(ctx, nil, &out, &errb, "read", "--root", m.root, "--me", m.me(), "--id", id, "--json")
		if err != nil {
			return drained, 1, err
		}
		if code == 3 {
			continue // gone since
		}
		if code != 0 {
			_, _ = m.stderr.Write(errb.Bytes())
			return drained, code, fmt.Errorf("read %s", id)
		}
		var msg struct {
			Body   string         `json:"body"`
			Header map[string]any `json:"header"`
		}
		if err := json.Unmarshal(out.Bytes(), &msg); err != nil || msg.Header == nil {
			return drained, 1, fmt.Errorf("decode amq read of %s: %q", id, out.Bytes())
		}
		item := msg.Header
		delete(item, "schema")
		if withBody {
			item["body"] = msg.Body
		}
		item["moved_to_cur"] = true
		drained = append(drained, item)
	}
	return drained, 0, nil
}

// printDrained writes drained as amq drain does: JSON {drained, count}, or
// one block per message, and nothing when there is none.
func (m managed) printDrained(drained []map[string]any, withBody, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(map[string]any{"drained": drained, "count": len(drained)}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(m.stdout, "%s\n", b)
		return err
	}
	if len(drained) == 0 {
		return nil
	}
	field := func(item map[string]any, k string) string {
		if v, ok := item[k].(string); ok && v != "" {
			return v
		}
		return "-"
	}
	fmt.Fprintf(m.stdout, "[AMQ] %d new message(s) for %s:\n\n", len(drained), m.me())
	for _, it := range drained {
		fmt.Fprintf(m.stdout, "- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Created: %s\n",
			field(it, "from"), field(it, "thread"), field(it, "id"), field(it, "subject"), field(it, "priority"), field(it, "kind"), field(it, "created"))
		if withBody {
			fmt.Fprintf(m.stdout, "  Body:\n%s\n\n---\n", strings.TrimRight(field(it, "body"), "\n"))
		}
	}
	return nil
}

// trace joins amq's evidence for a message the caller may read, and shows
// only the evidence the gateway scopes to it (traceScope). amq trace takes
// the id before its flags.
func (m managed) trace(ctx context.Context, args []string) (int, error) {
	fs := newMailFlags("trace <message-id>", m.stdout)
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	asJSON := fs.Bool("json", false, "Emit JSON output")
	fs.Bool("follow", false, "Follow recipient delivery evidence")
	fs.String("from", "", "Sender whose outbox to inspect with --follow")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	if id == "" {
		return 2, fmt.Errorf("%w: trace needs a message id first", ErrUsage)
	}
	msg, err := m.readable(id)
	if err != nil {
		return 1, err
	}
	var out, errb bytes.Buffer
	code, err := m.e.AMQ.Run(ctx, nil, &out, &errb, append([]string{"trace", id, "--root", m.root, "--json"}, mailFlagArgs(fs, "follow", "from")...)...)
	if err != nil || code != 0 {
		_, _ = m.stderr.Write(errb.Bytes())
		return code, err
	}
	var doc any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		return 1, fmt.Errorf("decode amq trace: %w", err)
	}
	doc = traceScope{m: m, traced: msg}.filter(doc)
	if *asJSON {
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return 1, err
		}
		_, err = fmt.Fprintf(m.stdout, "%s\n", b)
		return 0, err
	}
	return 0, renderTrace(m.stdout, doc)
}

// thread shows the messages of a thread in the caller's own mailbox that
// the gateway lets it see, read by exact id.
func (m managed) thread(ctx context.Context, args []string) (int, error) {
	fs := newMailFlags("thread", m.stdout)
	thread := fs.String("id", "", "Thread ID")
	only := fs.String("message-ids", "", "Exact message IDs, comma-separated")
	limit := fs.Int("limit", 0, "Max messages; 0 means no limit")
	fs.Bool("include-body", false, "Include message bodies")
	fs.Bool("json", false, "Emit JSON output")
	fs.Bool("strict", false, "Error on unknown handles")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	if *thread == "" {
		return 2, fmt.Errorf("%w: thread needs --id", ErrUsage)
	}
	if *limit < 0 {
		return 2, fmt.Errorf("%w: --limit must not be negative", ErrUsage)
	}
	restricted := mailFlagPresent(fs, "message-ids")
	want := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		want[strings.TrimSpace(id)] = true
	}
	if _, err := m.e.admit(m.rec); err != nil {
		return 1, err
	}
	var ids []string
	for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained, amq.BoxSent} {
		boxIDs, err := amq.IDs(m.root, m.me(), box)
		if err != nil {
			return 1, err
		}
		for _, id := range boxIDs {
			if restricted && !want[id] {
				continue
			}
			h, err := amq.ReadHeader(m.root, m.me(), box, id)
			if err != nil || h.Thread != *thread {
				continue
			}
			if m.e.exposed(m.rec.Handle, located{id: id, box: box, header: h}) == nil && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return 3, fmt.Errorf("no message of thread %s that %s may read", *thread, m.me())
	}
	return m.run(ctx, "thread", "--me", m.me(), "--id", *thread, "--agents", m.me(), "--message-ids", strings.Join(ids, ","), mailFlagArgs(fs, "limit", "include-body", "json", "strict"))
}

// dlq lets list, retry, and purge through: the list holds no sender or
// subject, and a retried message meets the arrival gate again. read shows a
// dead letter's body, so the gateway judges its original header; one whose
// header cannot be read names no sender and is refused.
func (m managed) dlq(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("%w: dlq needs a subcommand (list, read, retry, purge)", ErrUsage)
	}
	if args[0] == "-h" || args[0] == "--help" {
		_, err := parseMailFlags(newMailFlags("dlq <list|read|retry|purge>", m.stdout), args)
		if err != nil {
			return 2, err
		}
		return 0, nil
	}
	sub, args := args[0], args[1:]
	fs := newMailFlags("dlq "+sub, m.stdout)
	var id string
	switch sub {
	case "list":
		fs.Bool("new", false, "List unread dead letters")
		fs.Bool("cur", false, "List inspected dead letters")
	case "read", "retry":
		fs.StringVar(&id, "id", "", "Dead letter ID")
		if sub == "retry" {
			fs.Bool("all", false, "Retry all dead letters")
			fs.Bool("force", false, "Retry beyond max retries")
		}
	case "purge":
		fs.String("older-than", "", "Only purge dead letters older than this duration")
		fs.Bool("dry-run", false, "Show what would be removed")
		fs.Bool("yes", false, "Skip confirmation")
	default:
		return 2, fmt.Errorf("%w: unknown dlq subcommand %q", ErrUsage, sub)
	}
	fs.Bool("json", false, "Emit JSON output")
	fs.Bool("strict", false, "Error on unknown handles")
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	if sub == "read" {
		if id == "" {
			return 2, fmt.Errorf("%w: dlq read needs --id", ErrUsage)
		}
		var hdr amq.Header
		var err error
		for _, box := range []amq.Box{amq.BoxDLQNew, amq.BoxDLQCur} {
			if hdr, err = amq.ReadDLQHeader(m.root, m.me(), box, id); err == nil {
				break
			}
		}
		if err != nil {
			return 1, fmt.Errorf("%w: dead letter %s names no sender the gateway can judge: %v", ErrRefused, id, err)
		}
		if err := m.e.exposed(m.rec.Handle, located{id: id, box: amq.BoxDLQNew, header: hdr}); err != nil {
			return 1, err
		}
	}
	child := append([]string{"dlq", sub, "--root", m.root, "--me", m.me()}, mailFlagArgs(fs, "id", "new", "cur", "all", "force", "older-than", "dry-run", "yes", "json", "strict")...)
	return m.e.AMQ.Run(ctx, m.stdin, m.stdout, m.stderr, child...)
}

// list lists the caller's inbox as JSON, without the messages the gateway
// would not let it read.
func (m managed) list(ctx context.Context, args []string) (int, error) {
	fs := newMailFlags("list", m.stdout)
	fs.Bool("json", false, "Emit JSON output")
	fs.Bool("new", false, "List unread messages")
	fs.Bool("cur", false, "List drained messages")
	fs.Bool("strict", false, "Error on unknown handles")
	fs.String("from", "", "Filter by sender")
	fs.String("kind", "", "Filter by kind")
	fs.String("priority", "", "Filter by priority")
	fs.Int("limit", 0, "Max messages; 0 means no limit")
	fs.Int("offset", 0, "Offset into sorted results")
	var labels []string
	fs.Func("label", "Filter by label; repeatable", func(label string) error { labels = append(labels, label); return nil })
	if help, err := parseMailFlags(fs, args); help || err != nil {
		if help {
			return 0, nil
		}
		return 2, err
	}
	forwarded := mailFlagArgs(fs, "new", "cur", "strict", "from", "kind", "priority", "limit", "offset")
	for _, label := range labels {
		forwarded = append(forwarded, "--label="+label)
	}
	if _, err := m.e.admit(m.rec); err != nil {
		return 1, err
	}
	var out, errb bytes.Buffer
	code, err := m.e.AMQ.Run(ctx, nil, &out, &errb, append([]string{"list", "--root", m.root, "--me", m.me(), "--json"}, forwarded...)...)
	if err != nil || code != 0 {
		_, _ = m.stderr.Write(errb.Bytes())
		return code, err
	}
	var entries []map[string]any
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		return 1, fmt.Errorf("decode amq list: %w", err)
	}
	shown := []map[string]any{}
	for _, en := range entries {
		id, _ := en["id"].(string)
		box := amq.BoxDrained
		if en["box"] == "new" {
			box = amq.BoxUnread
		}
		h, err := amq.ReadHeader(m.root, m.me(), box, id)
		if err != nil || m.e.exposed(m.rec.Handle, located{id: id, box: box, header: h}) != nil {
			continue
		}
		shown = append(shown, en)
	}
	b, err := json.MarshalIndent(shown, "", "  ")
	if err != nil {
		return 1, err
	}
	_, err = fmt.Fprintf(m.stdout, "%s\n", b)
	return 0, err
}
