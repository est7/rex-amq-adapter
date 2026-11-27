// Package cli is the one declaration of every public rex-amq-adapter
// command (CommandSpec). Parsing, pre-I/O validation, the human guard, the
// three help tiers, `schema`, --input, --fields, and list bounds are all
// derived from it, so they cannot drift from each other or from dispatch.
package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

// Tier says how far a command reaches (agent-cli-design R6).
type Tier string

const (
	TierRead        Tier = "read"        // reads only
	TierUndoable    Tier = "undoable"    // a change the caller can undo
	TierRecoverable Tier = "recoverable" // closes or parks something it can bring back
	TierOutward     Tier = "outward"     // mail or a doorbell other agents see
	TierLaunch      Tier = "launch"      // starts billed agents
	TierDestructive Tier = "destructive" // cannot be undone
	TierUser        Tier = "user"        // acts as the user, or on global harness config
)

// Audience is who a command is for.
type Audience string

const (
	AudienceAgent Audience = "agent"
	AudienceHuman Audience = "human"
)

// Guard is the caller check the registry runs before a command does.
type Guard string

const (
	GuardNone Guard = ""
	// GuardHuman refuses (exit 6) any caller an agent runs or that runs
	// unattended (humanGate).
	GuardHuman Guard = "human_only"
	// GuardOperator refuses (exit 6) every caller but an Epic's Operator
	// (app.Env.OperatorGate), inside Rex and out alike.
	GuardOperator Guard = "operator"
	// GuardOperatorOrSelf is GuardOperator that also admits an Epic's own
	// lead or narrator, whose command reads its own session only.
	GuardOperatorOrSelf Guard = "operator_or_epic_self"
)

// Value is the declared kind of an argument or flag value.
type Value struct {
	Kind        Kind     `json:"kind"`
	Enum        []string `json:"enum,omitempty"`
	Min         int      `json:"min,omitempty"`
	Max         int      `json:"max,omitempty"`
	MaxDuration string   `json:"max_duration,omitempty"`
}

// Arg is one positional argument. A Rest arg takes every remaining word and
// must come last; each word is checked as its kind.
type Arg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Rest     bool   `json:"rest,omitempty"`
	Value
	Help string `json:"help"`
}

// Flag is one --name option. Bool flags take no value.
type Flag struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
	Repeat   bool   `json:"repeatable,omitempty"`
	Default  string `json:"default,omitempty"`
	Value
	Help string `json:"help"`
}

// Command is one public command: its words, declaration, and handler.
type Command struct {
	Path       []string `json:"-"`
	Summary    string   `json:"summary"`
	Args       []Arg    `json:"args"`
	Flags      []Flag   `json:"flags"`
	Tier       Tier     `json:"tier"`
	Mutating   bool     `json:"mutating"`
	Idempotent bool     `json:"idempotent"`
	// DryRun commands declare --dry-run: the whole resolution and
	// validation chain runs, and only the final write, send, or launch is
	// skipped.
	DryRun   bool     `json:"supports_dry_run"`
	Audience Audience `json:"audience"`
	Roles    []string `json:"roles,omitempty"`
	Guard    Guard    `json:"guard,omitempty"`
	// Input commands take --input @FILE|-|<json>: one object whose keys are
	// the args and flags, checked and guarded exactly as they are.
	Input bool `json:"input,omitempty"`
	// List commands return a List: --limit, --cursor, and --fields apply.
	List         bool `json:"list,omitempty"`
	DefaultLimit int  `json:"default_limit,omitempty"`
	MaxLimit     int  `json:"max_limit,omitempty"`
	Fields       bool `json:"fields,omitempty"`
	// FieldModel is a zero value of the struct (or list) data type this
	// command returns, used to validate --fields before mutating handlers run.
	FieldModel    any             `json:"-"`
	allowedFields map[string]bool // populated in Registry.Finish from FieldModel
	// DefaultFields is the projection without --fields.
	DefaultFields []string `json:"default_fields,omitempty"`
	Doc           bool     `json:"document,omitempty"`
	Stream        bool     `json:"stream,omitempty"`
	Examples      []string
	Guardrails    []string
	Result        string                                 `json:"result"`
	Run           func(c *Ctx, in Input) (Result, error) `json:"-"`
}

// Name is the command's words joined: "agent list".
func (c *Command) Name() string { return strings.Join(c.Path, " ") }

func (c *Command) flag(name string) *Flag {
	for i := range c.Flags {
		if c.Flags[i].Name == name {
			return &c.Flags[i]
		}
	}
	return nil
}

func (c *Command) arg(name string) *Arg {
	for i := range c.Args {
		if c.Args[i].Name == name {
			return &c.Args[i]
		}
	}
	return nil
}

// Input is a parsed, validated invocation.
type Input struct {
	Args  map[string][]string
	Flags map[string][]string
}

// Arg is a positional argument's value or "".
func (in Input) Arg(name string) string {
	if v := in.Args[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Rest is every word of a Rest argument.
func (in Input) Rest(name string) []string { return in.Args[name] }

// Str is a string flag's value or "".
func (in Input) Str(name string) string {
	if v := in.Flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// All is every value of a repeatable flag.
func (in Input) All(name string) []string { return in.Flags[name] }

// Has reports whether a flag was given.
func (in Input) Has(name string) bool { return len(in.Flags[name]) > 0 }

// Bool reports whether a bool flag was set.
func (in Input) Bool(name string) bool { return in.Str(name) == "true" }

// Int is a validated int flag's value, 0 when unset.
func (in Input) Int(name string) int {
	n, _ := strconv.Atoi(in.Str(name))
	return n
}

// Duration is a validated duration flag's value, def when unset.
func (in Input) Duration(name string, def time.Duration) time.Duration {
	if !in.Has(name) {
		return def
	}
	d, _ := time.ParseDuration(in.Str(name))
	return d
}

// DryRun reports whether --dry-run was given.
func (in Input) DryRun() bool { return in.Bool("dry-run") }

// Result is what a command returns. Data is its envelope data; a partial
// failure (exit 7) returns Data with a *fault.Error.
type Result struct {
	Data       any
	Warnings   []string
	Incomplete bool
	// Human renders Data for a person in human mode.
	Human func(w io.Writer) error
	// Markdown is a document command's text: data.markdown in machine
	// mode, printed raw in human mode.
	Markdown string
	// Streamed says the command already wrote its frames (a stream that
	// connected), so nothing more is printed on success.
	Streamed bool
}

// Ctx is what a command runs with.
type Ctx struct {
	context.Context
	Getenv func(string) string
	Stdin  io.Reader
	Out    out.Printer
	Warn   *out.Warnings
	Cmd    *Command
}

// List is the data of every list command.
type List[T any] struct {
	Items      []T    `json:"items"`
	Total      int    `json:"total"`
	Truncated  bool   `json:"truncated"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Usage is a usage error pointing at the command's help.
func Usage(c *Command, format string, a ...any) *fault.Error {
	e := fault.New(fault.Usage, format, a...)
	if c != nil {
		help := "rex-amq-adapter " + c.Name() + " --help"
		e.WithHint("run `" + help + "`").WithNext(help)
	}
	return e
}
