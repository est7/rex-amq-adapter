package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/out"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The four internal callbacks (§3.2): what installed hooks and amq wake run.
// Their argv, output, and exit codes are other programs' protocols, kept
// byte for byte; they are not in help, schema, or the skill.

// runHook is `hook <event> --agent <kind>`, the command every installed
// hook runs ('<bin>' hook <event> --agent <kind> 2>/dev/null || true). Every
// exit prints a reply in the harness's format, a panic included:
// Antigravity reads a JSON object even when the adapter has nothing to say
// or failed. Failures go to logs/hooks.log, the only place a hook's
// problems can be read.
func runHook(args []string, s out.Streams, getenv func(string) string, stdin io.Reader) (code int) {
	if len(args) == 0 {
		fmt.Fprintln(s.Stderr, "rex-amq-adapter: hook: missing event")
		return 1
	}
	event := args[0]
	log := openHookLog(event, getenv)
	defer log.Close()
	kind := core.Kind("")
	replied := false
	reply := func(r app.Reply) {
		if replied {
			return
		}
		replied = true
		if text, err := app.Render(kind, event, r); err == nil {
			out.HookReply(s, text)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(log, "panic: %v\n", r)
			if kind != "" {
				reply(app.Reply{})
			}
			code = 0
		}
	}()
	before := app.Diagnostics
	app.Diagnostics = log
	defer func() { app.Diagnostics = before }()
	budget, ok := install.Budget(event)
	if !ok {
		return hookFail(s, log, fmt.Errorf("unknown event %q", event))
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(log)
	agent := fs.String("agent", "", "agent kind: claude, codex, agy, or pi")
	if err := fs.Parse(args[1:]); err != nil {
		return hookFail(s, log, err)
	}
	k, err := core.ParseKind(*agent)
	if err != nil {
		return hookFail(s, log, err)
	}
	kind = k
	in, err := app.ReadHookInput(stdin)
	if err != nil {
		reply(app.Reply{})
		return hookFail(s, log, err)
	}
	in.SummonToken = getenv(app.SummonEnv)
	in.Launch = app.LaunchFromEnv(func(k string) (string, bool) { v := getenv(k); return v, v != "" })
	in.Unattended = getenv(app.UnattendedEnv) == "1"
	loc, err := app.Here()
	if errors.Is(err, app.ErrNotInRex) {
		reply(app.Reply{})
		return 0 // outside Rex the adapter does nothing
	}
	env, err := app.NewEnv(loc.Server)
	if err != nil {
		reply(app.Reply{})
		return hookFail(s, log, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if kind == core.Claude && in.AgentPID <= 0 {
		// The harness process, which is not the pane's foreground when Claude
		// Code's supervisor runs the session in the background.
		in.AgentPID, _ = strconv.Atoi(getenv("CLAUDE_PID"))
	}
	r, err := env.Hook(ctx, event, kind, loc, in)
	if err != nil {
		reply(app.Reply{})
		return hookFail(s, log, err)
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

func openHookLog(event string, getenv func(string) string) *hookLog {
	return &hookLog{prefix: event + " block=" + getenv("REX_BLOCK")}
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

func hookFail(s out.Streams, log *hookLog, err error) int {
	fmt.Fprintf(log, "%v\n", err)
	fmt.Fprintf(s.Stderr, "rex-amq-adapter: %v\n", err)
	return 1
}

// runInject is `inject <handle> <root> <payload>`, amq wake's injector:
// exactly three positional arguments, the third amq's notification payload,
// deliberately unused (it previews the sender's subject; see
// core.Doorbell). The Rex server and block are the handle's lease's, read
// when the doorbell fires. It reports on stderr with AMQ_INJECT_PROGRESS
// and exits 0 or 1.
func runInject(args []string, s out.Streams) int {
	if len(args) != 3 {
		out.InjectProgress(s, string(core.Failed), "usage", "inject: want <handle> <root> <payload>")
		return 1
	}
	handle, root := args[0], args[1]
	report := func(o core.Outcome) int {
		out.InjectProgress(s, string(o.Progress), o.Code, o.Note)
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
	env, err := app.NewEnvAt("", paths)
	if err != nil {
		return report(core.Outcome{Progress: core.Deferred, Code: "environment", Note: err.Error()})
	}
	// amq wake runs the injector with --inject-timeout 10s. Observation before
	// the write gets 5 s; confirming after it has its own app.ConfirmBudget.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return report(env.Inject(ctx, app.InjectRequest{Handle: h, Root: root}))
}

// runInternal is the adapter's own plumbing: `watch <server> <session>`, a
// session's watcher its orchestrator's or narrator's hooks start, and
// `pane-title --agent <kind>`, a harness asking for its window title.
func runInternal(args []string, s out.Streams, getenv func(string) string, stdin io.Reader) int {
	switch {
	case len(args) == 3 && args[0] == "watch":
		return runWatch(args[1], args[2], s)
	case len(args) == 3 && args[0] == "pane-title" && args[1] == "--agent":
		out.PaneTitle(s, paneTitle(args[2], stdin))
		return 0
	}
	fmt.Fprintln(s.Stderr, "rex-amq-adapter: __internal: want `watch <server> <session>` or `pane-title --agent K`")
	return 1
}

// runWatch is a session's watcher. It ends when the session has neither an
// orchestrator nor a narrator.
func runWatch(server, session string, s out.Streams) int {
	if server == "" || session == "" {
		out.WatchFailed(s, errors.New("__internal watch: want <server> <session>"))
		return 1
	}
	env, err := app.NewEnv(server)
	if err != nil {
		out.WatchFailed(s, err)
		return 1
	}
	if err := env.WatchSession(context.Background(), core.SessionKey{Server: server, SessionID: session}); err != nil {
		out.WatchFailed(s, err)
		return 1
	}
	return 0
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
