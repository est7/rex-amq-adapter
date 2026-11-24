package main

import (
	"strconv"
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

// hubInbox reads a Hub's inbox for the Operator (app.ReadHubInbox), from
// the records alone: it asks no Rex, so it reads with Rex down.
func hubInbox(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	wait := in.Duration("wait", 0)
	ctx, cancel := within(c, wait+30*time.Second)
	defer cancel()
	page, err := env.ReadHubInbox(ctx, c.Getenv, app.HubInboxRequest{Hub: in.Str("hub"), Since: int64(in.Int("since")), Wait: wait,
		Epic: in.Str("epic"), Kind: in.Str("kind"), Limit: in.Int("limit")})
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: page}, nil
}

func hubSend(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	return hubMessage(c, in, app.HubSendRequest{Session: in.Arg("session")})
}

func hubReply(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	seq, _ := strconv.ParseInt(in.Arg("seq"), 10, 64) // checked as an int by the registry
	return hubMessage(c, in, app.HubSendRequest{ReplyTo: seq})
}

// hubMessage sends the Operator's message to an Epic's narrator
// (app.SendToNarrator).
func hubMessage(c *cli.Ctx, in cli.Input, r app.HubSendRequest) (cli.Result, error) {
	body, err := cli.ReadBody(in.Str("body"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	env, op, err := operator(c)
	if err != nil {
		return cli.Result{}, err
	}
	env = onServer(env, c, op.Loc.Server)
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	r.Hub, r.Body, r.OpID, r.DryRun = in.Str("hub"), body, in.Str("op-id"), in.DryRun()
	res, err := env.SendToNarrator(ctx, c.Getenv, r)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: res}, nil
}
