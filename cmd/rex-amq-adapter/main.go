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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

const usage = `rex-amq-adapter: AMQ messaging between coding agents in Rex terminals

Agent and user commands:
  env                       print the AMQ identity of this Rex block (eval it)
  who [--all]               list running agents and where their panes are;
                            --all adds departed ones still reserved for a resume
  collect                   reconcile records with Rex now
  tab <crew> [--role R] [-c DIR] -- COMMAND...
                            open a Rex tab for a crew, its first pane labelled
                            with role R, running COMMAND (an agent and its brief)
  crews [dir]               list the crews (named by a Rex tab) and roles (named
                            by a pane label) that brief agents working in dir

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
		return runWho(rest)
	case "collect":
		return runCollect()
	case "crews":
		return runCrews(rest)
	case "tab":
		return runTab(rest)
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

func runWho(args []string) int {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	all := fs.Bool("all", false, "also list departed agents still reserved for a resume")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	env, err := app.NewEnv("")
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Reconcile first, so an agent whose pane closed is listed as departed.
	if err := env.Collect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: collect: %v\n", err)
	}
	recs, err := env.Store.Records()
	if err != nil {
		return fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HANDLE\tKIND\tSTATE\tUNREAD\tLAST DOORBELL\tSESSION\tTAB\tBLOCK")
	now := time.Now()
	where := locator{env: env, sessions: map[string]sessionLookup{}}
	for _, r := range recs {
		if !r.Live() && !*all {
			continue
		}
		state, session, tab := "live", "-", "-"
		if r.Live() {
			session, tab = where.find(ctx, r)
		} else {
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", r.Handle, r.Kind, state, len(unread), last, session, tab, r.BlockID)
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	return 0
}

// locator names the Rex session and tab a live agent's pane is in, asking
// each server about each session once. "?" marks a pane Rex could not place.
type locator struct {
	env      app.Env
	sessions map[string]sessionLookup
}

type sessionLookup struct {
	s   rex.Session
	err error
}

func (l locator) find(ctx context.Context, r store.Record) (session, tab string) {
	term := l.env.RexOn(r.Server)
	blk, err := term.InspectBlock(ctx, r.BlockID)
	if err != nil {
		return "?", "?"
	}
	key := r.Server + " " + blk.SessionID
	got, ok := l.sessions[key]
	if !ok {
		got.s, got.err = term.InspectSession(ctx, blk.SessionID)
		l.sessions[key] = got
	}
	if got.err != nil {
		return "?", "?"
	}
	tab = "?"
	for _, w := range got.s.Windows {
		if w.ID == blk.WindowID {
			tab = w.Label
		}
	}
	return got.s.Label, tab
}

func runTab(args []string) int {
	fs := flag.NewFlagSet("tab", flag.ContinueOnError)
	role := fs.String("role", "", "the role of the first pane's agent")
	dir := fs.String("c", "", "the working directory (default: the current one)")
	session := fs.String("s", os.Getenv("REX_SESSION"), "the Rex session (default: $REX_SESSION)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter tab <crew> [--role R] [-c DIR] [-s SESSION] -- COMMAND...")
		return 2
	}
	tabName := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *session == "" {
		return fail(errors.New("no Rex session: run inside Rex or pass -s"))
	}
	if *dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		*dir = wd
	}
	bin, err := rex.ResolveBin()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := rex.Client{Bin: bin, Server: os.Getenv("REX_SERVER")}
	w, b, err := c.NewWindow(ctx, *session, tabName, *role, *dir, fs.Args())
	if err != nil {
		return fail(err)
	}
	fmt.Printf("{\"window_id\":%q,\"block_ids\":[%q]}\n", w, b)
	return 0
}

func runCrews(args []string) int {
	dir, err := os.Getwd()
	if len(args) > 0 {
		dir, err = args[0], nil
	}
	if err != nil {
		return fail(err)
	}
	entries, err := crew.Load(dir).List()
	if err != nil {
		return fail(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CREW\tCONVENTIONS\tROLES\tFROM")
	for _, e := range entries {
		name, conv := e.Crew, "-"
		if name == "" {
			name = "(any tab)"
		}
		if e.Conventions {
			conv = "yes"
		}
		roles := strings.Join(e.Roles, ", ")
		if roles == "" {
			roles = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, conv, roles, e.Layer)
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
