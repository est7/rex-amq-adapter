package main

import (
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/hub"
)

// hubList is the resolved Hub directories and the configured list.
func hubList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	dirs, err := hub.Dirs(c.Getenv)
	if err != nil {
		return cli.Result{}, err
	}
	configured, err := hub.Configured(c.Getenv)
	if err != nil {
		return cli.Result{}, err
	}
	if configured == nil {
		configured = []string{}
	}
	path, err := hub.ConfigPath(c.Getenv)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"items": dirs, "total": len(dirs), "truncated": false, "configured": configured, "config": path}}, nil
}

func hubAdd(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	ch, err := hub.Add(c.Getenv, in.Arg("dir"), in.DryRun())
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: ch}, nil
}

func hubRemove(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	ch, err := hub.Remove(c.Getenv, in.Arg("dir"), in.DryRun())
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: ch}, nil
}

// hubNotify rings the live personal agent for an outside producer's event
// (app.NotifyPersonal). It acts as no agent, so it resolves no caller: a
// launchd job runs it. No live personal agent is a result (exit 0), not an
// error.
func hubNotify(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	n, err := app.ParseNotify(app.NotifyRequest{Ref: in.Str("ref"), EventID: in.Str("event-id"), Hub: in.Str("hub"), Personal: in.Str("personal"), Summary: in.Str("summary")})
	if err != nil {
		return cli.Result{}, err
	}
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	notify := env.NotifyPersonal
	if in.DryRun() {
		notify = env.PreviewNotify
	}
	res, err := notify(ctx, n)
	if err != nil {
		if res.Status != "" {
			return cli.Result{Data: res}, err
		}
		return cli.Result{}, err
	}
	return cli.Result{Data: res}, nil
}
