package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/blob"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

// The mail verbs run amq as the caller through the gateway
// (app.Env.ManagedMail), always with --json, and return what amq printed
// as data: bodies capped, list views bounded and reduced to snippets.

// mailbox is the caller's Env, location, and a way to run one managed verb.
type mailbox struct {
	env app.Env
	loc app.Location
	c   *cli.Ctx
}

func openMailbox(c *cli.Ctx) (mailbox, error) {
	env, loc, err := caller(c)
	if err != nil {
		return mailbox{}, err
	}
	return mailbox{env: env, loc: loc, c: c}, nil
}

// run runs managed verb with args and decodes amq's JSON output into v.
func (m mailbox) run(verb string, args []string, v any) error {
	return m.runIn(verb, args, "", v)
}

// runIn is run with stdin: a resolved body travels there, under --body -,
// so no layer below reads its text as @FILE or - again.
func (m mailbox) runIn(verb string, args []string, stdin string, v any) error {
	ctx, cancel := within(m.c, 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := m.env.ManagedMail(ctx, m.loc, verb, args, strings.NewReader(stdin), &stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if code == 3 {
			return fault.New(fault.NotFound, "amq %s: %s", verb, msg).With("kind", "message")
		}
		return fault.New(fault.Internal, "amq %s exited %d: %s", verb, code, msg)
	}
	if v == nil {
		return nil
	}
	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 {
		raw = []byte("null")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fault.New(fault.Internal, "decode amq %s: %v", verb, err)
	}
	return nil
}

// capBody bounds obj[key] to max bytes, spilling the whole body to the
// blob cache, and records truncated, full_path, and total_bytes beside it.
func (m mailbox) capBody(obj map[string]any, key string, max int) error {
	body, ok := obj[key].(string)
	if !ok {
		return nil
	}
	capped, err := blob.Cap(m.env.Paths.Blobs(), body, max)
	if err != nil {
		return err
	}
	obj[key], obj["truncated"], obj["total_bytes"] = capped.Body, capped.Truncated, capped.TotalBytes
	if capped.FullPath != "" {
		obj["full_path"] = capped.FullPath
	}
	return nil
}

// postArgs are a send's or reply's flags; the body goes on stdin (runIn).
func postArgs(in cli.Input) []string {
	var args []string
	for _, k := range []string{"subject", "kind", "priority", "thread", "refs", "control", "op-id"} {
		if v := in.Str(k); v != "" {
			args = append(args, "--"+k, v)
		}
	}
	return append(args, "--body=-", "--json")
}

func mailSend(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	var to []core.Handle
	for _, h := range strings.Split(in.Str("to"), ",") {
		if !slices.Contains(to, core.Handle(h)) {
			to = append(to, core.Handle(h))
		}
	}
	// amq refuses several recipients on its default p2p thread, and the
	// adapter never invents a thread name, so say so before anything runs.
	if len(to) > 1 && in.Str("thread") == "" {
		return cli.Result{}, fault.New(fault.Usage, "--to names %d recipients, and a send to several needs --thread <name> (one thread they all share)", len(to))
	}
	body, err := cli.ReadBody(in.Str("body"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	if in.DryRun() {
		p, err := m.env.PreviewPost(m.loc, to, "", in.Str("control"))
		return cli.Result{Data: map[string]any{"dry_run": true, "would": p, "subject": in.Str("subject"), "body_bytes": len(body)}}, err
	}
	var sent map[string]any
	names := make([]string, len(to))
	for i, h := range to {
		names[i] = string(h)
	}
	if err := m.runIn("send", append([]string{"--to", strings.Join(names, ",")}, postArgs(in)...), body, &sent); err != nil {
		return cli.Result{}, err
	}
	if sent["id"] == nil && sent["inbox_seq"] != nil {
		return cli.Result{Data: sent, Human: line(fmt.Sprintf("Kept in the Hub's inbox as #%v (routed to %v)", sent["inbox_seq"], sent["routed_to"]))}, nil
	}
	return cli.Result{Data: sent, Human: line(fmt.Sprintf("Sent %v to %v", sent["id"], sent["to"]))}, nil
}

func mailReply(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	body, err := cli.ReadBody(in.Str("body"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	if in.DryRun() {
		p, err := m.env.PreviewPost(m.loc, nil, in.Str("id"), "")
		return cli.Result{Data: map[string]any{"dry_run": true, "would": p, "body_bytes": len(body)}}, err
	}
	var sent map[string]any
	if err := m.runIn("reply", append([]string{"--id", in.Str("id")}, postArgs(in)...), body, &sent); err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: sent, Human: line(fmt.Sprintf("Replied %v to %v", sent["id"], sent["to"]))}, nil
}

func mailRead(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	var msg map[string]any
	if err := m.run("read", []string{"--id", in.Str("id"), "--json"}, &msg); err != nil {
		return cli.Result{}, err
	}
	if msg == nil {
		return cli.Result{}, fault.New(fault.NotFound, "no message %s", in.Str("id")).With("kind", "message")
	}
	if err := m.capBody(msg, "body", in.Int("max-body-bytes")); err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: msg, Human: messageText(msg)}, nil
}

// messageText renders one message for a person: its header lines, then its
// body, every control character made visible.
func messageText(msg map[string]any) func(io.Writer) error {
	return func(w io.Writer) error {
		if h, ok := msg["header"].(map[string]any); ok {
			for _, k := range []string{"id", "from", "to", "thread", "subject", "kind", "priority", "created"} {
				if v, ok := h[k]; ok {
					fmt.Fprintf(w, "%s: %s\n", k, out.Visible(fmt.Sprint(v)))
				}
			}
		}
		fmt.Fprintf(w, "\n%s\n", out.Visible(fmt.Sprint(msg["body"])))
		if msg["truncated"] == true {
			fmt.Fprintf(w, "[truncated: %v bytes in all; the whole body is in %v]\n", msg["total_bytes"], msg["full_path"])
		}
		return nil
	}
}

func mailDrain(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	args := []string{"--limit", strconv.Itoa(in.Int("limit")), "--json"}
	if in.Bool("include-body") {
		args = append(args, "--include-body")
	}
	var res struct {
		Drained []map[string]any `json:"drained"`
		Count   int              `json:"count"`
	}
	if err := m.run("drain", args, &res); err != nil {
		return cli.Result{}, err
	}
	if res.Drained == nil {
		res.Drained = []map[string]any{}
	}
	for _, it := range res.Drained {
		if err := m.capBody(it, "body", in.Int("max-body-bytes")); err != nil {
			return cli.Result{}, err
		}
	}
	data := cli.List[map[string]any]{Items: res.Drained, Total: len(res.Drained), Truncated: len(res.Drained) >= in.Int("limit")}
	return cli.Result{Data: data, Human: drainedText(m, res.Drained, in.Bool("include-body"))}, nil
}

// drainedText renders drained mail as the doorbell's reader expects it.
func drainedText(m mailbox, drained []map[string]any, withBody bool) func(io.Writer) error {
	return func(w io.Writer) error {
		if len(drained) == 0 {
			return nil
		}
		field := func(it map[string]any, k string) string {
			if v, ok := it[k].(string); ok && v != "" {
				return out.Visible(v)
			}
			return "-"
		}
		fmt.Fprintf(w, "[AMQ] %d new message(s):\n\n", len(drained))
		for _, it := range drained {
			fmt.Fprintf(w, "- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Created: %s\n",
				field(it, "from"), field(it, "thread"), field(it, "id"), field(it, "subject"), field(it, "priority"), field(it, "kind"), field(it, "created"))
			if withBody {
				fmt.Fprintf(w, "  Body:\n%s\n", strings.TrimRight(field(it, "body"), "\n"))
				if it["truncated"] == true {
					fmt.Fprintf(w, "  [truncated: the whole body is in %v]\n", it["full_path"])
				}
				fmt.Fprint(w, "\n---\n")
			}
		}
		return nil
	}
}

func mailList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	if in.Bool("new") && in.Bool("cur") {
		return cli.Result{}, cli.Usage(c.Cmd, "--new and --cur pick opposite boxes: give one, or neither for both")
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	var args []string
	for _, k := range []string{"new", "cur"} {
		if in.Bool(k) {
			args = append(args, "--"+k)
		}
	}
	for _, k := range []string{"from", "kind"} {
		if v := in.Str(k); v != "" {
			args = append(args, "--"+k, v)
		}
	}
	var entries []map[string]any
	if err := m.run("list", args, &entries); err != nil {
		return cli.Result{}, err
	}
	me := m.me()
	root := m.env.Paths.Root()
	items := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		id, _ := en["id"].(string)
		box := amq.BoxDrained
		if en["box"] == "new" {
			box = amq.BoxUnread
		}
		item := map[string]any{"snippet": ""}
		for _, k := range []string{"id", "from", "subject", "thread", "created", "box", "kind", "priority"} {
			if v, ok := en[k]; ok {
				item[k] = v
			}
		}
		if prefix, err := amq.BodyPrefix(root, me, box, id, 4096); err == nil {
			item["snippet"] = cli.Snippet(prefix, 200)
		}
		items = append(items, item)
	}
	scope := fmt.Sprintf("mail list|%s|new=%v|cur=%v|from=%s|kind=%s", me, in.Bool("new"), in.Bool("cur"), in.Str("from"), in.Str("kind"))
	page, err := cli.Page(items, in, scope)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: page}, nil
}

// me is the caller's handle, from its mailbox root's view.
func (m mailbox) me() string {
	se, err := m.env.ShellEnvOf(m.loc)
	if err != nil {
		return ""
	}
	return se.AMMe
}

func mailThread(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	args := []string{"--id", in.Str("id"), "--limit", strconv.Itoa(in.Int("limit")), "--json"}
	if in.Bool("include-body") {
		args = append(args, "--include-body")
	}
	var msgs []map[string]any
	if err := m.run("thread", args, &msgs); err != nil {
		return cli.Result{}, err
	}
	if msgs == nil {
		msgs = []map[string]any{}
	}
	for _, it := range msgs {
		if !in.Bool("include-body") {
			delete(it, "body")
			continue
		}
		if err := m.capBody(it, "body", in.Int("max-body-bytes")); err != nil {
			return cli.Result{}, err
		}
	}
	return cli.Result{Data: cli.List[map[string]any]{Items: msgs, Total: len(msgs), Truncated: len(msgs) >= in.Int("limit")}}, nil
}

func mailTrace(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	var doc any
	if err := m.run("trace", []string{in.Arg("id"), "--json"}, &doc); err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"id": in.Arg("id"), "trace": doc}}, nil
}

func mailDLQList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	if in.Bool("new") && in.Bool("cur") {
		return cli.Result{}, cli.Usage(c.Cmd, "--new and --cur pick opposite boxes: give one, or neither for both")
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	args := []string{"list", "--json"}
	for _, k := range []string{"new", "cur"} {
		if in.Bool(k) {
			args = append(args, "--"+k)
		}
	}
	var items []map[string]any
	if err := m.run("dlq", args, &items); err != nil {
		return cli.Result{}, err
	}
	page, err := cli.Page(items, in, fmt.Sprintf("mail dlq list|%s|new=%v|cur=%v", m.me(), in.Bool("new"), in.Bool("cur")))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: page}, nil
}

func mailDLQRead(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	var doc map[string]any
	if err := m.run("dlq", []string{"read", "--id", in.Str("id"), "--json"}, &doc); err != nil {
		return cli.Result{}, err
	}
	if doc != nil {
		if err := m.capBody(doc, "body", blob.DefaultMax); err != nil {
			return cli.Result{}, err
		}
	}
	return cli.Result{Data: doc}, nil
}

func mailDLQRetry(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	if (in.Str("id") == "") == !in.Bool("all") {
		return cli.Result{}, cli.Usage(c.Cmd, "name one dead letter with --id, or every one with --all")
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	if in.DryRun() {
		target := in.Str("id")
		if in.Bool("all") {
			target = "all"
		}
		return cli.Result{Data: map[string]any{"dry_run": true, "would": "retry", "target": target, "handle": m.me()}}, nil
	}
	args := []string{"retry", "--json"}
	if id := in.Str("id"); id != "" {
		args = append(args, "--id", id)
	}
	for _, k := range []string{"all", "force"} {
		if in.Bool(k) {
			args = append(args, "--"+k)
		}
	}
	var doc any
	if err := m.run("dlq", args, &doc); err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"retried": doc}}, nil
}

// mailDLQPurge deletes dead letters, which nothing brings back: it needs
// the named confirmation --confirm purge, never -y, except to preview.
func mailDLQPurge(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	if !in.DryRun() && in.Str("confirm") != "purge" {
		return cli.Result{}, fault.New(fault.ConfirmationRequired, "purging dead letters cannot be undone: confirm it with --confirm purge").
			WithHint("preview it first with --dry-run").WithNext("rex-amq-adapter mail dlq purge --confirm purge")
	}
	m, err := openMailbox(c)
	if err != nil {
		return cli.Result{}, err
	}
	args := []string{"purge", "--json", "--yes"}
	if d := in.Str("older-than"); d != "" {
		args = append(args, "--older-than", d)
	}
	if in.DryRun() {
		args = append(args, "--dry-run")
	}
	var doc any
	if err := m.run("dlq", args, &doc); err != nil {
		return cli.Result{}, err
	}
	d := map[string]any{"purged": doc}
	if in.DryRun() {
		d = map[string]any{"dry_run": true, "would_purge": doc}
	}
	return cli.Result{Data: d}, nil
}
