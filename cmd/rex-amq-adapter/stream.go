package main

import (
	"io"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
)

// A stream answers in two ways: a failure before its first frame is one v:2
// envelope like any command's; once a frame is out, stdout carries only
// schema:1 frames, so a later failure or a signal ends it with its exit
// status and no envelope.

// frameCounter remembers whether any frame byte reached stdout.
type frameCounter struct {
	w       io.Writer
	written bool
}

func (f *frameCounter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		f.written = true
	}
	return f.w.Write(b)
}

// streamed is the result of a stream that ended with err: answered on its
// own protocol once a frame went out, or interrupted by a signal; an
// envelope otherwise.
func streamed(c *cli.Ctx, fc *frameCounter, err error) (cli.Result, error) {
	if fc.written || c.Err() != nil {
		return cli.Result{Streamed: true}, err
	}
	return cli.Result{}, err
}

func streamEvents(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	session, tab, all := in.Str("session"), in.Bool("tab"), in.Bool("all")
	picked := 0
	for _, set := range []bool{session != "", tab, all} {
		if set {
			picked++
		}
	}
	if picked > 1 {
		return cli.Result{}, cli.Usage(c.Cmd, "--session, --tab, and --all pick different scopes: give one at most")
	}
	kind := app.ScopeSession
	switch {
	case tab:
		kind = app.ScopeTab
	case all:
		kind = app.ScopeAll
	}
	var env app.Env
	var loc app.Location
	var err error
	if kind == app.ScopeAll || session != "" {
		env, err = app.NewEnv(c.Getenv("REX_SERVER"))
	} else {
		env, loc, err = caller(c)
	}
	if err != nil {
		return cli.Result{}, err
	}
	rctx, cancel := within(c, 10*time.Second)
	sc, err := env.ResolveScope(rctx, kind, loc, session)
	cancel()
	if err != nil {
		return cli.Result{}, err
	}
	fc := &frameCounter{w: c.Out.Frames()}
	return streamed(c, fc, env.Stream(c, fc, sc, app.StreamTicks{}))
}

// streamPanel is the human's panel (app.Env.Panel); the registry's human
// guard has already refused any agent's caller.
func streamPanel(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	fc := &frameCounter{w: c.Out.Frames()}
	return streamed(c, fc, env.Panel(c, fc, c.Getenv, in.Str("session"), app.StreamTicks{}))
}
