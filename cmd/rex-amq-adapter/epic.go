package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/blob"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/fault"
)

// epicOpen opens an Epic for the personal agent running it (app.OpenEpic).
// What started is in data even when part of it failed: a partial start
// exits 7 (partial_start) so the caller reads data before ok.
func epicOpen(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	goal, asked := in.Str("goal"), in.Str("asked")
	if goal == "" && asked == "" {
		return cli.Result{}, cli.Usage(c.Cmd, "an Epic needs --goal (the contract), --asked (the user's own words), or both")
	}
	if goal == "-" && asked == "-" {
		return cli.Result{}, cli.Usage(c.Cmd, "--goal - and --asked - both read stdin: give one of them as a file or text")
	}
	goalFile, goalText, err := epicGoal(goal, c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	if asked, err = readText(asked, c.Stdin); err != nil {
		return cli.Result{}, err
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, in.Duration("timeout", 90*time.Second))
	defer cancel()
	res, err := env.OpenEpic(ctx, loc, app.EpicRequest{Name: in.Arg("name"), Dir: in.Str("cwd"), Goal: goalFile, GoalText: goalText,
		Asked: asked, Harness: in.Str("harness"), Crew: in.Str("crew"), OpID: in.Str("op-id"), DryRun: in.DryRun()})
	switch {
	case res.SessionID == "" && err != nil:
		return cli.Result{}, err
	case err != nil:
		return cli.Result{Data: res}, fault.New(fault.PartialStart, "the Epic's session %s started, then: %v", res.SessionID, app.Classify(err).Message).
			With("cause", app.Classify(err).Code)
	case !res.DryRun && !res.Ready():
		return cli.Result{Data: res}, fault.New(fault.PartialStart, "the Epic's session %s started, but not every agent was adopted or linked; the user should look at its panes", res.SessionID)
	}
	return cli.Result{Data: res}, nil
}

// epicGoal is the goal contract --goal names: a file's path as given, or
// with "-" the text piped in on stdin, read to one byte past app.EpicGoalMax
// so OpenEpic refuses an oversized one whole rather than a cut one. Empty
// stdin is refused here: it would leave the Epic with no contract.
func epicGoal(goal string, stdin io.Reader) (file, text string, err error) {
	if goal != "-" {
		return goal, "", nil
	}
	if stdin == nil {
		return "", "", fault.New(fault.Usage, "--goal - reads stdin, and there is none")
	}
	b, err := io.ReadAll(io.LimitReader(stdin, app.EpicGoalMax+1))
	if err != nil {
		return "", "", fault.New(fault.InvalidInput, "read the goal contract from stdin: %v", err)
	}
	if len(b) == 0 {
		return "", "", fault.New(fault.Usage, "--goal - read no goal contract on stdin")
	}
	return "", string(b), nil
}

// readText resolves a body-kind flag that may name @FILE or - for stdin.
func readText(v string, stdin io.Reader) (string, error) {
	if strings.HasPrefix(v, "@") || v == "-" {
		return cli.ReadBody(v, stdin)
	}
	return v, nil
}

// epicClose closes a linked Epic for the personal agent running it, on the
// user's words (--asked, the named confirmation, checked before anything
// else so a missing one is exit 2 anywhere).
func epicClose(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	session := in.Arg("session")
	asked, err := readText(in.Str("asked"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	if strings.TrimSpace(asked) == "" {
		return cli.Result{}, fault.New(fault.ConfirmationRequired, "closing an Epic kills its Rex session and what its panes show: it needs the user's own words asking for it (--asked)").
			WithHint("relay the user's words verbatim; never write them yourself").
			WithNext("rex-amq-adapter epic close " + session + " --asked \"<the user's words>\"")
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	res, err := env.CloseEpic(ctx, loc, app.CloseRequest{Session: session, Asked: asked, Force: in.Bool("force"), OpID: in.Str("op-id"), DryRun: in.DryRun()})
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: res}, nil
}

// epicList lists the Epics linked to a personal agent: the caller, or from
// outside its pane the one --personal or --hub names (app.ReadCockpitView).
func epicList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	raw, err := env.ReadCockpitView(ctx, c.Getenv, app.CockpitViewRequest{Personal: in.Str("personal"), Hub: in.Str("hub")})
	if err != nil {
		return cli.Result{}, err
	}
	var v app.CockpitView
	if err := json.Unmarshal(raw, &v); err != nil {
		return cli.Result{}, err
	}
	page, err := cli.Page(v.Epics, in, "epic list|"+string(v.Personal)+"|"+v.SessionID)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: epicPage{List: page, Personal: string(v.Personal), SessionID: v.SessionID}}, nil
}

// epicPage is the list of a personal agent's Epics, with whose they are.
type epicPage struct {
	cli.List[app.CockpitEpic]
	Personal  string `json:"personal"`
	SessionID string `json:"session_id"`
}

// epicShow is a session's view: the caller's own (a lead or narrator), a
// linked Epic's (a personal agent), or from outside Rex the cockpit's.
func epicShow(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	session := in.Arg("session")
	ctx, cancel := within(c, 60*time.Second)
	defer cancel()
	env, loc, err := caller(c)
	if errors.Is(err, app.ErrNotInRex) {
		if session == "" {
			return cli.Result{}, cli.Usage(c.Cmd, "outside Rex, name the Epic session to show")
		}
		if env, err = app.NewEnv(c.Getenv("REX_SERVER")); err != nil {
			return cli.Result{}, err
		}
		raw, err := env.ReadCockpitView(ctx, c.Getenv, app.CockpitViewRequest{Session: session})
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Result{Data: json.RawMessage(raw)}, nil
	}
	if err != nil {
		return cli.Result{}, err
	}
	raw, err := env.ReadSessionView(ctx, loc, session)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: json.RawMessage(raw)}, nil
}

// epicRefresh rebuilds the narrator's view, keeping or replacing its
// narrative.
func epicRefresh(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	summary, err := readText(in.Str("summary"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 60*time.Second)
	defer cancel()
	v, err := env.RefreshSessionView(ctx, loc, strings.TrimSpace(summary))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: v}, nil
}

// epicMessages reads messages of the narrator's session by id, each body
// capped (--max-body-bytes) with the whole one spilled to a 0600 file.
func epicMessages(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 60*time.Second)
	defer cancel()
	read, err := env.ReadSessionMessages(ctx, loc, strings.Split(in.Str("ids"), ","))
	if err != nil {
		return cli.Result{}, err
	}
	msgs := make([]map[string]any, 0, len(read.Messages))
	for _, m := range read.Messages {
		capped, err := blob.Cap(env.Paths.Blobs(), m.Body, in.Int("max-body-bytes"))
		if err != nil {
			return cli.Result{}, err
		}
		row := map[string]any{"ref": m.Ref, "from": m.From, "to": m.To, "thread": m.Thread, "subject": m.Subject, "created": m.Created,
			"kind": m.Kind, "priority": m.Priority, "body": capped.Body, "truncated": capped.Truncated || m.Truncated, "total_bytes": capped.TotalBytes}
		if capped.FullPath != "" {
			row["full_path"] = capped.FullPath
		}
		msgs = append(msgs, row)
	}
	return cli.Result{Data: map[string]any{"messages": msgs, "coverage_gaps": read.CoverageGaps}}, nil
}

// epicGrant is an Epic's notarized grant, its contract digest checked again.
func epicGrant(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	g, err := env.ReadGrant(loc, in.Arg("session"))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: g, Markdown: g.Markdown}, nil
}

// epicHandback records the lead's whole final report as its session's
// hand-back. Artifacts belong to design Epics, which no session is yet:
// one given is refused, never dropped.
func epicHandback(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	if len(in.All("artifact")) > 0 {
		return cli.Result{}, fault.New(fault.FailedPrecondition, "--artifact is for a design Epic's lead, and this adapter opens no design Epics yet").With("reason", "not_design_epic")
	}
	body, err := cli.ReadBody(in.Arg("report"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	if in.DryRun() {
		h, err := env.PreviewHandBack(loc, body)
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Result{Data: map[string]any{"dry_run": true, "by": h.By, "bytes": len(h.Body), "digest": h.Digest}}, nil
	}
	ctx, cancel := within(c, 10*time.Second)
	defer cancel()
	h, err := env.RecordHandBack(ctx, loc, body)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"by": h.By, "at": h.At, "bytes": len(h.Body), "seq": h.Seq, "digest": h.Digest, "view": "hand_back"}}, nil
}

// epicArtifacts lists the artifact manifests an Epic's hand-backs carry.
// Only a design Epic's hand-back carries any, and none is opened yet, so
// the list is empty: an empty list, not an error.
func epicArtifacts(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	return cli.Result{Data: cli.List[map[string]any]{Items: []map[string]any{}}}, nil
}

// epicMaterialize writes an Epic's artifacts into --into. No hand-back
// carries any yet (see epicArtifacts), so there is nothing to write: the
// directory is neither created nor touched.
func epicMaterialize(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	return cli.Result{}, fault.New(fault.NotFound, "Epic session %s has no artifacts to materialize", in.Arg("session")).With("kind", "artifact")
}

func epicLink(c *cli.Ctx, in cli.Input) (cli.Result, error) { return linkEpic(c, in, true) }

func epicUnlink(c *cli.Ctx, in cli.Input) (cli.Result, error) { return linkEpic(c, in, false) }

// linkEpic links or unlinks the caller's (a personal agent's) session with
// an Epic's narrator.
func linkEpic(c *cli.Ctx, in cli.Input, link bool) (cli.Result, error) {
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 10*time.Second)
	defer cancel()
	do := env.Link
	if in.DryRun() {
		do = env.PreviewLink
	}
	l, changed, err := do(ctx, loc, in.Arg("session"), link)
	if err != nil {
		return cli.Result{}, err
	}
	d := map[string]any{"personal_session": l.Personal.SessionID, "epic_session": l.Epic.SessionID, "linked": link, "changed": changed}
	if in.DryRun() {
		d["dry_run"] = true
	}
	return cli.Result{Data: d}, nil
}
