// Command rex-amq-adapter lets coding agents in Rex terminals message each
// other over AMQ. See docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

const usage = `rex-amq-adapter: AMQ messaging between coding agents in Rex terminals

Agent and user commands:
  env                       print the AMQ identity of the agent running it, and its
                            Rex pane when REX_BLOCK names another (eval it)
  title [TASK...]           show TASK after your role and handle in this pane's
                            label ("<role>(<handle>) | <task>"); no TASK clears it
  who [--all]               list agents in this Rex session only, with their roles
                            and tabs;
                            --all adds this session's departed agents reserved for a resume
  events --json [--session S | --tab | --all]
                            stream this Rex session's agents and mail as NDJSON
                            (snapshot, then upsert/remove/event/heartbeat
                            frames); read-only. --tab: this tab only; --all:
                            every agent of the server
  await <block> [--timeout D]
                            wait for the agent started in a block, then print
                            its outcome as JSON; exits 0 only when adopted
  collect                   reconcile records with Rex now
  join [CREW] [--role R]    take role R in crew CREW where you stand: name this
                            tab after CREW (while no other agent is in it),
                            relabel your pane, and print the briefing; without
                            --role, take the crew's lead
  tab <crew> [--role R] [-c DIR] [-s SESSION] -- COMMAND...
                            open a Rex tab for a crew, its first pane labelled
                            with role R, running COMMAND (an agent and its brief)
  crews [dir]               list the crews (named by a Rex tab), their lead and
                            members, and the roles (named by a pane label) that
                            brief agents working in dir
  summon <handle|role> --subject S --body B [--kind K] [--grant FILE | --asked "WORDS"] [--harness H] [--cwd DIR]
                            send to a peer by handle or role: a live one gets the
                            mail; a departed one of this session is resumed in a
                            pane beside you, then gets it; none starts one for the
                            role (its brief is the message), awaits its adoption,
                            then delivers. Guards: a request kind only, no cycle,
                            depth 2 at most, 6 live agents per session at most, and
                            a new agent only with the user's words or grant file
  trust <dir>               derive, for a linked worktree of a repository the user
                            already trusts in Antigravity, that trust for DIR, so a
                            peer started there meets no folder dialog; writes
                            nothing for any other directory (tab runs it for -c)

Installation:
  setup|status|remove       install, inspect, or remove the skill, hooks, and
                            harness settings (agy title, shared supervisors off,
                            native task tools on)

Internal (run by installed hooks and by amq wake):
  hook <event> --agent K    session-start | user-prompt-submit | post-tool-use | pre-invocation | stop | stop-failure | session-end | mail | settle
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
	case "title":
		return runTitle(rest)
	case "who":
		return runWho(rest)
	case "events":
		return runEvents(rest)
	case "await":
		return runAwait(rest)
	case "collect":
		return runCollect()
	case "crews":
		return runCrews(rest)
	case "tab":
		return runTab(rest)
	case "trust":
		return runTrust(rest)
	case "summon":
		return runSummon(rest)
	case "join":
		return runJoin(rest)
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
	agent := fs.String("agent", "", "agent kind: claude, codex, agy, or pi")
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
	in.SummonToken = os.Getenv(app.SummonEnv)
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
	if kind == core.Claude && in.AgentPID <= 0 {
		// The harness process, which is not the pane's foreground when Claude
		// Code's supervisor runs the session in the background.
		in.AgentPID, _ = strconv.Atoi(os.Getenv("CLAUDE_PID"))
	}
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

// caller resolves the Rex location of the agent running a command (see
// app.Env.Caller) and an Env that talks to its server.
func caller() (app.Env, app.Location, error) {
	env, err := app.NewEnv(os.Getenv("REX_SERVER"))
	if err != nil {
		return app.Env{}, app.Location{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loc, err := env.Caller(ctx, os.Getenv)
	if err != nil {
		return app.Env{}, app.Location{}, err
	}
	if loc.Server != "" && loc.Server != os.Getenv("REX_SERVER") {
		env.Rex = env.RexOn(loc.Server)
	}
	return env, loc, nil
}

// runEnv prints the shell text agents eval. A failure prints `false` as well,
// because `eval ""` succeeds and `eval "$(… env)" && …` would carry on. A
// caller located by its session, whose REX_BLOCK names another pane or
// nothing, also gets the Rex location of its own pane; a background job no
// pane shows has the inherited one unset, because it names another agent's
// pane.
func runEnv() int {
	failEnv := func(err error) int {
		if errors.Is(err, app.ErrJobNotShown) {
			fmt.Print("unset AM_ROOT AM_ME REX_BLOCK REX_SESSION; ")
		}
		fmt.Println("false")
		return fail(err)
	}
	env, loc, err := caller()
	if err != nil {
		return failEnv(err)
	}
	out, err := env.Exports(loc)
	if err != nil {
		return failEnv(err)
	}
	if loc.Block != os.Getenv("REX_BLOCK") || os.Getenv("REX_SESSION") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rexOut, err := env.RexExports(ctx, loc.Block)
		if err != nil {
			return failEnv(err)
		}
		out += rexOut
	}
	fmt.Print(out)
	return 0
}

func runTitle(args []string) int {
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	label, err := env.SetTask(ctx, loc, strings.Join(args, " "))
	if err != nil {
		return fail(err)
	}
	fmt.Println(label)
	return 0
}

func runWho(args []string) int {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	all := fs.Bool("all", false, "also list this session's departed agents still reserved for a resume")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := env.FindSession(ctx, loc.Block)
	if err != nil {
		return fail(err)
	}
	// Reconcile first, so an agent whose pane closed is listed as departed.
	// Collection is housekeeping; the listing must still happen.
	if err := env.CollectSession(ctx, session); err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: collect: %v\n", err)
	}
	if err := env.SuperviseSession(ctx, session); err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: supervise: %v\n", err)
	}
	agents, err := env.SessionAgents(session, *all)
	if err != nil {
		return fail(err)
	}
	now := env.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HANDLE\tKIND\tROLE\tTAB\tSTATE\tUNREAD\tLAST DOORBELL\tBLOCK")
	for _, a := range agents {
		state := "live"
		if d := a.Lifecycle.DepartedAt; d != nil {
			state = "departed " + d.Local().Format("01-02 15:04")
		}
		role := a.Role
		if role == "" {
			role = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", a.Handle, a.Kind, role, a.Tab, state, a.Delivery.Unread, a.DoorbellSummary(now), a.BlockID)
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	return 0
}

// runEvents streams the projection of a scope. It only reads: it takes no
// adapter lock and writes nothing but its frames, so watching changes nothing.
func runEvents(args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write NDJSON frames (the only format)")
	session := fs.String("session", "", "watch Rex session S")
	tab := fs.Bool("tab", false, "watch the caller's tab only")
	all := fs.Bool("all", false, "watch every agent of the Rex server")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	picked := 0
	for _, set := range []bool{*session != "", *tab, *all} {
		if set {
			picked++
		}
	}
	if !*asJSON || picked > 1 || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter events --json [--session S | --tab | --all]")
		return 2
	}
	kind := app.ScopeSession
	switch {
	case *tab:
		kind = app.ScopeTab
	case *all:
		kind = app.ScopeAll
	}
	var env app.Env
	var loc app.Location
	var err error
	if kind == app.ScopeAll || *session != "" {
		env, err = app.NewEnv(os.Getenv("REX_SERVER"))
	} else {
		env, loc, err = caller()
	}
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sc, err := env.ResolveScope(rctx, kind, loc, *session)
	cancel()
	if err != nil {
		return fail(err)
	}
	if err := env.Stream(ctx, os.Stdout, sc, app.StreamTicks{}); err != nil {
		return fail(err)
	}
	return 0
}

// runWatch is the orchestrator watcher, started by a orchestrator's hooks. It ends with
// its orchestrator.
func runWatch(server, handle string) int {
	h, err := core.ParseHandle(handle)
	if err != nil {
		return fail(err)
	}
	env, err := app.NewEnv(server)
	if err != nil {
		return fail(err)
	}
	if err := env.WatchOrchestrator(context.Background(), h); err != nil {
		return fail(err)
	}
	return 0
}

func runAwait(args []string) int {
	fs := flag.NewFlagSet("await", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter await <block> [--timeout D]")
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	env, err := app.NewEnv("")
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	s, err := env.AwaitStartup(ctx, args[0])
	if err != nil {
		return fail(err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		return fail(err)
	}
	fmt.Println(string(out))
	if s.Status != core.StartupAdopted {
		return 1
	}
	return 0
}

func runTab(args []string) int {
	fs := flag.NewFlagSet("tab", flag.ContinueOnError)
	role := fs.String("role", "", "the role of the first pane's agent")
	dir := fs.String("c", "", "the working directory (default: the current one)")
	session := fs.String("s", "", "the Rex session (default: the caller's)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter tab <crew> [--role R] [-c DIR] [-s SESSION] -- COMMAND...")
		return 2
	}
	tabName := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *session == "" {
		// The caller's own session: REX_SESSION is inherited, and in a Claude
		// Code background session it names another terminal's.
		env, loc, err := caller()
		if err != nil {
			return fail(fmt.Errorf("no Rex session (pass -s): %w", err))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		b, err := env.Rex.InspectBlock(ctx, loc.Block)
		cancel()
		if err != nil {
			return fail(err)
		}
		*session = b.SessionID
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
	w, b, err := c.NewWindow(ctx, *session, tabName, *role, *dir, trust.Args(fs.Args(), deriveTrust(*dir).Settings))
	if err != nil {
		return fail(err)
	}
	// The tab is open either way; unrecorded, it names its crew only while
	// its label leads with one that exists.
	if paths, err := store.DefaultPaths(); err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: record the tab %s: %v\n", w, err)
	} else if err := app.ClaimTab(store.Store{P: paths}, *session, w, tabName); err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: record the tab %s: %v\n", w, err)
	}
	fmt.Printf("{\"window_id\":%q,\"block_ids\":[%q]}\n", w, b)
	return 0
}

// deriveTrust extends, for one session, the user's Antigravity trust of a
// repository to the linked worktree a peer is about to start in. A failure is
// reported and does not stop the launch: the peer then meets the dialog.
func deriveTrust(dir string) trust.Result {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: trust: %v\n", err)
		return trust.Result{}
	}
	r, err := trust.Derive(home, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: trust: %v\n", err)
	} else if r.Settings != "" {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %s: %s\n", r.Harness, r.Note)
	}
	return r
}

func runSummon(args []string) int {
	fs := flag.NewFlagSet("summon", flag.ContinueOnError)
	subject := fs.String("subject", "", "the message's subject (a started peer's task label)")
	body := fs.String("body", "", "the message (a started peer's brief); @file reads it")
	kind := fs.String("kind", "question", "amq kind: question, review_request, todo, brainstorm summon; others deliver only")
	grant := fs.String("grant", "", "the user's grant file the brief cites (allows a start)")
	asked := fs.String("asked", "", "the user's own words asking for a new agent, quoted (allows a start)")
	harness := fs.String("harness", "", "claude, codex, agy, or pi for a start (default: by role)")
	cwd := fs.String("cwd", "", "the working directory of a started agent, such as its Slice worktree (default: yours)")
	timeout := fs.Duration("timeout", 45*time.Second, "how long to wait for a launched agent's adoption")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter summon <handle|role> --subject S --body B [--kind K] [--grant FILE | --asked WORDS] [--harness H] [--cwd DIR]")
		return 2
	}
	target := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if strings.HasPrefix(*body, "@") {
		b, err := os.ReadFile((*body)[1:])
		if err != nil {
			return fail(err)
		}
		*body = string(b)
	}
	if *subject == "" || *body == "" {
		return fail(errors.New("summon needs --subject and --body"))
	}
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	res, err := env.Summon(ctx, loc, app.SummonRequest{Target: target, Kind: *kind, Subject: *subject, Body: *body, Grant: *grant, Asked: *asked, Harness: *harness, Cwd: *cwd})
	cancel()
	if err != nil {
		return fail(err)
	}
	out := map[string]any{"action": res.Action, "handles": res.Handles, "block": res.Block, "reason": res.Reason, "parent": res.Parent, "depth": res.Depth, "mailed": res.Mailed}
	if res.Block != "" {
		wctx, wcancel := context.WithTimeout(context.Background(), *timeout)
		st, err := env.AwaitStartup(wctx, res.Block)
		wcancel()
		if err != nil {
			return fail(err)
		}
		out["startup"] = st.Status
		if st.Handle != "" {
			out["handles"] = []core.Handle{st.Handle}
			if !res.Mailed {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				err = env.Deliver(ctx, loc, st.Handle, *kind, *subject, *body)
				cancel()
				if err != nil {
					return fail(err)
				}
				out["mailed"] = true
			}
		}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
	if res.Action == "refused" {
		return 3
	}
	if st, ok := out["startup"].(core.StartupStatus); ok && st != core.StartupAdopted {
		return 1 // launched, not adopted: the mail waits, the user must look at the pane
	}
	return 0
}

func runTrust(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter trust <dir>  (prints agy's --settings value for dir; exit 1 when there is none)")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	r, err := trust.Derive(home, args[0])
	if err != nil {
		return fail(err)
	}
	// stdout is the --settings value alone, so a hand-started peer can take
	// it: agy --settings "$(rex-amq-adapter trust <dir>)" …
	fmt.Fprintf(os.Stderr, "%s: %s\n", r.Harness, r.Note)
	if r.Settings == "" {
		return 1
	}
	fmt.Println(r.Settings)
	return 0
}

func runJoin(args []string) int {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	role := fs.String("role", "", "the role to take (default: the crew's lead)")
	crewName := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		crewName, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || crewName == "" && *role == "" {
		fmt.Fprintln(os.Stderr, "usage: rex-amq-adapter join [CREW] [--role R]")
		return 2
	}
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	j, err := env.Join(ctx, loc, crewName, *role)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Your pane shows %s", j.Label)
	if j.Crew.Name != "" {
		fmt.Printf("; this tab is crew %s", j.Crew.Name)
	}
	fmt.Println(".")
	if j.Briefing != "" {
		fmt.Println()
		fmt.Println(j.Briefing)
	}
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
	fmt.Fprintln(tw, "CREW\tCONVENTIONS\tLEAD\tROLES\tFROM")
	for _, e := range entries {
		name, conv, lead := e.Crew, "-", e.Lead
		if name == "" {
			name = "(any tab)"
		}
		if e.Conventions {
			conv = "yes"
		}
		if lead == "" {
			lead = "-"
		}
		// A crew's roles are its members; one that declares none has the
		// role prompts of its own directory.
		roles := strings.Join(e.Members, ", ")
		if roles == "" {
			roles = strings.Join(e.Roles, ", ")
		}
		if roles == "" {
			roles = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, conv, lead, roles, e.Layer)
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
