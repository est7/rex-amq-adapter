package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// An Epic is one Rex session: an orchestrator that owns its goal and starts
// its crew, and a narrator that watches it for the user's personal agent.
// OpenEpic is the whole start, so a personal agent runs one command and
// tells the user only that the Epic started.

// EpicRequest is what a personal agent asks for.
type EpicRequest struct {
	Name string // the Epic: its Rex session's label
	Dir  string // the repository its agents work in
	// Goal is the file of the goal contract the lead runs (its grant), and
	// GoalText the text of one piped in (epic --goal -), which the Epic keeps
	// as its goal.md; at most one of them. Asked is the user's own words
	// asking for the Epic: the lead's brief when there is no contract, and
	// notarized beside one when there is (store.Grant). Starting agents needs
	// a contract or the words, as summon does, and takes both.
	Goal, GoalText, Asked string
	Harness               string // the orchestrator's CLI; "" is the role's default
	// Crew is the crew the Epic's session works as (Env.Join); "" for none.
	Crew string
	// OpID names this request, so a retry with the same OpID reconciles the
	// Epic the first run started instead of starting another; "" for none.
	OpID string
	// DryRun runs the caller, name, directory, routing, grant, and crew
	// checks, then reports the plan: no session, op record, trust grant,
	// or link is made.
	DryRun bool
}

// EpicPlan is what a dry run of OpenEpic would start.
type EpicPlan struct {
	Name            string    `json:"name"`
	Dir             string    `json:"dir"`
	Crew            string    `json:"crew,omitempty"`
	LeadHarness     core.Kind `json:"lead_harness"`
	NarratorHarness core.Kind `json:"narrator_harness"`
}

// EpicAgent is where one of the Epic's agents was started and how its
// startup ended.
type EpicAgent struct {
	Block string `json:"block"`
	core.Startup
}

// EpicResult is what was started. A failure part way leaves what started
// running: nothing is torn down. Replayed says a retry of OpID found the Epic
// its first run started and started nothing; LifecycleState is the Epic's
// lifecycle state when the command ended (store.EpicLifecycle).
type EpicResult struct {
	SessionID string `json:"session_id"`
	// Lead is the agent that runs the goal (epicLead): an orchestrator, a
	// pair Epic's implementer, or a design Epic's architect.
	Lead           EpicLead  `json:"lead"`
	Narrator       EpicAgent `json:"narrator"`
	Linked         bool      `json:"linked"`
	OpID           string    `json:"op_id,omitempty"`
	LifecycleState string    `json:"lifecycle_state,omitempty"`
	Replayed       bool      `json:"replayed,omitempty"`
	// GoalPath is the goal contract the lead runs, and GrantPath the Epic's
	// notarized grant (grant.md), once written.
	GoalPath  string `json:"goal_path,omitempty"`
	GrantPath string `json:"grant_path,omitempty"`
	// DryRun marks a preview, and Plan is what it would start.
	DryRun bool      `json:"dry_run,omitempty"`
	Plan   *EpicPlan `json:"plan,omitempty"`
}

// EpicLead is the Epic's lead: its role, and where it was started and how
// its startup ended.
type EpicLead struct {
	Role string `json:"role"`
	EpicAgent
}

// EpicGoalMax bounds a goal contract piped in with --goal -, as handBackMax
// bounds a hand-back.
const EpicGoalMax = handBackMax

// A pair Epic (epic --crew pair) runs one Slice with no orchestrator: its
// implementer leads, runs the goal itself, and hands back. A design Epic
// (epic --crew design) argues a design with no orchestrator either: its
// architect chairs the council and hands back the drafts (sessionLead).
const (
	pairCrew        = "pair"
	designCrew      = "design"
	implementerRole = "implementer"
	architectRole   = "architect"
)

// epicLead is the role of the agent that leads an Epic of crewName: a pair
// Epic's implementer, a design Epic's architect, else its orchestrator.
// OpenEpic records it as the session's lead_role (store.SessionPolicy).
func epicLead(crewName string) string {
	switch crewName {
	case pairCrew:
		return implementerRole
	case designCrew:
		return architectRole
	}
	return core.OrchestratorRole
}

// Ready reports whether the Epic is fully up: both agents adopted, and the
// caller linked to its narrator.
func (r EpicResult) Ready() bool {
	return r.Linked && r.Lead.Status == core.StartupAdopted && r.Narrator.Status == core.StartupAdopted
}

// OpenEpic starts an Epic for the personal agent in loc: a session labelled
// r.Name with a tab for its orchestrator, briefed with the goal contract or
// the user's words, and one for its narrator, told whom it narrates for; then it links the session to the caller and
// waits, until ctx ends, for both agents to be adopted. The Epic's lifecycle
// is partial_start from the moment its session exists, opened once both
// agents are adopted. With r.OpID, the op is recorded before Rex is asked
// for the session (store.EpicCreating, with the sessions Rex had then) and
// again with the session and its blocks before any wait, so a retry of the
// same request, however the first run ended, reconciles that session
// (adoptCreated, reconcileOpen) and never creates another; the op id reused
// for another request is refused.
func (e Env) OpenEpic(ctx context.Context, loc Location, r EpicRequest) (EpicResult, error) {
	caller, ok, err := e.agentIn(loc)
	if err != nil {
		return EpicResult{}, err
	}
	if !ok || !caller.Live() {
		return EpicResult{}, errors.New("no AMQ identity for this pane, so nothing can open an Epic")
	}
	if caller.Role != core.PersonalRole {
		return EpicResult{}, fmt.Errorf("%w: only a %s agent opens Epics, and %s is %q", ErrRefused, core.PersonalRole, caller.Handle, caller.Role)
	}
	// The session carries the Epic's whole name (Rex takes up to 128
	// characters); pane labels carry it shortened to their task width.
	name := strings.Join(strings.Fields(r.Name), " ")
	if name == "" {
		return EpicResult{}, fmt.Errorf("%w: name the Epic", ErrUsage)
	}
	if utf8.RuneCountInString(name) > maxSessionLabel {
		return EpicResult{}, fmt.Errorf("%w: an Epic's name is at most %d characters", ErrUsage, maxSessionLabel)
	}
	if err := ValidOpID(r.OpID); err != nil {
		return EpicResult{}, err
	}
	task := core.Task(name)
	dir, err := filepath.Abs(r.Dir)
	if err != nil {
		return EpicResult{}, err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return EpicResult{}, fmt.Errorf("%w: %s is not a directory", ErrUsage, r.Dir)
	}
	crewName := crew.Name(r.Crew)
	lead := epicLead(crewName)
	// Each agent starts on its role's first harness (--harness overrides the
	// lead's), with that harness's model and effort.
	orchRoute, err := e.route(dir, lead)
	if err != nil {
		return EpicResult{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	narrRoute, err := e.route(dir, core.NarratorRole)
	if err != nil {
		return EpicResult{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	kind := orchRoute.Harnesses[0]
	if r.Harness != "" {
		if kind, err = core.ParseKind(r.Harness); err != nil {
			return EpicResult{}, fmt.Errorf("%w: %v", ErrUsage, err)
		}
	}
	narrator := narrRoute.Harnesses[0]
	orchLaunch, narrLaunch := orchRoute.Launch(kind), narrRoute.Launch(narrator)
	grant, err := readEpicGrant(r)
	if err != nil {
		return EpicResult{}, err
	}
	brief := orchestratorBrief(kind, grant, crewName, dir)
	if crewName != "" {
		if e.Crews == nil {
			return EpicResult{}, fmt.Errorf("%w: no crews are available", ErrUsage)
		}
		if _, found, err := e.Crews(dir).Crew(crewName); err != nil {
			return EpicResult{}, err
		} else if !found {
			return EpicResult{}, fmt.Errorf("%w: no crew %q in %s: `rex-amq-adapter crew list` lists them", ErrUsage, crewName, dir)
		}
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return EpicResult{}, err
	}
	if r.DryRun {
		return EpicResult{OpID: r.OpID, Lead: EpicLead{Role: lead}, DryRun: true,
			Plan: &EpicPlan{Name: name, Dir: physical, Crew: crewName, LeadHarness: kind, NarratorHarness: narrator}}, nil
	}
	callerHub, err := e.hubOfSession(caller.SessionID)
	if err != nil {
		return EpicResult{}, err
	}
	seed := store.EpicLifecycle{Name: name, Dir: physical, Hub: callerHub, PersonalHandle: caller.Handle, PersonalSessionID: caller.SessionID, Server: caller.Server, Crew: crewName, OpenOpID: r.OpID}
	var op *store.EpicOpRecord
	// release frees LockEpicLifecycle when an op id took it: once the new
	// session is recorded, or on any return before.
	release := func() {}
	defer func() { release() }()
	if r.OpID != "" {
		id := openIdentity{Personal: caller.Endpoint().Session(), Name: name, Dir: physical, Asked: r.Asked, Crew: crewName, Goal: grant.goal}
		if grant.piped {
			id.GoalSHA256 = store.ContractDigest([]byte(grant.text))
		}
		if r.Harness != "" {
			id.Harness = string(kind)
		}
		hash := requestHash(id)
		// Held from the look-up until the new session is recorded, so two
		// runs of one op id create one session between them.
		unlock, err := e.Store.LockEpicLifecycle(ctx)
		if err != nil {
			return EpicResult{}, err
		}
		prev, found, err := e.Store.EpicOp(store.EpicOpOpen, r.OpID)
		switch {
		case err != nil:
			unlock()
			return EpicResult{}, err
		case found && prev.RequestHash != hash:
			unlock()
			return EpicResult{OpID: r.OpID}, errOpConflict(store.EpicOpOpen, r.OpID)
		case found && prev.SessionID == "":
			// The first run asked Rex for its session and ended before
			// recording which: find it, or learn that there is none.
			if found, err = e.adoptCreated(ctx, &prev, seed, name); err != nil {
				unlock()
				return EpicResult{OpID: r.OpID}, err
			}
		}
		if found {
			unlock()
			seed.SessionID = prev.SessionID
			return e.reconcileOpen(ctx, loc, prev, seed, grant)
		}
		release = unlock
		op = &store.EpicOpRecord{Kind: store.EpicOpOpen, OpID: r.OpID, RequestHash: hash, Name: name, Server: caller.Server, PersonalHandle: caller.Handle, PersonalSessionID: caller.SessionID, CreatedAt: e.Now().UTC()}
	}
	// Both agents' folder trust before the session exists: an Epic either
	// starts with no dialog in its panes or does not start.
	if err := e.trustGate().Preflight(ctx, dir, narrator, kind); err != nil {
		if trust.Refusal(err) {
			return EpicResult{}, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		return EpicResult{}, fmt.Errorf("trust preflight: %w", err)
	}
	narrate := fmt.Sprintf("Narrate the Epic %q for its Hub's personal agent (%s opened it). On this first turn, refresh the view, add your reading with --summary, and end your turn: do not mail %s now, nor any personal agent. Mail one only when your role prompt says so (its question, a hand-back or orchestrator departure, a wait on the human), always as `rex-amq-adapter mail send --to personal`: the adapter delivers to whichever personal agent of the Hub is with the user.", name, caller.Handle, caller.Handle)
	// The narrator's tab first and the orchestrator's last, which Rex makes
	// the session's active tab; both keep Rex's placeholder name, so each
	// follows its agent (core.FollowTab).
	// Both run unattended, named in their own commands (unattended.go).
	panes := []rex.Pane{
		{Label: core.NarratorRole + core.PaneSeparator + task, Dir: dir, Command: withUnattendedEnv(withLaunchEnv(narrLaunch, e.withTrust(dir, narrLaunch.Args(narrator, startCommand(narrator, narrate)))))},
		{Label: lead + core.PaneSeparator + task, Dir: dir, Command: withUnattendedEnv(withLaunchEnv(orchLaunch, e.withTrust(dir, orchLaunch.Args(kind, startCommand(kind, brief)))))},
	}
	if op != nil {
		// Recorded before Rex is asked, with the sessions it has, so a retry
		// after any failure from here tells the session this run creates.
		prior, err := e.Rex.ListSessions(ctx)
		if err != nil {
			return EpicResult{OpID: r.OpID}, fmt.Errorf("list Rex's sessions before creating the Epic's: %w", err)
		}
		op.State, op.PriorSessions, op.UpdatedAt = store.EpicCreating, prior, e.Now().UTC()
		if err := e.Store.PutEpicOp(*op); err != nil {
			return EpicResult{OpID: r.OpID}, fmt.Errorf("record --op-id %q before creating the Epic's session: %w", r.OpID, err)
		}
	}
	session, blocks, policyErr, crewErr, notaryErr, err := e.newEpicSession(ctx, name, panes, crewName, caller, grant)
	if err != nil {
		if op != nil {
			e.dropUnstartedOp(ctx, *op)
		}
		return EpicResult{OpID: r.OpID}, fmt.Errorf("create the Epic's session: %w", err)
	}
	res := EpicResult{SessionID: session, Narrator: EpicAgent{Block: blocks[0]}, Lead: EpicLead{Role: lead, EpicAgent: EpicAgent{Block: blocks[1]}}, OpID: r.OpID}
	var errs []error
	if policyErr != nil {
		errs = append(errs, fmt.Errorf("record the Epic policy: %w", policyErr))
	}
	if crewErr != nil {
		errs = append(errs, fmt.Errorf("record the crew: %w", crewErr))
	}
	if notaryErr != nil {
		errs = append(errs, fmt.Errorf("notarize the grant: %w", notaryErr))
	} else {
		res.GoalPath, res.GrantPath = e.grantPaths(session)
	}
	// Recorded before any wait, so a run that dies or times out from here on
	// leaves the session to reconcile.
	seed.SessionID = session
	if op != nil {
		op.SessionID, op.NarratorBlock, op.OrchestratorBlock, op.PriorSessions = session, blocks[0], blocks[1], nil
		lc, err := e.advanceEpic(seed, store.EpicPartialStart, op)
		release()
		release = func() {}
		if err != nil {
			// Not waited on: a retry of the same op id finds this session in
			// Rex (adoptCreated) and carries on from there.
			return res, fmt.Errorf("the Epic session %s started, but recording it under --op-id %q failed (retry with the same op id to carry on with it): %w", session, r.OpID, err)
		}
		res.LifecycleState = lc.State
	} else if lc, err := e.lockedAdvanceEpic(ctx, seed, store.EpicPartialStart, nil); err != nil {
		errs = append(errs, fmt.Errorf("record the lifecycle: %w", err))
	} else {
		res.LifecycleState = lc.State
	}
	// Linked before the narrator is adopted, so the caller can mail it the
	// moment it is.
	if _, _, err := e.Link(ctx, loc, session, true); err != nil {
		errs = append(errs, fmt.Errorf("link: %w", err))
	} else {
		res.Linked = true
	}
	errs = append(errs, e.awaitEpicAgents(ctx, &res)...)
	if res.Lead.Status == core.StartupAdopted && res.Narrator.Status == core.StartupAdopted {
		if lc, err := e.lockedAdvanceEpic(ctx, seed, store.EpicOpened, op); err != nil {
			errs = append(errs, fmt.Errorf("record the lifecycle: %w", err))
		} else {
			res.LifecycleState = lc.State
		}
	}
	return res, errors.Join(errs...)
}

// newEpicSession creates the Epic's session, records its unattended Epic
// policy (every Epic's, whoever asked for it: Claim C23), with crewName
// records it as the session's crew, and notarizes grant (notarize). The
// panes' agents start as the session is created, so the steps run under the
// lifecycle lock that adoption takes: neither agent is adopted, and briefed,
// before its crew and the grant its start notice names are recorded.
// policyErr, crewErr, and notaryErr are what could not be recorded; the
// session stands.
func (e Env) newEpicSession(ctx context.Context, name string, panes []rex.Pane, crewName string, by agent, grant epicGrant) (session string, blocks []string, policyErr, crewErr, notaryErr, err error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	defer unlock()
	session, _, blocks, err = e.Rex.NewSession(ctx, name, panes)
	if err != nil {
		return session, blocks, nil, nil, nil, err
	}
	now := e.Now().UTC()
	policyErr = e.Store.PutSessionPolicy(epicPolicy(session, by.Server, crewName, by.Handle, now))
	if crewName != "" {
		crewErr = e.Store.PutSessionCrew(session, store.SessionCrew{Crew: crewName, ClaimedBy: by.Handle, ClaimedAt: now})
	}
	return session, blocks, policyErr, crewErr, e.notarize(session, by.Handle, grant), nil
}

// epicPolicy is the policy of an Epic's session: unattended, led by its
// crew's lead role (epicLead).
func epicPolicy(session, server, crewName string, by core.Handle, at time.Time) store.SessionPolicy {
	return store.SessionPolicy{Kind: store.PolicyEpic, SessionID: session, Server: server, LeadRole: epicLead(crewName), Crew: crewName, Unattended: true, CreatedBy: by, CreatedAt: at}
}

// maxSessionLabel is the longest session label Rex takes (session.create).
const maxSessionLabel = 128

// epicGrant is what an Epic is opened on, read once before its session
// exists: the user's words and the goal contract, from its file (goal) or
// piped in (piped), with the contract's text as read then.
type epicGrant struct {
	asked string
	goal  string // the contract file's absolute path; "" when piped or none
	text  string // the contract's text; "" when there is none
	piped bool
}

// readEpicGrant reads r's grant: a contract file must be a readable regular
// file, a piped contract non-blank UTF-8 of at most EpicGoalMax bytes, and
// one of a contract or the user's words is needed.
func readEpicGrant(r EpicRequest) (epicGrant, error) {
	g := epicGrant{asked: r.Asked}
	switch {
	case r.Goal != "" && r.GoalText != "":
		return g, fmt.Errorf("%w: pass the goal contract as a file or piped in (--goal -), not both", ErrUsage)
	case r.GoalText != "":
		if len(r.GoalText) > EpicGoalMax || !utf8.ValidString(r.GoalText) || strings.TrimSpace(r.GoalText) == "" {
			return g, fmt.Errorf("%w: a goal contract piped in with --goal - is non-blank UTF-8 of at most %d bytes", ErrUsage, EpicGoalMax)
		}
		g.text, g.piped = r.GoalText, true
	case r.Goal != "":
		goal, err := filepath.Abs(r.Goal)
		if err != nil {
			return g, err
		}
		info, err := os.Stat(goal)
		if err != nil || !info.Mode().IsRegular() {
			return g, fmt.Errorf("%w: the goal contract %s is not a readable file, so it grants nothing", ErrRefused, r.Goal)
		}
		b, err := os.ReadFile(goal)
		if err != nil {
			return g, fmt.Errorf("%w: the goal contract %s is not a readable file, so it grants nothing", ErrRefused, r.Goal)
		}
		g.goal, g.text = goal, string(b)
	case r.Asked == "":
		return g, fmt.Errorf("%w: starting an Epic needs its goal contract (--goal <file>, or --goal - to pipe it in) or the user's own words asking for it (--asked \"<their words>\"), or both", ErrRefused)
	}
	return g, nil
}

// orchestratorBrief is the lead's first prompt: run the goal contract, which
// is its grant, or the user's words as they asked. A piped contract has no
// path until the session exists, so the lead finds it on the Goal: line of
// its start notice (grantNotice), which adoption gives it only once the
// grant is notarized. A design Epic's lead runs no goal: it gets the
// read-only deliberation brief (designBrief) instead.
func orchestratorBrief(kind core.Kind, g epicGrant, crewName, dir string) string {
	if crewName == designCrew {
		return designBrief(g, dir)
	}
	var at string
	switch {
	case g.goal != "":
		at = "at " + g.goal
	case g.piped:
		at = "at the path on the `Goal:` line of your AMQ start notice"
	default:
		return g.asked
	}
	// make-goal's prompt for a contract kept in a file, whole: run-goal ends
	// at either exit, so a run whose Slices are all blocked hands back.
	run := "Load skill run-goal. Execute the goal contract " + at + "; it is the grant. " +
		"Done when every DONE WHEN line holds on fresh evidence, or when every Slice left is blocked " +
		"(a thrash fuse, a Boundaries item, or a Slice it builds on) and the hand-back lists each under Blocked on me."
	if kind == core.Claude {
		run = "/goal " + run
	}
	return run
}

// designBrief is a design Epic's lead's first prompt. A design Epic writes
// no product code and runs no goal contract (so never run-goal): its lead
// chairs the design crew's deliberation, seats a council of architects
// beside itself, drafts the spec and goal files outside the repository,
// checks each goal file, and hands the drafts back as byte-for-byte
// artifacts, which the user's personal agent materializes. The grant's path
// exists only once the session does, so the lead reads it on the Grant:
// line of its start notice.
func designBrief(g epicGrant, dir string) string {
	var ask string
	switch {
	case g.goal != "":
		ask = "The question is the brief at " + g.goal + " (the path on the `Goal:` line of your AMQ start notice)."
	case g.piped:
		ask = "The question is the brief at the path on the `Goal:` line of your AMQ start notice."
	default:
		ask = "The question, in the user's words: " + g.asked
	}
	return "You lead a design Epic: a read-only deliberation that ends in drafts, not code. " + ask + "\n\n" +
		"1. Chair it by the design crew's conventions (your start notice carries them): seal your own proposal first, then run the rounds, the claim ledger, the vote, and the report.\n" +
		"2. Convene the council of architects beside you: `rex-amq-adapter agent summon architect --council --cwd " + dir + " --grant <the path on the Grant: line of your start notice> --kind brainstorm --subject \"<the question in a few words>\" --body @<the question file>`. You hold your own harness's seat; one architect starts on each other harness.\n" +
		"3. Edit nothing in " + dir + ", and let no member edit it: read it, and probe only outside it. Draft `spec.md` and one goal file per Goal (`NN-<slug>.md`) in a run directory or a temporary directory of your own, never in the repository.\n" +
		"4. Check each goal file with make-goal's linter (`python3 <make-goal skill dir>/scripts/lint_goal.py <file>`) and fix every FAIL.\n" +
		"5. Hand back the report and the drafts byte for byte: `rex-amq-adapter epic handback @<report file> --artifact spec.md=@<draft dir>/spec.md --artifact 01-<slug>.md=@<draft dir>/01-<slug>.md` (one --artifact per file). The user's personal agent materializes them; a revision the user asks for comes back to you, and you hand back again."
}

// notarize keeps g for session as the personal agent by relayed it: a piped
// contract as the session's goal.md, then the read-only grant
// (store.PutGrant), once; a retry keeps what the first run wrote.
func (e Env) notarize(session string, by core.Handle, g epicGrant) error {
	contract := g.goal
	if g.piped {
		if err := e.Store.PutGoal(session, g.text); err != nil {
			return err
		}
		contract = e.Paths.GoalPath(session)
	}
	rec := store.Grant{Asked: g.asked, RelayedBy: by, RelayedAt: e.Now().UTC(), SessionID: session, Contract: contract}
	if contract != "" {
		rec.ContractSHA256 = store.ContractDigest([]byte(g.text))
	}
	_, err := e.Store.PutGrant(rec)
	return err
}

// grantPaths are the goal contract and grant.md of session's notarized
// grant; both "" when it has none (or it cannot be read).
func (e Env) grantPaths(session string) (goal, grant string) {
	g, ok, err := e.Store.Grant(session)
	if err != nil || !ok {
		return "", ""
	}
	return g.Contract, e.Paths.GrantPath(session)
}

// withTrust carries into an Antigravity or Pi session in dir the trust the
// user gave its project (trust.Gate.Args); any other command is unchanged.
func (e Env) withTrust(dir string, cmd []string) []string {
	return e.trustGate().Args(dir, cmd)
}

// trustGate is folder trust under the harnesses' home and the adapter's
// state directory.
func (e Env) trustGate() trust.Gate {
	return trust.Gate{Stores: trust.StoresFor(e.TranscriptHome, e.Getenv), State: e.Paths.Home, Now: e.Now}
}
