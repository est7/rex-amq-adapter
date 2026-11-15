package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Goal is the goal contract the orchestrator runs (its grant), Asked the
	// user's own words asking for the Epic, its brief when there is no
	// contract; starting agents needs one of them, as summon does.
	Goal, Asked string
	Harness     string // the orchestrator's CLI; "" is the role's default
	// Crew is the crew the Epic's session works as (Env.Join); "" for none.
	Crew string
}

// EpicAgent is where one of the Epic's agents was started and how its
// startup ended.
type EpicAgent struct {
	Block string `json:"block"`
	core.Startup
}

// EpicResult is what was started. A failure part way leaves what started
// running: nothing is torn down.
type EpicResult struct {
	SessionID    string    `json:"session_id"`
	Orchestrator EpicAgent `json:"orchestrator"`
	Narrator     EpicAgent `json:"narrator"`
	Linked       bool      `json:"linked"`
}

// Ready reports whether the Epic is fully up: both agents adopted, and the
// caller linked to its narrator.
func (r EpicResult) Ready() bool {
	return r.Linked && r.Orchestrator.Status == core.StartupAdopted && r.Narrator.Status == core.StartupAdopted
}

// OpenEpic starts an Epic for the personal agent in loc: a session labelled
// r.Name with a tab for its orchestrator, briefed with the goal contract or
// the user's words, and one for its narrator, told whom it narrates for; then it links the session to the caller and
// waits, until ctx ends, for both agents to be adopted.
func (e Env) OpenEpic(ctx context.Context, loc Location, r EpicRequest) (EpicResult, error) {
	caller, ok, err := e.Store.Record(loc.Block)
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
	task := core.Task(name)
	dir, err := filepath.Abs(r.Dir)
	if err != nil {
		return EpicResult{}, err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return EpicResult{}, fmt.Errorf("%w: %s is not a directory", ErrUsage, r.Dir)
	}
	kind := core.DefaultHarness(core.OrchestratorRole)
	if r.Harness != "" {
		if kind, err = core.ParseKind(r.Harness); err != nil {
			return EpicResult{}, fmt.Errorf("%w: %v", ErrUsage, err)
		}
	}
	brief, err := orchestratorBrief(kind, r)
	if err != nil {
		return EpicResult{}, err
	}
	crewName := crew.Name(r.Crew)
	if crewName != "" {
		if e.Crews == nil {
			return EpicResult{}, fmt.Errorf("%w: no crews are available", ErrUsage)
		}
		if _, found, err := e.Crews(dir).Crew(crewName); err != nil {
			return EpicResult{}, err
		} else if !found {
			return EpicResult{}, fmt.Errorf("%w: no crew %q in %s: `rex-amq-adapter crews` lists them", ErrUsage, crewName, dir)
		}
	}
	// Both agents' folder trust before the session exists: an Epic either
	// starts with no dialog in its panes or does not start.
	if err := e.trustGate().Preflight(ctx, dir, core.DefaultHarness(core.NarratorRole), kind); err != nil {
		if trust.Refusal(err) {
			return EpicResult{}, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		return EpicResult{}, fmt.Errorf("trust preflight: %w", err)
	}
	narrate := fmt.Sprintf("Narrate the Epic %q for the personal agent %s.", name, caller.Handle)
	// The narrator's tab first and the orchestrator's last, which Rex makes
	// the session's active tab; both keep Rex's placeholder name, so each
	// follows its agent (core.FollowTab).
	narrator := core.DefaultHarness(core.NarratorRole)
	panes := []rex.Pane{
		{Label: core.NarratorRole + core.PaneSeparator + task, Dir: dir, Command: e.withTrust(dir, withModel(startCommand(narrator, narrate), core.RoleModel(core.NarratorRole, narrator)))},
		{Label: core.OrchestratorRole + core.PaneSeparator + task, Dir: dir, Command: e.withTrust(dir, startCommand(kind, brief))},
	}
	session, blocks, crewErr, err := e.newEpicSession(ctx, name, panes, crewName, caller.Handle)
	if err != nil {
		return EpicResult{}, fmt.Errorf("create the Epic's session: %w", err)
	}
	res := EpicResult{SessionID: session, Narrator: EpicAgent{Block: blocks[0]}, Orchestrator: EpicAgent{Block: blocks[1]}}
	var errs []error
	if crewErr != nil {
		errs = append(errs, fmt.Errorf("record the crew: %w", crewErr))
	}
	// Linked before the narrator is adopted, so the caller can mail it the
	// moment it is.
	if _, _, err := e.Link(ctx, loc, session, true); err != nil {
		errs = append(errs, fmt.Errorf("link: %w", err))
	} else {
		res.Linked = true
	}
	for _, a := range []*EpicAgent{&res.Orchestrator, &res.Narrator} {
		s, err := e.AwaitStartup(ctx, a.Block)
		if err != nil {
			errs = append(errs, fmt.Errorf("await %s: %w", a.Block, err))
			continue
		}
		a.Startup = s
	}
	return res, errors.Join(errs...)
}

// newEpicSession creates the Epic's session and, with crewName, records it
// as the session's crew. The panes' agents start as the session is created,
// so with a crew both steps run under the lifecycle lock that adoption
// takes: neither agent is adopted, and briefed, before the crew is recorded.
// crewErr is a crew that could not be recorded; the session stands.
func (e Env) newEpicSession(ctx context.Context, name string, panes []rex.Pane, crewName string, by core.Handle) (session string, blocks []string, crewErr, err error) {
	if crewName != "" {
		unlock, err := e.Store.LockLifecycle(ctx)
		if err != nil {
			return "", nil, nil, err
		}
		defer unlock()
	}
	session, _, blocks, err = e.Rex.NewSession(ctx, name, panes)
	if err != nil || crewName == "" {
		return session, blocks, nil, err
	}
	return session, blocks, e.Store.PutSessionCrew(session, store.SessionCrew{Crew: crewName, ClaimedBy: by, ClaimedAt: e.Now().UTC()}), nil
}

// maxSessionLabel is the longest session label Rex takes (session.create).
const maxSessionLabel = 128

// orchestratorBrief is the orchestrator's first prompt: run the goal
// contract, which is its grant, or the user's words as they asked.
func orchestratorBrief(kind core.Kind, r EpicRequest) (string, error) {
	if r.Goal == "" {
		if r.Asked == "" {
			return "", fmt.Errorf("%w: starting an Epic needs its goal contract (--goal <file>) or the user's own words asking for it (--asked \"<their words>\")", ErrRefused)
		}
		return r.Asked, nil
	}
	goal, err := filepath.Abs(r.Goal)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(goal); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: the goal contract %s is not a readable file, so it grants nothing", ErrRefused, r.Goal)
	}
	// make-goal's prompt for a contract kept in a file, whole: run-goal ends
	// at either exit, so a run whose Slices are all blocked hands back.
	run := "Load skill run-goal. Execute the goal contract at " + goal + "; it is the grant. " +
		"Done when every DONE WHEN line holds on fresh evidence, or when every Slice left is blocked " +
		"(a thrash fuse, a Boundaries item, or a Slice it builds on) and the hand-back lists each under Blocked on me."
	if kind == core.Claude {
		run = "/goal " + run
	}
	return run, nil
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
