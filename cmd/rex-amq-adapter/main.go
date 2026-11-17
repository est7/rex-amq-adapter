// Command rex-amq-adapter lets coding agents in Rex terminals message each
// other over AMQ. See docs/design.md.
//
// Every public command is declared once in the registry (commands.go) and
// prints one v:2 envelope through internal/out. Four internal callbacks
// stay outside it, byte for byte as installed hooks and amq wake call them:
// hook, inject, __internal watch, and __internal pane-title.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:], out.OS(), os.Getenv, os.Stdin))
}

// run runs one invocation and returns its exit status. A panic in a public
// command is caught here: it answers with an internal error (exit 1) and a
// redacted crash dump under <state>/logs. The hook callback catches its own
// (runHook), because a harness waits for a reply on every exit.
func run(args []string, s out.Streams, getenv func(string) string, stdin io.Reader) int {
	if len(args) > 0 {
		switch args[0] {
		case "hook":
			return runHook(args[1:], s, getenv, stdin)
		case "inject":
			return runInject(args[1:], s)
		case "__internal":
			return runInternal(args[1:], s, getenv, stdin)
		}
	}
	p := cli.Printer(args, s, getenv)
	c := &cli.Ctx{Getenv: getenv, Stdin: stdin, Out: p}
	return protect(c, func() int { return execute(c, args) })
}

// protect runs f and turns a panic in it into the invocation's answer: an
// internal error (exit 1) in the output mode the caller asked for, naming
// a redacted crash dump under <state>/logs.
func protect(c *cli.Ctx, f func() int) (code int) {
	defer func() {
		if r := recover(); r != nil {
			p := c.Out
			if c.Cmd != nil {
				p.Verb = c.Cmd.Name()
			}
			path := writeCrash(p.Verb, p.TraceID, r, debug.Stack())
			code = p.Fail(fault.New(fault.Internal, "rex-amq-adapter crashed: %v", r).With("crash_dump", path))
		}
	}()
	return f()
}

// execute runs a public command with signal handling: SIGINT or SIGTERM
// cancels its context, and once it has cleaned up the exit status is the
// signal's (130 or 143).
func execute(c *cli.Ctx, args []string) int {
	p := c.Out
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got os.Signal
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case got = <-sig:
			cancel()
		case <-done:
		}
	}()
	c.Context = ctx
	c.Warn = out.NewWarnings(p)
	restore := divertDiagnostics(c.Warn)
	defer restore()
	code := registry().Execute(c, args)
	if ctx.Err() != nil && got != nil {
		return signalExit(got)
	}
	return code
}

// signalExit is the exit status after a signal and cleanup: 130 for SIGINT,
// 143 for SIGTERM.
func signalExit(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return fault.ExitCode(fault.Terminated)
	}
	return fault.ExitCode(fault.Interrupted)
}

// divertDiagnostics sends the app's housekeeping lines to the run's
// warnings, so a machine caller's stderr stays empty.
func divertDiagnostics(w io.Writer) func() {
	before := app.Diagnostics
	app.Diagnostics = w
	return func() { app.Diagnostics = before }
}

// writeCrash writes a redacted crash dump under <state>/logs and returns
// its path, "" when it could not be written.
func writeCrash(verb, trace string, r any, stack []byte) string {
	paths, err := store.DefaultPaths()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(paths.Logs(), 0o700); err != nil {
		return ""
	}
	now := time.Now().UTC()
	path := filepath.Join(paths.Logs(), "crash-"+now.Format("20060102T150405.000000000Z")+".txt")
	body := fmt.Sprintf("time: %s\nversion: %s\nverb: %s\ntrace_id: %s\npanic: %v\n\n%s", now.Format(time.RFC3339Nano), version(), verb, trace, r, stack)
	if err := os.WriteFile(path, []byte(out.Redact(body)), 0o600); err != nil {
		return ""
	}
	return path
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

// caller resolves the Rex location of the agent running a command (see
// app.Env.Caller) and an Env that talks to its server.
func caller(c *cli.Ctx) (app.Env, app.Location, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return app.Env{}, app.Location{}, err
	}
	ctx, cancel := context.WithTimeout(c, 10*time.Second)
	defer cancel()
	loc, err := env.Caller(ctx, c.Getenv)
	if err != nil {
		return app.Env{}, app.Location{}, err
	}
	if loc.Server != "" && loc.Server != c.Getenv("REX_SERVER") {
		env.Rex = env.RexOn(loc.Server)
	}
	return env, loc, nil
}

// within is c's context bounded by d.
func within(c *cli.Ctx, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c, d)
}
