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
	"github.com/est7/rex-amq-adapter/internal/store"
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

// epicRevive resumes an Epic's agents in place after a Rex restart, for a
// personal agent of a Hub it is linked to or for the user; what it began is
// in data even when part of it failed, which exits 7 (partial_start).
func epicRevive(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	asked, err := readText(in.Str("asked"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	env, _, err := caller(c)
	if errors.Is(err, app.ErrNotInRex) {
		if env, err = app.NewEnv(c.Getenv("REX_SERVER")); err != nil {
			return cli.Result{}, err
		}
	} else if err != nil {
		return cli.Result{}, err
	}
	timeout := in.Duration("timeout", 90*time.Second)
	ctx, cancel := within(c, timeout)
	defer cancel()
	res, err := env.ReviveEpic(ctx, c.Getenv, app.ReviveRequest{Session: in.Arg("session"), Asked: strings.TrimSpace(asked),
		OpID: in.Str("op-id"), Timeout: timeout, DryRun: in.DryRun()})
	switch {
	case err != nil && !res.Touched():
		return cli.Result{}, err
	case err != nil:
		return cli.Result{Data: res}, fault.New(fault.PartialStart, "part of the Epic %s was revived, then: %v", res.SessionID, app.Classify(err).Message).
			With("cause", app.Classify(err).Code)
	case !res.DryRun && !res.Ready():
		return cli.Result{Data: res}, fault.New(fault.PartialStart, "the Epic %s was revived, but not every agent was adopted within %s; data says what each one's startup showed", res.SessionID, timeout)
	}
	return cli.Result{Data: res}, nil
}

// epicList lists the Epics linked to a Hub: the caller's, or from outside a
// personal agent's pane the one --hub names, the Hub of the personal agent
// --personal names (a filter too), or the default Hub (app.ReadCockpitView).
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
	page, err := cli.Page(v.Epics, in, "epic list|"+v.Hub+"|"+string(v.Personal)+"|"+v.SessionID)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: epicPage{List: page, Hub: v.Hub, Personal: string(v.Personal), SessionID: v.SessionID}}, nil
}

// epicPage is the list of a Hub's Epics, with whose they are.
type epicPage struct {
	cli.List[app.CockpitEpic]
	Hub       string `json:"hub,omitempty"`
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
		raw, err := env.ReadCockpitView(ctx, c.Getenv, app.CockpitViewRequest{Session: session, Hub: in.Str("hub"), Personal: in.Str("personal")})
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

// epicGrant is an Epic's notarized grant, its contract digest checked
// again: for the Epic's lead, a personal agent linked to it, or the user
// outside Rex, who names the session.
func epicGrant(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, loc, err := caller(c)
	if errors.Is(err, app.ErrNotInRex) {
		session := in.Arg("session")
		if session == "" {
			return cli.Result{}, cli.Usage(c.Cmd, "outside Rex, name the Epic session whose grant to read")
		}
		if env, err = app.NewEnv(c.Getenv("REX_SERVER")); err != nil {
			return cli.Result{}, err
		}
		g, err := env.ReadGrantAsHuman(c.Getenv, session)
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Result{Data: g, Markdown: g.Markdown}, nil
	}
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
// hand-back, with the artifacts --artifact names snapshot byte for byte
// before anything is recorded (app.SnapshotArtifacts). The caller is looked
// up first, so a refused caller reads no artifact source.
func epicHandback(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	body, err := cli.ReadBody(in.Arg("report"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	var specs []app.ArtifactSpec
	for _, v := range in.All("artifact") {
		s, err := app.ParseArtifact(v)
		if err != nil {
			return cli.Result{}, err
		}
		specs = append(specs, s)
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	if _, err := env.PreviewHandBack(loc, body); err != nil {
		return cli.Result{}, err
	}
	blobs, err := app.SnapshotArtifacts(specs)
	if err != nil {
		return cli.Result{}, err
	}
	req := app.HandBackRequest{Body: body, Artifacts: blobs, OpID: in.Str("op-id")}
	if in.DryRun() {
		h, err := env.PreviewHandBackWith(loc, req)
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Result{Data: map[string]any{"dry_run": true, "by": h.By, "bytes": len(h.Body), "digest": h.Digest, "artifacts": nonNil(h.Artifacts)}}, nil
	}
	ctx, cancel := within(c, 10*time.Second)
	defer cancel()
	h, changed, err := env.RecordHandBackWith(ctx, loc, req)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"by": h.By, "at": h.At, "bytes": len(h.Body), "seq": h.Seq, "digest": h.Digest, "changed": changed,
		"artifacts": nonNil(h.Artifacts), "view": "hand_back"}}, nil
}

// epicArtifacts lists the artifact manifests of an Epic's hand-backs, for
// its linked personal agent or the user (app.Artifacts).
func epicArtifacts(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	ms, err := env.Artifacts(ctx, c.Getenv, in.Arg("session"), in.Int("seq"))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: cli.List[store.Manifest]{Items: ms, Total: len(ms)}}, nil
}

// epicMaterialize writes an Epic's artifacts into --into, byte for byte
// (app.Materialize), reported by materializeResult.
func epicMaterialize(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv(c.Getenv("REX_SERVER"))
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	res, err := env.Materialize(ctx, c.Getenv, app.MaterializeRequest{Session: in.Arg("session"), Into: in.Str("into"), Seq: in.Int("seq"),
		Replace: in.Bool("replace"), DryRun: in.DryRun()})
	return materializeResult(res, err)
}

// materializeResult is the envelope of a materialization. Writing into an
// existing directory is a non-atomic batch, file by file (spec 4.3(8)): it
// is meta.complete=false whenever it wrote, with each file's status in
// data, and so is one that stopped part way. A new directory, created whole,
// a dry run, and a run that found every file as it should be wrote no such
// batch.
func materializeResult(res app.MaterializeResult, err error) (cli.Result, error) {
	if err != nil {
		if len(res.Items) > 0 {
			return cli.Result{Data: res, Incomplete: !res.Complete}, err
		}
		return cli.Result{}, err
	}
	return cli.Result{Data: res, Incomplete: !res.Atomic && res.Changed && !res.DryRun}, nil
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
