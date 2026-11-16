package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/transcript"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// Summon on send (core/summon.go for the guards): an agent addresses a peer
// by handle or role; the adapter delivers to a live one, resumes a departed
// one of this session in a tab of its own, or starts one, then
// delivers. Mail goes out first in every case that has a mailbox, so a
// failed resume or start leaves it waiting. A launch carries a token in its
// environment (SummonEnv); the adoption that sees it takes the lineage, and
// until then the token reserves a place in the session's cap.

// SummonEnv is the environment variable a launched agent carries.
const SummonEnv = "REX_AMQ_SUMMON"

// SummonRequest is what the summoner asks.
type SummonRequest struct {
	Target  string // a handle or a role
	Kind    string // an amq message kind; requests may summon
	Subject string
	Body    string
	// Grant is the user's grant file the brief cites, Asked the user's own
	// words asking for a new agent (self-attested, logged); starting a new
	// agent needs one of them. Neither is verified here.
	Grant, Asked string
	// Harness overrides the role's default harness for a start; for a role
	// target it also passes over any agent of the role on another harness.
	Harness string
	// Cwd is the working directory of a started agent (a Slice worktree);
	// empty means the caller's. A resume keeps the departed agent's own.
	Cwd string
	// Split opens a launched agent in a pane beside the caller, which the
	// human asked for, instead of the default, a tab of its own.
	Split bool
}

// SummonResult is what happened. Block is set when an agent was launched
// (resume or start): the caller awaits its adoption and then delivers.
type SummonResult struct {
	Action  string        `json:"action"` // deliver, resume, start, refused
	Handles []core.Handle `json:"handles,omitempty"`
	Block   string        `json:"block,omitempty"`
	Label   string        `json:"label,omitempty"`
	Token   string        `json:"-"`
	Reason  string        `json:"reason,omitempty"`
	Parent  core.Handle   `json:"parent,omitempty"`
	Depth   int           `json:"depth"`
	Mailed  bool          `json:"mailed"` // the message is in a mailbox already
	// Harness is the CLI a start launched; Passed names each default
	// harness it passed over, and why (not installed, unapproved_hooks, …).
	Harness core.Kind `json:"harness,omitempty"`
	Passed  []string  `json:"passed_over,omitempty"`
}

// Summon resolves r.Target in the caller's session and acts.
func (e Env) Summon(ctx context.Context, loc Location, r SummonRequest) (SummonResult, error) {
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return SummonResult{}, err
	}
	if !ok || !caller.Live() {
		return SummonResult{}, errors.New("no AMQ identity for this pane, so nothing can summon")
	}
	if r.Kind == "" {
		r.Kind = "question"
	}
	// Before any mail or pane: a refused summoner leaves nothing behind.
	if g := core.GuardSummoner(caller.Role); g.Refused {
		res := SummonResult{Action: "refused", Reason: g.Reason, Parent: caller.Handle, Depth: caller.Depth + 1}
		e.summonLog(caller, r, res)
		return res, nil
	}
	// A target is a handle, or a role in the form adoption records it.
	target, isHandle := r.Target, core.LooksLikeHandle(r.Target)
	if !isHandle {
		if target = core.Role(r.Target); target == "" {
			return SummonResult{}, fmt.Errorf("%q is neither a handle (<kind>-<NN>) nor a role", r.Target)
		}
	}
	s, err := e.FindSession(ctx, loc.Block)
	if err != nil {
		return SummonResult{}, err
	}
	recs, err := e.Store.Records()
	if err != nil {
		return SummonResult{}, err
	}
	var live []store.Record
	var departed *store.Record
	for i, rec := range recs {
		if rec.Handle == caller.Handle || !matches(rec, target, isHandle) {
			continue // nobody summons themselves
		}
		// An explicit --harness for a role asks for that CLI: a peer of the
		// role on another one, departed or live and stuck, never stands in
		// for it (a handle is the agent it names, whatever --harness says).
		// With --cwd nobody stands in anyway, and every live peer of the role
		// still counts for the guard against sharing a directory.
		if !isHandle && r.Harness != "" && r.Cwd == "" && string(rec.Kind) != r.Harness {
			continue
		}
		switch {
		case rec.Live() && s.holds(rec):
			live = append(live, rec)
		case !rec.Live() && s.recorded(rec) && (departed == nil || rec.AdoptedAt.After(departed.AdoptedAt)):
			departed = &recs[i]
		}
	}
	res := SummonResult{Parent: caller.Handle, Depth: caller.Depth + 1}
	if r.Split && stealthTarget(recs, target, isHandle) {
		res.Action, res.Reason = "refused", "a narrator or personal agent never shares a worker's tab: it runs in a tab of its own, so summon it without --split"
		e.summonLog(caller, r, res)
		return res, nil
	}
	var busy []core.Kind
	if r.Cwd != "" {
		// --cwd always means a new agent in that directory: a live one would get
		// the mail in its own directory and a departed one would resume in its
		// own, so neither may silently stand in. A second agent of a live role
		// starts, unless the live one already works in that directory.
		switch {
		case isHandle:
			res.Action, res.Reason = "refused", "--cwd starts a new agent for a role; "+r.Target+" names an existing agent, which keeps its own directory"
			e.summonLog(caller, r, res)
			return res, nil
		}
		// A review role only reads the checkout, so seats on distinct
		// harnesses may share it (a council): only a second live seat of the
		// same harness there is refused, and a start that names no harness
		// passes over the harnesses already seated (busy).
		for _, rec := range live {
			if !sameDir(rec.Cwd, r.Cwd) {
				continue
			}
			if core.ReviewRole(target) && (r.Harness == "" || string(rec.Kind) != r.Harness) {
				busy = append(busy, rec.Kind)
				continue
			}
			if core.ReviewRole(target) {
				res.Action, res.Reason = "refused", fmt.Sprintf("a live %s on %s (%s) already works in %s; name another harness with --harness for another seat", target, rec.Kind, rec.Handle, r.Cwd)
			} else {
				res.Action, res.Reason = "refused", fmt.Sprintf("a live %s (%s) already works in %s; two agents of one role in one directory would edit the same files", target, rec.Handle, r.Cwd)
			}
			e.summonLog(caller, r, res)
			return res, nil
		}
		live, departed = nil, nil // a second agent of the role, in its own directory or seat
	}
	if len(live) > 1 {
		names := make([]string, len(live))
		for i, rec := range live {
			names[i] = string(rec.Handle)
		}
		res.Action, res.Reason = "refused", fmt.Sprintf("%d live %s agents (%s) would all get this; name one by handle, or pass --cwd to start a new one", len(live), target, strings.Join(names, ", "))
		e.summonLog(caller, r, res)
		return res, nil
	}
	if len(live) > 0 {
		for _, rec := range live {
			if err := e.mailAs(ctx, caller.Handle, rec.Handle, r.Kind, r.Subject, r.Body); err != nil {
				return res, err
			}
			res.Handles = append(res.Handles, rec.Handle)
		}
		res.Action, res.Mailed = "deliver", true
		e.summonLog(caller, r, res)
		return res, nil
	}
	if departed != nil {
		// Its mailbox waits for a resume whatever the guards say.
		if err := e.mailAs(ctx, caller.Handle, departed.Handle, r.Kind, r.Subject, r.Body); err != nil {
			return res, err
		}
		res.Mailed = true
		res.Handles = []core.Handle{departed.Handle}
	}
	chain, complete := e.chain(recs, caller)
	if !complete {
		res.Action, res.Reason = "refused", "your summon chain cannot be checked: an agent that summoned you is no longer recorded, so no new agent is started or resumed"
		return e.refused(caller, r, res, departed)
	}
	// The guards and the reservation, together under the lifecycle lock:
	// two summoners racing the cap both see the other's reservation.
	token, err := e.reserve(ctx, caller, s, r.Kind, chain, target, departed)
	if err != nil {
		var refusal core.SummonCheck
		if errors.As(err, &refusal) {
			res.Action, res.Reason = "refused", refusal.Reason
			return e.refused(caller, r, res, departed)
		}
		return res, err
	}
	res.Token = token
	if departed != nil {
		return e.resume(ctx, caller, s, r, *departed, res)
	}
	return e.start(ctx, caller, s, r, target, busy, res)
}

func (e Env) refused(caller store.Record, r SummonRequest, res SummonResult, departed *store.Record) (SummonResult, error) {
	if departed != nil {
		res.Reason += "; the message waits in " + string(departed.Handle) + "'s mailbox for a resume"
	}
	e.summonLog(caller, r, res)
	return res, nil
}

// stealthTarget reports whether target names a Stealth agent (core.Stealth):
// a Stealth role, or a handle whose record, live or departed, has one.
func stealthTarget(recs []store.Record, target string, isHandle bool) bool {
	if !isHandle {
		return core.Stealth(target)
	}
	for _, rec := range recs {
		if string(rec.Handle) == target && core.Stealth(rec.Role) {
			return true
		}
	}
	return false
}

// matches reports whether rec is addressed by target: its handle, or its
// role (in adoption's canonical form).
func matches(rec store.Record, target string, isHandle bool) bool {
	if isHandle {
		return string(rec.Handle) == target
	}
	return rec.Role != "" && rec.Role == target
}

// chain is the summon chain from the user's agent down to rec, rec last.
// complete is false when an ancestor is no longer recorded (collected after
// its seven days), so the chain cannot be proven.
func (e Env) chain(recs []store.Record, rec store.Record) (chain []core.Peer, complete bool) {
	byHandle := map[core.Handle]store.Record{}
	for _, r := range recs {
		byHandle[r.Handle] = r
	}
	cur := rec
	for len(chain) < 64 {
		chain = append([]core.Peer{peerOf(cur)}, chain...)
		if cur.Parent == "" {
			return chain, true
		}
		next, ok := byHandle[cur.Parent]
		if !ok {
			return chain, false
		}
		cur = next
	}
	return chain, false
}

// reserve runs the guards against the session's live agents plus its
// pending launches, and on success writes the summon under a fresh token,
// which reserves a place until adoption consumes it or it expires.
func (e Env) reserve(ctx context.Context, caller store.Record, s Session, kind string, chain []core.Peer, target string, departed *store.Record) (string, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Adoption can commit a record and remove its pending token after the
	// caller's layout snapshot. Count against a layout read under the same lock.
	s, err = e.loadSession(ctx, s.server, s.id)
	if err != nil {
		return "", err
	}
	recs, err := e.Store.Records()
	if err != nil {
		return "", err
	}
	live := 0
	for _, rec := range recs {
		// A Stealth agent takes no place a worker would.
		if rec.Live() && s.holds(rec) && !core.Stealth(rec.Role) {
			live++
		}
	}
	pending, err := e.Store.PendingSummons(s.id, e.Now())
	if err != nil {
		return "", err
	}
	if g := core.GuardSummon(kind, chain, caller.Depth, target, live+pending); g.Refused {
		return "", g
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	sm := store.Summon{Parent: caller.Handle, Depth: caller.Depth + 1, For: target, Session: s.id, At: e.Now()}
	if departed != nil {
		// A resumed agent keeps its own lineage; the token only holds its place.
		sm.Parent, sm.Depth = departed.Parent, departed.Depth
	}
	return token, e.Store.PutSummon(token, sm)
}

func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resume starts the departed agent's native session again in a tab of its
// own (launch), in the directory it ran in, with a fixed first prompt that
// makes it read its mailbox: a resumed session runs no start hook, so the
// prompt is what re-adopts it and delivers the waiting mail. Its record
// keeps its handle, lineage and mail.
func (e Env) resume(ctx context.Context, caller store.Record, s Session, r SummonRequest, departed store.Record, res SummonResult) (SummonResult, error) {
	cwd := departed.Cwd
	if cwd == "" {
		cwd = transcript.Cwd(ctx, e.harnessDirs(), departed.Kind, departed.AgentSessionID)
	}
	if cwd == "" {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", fmt.Sprintf("%s cannot be resumed: its working directory is not recorded and its transcript does not name one; the message waits in its mailbox", departed.Handle)
		e.summonLog(caller, r, res)
		return res, nil
	}
	bootstrap := fmt.Sprintf("Resumed by %s (summon on send): mail waits for you. Run %s drain --include-body and act on it.", caller.Handle, e.SelfBin)
	cmd, ok := resumeCommand(departed.Kind, departed.AgentSessionID, bootstrap)
	if !ok {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", fmt.Sprintf("%s (%s) cannot be resumed from outside; the message waits in its mailbox", departed.Handle, departed.Kind)
		e.summonLog(caller, r, res)
		return res, nil
	}
	launch, err := e.resumeLaunch(cwd, departed)
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", fmt.Sprintf("%s cannot be resumed on what it was launched on: %v; the message waits in its mailbox", departed.Handle, err)
		e.summonLog(caller, r, res)
		return res, nil
	}
	if refusal, err := e.preflight(ctx, cwd, departed.Kind); err != nil || refusal != "" {
		_ = e.Store.DeleteSummon(res.Token)
		if err != nil {
			return res, err
		}
		res.Action, res.Reason = "refused", refusal+"; the message waits in "+string(departed.Handle)+"'s mailbox"
		e.summonLog(caller, r, res)
		return res, nil
	}
	unattended, err := e.launchUnattended(s.id, departed.Role, caller, &departed)
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	label := core.PaneLabel(departed.Handle, departed.Role, departed.Task)
	block, err := e.launch(ctx, caller, s, r.Split, label, cwd, res.Token, unattended, launch, launch.Args(departed.Kind, cmd))
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	res.Action, res.Block, res.Label = "resume", block, label
	e.summonLog(caller, r, res)
	return res, nil
}

// start launches a new agent for a role (launch), with the message
// as its brief. A handle that names no agent cannot be started.
func (e Env) start(ctx context.Context, caller store.Record, s Session, r SummonRequest, role string, busy []core.Kind, res SummonResult) (SummonResult, error) {
	refuse := func(reason string) (SummonResult, error) {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", reason
		e.summonLog(caller, r, res)
		return res, nil
	}
	if core.LooksLikeHandle(r.Target) {
		return refuse(fmt.Sprintf("no agent %s in this session, live or departed; a new agent is started by role, not by handle", r.Target))
	}
	if r.Grant != "" {
		info, err := os.Stat(r.Grant)
		if err != nil {
			return refuse("the grant file " + r.Grant + " is not readable, so it authorizes nothing: " + err.Error())
		}
		if !info.Mode().IsRegular() {
			return refuse("the grant file " + r.Grant + " is not a regular file, so it authorizes nothing")
		}
	}
	if r.Grant == "" && r.Asked == "" {
		return refuse("starting a new agent needs the user's own words in this session asking for it (--asked \"<their words>\", your attestation, logged) or a brief that cites their grant file (--grant <file>, which you verified and the brief names)")
	}
	brief := r.Body + "\n\nYou were started by " + string(caller.Handle) + " (summon on send). Report back with `" + e.SelfBin + " send --to " + string(caller.Handle) + " --subject \"…\" --body \"…\"`; the same request waits in your mailbox once you are adopted, so you can `" + e.SelfBin + " reply` to it."
	label := role + " | " + core.Task(r.Subject)
	cwd := caller.Cwd
	if r.Cwd != "" {
		info, err := os.Stat(r.Cwd)
		if err != nil || !info.IsDir() {
			return refuse("--cwd " + r.Cwd + " is not a directory, so the agent was not started")
		}
		cwd = r.Cwd
	}
	route, err := e.route(cwd, role)
	if err != nil {
		return refuse(fmt.Sprintf("the routing of role %s cannot be read, so no agent was started: %v", role, err))
	}
	// A review role takes the first ready harness of another family than the
	// caller's, installed even when it is the only one; every other role its
	// first harness, launched even when this process's PATH lacks it
	// (pickHarness).
	// An explicit --harness is the caller's choice and is never filtered.
	kinds := route.Harnesses[:1]
	switch {
	case r.Harness != "":
		kind, err := core.ParseKind(r.Harness)
		if err != nil {
			return refuse(err.Error())
		}
		kinds = []core.Kind{kind}
	case core.ReviewRole(role):
		mine := core.Family(caller.Kind, caller.Model)
		if kinds = otherFamilies(route, mine); len(kinds) == 0 {
			return refuse(fmt.Sprintf("no harness in the routing of %s (%s) runs a model family known to differ from yours (%s %s), so no agent was started; name one with --harness", role, strings.Join(kindNames(route.Harnesses), ", "), caller.Kind, orUnknown(mine)))
		}
		if len(busy) > 0 {
			free := slices.DeleteFunc(slices.Clone(kinds), func(k core.Kind) bool { return slices.Contains(busy, k) })
			if len(free) == 0 {
				return refuse(fmt.Sprintf("every harness the routing of %s offers you (%s) already has a live %s in %s, so no agent was started; name another with --harness", role, strings.Join(kindNames(kinds), ", "), role, cwd))
			}
			kinds = free
		}
	}
	kind, passed, refusal, err := e.pickHarness(ctx, cwd, kinds, r.Harness == "" && core.ReviewRole(role))
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	} else if refusal != "" {
		res.Passed = passed
		return refuse(refusal)
	}
	res.Harness, res.Passed = kind, passed
	cmd := startCommand(kind, brief)
	if len(cmd) == 0 {
		return refuse(fmt.Sprintf("unsupported harness %q; no agent was started", kind))
	}
	unattended, err := e.launchUnattended(s.id, role, caller, nil)
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	launch := route.Launch(kind)
	block, err := e.launch(ctx, caller, s, r.Split, label, cwd, res.Token, unattended, launch, launch.Args(kind, cmd))
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	res.Action, res.Block, res.Label = "start", block, label
	e.summonLog(caller, r, res)
	return res, nil
}

// pickHarness picks the harness a start launches in cwd. With choosing (a
// review role's default) it is the first of kinds that is installed and
// passes the preflight: it passes over each unready harness, naming why in
// passed, so an unapproved Codex hook sends a review to Antigravity instead
// of a pane stuck on "Hooks need review"; when none is ready the summon
// refuses with every reason, and never falls back to the caller's own
// family. Otherwise (an explicit --harness, or another role's first
// harness: one kind) it is never swapped and never looked up, since this
// process's PATH may lack a CLI the pane's shell finds; its preflight
// refusal is the summon's.
func (e Env) pickHarness(ctx context.Context, cwd string, kinds []core.Kind, choosing bool) (kind core.Kind, passed []string, refusal string, err error) {
	for _, k := range kinds {
		if choosing && e.LookPath != nil {
			if _, err := e.LookPath(string(k)); err != nil {
				passed = append(passed, fmt.Sprintf("%s: not installed", k))
				continue
			}
		}
		refusal, err := e.preflight(ctx, cwd, k)
		if err != nil {
			return "", passed, "", err
		}
		if refusal == "" {
			return k, passed, "", nil
		}
		if !choosing {
			return "", nil, refusal, nil
		}
		passed = append(passed, fmt.Sprintf("%s: %s", k, refusal))
	}
	return "", passed, "no harness for this role is ready, so no agent was started: " + strings.Join(passed, "; ") + "; clear one of these, or name another harness with --harness", nil
}

// preflight checks, before a pane opens, that kind meets no trust screen in
// cwd (trust.Gate.Preflight: Codex hooks the user has not approved, or a
// folder-trust dialog, granting a project under a declared root). refusal
// is the unapproved_hooks or untrusted_folder reason, or a directory that
// no longer exists; err is a failure to decide, which starts nothing
// either.
func (e Env) preflight(ctx context.Context, cwd string, kind core.Kind) (refusal string, err error) {
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	// Rex starts a pane whose directory is gone in the home directory
	// instead, where no trust is ever recorded.
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return fmt.Sprintf("the working directory %s does not exist (a removed worktree?), so no agent was started; name another with --cwd", cwd), nil
	}
	err = e.trustGate().Preflight(ctx, cwd, kind)
	if trust.Refusal(err) {
		return err.Error(), nil
	}
	if err != nil {
		return "", fmt.Errorf("trust preflight in %s: %w", cwd, err)
	}
	return "", nil
}

// launch opens a tab of its own, which follows the agent's pane label from
// its adoption on (core.FollowTab), running cmd with the token, what it is
// launched on (l, already in cmd's flags) and, when unattended, UnattendedEnv
// in its environment; with split, the human's choice, a pane beside the
// caller instead. A new tab is opened in the background, so the human's view
// stays where it is.
func (e Env) launch(ctx context.Context, caller store.Record, s Session, split bool, label, cwd, token string, unattended bool, l core.Launch, cmd []string) (string, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// Trust Antigravity and Pi keep no route for goes in for the session.
	cmd = e.trustGate().Args(cwd, cmd)
	cmd = withLaunchEnv(l, cmd)
	if unattended {
		cmd = withUnattendedEnv(cmd)
	}
	// The token stays the first assignment, where a reader of the command finds it.
	cmd = append([]string{"env", SummonEnv + "=" + token}, cmd[1:]...)
	if split {
		return e.Rex.NewSplit(ctx, s.id, caller.BlockID, label, cwd, cmd)
	}
	window, block, err := e.Rex.NewWindow(ctx, s.id, label, label, cwd, cmd)
	if err != nil {
		return "", err
	}
	if err := e.Store.PutTab(store.Tab{WindowID: window, SessionID: s.id, Mode: core.WindowAuto, LastLabel: label}); err != nil {
		fmt.Fprintf(Diagnostics, "record the tab %s: %v\n", window, err)
	}
	// The agent runs from the moment the tab opens: an adoption that
	// followed the tab before it was recorded found a named tab it does not
	// own and left it, so follow it once more now that it is.
	e.followTab(ctx, s.id, window)
	return block, nil
}

func startCommand(kind core.Kind, brief string) []string {
	switch kind {
	case core.Claude:
		return []string{"claude", "--", brief}
	case core.Codex:
		return []string{"codex", "--no-daemon", "--", brief}
	case core.Agy:
		return []string{"agy", "--dangerously-skip-permissions", "--mode", "auto", "-i", brief}
	case core.Pi:
		return []string{"pi", "--", brief}
	}
	return nil
}

// LaunchModelEnv and LaunchEffortEnv carry the model and effort an agent
// was launched on to its adoption, which records them (store.Record).
const (
	LaunchModelEnv  = "REX_AMQ_MODEL"
	LaunchEffortEnv = "REX_AMQ_EFFORT"
)

// withLaunchEnv runs cmd under env with l in LaunchModelEnv and
// LaunchEffortEnv, set even when empty: the adoption then knows the adapter
// launched it on its CLI's defaults.
func withLaunchEnv(l core.Launch, cmd []string) []string {
	return append([]string{"env", LaunchModelEnv + "=" + l.Model, LaunchEffortEnv + "=" + l.Effort}, cmd...)
}

// LaunchFromEnv reads what the adapter launched this agent on from the
// hook's environment; nil when neither variable is set.
func LaunchFromEnv(lookup func(string) (string, bool)) *core.Launch {
	model, m := lookup(LaunchModelEnv)
	effort, ef := lookup(LaunchEffortEnv)
	if !m && !ef {
		return nil
	}
	return &core.Launch{Model: model, Effort: effort}
}

// route is role's routing for an agent working in dir: the crews library's
// layers, or only the built-in table when the Env has none.
func (e Env) route(dir, role string) (crew.Route, error) {
	lib := crew.Builtin()
	if e.Crews != nil {
		lib = e.Crews(dir)
	}
	return lib.Routing(role)
}

// resumeLaunch is what departed resumes on: what it was launched on, checked
// again, since a record is a file on disk. A record without a launch (an
// agent launched before records kept them, or one the user started) takes
// its role's routing on its harness, so a narrator stays on its cheap tier;
// one with no role keeps its CLI's defaults.
func (e Env) resumeLaunch(cwd string, departed store.Record) (core.Launch, error) {
	if departed.Launched || departed.Model != "" || departed.Effort != "" {
		return core.ParseLaunch(departed.Kind, departed.Model, departed.Effort)
	}
	if departed.Role == "" {
		return core.Launch{}, nil
	}
	route, err := e.route(cwd, departed.Role)
	if err != nil {
		return core.Launch{}, err
	}
	return route.Launch(departed.Kind), nil
}

// otherFamilies are route's harnesses whose model is of a known family other
// than mine; an unknown family (Pi on its user's default) is never another.
func otherFamilies(route crew.Route, mine string) []core.Kind {
	var out []core.Kind
	for _, k := range route.Harnesses {
		if f := core.Family(k, route.Launch(k).Model); f != "" && f != mine {
			out = append(out, k)
		}
	}
	return out
}

func kindNames(kinds []core.Kind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func orUnknown(family string) string {
	if family == "" {
		return "of an unknown family"
	}
	return family
}

// resumeCommand resumes session in its harness with a first prompt: every
// CLI here takes one beside its resume flag.
func resumeCommand(kind core.Kind, session, prompt string) ([]string, bool) {
	if session == "" {
		return nil, false
	}
	switch kind {
	case core.Claude:
		return []string{"claude", "--resume", session, prompt}, true
	case core.Codex:
		return []string{"codex", "--no-daemon", "resume", session, prompt}, true
	case core.Agy:
		return []string{"agy", "--dangerously-skip-permissions", "--mode", "auto", "--conversation", session, "-i", prompt}, true
	case core.Pi:
		return []string{"pi", "--session", session, prompt}, true
	}
	return nil, false
}

// Deliver mails body to h in the caller's name.
func (e Env) Deliver(ctx context.Context, loc Location, h core.Handle, kind, subject, body string) error {
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return err
	}
	if !ok || !caller.Live() {
		return errors.New("no AMQ identity for this pane")
	}
	return e.mailAs(ctx, caller.Handle, h, kind, subject, body)
}

// summonLog appends one JSON line per summon to logs/summon.log. It is an
// audit trail: a failure is reported to Diagnostics and never fails the
// summon, whose mail or pane already exists.
func (e Env) summonLog(caller store.Record, r SummonRequest, res SummonResult) {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "summoner": caller.Handle, "parent": caller.Parent, "depth": caller.Depth, "target": r.Target, "kind": r.Kind, "subject": r.Subject, "action": res.Action, "handles": res.Handles, "block": res.Block, "reason": res.Reason, "grant": r.Grant, "asked": r.Asked}
	if res.Harness != "" {
		entry["harness"] = res.Harness
	}
	if len(res.Passed) > 0 {
		entry["passed_over"] = res.Passed
	}
	b, _ := json.Marshal(entry)
	write := func() error {
		if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "summon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(b, '\n'))
		return err
	}
	if err := write(); err != nil {
		fmt.Fprintf(Diagnostics, "summon log: %v\n", err)
	}
}

// Summons lists the summons a handle made, newest last, for its hand-back.
func (e Env) Summons(h core.Handle) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(e.Paths.Logs(), "summon.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["summoner"] != string(h) {
			continue
		}
		var hs []string
		if arr, ok := m["handles"].([]any); ok {
			for _, x := range arr {
				hs = append(hs, fmt.Sprint(x))
			}
		}
		sort.Strings(hs)
		out = append(out, fmt.Sprintf("%s %s %s -> %s: %s", m["at"], m["action"], m["target"], strings.Join(hs, ","), m["subject"]))
	}
	return out, nil
}

// sameDir reports whether two paths name one directory; an unknown path matches nothing.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}
