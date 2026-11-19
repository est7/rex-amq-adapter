package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

// Help is a help page: the text a person reads, and in machine mode the
// envelope data an agent reads.
type Help struct {
	Command  string        `json:"command"`
	Text     string        `json:"help"`
	Commands []CommandInfo `json:"commands,omitempty"`
	Spec     *CommandInfo  `json:"spec,omitempty"`
}

func (r *Registry) emitHelp(p out.Printer, h Help) int {
	if p.Mode == out.Human {
		p.Raw(h.Text)
		return 0
	}
	return p.Emit(out.Response{Data: h})
}

// Signature renders "agent retire <handle> [--force] [--dry-run]".
func (c *Command) Signature() string {
	parts := []string{"rex-amq-adapter", c.Name()}
	for _, a := range c.Args {
		n := a.Name
		if a.Rest {
			n += "..."
		}
		if a.Required {
			parts = append(parts, "<"+n+">")
		} else {
			parts = append(parts, "["+n+"]")
		}
	}
	for _, f := range c.Flags {
		s := "--" + f.Name
		if f.Kind != KindBool {
			s += " " + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		}
		if !f.Required {
			s = "[" + s + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func (r *Registry) groupSummary(path []string) string {
	for _, g := range r.Groups {
		if slices.Equal(g.Path, path) {
			return g.Summary
		}
	}
	return ""
}

// groupHelp lists what a group holds: the nouns at the root, a noun's
// verbs (and sub-nouns) below it.
func (r *Registry) groupHelp(group []string) Help {
	var b strings.Builder
	name := strings.Join(group, " ")
	if len(group) == 0 {
		b.WriteString("rex-amq-adapter: AMQ messaging between coding agents in Rex terminals\n\n")
		b.WriteString("Usage: rex-amq-adapter <noun> <verb> [ARGS] [--json | --text] [--help]\n\n")
	} else {
		fmt.Fprintf(&b, "rex-amq-adapter %s: %s\n\nUsage: rex-amq-adapter %s <verb> [ARGS] [--help]\n\n", name, r.groupSummary(group), name)
	}
	type row struct{ name, summary string }
	var rows []row
	seen := map[string]bool{}
	var infos []CommandInfo
	for _, c := range r.Commands {
		if len(c.Path) <= len(group) || !slices.Equal(c.Path[:len(group)], group) {
			continue
		}
		next := c.Path[len(group)]
		if len(c.Path) == len(group)+1 {
			rows = append(rows, row{next, c.Summary})
			infos = append(infos, c.Info())
			continue
		}
		if !seen[next] {
			seen[next] = true
			rows = append(rows, row{next + " ...", r.groupSummary(append(slices.Clone(group), next))})
		}
	}
	width := 0
	for _, rw := range rows {
		width = max(width, len(rw.name))
	}
	label := "Verbs"
	if len(group) == 0 {
		label = "Commands"
	}
	fmt.Fprintf(&b, "%s:\n", label)
	for _, rw := range rows {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, rw.name, rw.summary)
	}
	if len(group) == 0 {
		b.WriteString("\nOff a terminal (or with --json) every command prints one v:2 JSON envelope line on stdout\n" +
			"and nothing on stderr: {ok, v, data, error, meta, warnings}. --text forces text.\n" +
			"`rex-amq-adapter <noun> --help` lists a noun's verbs; `rex-amq-adapter schema` describes every\n" +
			"command offline, and `rex-amq-adapter schema errors` lists the error codes and exit statuses.\n")
	}
	return Help{Command: name, Text: b.String(), Commands: infos}
}

// commandHelp is a command's page: signature, flags, guardrails, examples.
func (r *Registry) commandHelp(c *Command) Help {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n", c.Signature(), c.Summary)
	traits := []string{"tier " + string(c.Tier)}
	if c.Mutating {
		traits = append(traits, "mutating")
	} else {
		traits = append(traits, "read-only")
	}
	if c.Idempotent {
		traits = append(traits, "idempotent")
	}
	if c.DryRun {
		traits = append(traits, "--dry-run previews it")
	}
	if c.Guard != GuardNone {
		traits = append(traits, "guard "+string(c.Guard))
	}
	if len(c.Roles) > 0 {
		traits = append(traits, "for "+strings.Join(c.Roles, ", "))
	}
	fmt.Fprintf(&b, "(%s)\n", strings.Join(traits, "; "))
	if len(c.Args) > 0 {
		b.WriteString("\nArguments:\n")
		for _, a := range c.Args {
			fmt.Fprintf(&b, "  <%s>  (%s) %s\n", a.Name, a.Kind, a.Help)
		}
	}
	if len(c.Flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, f := range c.Flags {
			def := ""
			if f.Default != "" {
				def = " (default " + f.Default + ")"
			}
			kind := string(f.Kind)
			if f.Kind == KindEnum {
				kind = strings.Join(f.Enum, "|")
			}
			fmt.Fprintf(&b, "  --%s  (%s) %s%s\n", f.Name, kind, f.Help, def)
		}
	}
	b.WriteString("  --json | --text  output mode (default: text on a terminal, JSON otherwise)\n")
	if len(c.Guardrails) > 0 {
		b.WriteString("\nGuardrails:\n")
		for _, g := range c.Guardrails {
			fmt.Fprintf(&b, "  - %s\n", g)
		}
	}
	fmt.Fprintf(&b, "\nResult: %s\n\nExamples:\n", c.Result)
	for _, e := range c.Examples {
		fmt.Fprintf(&b, "  %s\n", e)
	}
	info := c.Info()
	return Help{Command: c.Name(), Text: b.String(), Spec: &info}
}

// CommandInfo is a command as `schema` describes it.
type CommandInfo struct {
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	Args          []Arg    `json:"args"`
	Flags         []Flag   `json:"flags"`
	Tier          Tier     `json:"tier"`
	Mutating      bool     `json:"mutating"`
	Idempotent    bool     `json:"idempotent"`
	DryRun        bool     `json:"supports_dry_run"`
	Audience      Audience `json:"audience"`
	Roles         []string `json:"roles"`
	Guard         Guard    `json:"guard,omitempty"`
	Input         bool     `json:"input"`
	List          bool     `json:"list"`
	DefaultLimit  int      `json:"default_limit,omitempty"`
	MaxLimit      int      `json:"max_limit,omitempty"`
	DefaultFields []string `json:"default_fields,omitempty"`
	Document      bool     `json:"document"`
	Stream        bool     `json:"stream"`
	Examples      []string `json:"examples"`
	Guardrails    []string `json:"guardrails"`
	Result        string   `json:"result"`
}

// Info is c's schema entry.
func (c *Command) Info() CommandInfo {
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	args, flags := c.Args, c.Flags
	if args == nil {
		args = []Arg{}
	}
	if flags == nil {
		flags = []Flag{}
	}
	return CommandInfo{
		Name: c.Name(), Summary: c.Summary, Args: args, Flags: flags, Tier: c.Tier, Mutating: c.Mutating,
		Idempotent: c.Idempotent, DryRun: c.DryRun, Audience: c.Audience, Roles: nonNil(c.Roles), Guard: c.Guard,
		Input: c.Input, List: c.List, DefaultLimit: c.DefaultLimit, MaxLimit: c.MaxLimit, DefaultFields: c.DefaultFields,
		Document: c.Doc, Stream: c.Stream, Examples: nonNil(c.Examples), Guardrails: nonNil(c.Guardrails), Result: c.Result,
	}
}

// Schema is the whole command set and the output contract.
type Schema struct {
	Envelope Contract      `json:"envelope"`
	Groups   []GroupInfo   `json:"groups"`
	Commands []CommandInfo `json:"commands"`
	Errors   []fault.Info  `json:"errors"`
}

// GroupInfo is a noun as `schema` describes it.
type GroupInfo struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// Contract states where results and errors go.
type Contract struct {
	Version  int    `json:"v"`
	Success  string `json:"success"`
	Failure  string `json:"failure"`
	List     string `json:"list"`
	Partial  string `json:"partial"`
	Streams  string `json:"streams"`
	Modes    string `json:"modes"`
	TraceEnv string `json:"trace_env"`
}

// Schema describes every command.
func (r *Registry) Schema() Schema {
	s := Schema{
		Envelope: Contract{
			Version:  out.Version,
			Success:  `{"ok":true,"v":2,"data":{...},"meta":{"verb":"...","trace_id":"..."},"warnings":[]}`,
			Failure:  `{"ok":false,"v":2,"data":null,"error":{"code","message","retryable","hint","next_action","details"},"meta":{...},"warnings":[]}`,
			List:     `data is {"items":[...],"total":N,"truncated":bool,"next_cursor":"..."}; an empty list exits 0`,
			Partial:  `exit 7: ok:false and data holds what took effect; read data before ok`,
			Streams:  `machine mode: one JSON line on stdout, nothing on stderr, failures and panics included; stream commands print schema:1 NDJSON frames once connected`,
			Modes:    `machine off a terminal or with --json; text on a terminal or with --text`,
			TraceEnv: `meta.trace_id is REX_AMQ_ADAPTER_TRACE_ID, else AGENT_TRACE_ID`,
		},
		Errors: fault.Table(),
	}
	for _, g := range r.Groups {
		s.Groups = append(s.Groups, GroupInfo{Name: strings.Join(g.Path, " "), Summary: g.Summary})
	}
	for _, c := range r.Commands {
		s.Commands = append(s.Commands, c.Info())
	}
	return s
}
