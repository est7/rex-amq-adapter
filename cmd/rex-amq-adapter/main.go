// Command rex-amq-adapter lets coding agents in Rex terminals message each
// other over AMQ. See docs/design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/store"
)

const usage = `rex-amq-adapter: AMQ messaging between coding agents in Rex terminals

Agent and user commands:
  env                       print the AMQ identity of this Rex block (eval it)
  who                       list adopted agents
  collect                   reconcile records with Rex now

Installation:
  setup|status|remove       install, inspect, or remove the skill and hooks

Internal (run by installed hooks and by amq wake):
  hook <event> --agent K    session-start | user-prompt-submit | post-tool-use | pre-invocation | stop | session-end | mail | settle
  inject <server> <block> <handle> <root> <payload>
  __internal skill          print the agent guide
  version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "hook":
		return runHook(rest)
	case "inject":
		return runInject(rest)
	case "env":
		return runEnv()
	case "who":
		return runWho()
	case "collect":
		return runCollect()
	case "setup", "status", "remove":
		return runInstall(cmd, rest)
	case "__internal":
		return runInternal(rest)
	case "version":
		fmt.Println(version())
		return 0
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "rex-amq-adapter: unknown command %q\n\n%s", cmd, usage)
	return 2
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n", err)
	return 1
}

func runHook(args []string) int {
	if len(args) == 0 {
		return fail(errors.New("hook: missing event"))
	}
	event := args[0]
	log := openHookLog(event)
	defer log.Close()
	app.Diagnostics = log
	budget, ok := install.Budget(event)
	if !ok {
		return hookFail(log, fmt.Errorf("unknown event %q", event))
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(log)
	agent := fs.String("agent", "", "agent kind: claude or codex")
	if err := fs.Parse(args[1:]); err != nil {
		return hookFail(log, err)
	}
	kind, err := core.ParseKind(*agent)
	if err != nil {
		return hookFail(log, err)
	}
	// Every exit prints a reply in the harness's format: Antigravity reads a
	// JSON object even when the adapter has nothing to say or failed.
	reply := func(r app.Reply) {
		if out, err := app.Render(kind, event, r); err == nil {
			fmt.Print(out)
		}
	}
	in, err := app.ReadHookInput(os.Stdin)
	if err != nil {
		reply(app.Reply{})
		return hookFail(log, err)
	}
	loc, err := app.Here()
	if errors.Is(err, app.ErrNotInRex) {
		reply(app.Reply{})
		return 0 // outside Rex the adapter does nothing
	}
	env, err := app.NewEnv(loc.Server)
	if err != nil {
		reply(app.Reply{})
		return hookFail(log, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	r, err := env.Hook(ctx, event, kind, loc, in)
	if err != nil {
		reply(app.Reply{})
		return hookFail(log, err)
	}
	reply(r)
	return 0
}

// hookLog appends timestamped lines to logs/hooks.log, opened on first use
// so a healthy hook touches no file. Hooks run with stderr discarded; this
// log is the only place their problems can be read.
type hookLog struct {
	prefix string
	f      *os.File
}

func openHookLog(event string) *hookLog {
	return &hookLog{prefix: event + " block=" + os.Getenv("REX_BLOCK")}
}

func (l *hookLog) Write(p []byte) (int, error) {
	if l.f == nil {
		paths, err := store.DefaultPaths()
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(paths.Logs(), 0o700); err != nil {
			return 0, err
		}
		if l.f, err = os.OpenFile(filepath.Join(paths.Logs(), "hooks.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(l.f, "%s %s: %s", time.Now().Format(time.RFC3339), l.prefix, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (l *hookLog) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

func hookFail(log *hookLog, err error) int {
	fmt.Fprintf(log, "%v\n", err)
	return fail(err)
}

func runInject(args []string) int {
	if len(args) != 5 {
		fmt.Fprintln(os.Stderr, "AMQ_INJECT_PROGRESS=failed")
		return fail(errors.New("inject: want <server> <block> <handle> <root> <payload>"))
	}
	// The fifth argument is amq's notification payload, deliberately unused:
	// it previews the sender's subject (see core.Doorbell).
	server, block, handle, root := args[0], args[1], args[2], args[3]
	report := func(o core.Outcome) int {
		fmt.Fprintf(os.Stderr, "AMQ_INJECT_PROGRESS=%s\n", o.Progress)
		if o.Code != "" {
			fmt.Fprintf(os.Stderr, "rex-amq-adapter: %s: %s\n", o.Code, o.Note)
		}
		return o.ExitCode()
	}
	h, err := core.ParseHandle(handle)
	if err != nil {
		return report(core.Outcome{Progress: core.Failed, Code: "usage", Note: err.Error()})
	}
	paths, err := store.PathsForRoot(root)
	if err != nil {
		return report(core.Outcome{Progress: core.Failed, Code: "usage", Note: err.Error()})
	}
	env, err := app.NewEnvAt(server, paths)
	if err != nil {
		return report(core.Outcome{Progress: core.Deferred, Code: "environment", Note: err.Error()})
	}
	// amq wake runs the injector with --inject-timeout 10s. Observation before
	// the write gets 5 s; confirming after it has its own app.ConfirmBudget.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return report(env.Inject(ctx, app.InjectRequest{Block: block, Handle: h, Root: root}))
}

func runEnv() int {
	loc, err := app.Here()
	if err != nil {
		return fail(err)
	}
	env, err := app.NewEnv(loc.Server)
	if err != nil {
		return fail(err)
	}
	out, err := env.Exports(loc)
	if err != nil {
		return fail(err)
	}
	fmt.Print(out)
	return 0
}

func runWho() int {
	env, err := app.NewEnv("")
	if err != nil {
		return fail(err)
	}
	recs, err := env.Store.Records()
	if err != nil {
		return fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HANDLE\tKIND\tSTATE\tUNREAD\tLAST DOORBELL\tBLOCK")
	now := time.Now()
	for _, r := range recs {
		state := "live"
		if !r.Live() {
			state = "departed " + r.DepartedAt.Local().Format("01-02 15:04")
		}
		unread, err := amq.Unread(env.Paths.Root(), string(r.Handle))
		if err != nil {
			return fail(err)
		}
		last := "in-process" // Pi: its extension delivers; no doorbell is typed
		if !r.Kind.InProcess() {
			history, err := env.Store.InjectHistory(r.Handle)
			if err != nil {
				return fail(err)
			}
			last = core.LastDelivery(history, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", r.Handle, r.Kind, state, len(unread), last, r.BlockID)
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	return 0
}

func runCollect() int {
	env, err := app.NewEnv("")
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := env.Collect(ctx); err != nil {
		return fail(err)
	}
	return 0
}

// buildVersion is set at release builds: -ldflags "-X main.buildVersion=v0.1.0".
var buildVersion string

func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	if dirty {
		rev += "-modified"
	}
	return rev
}
