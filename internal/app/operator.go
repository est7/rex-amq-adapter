package app

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The Operator is the one principal that runs an Epic's lifecycle (epic
// open, close, revive) and reads a Hub's Epics (epic list, show, grant,
// artifacts, materialize). It is a positive allowlist, failing closed:
// everything it does not name is refused, whatever its environment says.
//
//  1. A live personal agent with no live agent of another role above it.
//  2. A top-level attended standalone agent: no role, depth 0, no parent,
//     not launched, not unattended, in a session with no recorded policy,
//     crew, or Epic lifecycle, and with no other live agent above it. Those
//     are persisted records only: an Epic whose narrator crashed is still
//     an Epic.
//  3. No agent at all: a shell outside Rex or in a Rex pane with no live
//     agent, or a harness session no hook adopted (a Desktop app's agent)
//     outside any agent's pane; never unattended (unattendedMarker), and
//     with no live agent above it.
//
// It is decided from the adapter's records, the environment's session and
// unattended variables, and this process's ancestors alone, before any Rex
// or amq I/O, so the refusal of a caller is the same inside Rex and out,
// and does not wait on, or hide behind, a missing dependency. Nothing reads
// the command line.

// Operator is a caller the allowlist admits.
type Operator struct {
	// Agent is the live agent the command runs as (rules 1 and 2); nil for
	// the user (rule 3).
	Agent *agent
	// Harness is the kind of an unadopted harness session that runs the
	// command (a Desktop app's agent); "" for none.
	Harness core.Kind
	// Loc is Agent's pane, or the Rex pane the user runs in (Block "" outside
	// Rex; Server is REX_SERVER, "" when unset).
	Loc Location
}

// Personal reports whether the Operator is a personal agent, whose scope is
// its own Hub's (or session's) links.
func (o Operator) Personal() bool { return o.Agent != nil && o.Agent.Role == core.PersonalRole }

// By is who the Operator is in a grant's relayed_by: the agent's handle, an
// external harness's kind, or core.UserHandle.
func (o Operator) By() core.Handle {
	switch {
	case o.Agent != nil:
		return o.Agent.Handle
	case o.Harness != "":
		return core.Handle(o.Harness)
	}
	return core.UserHandle
}

// GrantRole is how a grant names the Operator that relayed it.
func (o Operator) GrantRole() string {
	switch {
	case o.Personal():
		return core.GrantRolePersonal
	case o.Agent != nil, o.Harness != "":
		return core.GrantRoleInteractive
	}
	return core.GrantRoleUser
}

// GuardOperatorOnly is the details.reason of a caller the Operator
// allowlist does not admit.
const GuardOperatorOnly = "operator_only"

// unattendedEnvs are the variables, beside UnattendedEnv=1, that mark a run
// nobody attends (an agy unattended run, an Owl dispatch): set to anything
// but blanks, they refuse the user's rule.
var unattendedEnvs = []string{"AGY_UNATTENDED", "OWL_DISPATCH_RUN", "OWL_DISPATCH_RUN_ID"}

// unattendedMarker names the variable that marks this run unattended, ""
// when none does.
func unattendedMarker(getenv func(string) string) string {
	if getenv(UnattendedEnv) == "1" {
		return UnattendedEnv + "=1"
	}
	for _, name := range unattendedEnvs {
		if strings.TrimSpace(getenv(name)) != "" {
			return name
		}
	}
	return ""
}

// runner is who runs a command, as the records and the process tree show.
type runner struct {
	rec     *agent    // the live agent the command runs as; nil for none
	harness core.Kind // an unadopted harness session's kind, when rec is nil
	loc     Location
	// others are the live agents other than rec that the session variables
	// name or that run above this process.
	others []agent
	// deep is a walk up the process tree that stopped at its bound: an
	// agent above it may have gone unread.
	deep bool
}

// whoRuns identifies the caller from getenv, the records, and the process
// tree. The command is the nearest adopted session's the environment names
// (as Caller decides, with its incarnation checked), else REX_BLOCK's live
// agent; a harness session no hook adopted is no agent, and refused in a
// live agent's pane (a worker's nested `claude -p`). A Claude Code
// background job with no record is refused: no pane shows it.
func (e Env) whoRuns(getenv func(string) string) (runner, error) {
	recs, err := e.agents()
	if err != nil {
		return runner{}, err
	}
	ancestors, err := e.ancestors()
	if err != nil {
		return runner{}, fmt.Errorf("tell which agent runs this command: %w", err)
	}
	r := runner{loc: Location{Server: getenv("REX_SERVER"), Block: getenv("REX_BLOCK")}, deep: len(ancestors) >= maxAncestors}
	var named []agent
	var sessions []string
	for _, v := range sessionVars {
		id := getenv(v.name)
		if id == "" {
			continue
		}
		if i := slices.IndexFunc(recs, func(a agent) bool { return a.Live() && a.Kind == v.kind && a.Session() == id }); i >= 0 {
			named, sessions = append(named, recs[i]), append(sessions, id)
			continue
		}
		if v.kind == core.Claude {
			if _, isJob, err := e.claudeJob(id); err != nil {
				return runner{}, err
			} else if isJob {
				return runner{}, operatorRefused(GuardOperatorOnly, "this command runs in a Claude Code background session (%s) that no pane shows, so it has no identity", id)
			}
		}
		if r.harness == "" {
			r.harness = v.kind
		}
	}
	var inBlock *agent
	if r.loc.Block != "" {
		if a, ok, err := e.agentAt(r.loc.Block); err != nil {
			return runner{}, err
		} else if ok && a.Live() {
			inBlock = &a
		}
	}
	switch {
	case len(named) > 0:
		rec, session := named[0], sessions[0]
		if len(named) > 1 {
			if rec, err = e.nearest(named); err != nil {
				return runner{}, err
			}
			session = sessions[slices.IndexFunc(named, func(a agent) bool { return a.Handle == rec.Handle })]
		}
		if err := e.callerUnder(rec, session); err != nil {
			// A pane that resumed the conversation another pane's agent
			// holds does not speak for it.
			return runner{}, operatorRefused(GuardOperatorOnly, "%v", err)
		}
		r.rec, r.harness = &rec, ""
	case r.harness != "" && inBlock != nil:
		return runner{}, operatorRefused(GuardOperatorOnly, "this %s session has no AMQ identity and runs in %s's pane (%s)", r.harness, inBlock.Handle, whoIs(*inBlock))
	case inBlock != nil:
		r.rec = inBlock
	}
	if r.rec != nil {
		r.loc = Location{Server: r.rec.Server, Block: r.rec.BlockID}
	}
	self := func(a agent) bool { return r.rec != nil && a.Handle == r.rec.Handle }
	for _, a := range named {
		if !self(a) {
			r.others = append(r.others, a)
		}
	}
	for _, p := range ancestors {
		for _, a := range recs {
			if a.Live() && a.pid() > 1 && a.pid() == p.PID && !self(a) && !slices.ContainsFunc(r.others, func(o agent) bool { return o.Handle == a.Handle }) {
				r.others = append(r.others, a)
			}
		}
	}
	return r, nil
}

// operatorOf is r as an Operator, or the guard_denied refusal of the rule
// that does not admit it.
func (e Env) operatorOf(r runner, getenv func(string) string) (Operator, error) {
	if r.deep {
		return Operator{}, operatorRefused(GuardOperatorOnly, "this command runs at least %d processes deep, too deep to tell which agent started it", maxAncestors)
	}
	if r.rec == nil {
		if len(r.others) > 0 {
			return Operator{}, operatorUnder(r.others[0])
		}
		if m := unattendedMarker(getenv); m != "" {
			return Operator{}, operatorRefused(GuardUnattended, "an Epic's operator is attended, and this command runs unattended (%s)", m)
		}
		return Operator{Harness: r.harness, Loc: r.loc}, nil
	}
	rec := *r.rec
	if rec.Role == core.PersonalRole {
		if i := slices.IndexFunc(r.others, func(a agent) bool { return a.Role != core.PersonalRole }); i >= 0 {
			return Operator{}, operatorUnder(r.others[i])
		}
		return Operator{Agent: &rec, Loc: r.loc}, nil
	}
	if err := e.standalone(rec); err != nil {
		return Operator{}, err
	}
	if len(r.others) > 0 {
		return Operator{}, operatorUnder(r.others[0])
	}
	if rec.Unattended {
		return Operator{}, operatorRefused(GuardUnattended, "an Epic's operator is attended, and %s runs unattended", rec.Handle)
	}
	if m := unattendedMarker(getenv); m != "" {
		return Operator{}, operatorRefused(GuardUnattended, "an Epic's operator is attended, and this command runs unattended (%s)", m)
	}
	return Operator{Agent: &rec, Loc: r.loc}, nil
}

// standalone admits rec, an agent that is not a personal one, under rule 2:
// the user started it, with no role, in a session nothing records as an
// Epic's, a Hub's, or a crew's.
func (e Env) standalone(rec agent) error {
	switch {
	case rec.Role != "":
		return operatorRefused(GuardOperatorOnly, "an Epic's operator is the user, a personal agent, or a top-level agent of no role, and %s is %s", rec.Handle, whoIs(rec))
	case rec.Parent != "" || rec.Depth > 0 || rec.Launched:
		return operatorRefused(GuardOperatorOnly, "an Epic's operator is an agent the user started, and %s was launched or summoned", rec.Handle)
	case !placed(rec):
		return operatorRefused(GuardOperatorOnly, "%s is in no Rex session, so nothing tells its session is no Epic's", rec.Handle)
	}
	if _, ok, err := e.Store.SessionPolicy(rec.SessionID); err != nil {
		return err
	} else if ok {
		return operatorRefused(GuardOperatorOnly, "%s runs in a Rex session with a recorded policy (a Hub's or an Epic's), where only a personal agent operates", rec.Handle)
	}
	if _, ok, err := e.Store.SessionCrew(rec.SessionID); err != nil {
		return err
	} else if ok {
		return operatorRefused(GuardOperatorOnly, "%s runs in a crew's Rex session", rec.Handle)
	}
	if _, ok, err := e.Store.EpicLifecycle(rec.SessionID); err != nil {
		return err
	} else if ok {
		return operatorRefused(GuardOperatorOnly, "%s runs in an Epic's Rex session", rec.Handle)
	}
	return nil
}

// ResolveOperator is the Operator that runs the command whose environment
// getenv reads, or the guard_denied refusal (exit 6) of a caller that is
// none. It reads the records and the process tree only.
func (e Env) ResolveOperator(getenv func(string) string) (Operator, error) {
	r, err := e.whoRuns(getenv)
	if err != nil {
		return Operator{}, err
	}
	return e.operatorOf(r, getenv)
}

// operatorIn is the live agent in loc as an Operator, for a caller that
// located it already (OpenEpic, CloseEpic): a personal agent, or a
// standalone one (rule 2) that runs attended.
func (e Env) operatorIn(loc Location) (Operator, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return Operator{}, err
	}
	if !ok || !rec.Live() {
		return Operator{}, fmt.Errorf("no AMQ identity for this pane (%s)", loc.Block)
	}
	if rec.Role != core.PersonalRole {
		if err := e.standalone(rec); err != nil {
			return Operator{}, err
		}
		if rec.Unattended {
			return Operator{}, operatorRefused(GuardUnattended, "an Epic's operator is attended, and %s runs unattended", rec.Handle)
		}
	}
	return Operator{Agent: &rec, Loc: loc}, nil
}

// hubFlagDir is the Hub directory a --hub flag names, resolved; "" for
// none. One that is not a directory is ErrUsage.
func hubFlagDir(flag string) (string, error) {
	if flag == "" {
		return "", nil
	}
	dir, err := resolvedDir(flag)
	if err != nil {
		return "", fmt.Errorf("%w: --hub %s: %v", ErrUsage, flag, err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: --hub %s is not a directory", ErrUsage, flag)
	}
	return dir, nil
}

// epicSelf reports whether rec is an Epic's own lead or narrator: the
// readers of its own session's view and grant beside the Operator.
func (e Env) epicSelf(rec agent) (bool, error) {
	if rec.Role == core.NarratorRole && placed(rec) {
		return true, nil
	}
	return e.sessionLead(rec)
}

// OperatorGate admits an Operator (ResolveOperator) and, with self, an
// Epic's own lead or narrator, whose command then reads its own session
// only. Every other caller is refused with guard_denied.
func (e Env) OperatorGate(getenv func(string) string, self bool) error {
	r, err := e.whoRuns(getenv)
	if err != nil {
		return err
	}
	_, opErr := e.operatorOf(r, getenv)
	if opErr == nil || !self || r.rec == nil || r.deep {
		return opErr
	}
	if ok, err := e.epicSelf(*r.rec); err != nil || ok {
		return err
	}
	return opErr
}

// OperatorGate is Env.OperatorGate on the default state directory. Like
// HumanGate it needs no Rex or amq binary.
func OperatorGate(getenv func(string) string, self bool) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Now: time.Now, Getenv: os.Getenv, LookPath: exec.LookPath,
		ClaudeDir: core.HarnessDirs(userHome(), os.Getenv).Claude}
	return e.OperatorGate(getenv, self)
}

func operatorRefused(reason, format string, a ...any) error {
	return fault.New(fault.GuardDenied, "%s: %s", ErrRefused, fmt.Sprintf(format, a...)).
		WithCause(ErrRefused).With("reason", reason).
		WithHint("an Epic is operated by the user, their personal agent, or a top-level agent they started; an agent working in an Epic asks its lead or narrator instead")
}

func operatorUnder(rec agent) error {
	return operatorRefused(GuardOperatorOnly, "an Epic's operator runs under no other agent, and this command runs under %s (%s)", rec.Handle, whoIs(rec))
}
