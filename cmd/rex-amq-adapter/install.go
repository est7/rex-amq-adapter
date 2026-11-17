package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
)

// installResult is what install setup, status, and remove return.
type installResult struct {
	Binary     string         `json:"binary"`
	DryRun     bool           `json:"dry_run"`
	Items      []install.Item `json:"items"`
	CodexHooks []codexHooks   `json:"codex_hooks,omitempty"`
	Notes      []string       `json:"notes"`
	verb       string
}

func installSetup(c *cli.Ctx, in cli.Input) (cli.Result, error) { return runInstall(c, in, "setup") }

func installStatus(c *cli.Ctx, in cli.Input) (cli.Result, error) { return runInstall(c, in, "status") }

func installRemove(c *cli.Ctx, in cli.Input) (cli.Result, error) { return runInstall(c, in, "remove") }

func runInstall(c *cli.Ctx, in cli.Input, verb string) (cli.Result, error) {
	var only []core.Kind
	for _, k := range in.All("agent") {
		kind, err := core.ParseKind(k)
		if err != nil {
			return cli.Result{}, cli.Usage(c.Cmd, "%v", err)
		}
		only = append(only, kind)
	}
	self, err := selfPath()
	if err != nil {
		return cli.Result{}, err
	}
	agents, err := install.Agents(only)
	if err != nil {
		return cli.Result{}, err
	}
	if len(only) == 0 {
		agents = foundOnly(agents)
	}
	opts := install.Options{SelfBin: self, DryRun: in.DryRun(), Force: in.Bool("force")}
	var items []install.Item
	switch verb {
	case "setup":
		items, err = install.Setup(agents, opts)
	case "status":
		items, err = install.Status(agents, self)
	case "remove":
		items, err = install.Remove(agents, opts)
	}
	if err != nil {
		return cli.Result{}, err
	}
	res := installResult{Binary: self, DryRun: in.DryRun(), Items: items, Notes: []string{}, verb: verb}
	if res.Items == nil {
		res.Items = []install.Item{}
	}
	// Codex runs none of the adapter's hooks until the user approves them
	// in /hooks: status reports which still wait (read-only).
	if verb == "status" {
		if res.CodexHooks, err = codexHookApprovals(agents); err != nil {
			return cli.Result{}, err
		}
		for _, a := range res.CodexHooks {
			res.Notes = append(res.Notes, strings.TrimSpace(a.note()))
		}
	}
	switch verb {
	case "setup":
		res.Notes = append(res.Notes, setupNotes(agents, items)...)
	case "remove":
		for _, it := range items {
			if it.Kind == "hook" && it.Change == "removed" && strings.Contains(it.Path, ".codex") {
				res.Notes = append(res.Notes, "Codex trusts hooks by their position: hooks that came after the removed entries may ask for approval again in /hooks.")
				break
			}
		}
	}
	return cli.Result{Data: res, Human: res.text}, nil
}

func (r installResult) text(w io.Writer) error {
	home, _ := os.UserHomeDir()
	last := ""
	for _, it := range r.Items {
		if it.Agent != last {
			if last != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, it.Agent)
			last = it.Agent
		}
		col := it.State
		if r.verb != "status" {
			col = it.Change
		}
		fmt.Fprintf(w, "  %-10s %-10s %s\n", it.Kind, col, strings.Replace(it.Path, home, "~", 1))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "\n%s\n", n)
	}
	return nil
}

// codexHooks is a Codex hooks file and the hooks in it the user has not
// approved.
type codexHooks struct {
	File       string                    `json:"file"`
	Unapproved []install.CodexHookReview `json:"unapproved"`
}

func codexHookApprovals(agents []install.Agent) ([]codexHooks, error) {
	var out []codexHooks
	for _, a := range agents {
		if a.Kind != core.Codex {
			continue
		}
		review, err := a.CodexHookReview()
		if err != nil {
			return nil, fmt.Errorf("codex hook approvals: %w", err)
		}
		out = append(out, codexHooks{File: a.HookFile, Unapproved: append([]install.CodexHookReview{}, review...)})
	}
	return out, nil
}

// note is the status line for c: approved, or which hooks wait and what
// that does to Codex sessions and summons until the user approves them.
func (c codexHooks) note() string {
	if len(c.Unapproved) == 0 {
		return "\nCodex hooks: every hook in " + c.File + " is approved.\n"
	}
	var which []string
	for _, h := range c.Unapproved {
		which = append(which, "  "+h.Key+" ("+h.Status+")")
	}
	return fmt.Sprintf("\nCodex hooks: %d hook(s) in %s wait for your approval (unapproved_hooks):\n%s\nUntil you approve them with /hooks in a Codex session, every new Codex session stops on \"Hooks need review\", `epic open` and `agent summon --harness codex` refuse Codex, and `agent summon` without --harness starts a review role in another family.\n",
		len(c.Unapproved), c.File, strings.Join(which, "\n"))
}

func foundOnly(agents []install.Agent) []install.Agent {
	var out []install.Agent
	for _, a := range agents {
		if a.Found {
			out = append(out, a)
		}
	}
	return out
}

// selfPath is the binary the hooks will run. A `go run` build lives in a
// temporary directory that disappears, so it is refused.
func selfPath() (string, error) {
	self, err := app.SelfPath()
	if err != nil {
		return "", err
	}
	if strings.Contains(self, string(filepath.Separator)+"go-build") {
		return "", errors.New("this is a temporary `go run` build; install the binary (go install ./cmd/rex-amq-adapter) and run setup from it")
	}
	return self, nil
}

// conflictWhat names what a conflicting item holds.
func conflictWhat(kind string) string {
	switch kind {
	case "title":
		return "a window title command"
	case "hook":
		return "a rex-amq-adapter extension"
	}
	return "a rex-amq-adapter skill"
}

func setupNotes(agents []install.Agent, items []install.Item) []string {
	var notes []string
	for _, it := range items {
		switch {
		case it.Change == "conflict" && it.Kind == "supervisor":
			notes = append(notes, fmt.Sprintf("%s: %s keeps the shared supervisor that runs sessions outside their pane (README, Recommended configuration) on: %s.", it.Agent, it.Path, it.Note))
		case it.Change == "conflict" && it.Kind == "tasks":
			notes = append(notes, fmt.Sprintf("%s: %s keeps native task-list tools (README, Recommended configuration) off: %s.", it.Agent, it.Path, it.Note))
		case it.Change == "conflict":
			notes = append(notes, fmt.Sprintf("%s: %s holds %s that setup did not install; it was left alone. Use --force to replace it.", it.Agent, it.Path, conflictWhat(it.Kind)))
		}
	}
	for _, a := range agents {
		if a.Kind == core.Codex {
			notes = append(notes, "Start Codex with --no-daemon in Rex so its hooks and tools inherit the pane's Rex environment. Codex asks before it runs a new or changed hook. Approve the adapter's hooks with /hooks in Codex; until then Codex skips them and the agent is not adopted.")
		}
	}
	return append(notes, "Start a new agent session in a Rex terminal to use it.")
}
