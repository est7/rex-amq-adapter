package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
)

// agentRow is an agent as `agent list` returns it: every core.Agent field,
// plus state (live or departed) and unread, its default columns.
func agentRow(a core.Agent) (map[string]any, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["state"] = "live"
	if a.Lifecycle.DepartedAt != nil {
		m["state"] = "departed"
	}
	m["unread"] = a.Delivery.Unread
	return m, nil
}

// agentList lists the agents of the caller's Rex session, or of --session.
// It only reads: no collection, no waker repair (`agent collect` and
// `mail send` own those).
func agentList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	var env app.Env
	var err error
	sessionID := in.Str("session")
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	var s app.Session
	if sessionID != "" {
		if env, err = app.NewEnv(c.Getenv("REX_SERVER")); err != nil {
			return cli.Result{}, err
		}
		if s, err = env.SessionByID(ctx, sessionID); err != nil {
			return cli.Result{}, err
		}
	} else {
		var loc app.Location
		if env, loc, err = caller(c); err != nil {
			return cli.Result{}, err
		}
		if s, err = env.FindSession(ctx, loc.Block); err != nil {
			return cli.Result{}, err
		}
	}
	agents, err := env.SessionAgents(s, in.Bool("all"))
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([]map[string]any, 0, len(agents))
	for _, ag := range agents {
		row, err := agentRow(ag)
		if err != nil {
			return cli.Result{}, err
		}
		rows = append(rows, row)
	}
	page, err := cli.Page(rows, in, fmt.Sprintf("agent list|%s|all=%v", s.ID(), in.Bool("all")))
	if err != nil {
		return cli.Result{}, err
	}
	now := env.Now()
	shown := agents
	return cli.Result{Data: page, Human: func(w io.Writer) error {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "HANDLE\tKIND\tROLE\tTAB\tSTATE\tUNREAD\tLAST DOORBELL\tBLOCK")
		byHandle := map[string]core.Agent{}
		for _, ag := range shown {
			byHandle[string(ag.Handle)] = ag
		}
		for _, row := range page.Items {
			ag := byHandle[fmt.Sprint(row["handle"])]
			state := "live"
			if d := ag.Lifecycle.DepartedAt; d != nil {
				state = "departed " + d.Local().Format("01-02 15:04")
			}
			role := ag.Role
			if role == "" {
				role = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", out.Visible(string(ag.Handle)), ag.Kind, out.Visible(role), out.Visible(ag.Tab), state, ag.Delivery.Unread, ag.DoorbellSummary(now), ag.BlockID)
		}
		return tw.Flush()
	}}, nil
}

// agentEnv is the caller's AMQ identity: an envelope, or with --sh the
// export lines agents eval. A failure under --sh prints `false` as well,
// because `eval ""` succeeds and `eval "$(… --sh)" && …` would carry on; a
// background job no pane shows has the inherited Rex location unset, since
// it names another agent's pane.
func agentEnv(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	sh := in.Bool("sh")
	data, exports, err := agentEnvData(c)
	if sh {
		if err != nil {
			prefix := ""
			if errors.Is(err, app.ErrJobNotShown) {
				prefix = "unset AM_ROOT AM_ME REX_BLOCK REX_SESSION; "
			}
			io.WriteString(c.Out.Frames(), prefix+"false\n")
			f := app.Classify(err)
			fmt.Fprintf(c.Out.Stderr, "rex-amq-adapter: %s\n", out.Visible(f.Message))
			return cli.Result{Streamed: true}, f
		}
		io.WriteString(c.Out.Frames(), exports)
		return cli.Result{Streamed: true}, nil
	}
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: data, Human: func(w io.Writer) error { _, err := io.WriteString(w, exports); return err }}, nil
}

func agentEnvData(c *cli.Ctx) (map[string]any, string, error) {
	env, loc, err := caller(c)
	if err != nil {
		return nil, "", err
	}
	se, err := env.ShellEnvOf(loc)
	if err != nil {
		return nil, "", err
	}
	exports, err := env.Exports(loc)
	if err != nil {
		return nil, "", err
	}
	data := map[string]any{"AM_ROOT": se.AMRoot, "AM_ME": se.AMMe, "ME_UNATTENDED": se.Unattended, "role": se.Role, "REX_BLOCK": loc.Block, "REX_SESSION": c.Getenv("REX_SESSION")}
	if loc.Block != c.Getenv("REX_BLOCK") || c.Getenv("REX_SESSION") == "" {
		ctx, cancel := within(c, 10*time.Second)
		defer cancel()
		rexOut, err := env.RexExports(ctx, loc.Block)
		if err != nil {
			return nil, "", err
		}
		session, err := env.SessionOfBlock(ctx, loc.Block)
		if err != nil {
			return nil, "", err
		}
		data["REX_SESSION"] = session
		exports += rexOut
	}
	return data, exports, nil
}

func agentTitle(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 10*time.Second)
	defer cancel()
	label, err := env.SetTask(ctx, loc, strings.Join(in.Rest("task"), " "))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"label": label}, Human: line(label)}, nil
}

// line renders one line of text.
func line(s string) func(io.Writer) error {
	return func(w io.Writer) error { _, err := fmt.Fprintln(w, out.Visible(s)); return err }
}

func agentJoin(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	crewName, role := in.Arg("crew"), in.Str("role")
	if crewName == "" && role == "" {
		return cli.Result{}, cli.Usage(c.Cmd, "name a CREW, a --role, or both")
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	if in.DryRun() {
		return cli.Result{Data: map[string]any{"dry_run": true, "would": "join", "crew": crewName, "role": role, "block": loc.Block}}, nil
	}
	ctx, cancel := within(c, 20*time.Second)
	defer cancel()
	j, err := env.Join(ctx, loc, crewName, role)
	if err != nil {
		return cli.Result{}, err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "Your pane shows %s", j.Label)
	if j.Crew.Name != "" {
		fmt.Fprintf(&md, "; this Rex session is crew %s", j.Crew.Name)
	}
	md.WriteString(".\n")
	if j.Briefing != "" {
		md.WriteString("\n" + j.Briefing + "\n")
	}
	return cli.Result{Data: map[string]any{"label": j.Label, "crew": j.Crew.Name, "briefing": j.Briefing, "markdown": md.String()}, Markdown: md.String()}, nil
}

// summonData is the summon result as data: what it did, and for a start the
// harness it launched and each default it passed over.
func summonData(res app.SummonResult) map[string]any {
	d := map[string]any{"action": res.Action, "handles": res.Handles, "block": res.Block, "reason": res.Reason, "parent": res.Parent, "depth": res.Depth, "mailed": res.Mailed}
	if res.Handles == nil {
		d["handles"] = []core.Handle{}
	}
	if res.Harness != "" {
		d["harness"] = res.Harness
	}
	if len(res.Passed) > 0 {
		d["passed_over"] = res.Passed
	}
	if res.AgentSessionID != "" {
		d["agent_session_id"] = res.AgentSessionID
	}
	if res.Cwd != "" {
		d["cwd"] = res.Cwd
	}
	if res.DryRun {
		d["dry_run"] = true
	}
	return d
}

func agentSummon(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	target := in.Arg("target")
	if in.Bool("council") {
		// Settled before the caller is: a usage error is one anywhere.
		if err := app.CouncilUsage(target, in.Str("harness")); err != nil {
			return cli.Result{}, cli.Usage(c.Cmd, "%v", err)
		}
	}
	body, err := cli.ReadBody(in.Str("body"), c.Stdin)
	if err != nil {
		return cli.Result{}, err
	}
	asked := in.Str("asked")
	if strings.HasPrefix(asked, "@") || asked == "-" {
		if asked, err = cli.ReadBody(asked, c.Stdin); err != nil {
			return cli.Result{}, err
		}
	}
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	req := app.SummonRequest{Target: target, Kind: in.Str("kind"), Subject: in.Str("subject"), Body: body, Grant: in.Str("grant"), Asked: asked,
		Harness: in.Str("harness"), Cwd: in.Str("cwd"), Split: in.Bool("split"), Council: in.Bool("council"), DryRun: in.DryRun()}
	res, err := env.Summon(ctx, loc, req)
	cancel()
	if err != nil {
		if len(res.Seats) > 0 {
			// A council that failed midway: say which seats were mailed or launched.
			d := summonData(res)
			d["seats"] = res.Seats
			return cli.Result{Data: d}, fault.Wrap(err, fault.SeatFailed)
		}
		return cli.Result{}, err
	}
	if res.Action == "refused" {
		return cli.Result{}, app.SummonRefusal(res.Reason).With("summon", summonData(res))
	}
	timeout := in.Duration("timeout", 45*time.Second)
	if req.Council {
		return summonCouncil(c, env, loc, req, res, timeout)
	}
	d := summonData(res)
	if res.Block == "" || res.DryRun {
		return cli.Result{Data: d}, nil
	}
	wctx, wcancel := within(c, timeout)
	st, err := env.AwaitStartup(wctx, res.Block)
	wcancel()
	if err != nil {
		return cli.Result{Data: d}, err
	}
	if st.Handle != "" {
		// The brief first: reading the adopted identity back only adds its
		// session and directory to the result.
		if !res.Mailed {
			ctx, cancel := within(c, 20*time.Second)
			err = env.Deliver(ctx, loc, st.Handle, req.Kind, req.Subject, req.Body)
			cancel()
			if err != nil {
				return cli.Result{Data: d}, err
			}
			res.Mailed = true
		}
		if res, err = env.Adopted(res, st.Handle); err != nil {
			return cli.Result{Data: d}, err
		}
		d = summonData(res)
	}
	d["startup"] = st.Status
	if st.Status != core.StartupAdopted {
		// Launched, not adopted: the mail waits, the user must look at the pane.
		return cli.Result{Data: d}, fault.New(fault.NotAdopted, "%s was launched in %s but not adopted (%s); the user should look at its pane", target, res.Block, st.Status).With("startup", st.Status)
	}
	return cli.Result{Data: d}, nil
}

// summonCouncil awaits the seats a council launched and returns every
// seat's outcome; a launched seat that was not adopted is seat_failed.
func summonCouncil(c *cli.Ctx, env app.Env, loc app.Location, req app.SummonRequest, res app.SummonResult, timeout time.Duration) (cli.Result, error) {
	if !res.DryRun {
		ctx, cancel := within(c, timeout+20*time.Second)
		err := env.AwaitCouncil(ctx, loc, req, &res, timeout)
		cancel()
		if err != nil {
			return cli.Result{}, err
		}
	}
	d := summonData(res)
	d["seats"] = res.Seats
	for _, st := range res.Seats {
		if st.Block != "" && st.Startup != core.StartupAdopted {
			return cli.Result{Data: d}, fault.New(fault.SeatFailed, "a council seat launched in %s was not adopted (%s); the user should look at its pane", st.Block, st.Startup)
		}
	}
	return cli.Result{Data: d}, nil
}

func agentAwait(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, err := app.NewEnv("")
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, in.Duration("timeout", 30*time.Second))
	defer cancel()
	s, err := env.AwaitStartup(ctx, in.Arg("block"))
	if err != nil {
		return cli.Result{}, err
	}
	if s.Status != core.StartupAdopted {
		return cli.Result{Data: s}, fault.New(fault.NotAdopted, "the agent in %s was not adopted: %s", in.Arg("block"), s.Status).With("status", s.Status)
	}
	return cli.Result{Data: s}, nil
}

func agentRetire(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	env, loc, err := caller(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	res, err := env.Retire(ctx, loc, app.RetireRequest{Handle: core.Handle(in.Arg("handle")), Force: in.Bool("force"), DryRun: in.DryRun()})
	if res.Action == "" {
		return cli.Result{}, err
	}
	return cli.Result{Data: res}, err
}

// agentCollect reconciles records with Rex now and brings back the dead
// wakers of live agents: those of the caller's session from inside Rex
// (what listing agents used to do on the side), every record from outside.
// A record that fails does not stop the rest; it is reported.
func agentCollect(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	env, loc, err := caller(c)
	if errors.Is(err, app.ErrNotInRex) {
		if env, err = app.NewEnv(c.Getenv("REX_SERVER")); err != nil {
			return cli.Result{}, err
		}
		if in.DryRun() {
			return cli.Result{Data: map[string]any{"dry_run": true, "scope": "all"}}, nil
		}
		res := cli.Result{Data: map[string]any{"scope": "all"}}
		if err := env.Collect(ctx); err != nil {
			res.Warnings, res.Incomplete = []string{"collect: " + err.Error()}, true
		}
		return res, nil
	}
	if err != nil {
		return cli.Result{}, err
	}
	s, err := env.FindSession(ctx, loc.Block)
	if err != nil {
		return cli.Result{}, err
	}
	data := map[string]any{"scope": "session", "session_id": s.ID()}
	if in.DryRun() {
		data["dry_run"] = true
		return cli.Result{Data: data}, nil
	}
	res := cli.Result{Data: data}
	if err := env.CollectSession(ctx, s); err != nil {
		res.Warnings, res.Incomplete = append(res.Warnings, "collect: "+err.Error()), true
	}
	if err := env.SuperviseSession(ctx, s); err != nil {
		res.Warnings, res.Incomplete = append(res.Warnings, "supervise: "+err.Error()), true
	}
	return res, nil
}
