package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

// Group is a noun (or a noun's sub-noun, "trust root") that holds commands.
type Group struct {
	Path    []string
	Summary string
}

// Registry holds every public command.
type Registry struct {
	Groups   []Group
	Commands []*Command
	// HumanGate refuses a caller an agent runs (GuardHuman); it returns a
	// *fault.Error with code guard_denied.
	HumanGate func(getenv func(string) string) error
	// OperatorGate refuses a caller that is no Epic Operator (GuardOperator)
	// or, with self, also no Epic's own lead or narrator
	// (GuardOperatorOrSelf); it returns a *fault.Error with code
	// guard_denied.
	OperatorGate func(getenv func(string) string, self bool) error
	// Classify maps a command's error to its code; nil keeps fault errors
	// and makes every other error internal.
	Classify func(error) *fault.Error
}

// Declared flags every command of a kind gets.
var (
	flagDryRun = Flag{Name: "dry-run", Value: Value{Kind: KindBool}, Help: "run every check and resolve the target, then report what would happen without writing, sending, or launching"}
	flagInput  = Flag{Name: "input", Value: Value{Kind: KindInput}, Help: "one JSON object of this command's args and flags: @FILE, - (stdin), or inline; unknown keys are refused"}
	flagFields = Flag{Name: "fields", Value: Value{Kind: KindFields}, Help: "comma-separated fields to keep in data (in each item of a list)"}
	flagCursor = Flag{Name: "cursor", Value: Value{Kind: KindCursor}, Help: "the next_cursor of the previous page; bound to the same scope and filters"}
)

// Finish adds the declared flags each command's marks imply and checks the
// declarations; a failure is a bug in the registry itself.
func (r *Registry) Finish() error {
	seen := map[string]bool{}
	for _, c := range r.Commands {
		if seen[c.Name()] {
			return fmt.Errorf("duplicate command %q", c.Name())
		}
		seen[c.Name()] = true
		if c.Run == nil {
			return fmt.Errorf("%s: no Run", c.Name())
		}
		if c.DryRun {
			c.Flags = append(c.Flags, flagDryRun)
		}
		if c.Input {
			c.Flags = append(c.Flags, flagInput)
		}
		if c.List {
			if c.DefaultLimit == 0 {
				c.DefaultLimit = 20
			}
			if c.MaxLimit == 0 {
				c.MaxLimit = 100
			}
			c.Flags = append(c.Flags,
				Flag{Name: "limit", Default: strconv.Itoa(c.DefaultLimit), Value: Value{Kind: KindInt, Min: 1, Max: c.MaxLimit}, Help: fmt.Sprintf("at most this many items (default %d, max %d)", c.DefaultLimit, c.MaxLimit)},
				flagCursor)
			c.Fields = true
		}
		if c.Fields {
			c.Flags = append(c.Flags, flagFields)
		}
		if c.Mutating && !c.DryRun && c.Tier != TierUndoable {
			return fmt.Errorf("%s: a mutating command beyond undoable must declare --dry-run", c.Name())
		}
		for i, a := range c.Args {
			if a.Rest && i != len(c.Args)-1 {
				return fmt.Errorf("%s: rest arg %q must be last", c.Name(), a.Name)
			}
		}
		names := map[string]bool{}
		for _, f := range c.Flags {
			if names[f.Name] || f.Name == "json" || f.Name == "text" || f.Name == "help" || f.Name == "yes" {
				return fmt.Errorf("%s: flag --%s is reserved or declared twice", c.Name(), f.Name)
			}
			names[f.Name] = true
		}
		if len(c.Examples) < 2 {
			return fmt.Errorf("%s: declare at least two examples", c.Name())
		}
	}
	return nil
}

// Find is the command whose path is words.
func (r *Registry) Find(words ...string) *Command {
	for _, c := range r.Commands {
		if slices.Equal(c.Path, words) {
			return c
		}
	}
	return nil
}

// isGroup reports whether words name a group of commands.
func (r *Registry) isGroup(words []string) bool {
	if len(words) == 0 {
		return true
	}
	for _, c := range r.Commands {
		if len(c.Path) > len(words) && slices.Equal(c.Path[:len(words)], words) {
			return true
		}
	}
	return false
}

// Globals are the flags every command takes, wherever they stand.
type Globals struct {
	JSON, Text, Help bool
}

// ScanGlobals finds --json, --text, and --help (or -h) where a flag
// stands, before "--": the value of a flag that takes one is that value,
// never a global (mail send --body --help sends "--help").
func (r *Registry) ScanGlobals(args []string) Globals {
	cmd, _ := r.resolve(args)
	var g Globals
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return g
		case a == "--json":
			g.JSON = true
		case a == "--text":
			g.Text = true
		case a == "--help" || a == "-h":
			g.Help = true
		case cmd != nil && strings.HasPrefix(a, "--") && !strings.Contains(a, "="):
			if f := cmd.flag(a[2:]); f != nil && f.Kind != KindBool {
				i++ // its value, whatever it looks like
			}
		}
	}
	return g
}

// resolve is the longest declared command the leading words of args name,
// and how many words it takes.
func (r *Registry) resolve(args []string) (*Command, int) {
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	for i := len(words); i > 0; i-- {
		if c := r.Find(words[:i]...); c != nil {
			return c, i
		}
	}
	return nil, 0
}

// Printer is the printer for args, before the command runs.
func (r *Registry) Printer(args []string, s out.Streams, getenv func(string) string) out.Printer {
	g := r.ScanGlobals(args)
	return out.Printer{Streams: s, Mode: out.SelectMode(g.JSON, g.Text, s.StdoutTTY), TraceID: out.TraceID(getenv)}
}

// Execute runs one invocation of a public command and returns its exit
// status. ctx carries the context, environment, stdin, and printer.
func (r *Registry) Execute(ctx *Ctx, args []string) int {
	p := ctx.Out
	g := r.ScanGlobals(args)
	if g.JSON && g.Text {
		return p.Fail(Usage(nil, "--json and --text pick opposite output modes: give one").WithHint("run `rex-amq-adapter --help`"))
	}
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	cmd, n := r.resolve(args)
	if cmd == nil {
		// The longest group the leading words name.
		k := 0
		for i := len(words); i >= 0; i-- {
			if r.isGroup(words[:i]) {
				k = i
				break
			}
		}
		group := words[:k]
		p.Verb = strings.Join(group, " ")
		if g.Help {
			return r.emitHelp(p, r.groupHelp(group))
		}
		if len(words) > k {
			kind := "command"
			if k > 0 {
				kind = "verb for rex-amq-adapter " + strings.Join(group, " ")
			}
			return p.Fail(groupUsage(group, "unknown %s %s", kind, q(words[k])))
		}
		if k == 0 {
			return p.Fail(groupUsage(nil, "rex-amq-adapter needs a command"))
		}
		return p.Fail(groupUsage(group, "rex-amq-adapter %s needs a verb", strings.Join(group, " ")))
	}
	p.Verb = cmd.Name()
	ctx.Out, ctx.Cmd = p, cmd
	if ctx.Warn == nil {
		ctx.Warn = out.NewWarnings(p)
	}
	if g.Help {
		return r.emitHelp(p, r.commandHelp(cmd))
	}
	in, err := cmd.parse(args[n:])
	if err != nil {
		return p.Fail(asFault(err))
	}
	switch cmd.Guard {
	case GuardHuman:
		if r.HumanGate == nil {
			return p.Fail(fault.New(fault.Internal, "%s is human-only and no guard is configured", cmd.Name()))
		}
		if err := r.HumanGate(ctx.Getenv); err != nil {
			return p.Fail(r.classify(err))
		}
	case GuardOperator, GuardOperatorOrSelf:
		if r.OperatorGate == nil {
			return p.Fail(fault.New(fault.Internal, "%s is the Operator's and no guard is configured", cmd.Name()))
		}
		if err := r.OperatorGate(ctx.Getenv, cmd.Guard == GuardOperatorOrSelf); err != nil {
			return p.Fail(r.classify(err))
		}
	}
	if raw := in.Str("input"); raw != "" {
		b, err := readInput(raw, ctx.Stdin)
		if err != nil {
			return p.Fail(asFault(err))
		}
		if in, err = cmd.mergeInput(in, b); err != nil {
			return p.Fail(asFault(err))
		}
	}
	res, err := cmd.Run(ctx, in)
	return r.emit(ctx, cmd, in, res, err)
}

func (r *Registry) classify(err error) *fault.Error {
	if r.Classify != nil {
		return r.Classify(err)
	}
	if e, ok := fault.As(err); ok {
		return e
	}
	return fault.Wrap(err, fault.Internal)
}

func asFault(err error) *fault.Error {
	if e, ok := fault.As(err); ok {
		return e
	}
	return fault.Wrap(err, fault.Internal)
}

// emit prints a command's result: projected by --fields, its warnings and
// the run's diagnostics in band.
func (r *Registry) emit(ctx *Ctx, cmd *Command, in Input, res Result, err error) int {
	p := ctx.Out
	warnings := append(res.Warnings, ctx.Warn.Lines()...)
	if sig, ok := Signalled(ctx); ok {
		// A signal ended the command, whatever it returned while it
		// cleaned up: its answer is interrupted or terminated.
		if res.Streamed {
			return sig.Exit()
		}
		return p.Emit(out.Response{Err: sig, Warnings: warnings})
	}
	if res.Streamed {
		// The command already answered on its own protocol (stream frames,
		// export lines): print nothing more, exit with its status.
		if err != nil {
			return r.classify(err).Exit()
		}
		return 0
	}
	if err != nil {
		e := r.classify(err)
		return p.Emit(out.Response{Data: res.Data, Err: e, Warnings: warnings})
	}
	if p.Mode == out.Human && res.Markdown != "" {
		p.Raw(res.Markdown)
		for _, w := range warnings {
			ctx.Warn.Add(w)
		}
		return 0
	}
	data, human := res.Data, res.Human
	if cmd.Fields {
		fields, strict := cmd.DefaultFields, false
		if in.Has("fields") {
			fields, human, strict = strings.Split(in.Str("fields"), ","), nil, true
		}
		if len(fields) > 0 {
			projected, perr := Project(data, fields, strict)
			if perr != nil {
				return p.Fail(asFault(perr))
			}
			data = projected
		}
	}
	return p.Emit(out.Response{Data: data, Warnings: warnings, Incomplete: res.Incomplete, Human: human})
}

// parse turns the words after the command's path into an Input, every
// value checked against its kind. Nothing here reads a file or the network.
func (c *Command) parse(words []string) (Input, error) {
	in := Input{Args: map[string][]string{}, Flags: map[string][]string{}}
	var pos []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			pos = append(pos, words[i+1:]...)
			break
		}
		if w == "-" || !strings.HasPrefix(w, "-") {
			pos = append(pos, w)
			continue
		}
		switch w {
		case "--json", "--text", "--help", "-h":
			continue
		}
		if !strings.HasPrefix(w, "--") {
			return in, Usage(c, "unknown flag %s (the only short flag is -h; text that starts with - goes after --)", q(w))
		}
		name, val, hasVal := strings.Cut(w[2:], "=")
		f := c.flag(name)
		if f == nil {
			return in, Usage(c, "unknown flag --%s", Visible(name))
		}
		if f.Kind == KindBool {
			if hasVal {
				return in, Usage(c, "--%s takes no value", name)
			}
			in.Flags[name] = []string{"true"}
			continue
		}
		if !hasVal {
			if i+1 >= len(words) {
				return in, Usage(c, "--%s needs a value", name)
			}
			i++
			val = words[i]
		}
		if !f.Repeat && len(in.Flags[name]) > 0 {
			return in, Usage(c, "--%s given twice", name)
		}
		in.Flags[name] = append(in.Flags[name], val)
	}
	for _, a := range c.Args {
		if a.Rest {
			if len(pos) > 0 {
				in.Args[a.Name] = pos
			}
			pos = nil
			break
		}
		if len(pos) == 0 {
			break
		}
		in.Args[a.Name] = []string{pos[0]}
		pos = pos[1:]
	}
	if len(pos) > 0 {
		return in, Usage(c, "unexpected argument %s", q(pos[0]))
	}
	if in.Has("input") {
		// Required values may come from --input; check what is here.
		return in, c.check(in, false)
	}
	return in, c.check(in, true)
}

// check applies defaults, then validates every value against its kind and,
// when complete, that every required value is present.
func (c *Command) check(in Input, complete bool) error {
	for _, f := range c.Flags {
		if complete && len(in.Flags[f.Name]) == 0 && f.Default != "" {
			in.Flags[f.Name] = []string{f.Default}
		}
		if complete && f.Required && len(in.Flags[f.Name]) == 0 {
			return Usage(c, "--%s is required", f.Name)
		}
		if f.Kind == KindBool {
			continue
		}
		for i, v := range in.Flags[f.Name] {
			clean, err := f.check(v)
			if err != nil {
				return invalidInput(c, "--"+f.Name, f.Kind, err)
			}
			in.Flags[f.Name][i] = clean
		}
	}
	for _, a := range c.Args {
		vals := in.Args[a.Name]
		if complete && a.Required && len(vals) == 0 {
			return Usage(c, "missing <%s>", a.Name)
		}
		for i, v := range vals {
			clean, err := a.check(v)
			if err != nil {
				return invalidInput(c, "<"+a.Name+">", a.Kind, err)
			}
			vals[i] = clean
		}
	}
	return nil
}

func invalidInput(c *Command, field string, kind Kind, err error) *fault.Error {
	var iv invalid
	msg := err.Error()
	if errors.As(err, &iv) {
		msg = iv.why
	}
	help := "rex-amq-adapter " + c.Name() + " --help"
	return fault.New(fault.InvalidInput, "%s %s", field, msg).
		With("field", field).With("kind", string(kind)).
		WithHint("run `" + help + "`").WithNext(help)
}

// readInput reads --input: inline JSON, @FILE, or - for stdin, bounded.
func readInput(raw string, stdin io.Reader) ([]byte, error) {
	var b []byte
	var err error
	switch {
	case raw == "-":
		if stdin == nil {
			return nil, fault.New(fault.Usage, "--input - reads stdin, and there is none")
		}
		b, err = io.ReadAll(io.LimitReader(stdin, 2*BodyMax+1))
	case strings.HasPrefix(raw, "@"):
		b, err = readBounded(raw[1:], 2*BodyMax)
	default:
		b = []byte(raw)
	}
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "--input: %v", err)
	}
	if len(b) > 2*BodyMax {
		return nil, fault.New(fault.InvalidInput, "--input is over %d bytes", 2*BodyMax)
	}
	return b, nil
}

// readBounded reads at most max bytes of path, refusing a bigger file.
func readBounded(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	return b, nil
}

// ReadBody resolves a body value: inline text, @FILE, or - for stdin, and
// checks the text it reads as the body kind does.
func ReadBody(v string, stdin io.Reader) (string, error) {
	var b []byte
	var err error
	switch {
	case v == "-":
		if stdin == nil {
			return "", fault.New(fault.Usage, "- reads stdin, and there is none")
		}
		b, err = io.ReadAll(io.LimitReader(stdin, BodyMax+1))
	case strings.HasPrefix(v, "@"):
		b, err = readBounded(v[1:], BodyMax)
	default:
		return v, nil
	}
	if err != nil {
		return "", fault.New(fault.InvalidInput, "read %s: %v", Visible(v), err)
	}
	if err := CheckBody(string(b)); err != nil {
		return "", fault.New(fault.InvalidInput, "%s %v", Visible(v), err)
	}
	return string(b), nil
}

// ReadJSON resolves a JSON-object flag value: inline JSON, @FILE, or - for
// stdin, bounded by BodyMax. Empty stdin returns a usage error (exit 2).
func ReadJSON(v string, stdin io.Reader) ([]byte, error) {
	var b []byte
	var err error
	switch {
	case v == "-":
		if stdin == nil {
			return nil, fault.New(fault.Usage, "- reads stdin, and there is none")
		}
		b, err = io.ReadAll(io.LimitReader(stdin, BodyMax+1))
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "read stdin: %v", err)
		}
		if len(b) > BodyMax {
			return nil, fault.New(fault.InvalidInput, "stdin JSON is over %d bytes", BodyMax)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fault.New(fault.Usage, "- read no JSON object on stdin")
		}
		return b, nil
	case strings.HasPrefix(v, "@"):
		b, err = readBounded(v[1:], BodyMax)
		if err != nil {
			return nil, fault.New(fault.InvalidInput, "read %s: %v", Visible(v), err)
		}
		return b, nil
	default:
		if len(v) > BodyMax {
			return nil, fault.New(fault.InvalidInput, "JSON value is over %d bytes", BodyMax)
		}
		return []byte(v), nil
	}
}

// mergeInput folds a --input object into in: keys are arg and flag names
// (snake_case flag aliases like clear_progress map to clear-progress),
// unknown keys and keys also on the command line are usage errors, and the
// result is checked as the command line is.
func (c *Command) mergeInput(in Input, raw []byte) (Input, error) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return in, Usage(c, "--input must be one JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return in, Usage(c, "--input must be exactly one JSON object")
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seenFlags := map[string]bool{}
	for _, key := range keys {
		val := obj[key]
		if key == "input" {
			return in, Usage(c, "--input cannot set input")
		}
		a, f := c.arg(key), c.flag(key)
		if a == nil && f == nil && strings.Contains(key, "_") {
			if kebab := strings.ReplaceAll(key, "_", "-"); kebab != "input" {
				f = c.flag(kebab)
			}
		}
		switch {
		case a != nil:
			if len(in.Args[key]) > 0 {
				return in, Usage(c, "%s is given both on the command line and in --input", key)
			}
			if a.Rest {
				var list []string
				if json.Unmarshal(val, &list) != nil {
					var one string
					if json.Unmarshal(val, &one) != nil {
						return in, Usage(c, "--input %s must be a string or a list of strings", key)
					}
					list = []string{one}
				}
				in.Args[key] = list
				continue
			}
			var s string
			if json.Unmarshal(val, &s) != nil {
				return in, Usage(c, "--input %s must be a string", key)
			}
			in.Args[key] = []string{s}
		case f != nil:
			name := f.Name
			if seenFlags[name] {
				return in, Usage(c, "--input sets %s more than once", name)
			}
			seenFlags[name] = true
			if len(in.Flags[name]) > 0 {
				return in, Usage(c, "--%s is given both on the command line and in --input", name)
			}
			switch {
			case f.Kind == KindBool:
				var b bool
				if json.Unmarshal(val, &b) != nil {
					return in, Usage(c, "--input %s must be true or false", key)
				}
				if b {
					in.Flags[name] = []string{"true"}
				}
			case f.Kind == KindInt:
				var n json.Number
				d := json.NewDecoder(bytes.NewReader(val))
				d.UseNumber()
				if d.Decode(&n) != nil {
					return in, Usage(c, "--input %s must be an integer", key)
				}
				in.Flags[name] = []string{n.String()}
			case f.Kind == KindJSON:
				trimmed := bytes.TrimSpace(val)
				if len(trimmed) > 0 && trimmed[0] == '{' {
					in.Flags[name] = []string{string(trimmed)}
					continue
				}
				if len(trimmed) > 0 && trimmed[0] == '"' {
					var s string
					if json.Unmarshal(trimmed, &s) != nil {
						return in, Usage(c, "--input %s must be a JSON object or string", key)
					}
					in.Flags[name] = []string{s}
					continue
				}
				return in, invalidInput(c, "--"+name, f.Kind, bad("must be a JSON object"))
			case f.Repeat:
				var list []string
				if json.Unmarshal(val, &list) != nil {
					var one string
					if json.Unmarshal(val, &one) != nil {
						return in, Usage(c, "--input %s must be a string or a list of strings", key)
					}
					list = []string{one}
				}
				in.Flags[name] = list
			default:
				var s string
				if json.Unmarshal(val, &s) != nil {
					return in, Usage(c, "--input %s must be a string", key)
				}
				in.Flags[name] = []string{s}
			}
		default:
			return in, Usage(c, "--input has unknown field %s", q(key))
		}
	}
	return in, c.check(in, true)
}

// Project keeps fields of data, or of each item of a list's data. ok, error,
// and meta are the envelope's and never touched. A field no item has is an
// invalid_input naming the fields there are when strict (the caller's own
// --fields); a default projection keeps what each item has.
func Project(data any, fields []string, strict bool) (any, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	m, ok := generic.(map[string]any)
	if !ok {
		return nil, fault.New(fault.InvalidInput, "--fields: this command's data has no fields")
	}
	keep := func(obj map[string]any, have map[string]bool) map[string]any {
		o := map[string]any{}
		for _, f := range fields {
			if v, ok := obj[f]; ok {
				o[f] = v
				have[f] = true
			}
		}
		return o
	}
	have := map[string]bool{}
	declared := declaredJSONFields(data)
	knownSet := map[string]bool{}
	for k := range declared {
		knownSet[k] = true
	}
	if items, ok := m["items"].([]any); ok {
		for i, it := range items {
			if obj, ok := it.(map[string]any); ok {
				items[i] = keep(obj, have)
				for k := range obj {
					knownSet[k] = true
				}
			}
		}
		if len(items) == 0 {
			return m, nil
		}
		m["items"] = items
	} else {
		for k := range m {
			knownSet[k] = true
		}
		m = keep(m, have)
	}
	for _, f := range fields {
		if strict && !have[f] && !declared[f] && len(knownSet) > 0 {
			known := make([]string, 0, len(knownSet))
			for k := range knownSet {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fault.New(fault.InvalidInput, "--fields: no field %s; there are %s", q(f), strings.Join(known, ", ")).With("field", f).With("available", known)
		}
	}
	return m, nil
}

// declaredJSONFields returns the JSON field names declared on data's struct
// (or on its `items` slice element struct for list responses), so strict
// --fields accepts valid optional fields omitted by omitempty.
func declaredJSONFields(data any) map[string]bool {
	if data == nil {
		return nil
	}
	t := derefType(reflect.TypeOf(data))
	if t.Kind() != reflect.Struct {
		return nil
	}
	if elem, ok := itemsElemStruct(t); ok {
		out := map[string]bool{}
		collectStructJSONFields(elem, out)
		return out
	}
	out := map[string]bool{}
	collectStructJSONFields(t, out)
	return out
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func itemsElemStruct(t reflect.Type) (reflect.Type, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, skip := parseJSONTag(f)
		if skip {
			continue
		}
		if f.Anonymous && f.Tag.Get("json") == "" {
			if ft := derefType(f.Type); ft.Kind() == reflect.Struct {
				if elem, ok := itemsElemStruct(ft); ok {
					return elem, true
				}
			}
			continue
		}
		if name == "items" && f.Type.Kind() == reflect.Slice {
			if et := derefType(f.Type.Elem()); et.Kind() == reflect.Struct {
				return et, true
			}
		}
	}
	return nil, false
}

func collectStructJSONFields(t reflect.Type, dst map[string]bool) {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, explicit, skip := parseJSONTag(f)
		if skip {
			continue
		}
		if f.Anonymous && !explicit {
			if ft := derefType(f.Type); ft.Kind() == reflect.Struct {
				collectStructJSONFields(ft, dst)
				continue
			}
		}
		dst[name] = true
	}
}

func parseJSONTag(f reflect.StructField) (name string, explicit, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag != "" {
		head, _, _ := strings.Cut(tag, ",")
		if head != "" {
			return head, true, false
		}
	}
	return f.Name, false, false
}

// Page is the page of items the invocation asks for: --limit items from
// the --cursor's offset. The cursor is bound to scope (the command, its
// scope, and its filters), so one from another listing is refused.
func Page[T any](items []T, in Input, scope string) (List[T], error) {
	limit := in.Int("limit")
	if limit <= 0 {
		limit = 20
	}
	offset := 0
	if cur := in.Str("cursor"); cur != "" {
		o, err := decodeCursor(cur, scope)
		if err != nil {
			return List[T]{}, err
		}
		offset = o
	}
	if items == nil {
		items = []T{}
	}
	total := len(items)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	l := List[T]{Items: items[offset:end], Total: total, Truncated: offset > 0 || end < total}
	if end < total {
		l.NextCursor = encodeCursor(end, scope)
	}
	if l.Items == nil {
		l.Items = []T{}
	}
	return l, nil
}

type cursor struct {
	O int    `json:"o"`
	S string `json:"s"`
}

func scopeHash(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:8])
}

func encodeCursor(offset int, scope string) string {
	b, _ := json.Marshal(cursor{O: offset, S: scopeHash(scope)})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, scope string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.O < 0 {
		return 0, fault.New(fault.InvalidInput, "--cursor is not a cursor this command returned")
	}
	if c.S != scopeHash(scope) {
		return 0, fault.New(fault.InvalidInput, "--cursor belongs to another listing: repeat the scope and filters it came from")
	}
	return c.O, nil
}

// Visible is s with its control characters escaped, for messages.
func Visible(s string) string { return out.Visible(s) }

func groupUsage(group []string, format string, a ...any) *fault.Error {
	help := strings.Join(append(append([]string{"rex-amq-adapter"}, group...), "--help"), " ")
	return fault.New(fault.Usage, format, a...).WithHint("run `" + help + "`; `rex-amq-adapter schema` describes every command").WithNext(help)
}

// Signalled is the interrupted or terminated error ctx was cancelled with,
// when a signal cancelled it.
func Signalled(ctx context.Context) (*fault.Error, bool) {
	if ctx == nil || ctx.Err() == nil {
		return nil, false
	}
	f, ok := fault.As(context.Cause(ctx))
	if !ok || f.Code != fault.Interrupted && f.Code != fault.Terminated {
		return nil, false
	}
	return f, true
}
