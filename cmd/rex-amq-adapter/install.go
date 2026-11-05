package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/guide"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/store"
)

type kindList []core.Kind

func (k *kindList) String() string { return fmt.Sprint(*k) }
func (k *kindList) Set(s string) error {
	kind, err := core.ParseKind(s)
	if err != nil {
		return err
	}
	*k = append(*k, kind)
	return nil
}

func runInstall(cmd string, args []string) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var only kindList
	fs.Var(&only, "agent", "only this agent: claude, codex, agy, or pi (repeatable)")
	dryRun := fs.Bool("dry-run", false, "report what would change without changing anything")
	force := fs.Bool("force", false, "replace what setup did not install: a rex-amq-adapter skill or Pi extension, or Antigravity's window title command")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := selfPath()
	if err != nil {
		return fail(err)
	}
	agents, err := install.Agents(only)
	if err != nil {
		return fail(err)
	}
	if len(only) == 0 {
		agents = foundOnly(agents)
	}
	opts := install.Options{SelfBin: self, DryRun: *dryRun, Force: *force}
	var items []install.Item
	switch cmd {
	case "setup":
		items, err = install.Setup(agents, opts)
	case "status":
		items, err = install.Status(agents, self)
	case "remove":
		items, err = install.Remove(agents, opts)
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		b, err := json.MarshalIndent(map[string]any{"binary": self, "dry_run": *dryRun, "items": items}, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(b))
		return 0
	}
	printItems(cmd, items)
	switch cmd {
	case "setup":
		printSetupNotes(agents, items)
	case "remove":
		for _, it := range items {
			if it.Kind == "hook" && it.Change == "removed" && strings.Contains(it.Path, ".codex") {
				fmt.Println("\nCodex trusts hooks by their position: hooks that came after the removed entries may ask for approval again in /hooks.")
				break
			}
		}
	}
	return 0
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

func printItems(cmd string, items []install.Item) {
	home, _ := os.UserHomeDir()
	last := ""
	for _, it := range items {
		if it.Agent != last {
			if last != "" {
				fmt.Println()
			}
			fmt.Println(it.Agent)
			last = it.Agent
		}
		col := it.State
		if cmd != "status" {
			col = it.Change
		}
		fmt.Printf("  %-6s %-10s %s\n", it.Kind, col, strings.Replace(it.Path, home, "~", 1))
	}
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

func printSetupNotes(agents []install.Agent, items []install.Item) {
	for _, it := range items {
		if it.Change == "conflict" {
			fmt.Printf("\n%s: %s holds %s that setup did not install; it was left alone. Use --force to replace it.\n", it.Agent, it.Path, conflictWhat(it.Kind))
		}
	}
	for _, a := range agents {
		if a.Kind == core.Codex {
			fmt.Println("\nStart Codex with --no-daemon in Rex so its hooks and tools inherit the pane's Rex environment. Codex asks before it runs a new or changed hook. Approve the adapter's hooks with /hooks in Codex; until then Codex skips them and the agent is not adopted.")
		}
	}
	fmt.Println("\nStart a new agent session in a Rex terminal to use it.")
}

func runInternal(args []string) int {
	if len(args) == 1 && args[0] == "skill" {
		fmt.Print(guide.Skill)
		return 0
	}
	if len(args) == 3 && args[0] == "watch" {
		return runWatch(args[1], args[2])
	}
	if len(args) == 3 && args[0] == "pane-title" && args[1] == "--agent" {
		fmt.Println(paneTitle(args[2], os.Stdin))
		return 0
	}
	return fail(errors.New("__internal: want `skill`, `watch <server> <handle>`, or `pane-title --agent K`"))
}

// paneTitle answers a harness that asks for its window title (Antigravity's
// "title" command, with its status JSON on stdin). It always prints a line
// and never fails: agy stops asking after repeated failures.
func paneTitle(agent string, stdin io.Reader) string {
	var status struct {
		Cwd string `json:"cwd"`
	}
	_ = json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&status)
	kind, err := core.ParseKind(agent)
	if err != nil {
		return app.DirTitle(status.Cwd)
	}
	loc, err := app.Here()
	if err != nil {
		return app.DirTitle(status.Cwd)
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return app.DirTitle(status.Cwd)
	}
	env := app.Env{Paths: paths, Store: store.Store{P: paths}}
	return env.PaneTitle(loc, kind, status.Cwd)
}
