package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Closing an Epic is the second half of a two-phase end: the orchestrator
// hands back and the narrator tells the personal agent, who asks the user;
// on the user's words the personal agent closes the Epic's Rex session.
// Nothing closes an Epic on its own: a hand-back can leave items only the
// user can act on, and a killed session's screens are gone for good.

// CloseRequest is what the Operator asks.
type CloseRequest struct {
	Session string // the Epic's Rex session id
	// Hub, when set, closes only an Epic of that Hub directory.
	Hub string
	// Asked is the user's own words asking to close it, its grant.
	Asked string
	// Force closes while an agent of the Epic is working, waiting on a
	// dialog, holds unread mail, or reports no state.
	Force bool
	// OpID names this request, so a retry with the same OpID finishes the
	// close the first run started, even once its link is gone; "" for none.
	OpID string
	// DryRun runs every guard, the busy check included, and stops before
	// anything is recorded, killed, parked, or unlinked.
	DryRun bool
}

// CloseResult is what was closed. Replayed says a retry of OpID found the
// close its first run started and finished it. Changed is false when the
// Epic was closed already (by another personal agent of the Hub, or another
// op id): nothing was killed, parked, or unlinked again.
type CloseResult struct {
	SessionID      string        `json:"session_id"`
	Changed        bool          `json:"changed"`
	Parked         []core.Handle `json:"parked"`
	Unlinked       bool          `json:"unlinked"`
	OpID           string        `json:"op_id,omitempty"`
	LifecycleState string        `json:"lifecycle_state,omitempty"`
	Replayed       bool          `json:"replayed,omitempty"`
	// DryRun marks a preview of a close every guard allowed.
	DryRun bool `json:"dry_run,omitempty"`
}

// CloseEpic closes the Epic session r.Session for the Operator op: it kills the Rex session, parks every live agent recorded in it as
// departed for epic_closed (handles stay reserved for a resume, ReserveFor),
// removes every link to it the caller's Hub holds, and records the Epic's
// lifecycle closed. A personal agent closes an Epic linked to its Hub (or
// its session): any of the Hub's may (core.LinkMatchesPersonal), and the
// lifecycle's personal agent is a delivery preference, never the one
// allowed. Any other Operator, the user's, closes any Epic the records
// know (its lifecycle, link, or policy), never a Hub's session, and with
// r.Hub only one of that Hub; it removes every link to it.
//
// It refuses a request without the user's words, and an Epic the Operator
// may not close; unless
// forced, also while an agent other than a Stealth one is not at rest or
// holds unread mail. Membership, the link, and the busy check are decided
// under LockEpicLifecycle and then LockLifecycle, so two closes of one Epic
// (two tabs of the Hub, two op ids) converge on one termination: the second
// finds the closed lifecycle (its tombstone) and returns changed false,
// killing nothing. With r.OpID (whose identity is the Hub, the Epic, and
// --force), the close is recorded once those guards pass and before the
// kill, so a retry of the same request finishes it whatever the first run
// got to, the unlink included; the op id reused for another request is
// refused. Without one, a retry before the lifecycle is closed finishes
// what is left: a session already gone is not an error.
func (e Env) CloseEpicFor(ctx context.Context, op Operator, r CloseRequest) (CloseResult, error) {
	var settle closeSettle
	res, err := e.closeEpic(ctx, op, r, &settle)
	if r.DryRun || settle.key == (core.SessionKey{}) {
		return res, err
	}
	// Only once the guards passed (closed now, replayed, or closed already
	// for this caller): any departure of the Epic's close left unfinished is
	// finished here.
	err = errors.Join(err, e.finishQuietly(ctx, e.closedPending(settle.key)))
	// A closed Epic is never revived: its handles leave the amq root, and
	// their identities go. A handle whose mailbox holds mail stays; the
	// roster sweep tries again.
	if res.LifecycleState == store.EpicClosed {
		if rep, rerr := e.retireEpicRoster(ctx, settle.key); rerr != nil || len(rep.Kept) > 0 {
			fmt.Fprintf(Diagnostics, "retire the roster of %s: pruned %v, kept %v: %v\n", settle.key.SessionID, rep.Pruned, rep.Kept, rerr)
		}
	}
	return res, err
}

// CloseEpic is CloseEpicFor the agent in loc, which must be an Operator's.
func (e Env) CloseEpic(ctx context.Context, loc Location, r CloseRequest) (CloseResult, error) {
	op, err := e.operatorIn(loc)
	if err != nil {
		return CloseResult{SessionID: r.Session, OpID: r.OpID}, err
	}
	return e.CloseEpicFor(ctx, op, r)
}

// closer is who closes an Epic: the Operator, with its gateway endpoint
// when it is a personal agent, and the Hub directory --hub names.
type closer struct {
	op     Operator
	me     core.Endpoint
	hubDir string
}

func (c closer) personal() bool { return c.op.Personal() }

// closeHashes is the closeIdentity hash of r for c, read before any lock: a personal
// agent's Hub (its session without one), or for any other Operator the Hub
// the Epic's lifecycle records, which only its open writes; legacy is the
// hash the same request had before v0.27, "" when c could not have made it.
func (e Env) closeHashes(c closer, r CloseRequest) (hash, legacy string, err error) {
	id := closeIdentity{Session: r.Session, Force: r.Force}
	if c.personal() {
		id.HubDir = c.me.Hub.Dir()
		if c.me.Hub == "" {
			id.Personal = c.me.Session()
		}
		return requestHash(id), requestHash(legacyCloseIdentityOf(c.me, r)), nil
	}
	if id.HubDir, err = e.epicHubDir(r.Session); err != nil {
		return "", "", err
	}
	return requestHash(id), "", nil
}

// closeSettle is what a close may settle once its locks are released: the
// Epic it was authorized for; zero when its guards refused it.
type closeSettle struct{ key core.SessionKey }

// closedPending is a snapshot of the departures epic's close left
// unfinished: same server and session, departed for epic_closed.
func (e Env) closedPending(epic core.SessionKey) []agent {
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "list the departures of %s: %v\n", epic.SessionID, err)
		return nil
	}
	var out []agent
	for _, r := range recs {
		if p := r.departure(); p != nil && p.Unfinished && p.DepartReason == store.DepartEpicClosed && r.Server == epic.Server && r.SessionID == epic.SessionID {
			out = append(out, r)
		}
	}
	return out
}

// closeEpic is CloseEpicFor up to the agents' quiet departures, which it
// leaves to finish with no lock held.
func (e Env) closeEpic(ctx context.Context, op Operator, r CloseRequest, settle *closeSettle) (CloseResult, error) {
	res := CloseResult{SessionID: r.Session, OpID: r.OpID}
	if strings.TrimSpace(r.Asked) == "" {
		return res, fault.New(fault.ConfirmationRequired, "%s: closing an Epic kills its Rex session and what its panes show; it needs the user's own words asking for it (--asked \"<their words>\")", ErrRefused).
			WithCause(ErrRefused).WithNext("rex-amq-adapter epic close " + r.Session + " --asked \"<the user's words>\"")
	}
	if err := ValidOpID(r.OpID); err != nil {
		return res, err
	}
	c := closer{op: op}
	if op.Personal() && (r.Session == "" || r.Session == op.Agent.SessionID) || r.Session == "" {
		return res, fmt.Errorf("%w: name an Epic session other than your own", ErrUsage)
	}
	var err error
	if c.hubDir, err = hubFlagDir(r.Hub); err != nil {
		return res, err
	}
	if op.Personal() {
		if c.me, err = e.gatewayEndpoint(*op.Agent); err != nil {
			return res, err
		}
		if c.hubDir != "" && c.me.Hub.Dir() != c.hubDir && !dirWithin(op.Agent.Cwd, c.hubDir) {
			return res, fmt.Errorf("%w: %s does not work in %s", ErrRefused, op.Agent.Handle, r.Hub)
		}
	}
	hash, legacy, err := e.closeHashes(c, r)
	if err != nil {
		return res, err
	}
	// A recorded close is looked up before the link: the run that recorded
	// it may have removed the link before it died.
	if r.OpID != "" {
		if out, done, err := e.replayClose(ctx, c, r, hash, legacy, settle); done || err != nil {
			return out, err
		}
	}
	unlockEpic, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlockEpic()
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	if done, err := e.closedAlready(c, r, hash, legacy, &res, settle); done || err != nil {
		return res, err
	}
	epic, err := e.closeTarget(c, r.Session)
	if err != nil {
		return res, err
	}
	on := e.Rex
	if e.RexOn != nil && epic.Server != "" {
		on = e.RexOn(epic.Server)
	}
	if !r.Force {
		if busy, err := e.unsettled(ctx, on, epic); err != nil {
			return res, err
		} else if len(busy) > 0 {
			return res, fault.New(fault.Busy, "%s: not every agent of the Epic is at rest (%s); ask the user again", ErrRefused, strings.Join(busy, "; ")).
				WithCause(ErrRefused).With("reason", "not_at_rest").With("busy", busy).
				WithHint("--force closes it anyway; use it only when the user says to close it while its agents work").
				WithNext("rex-amq-adapter epic close " + r.Session + " --asked \"<the user's words>\" --force")
		}
	}
	if r.DryRun {
		res.DryRun, res.Parked, res.Changed = true, []core.Handle{}, true
		return res, nil
	}
	var rec *store.EpicOpRecord
	if r.OpID != "" {
		prev, found, err := e.Store.EpicOp(store.EpicOpClose, r.OpID)
		if err != nil {
			return res, err
		}
		if found {
			if same, err := e.opHashMatches(&prev, hash, legacy, false); err != nil {
				return res, err
			} else if !same {
				return res, errOpConflict(store.EpicOpClose, r.OpID)
			}
			rec = &prev // a run of the same close got here meanwhile
		} else {
			hubs, err := e.epicHubDirs(epic.SessionID)
			if err != nil {
				return res, err
			}
			closeLinks, err := e.closeLinksFor(c, epic)
			if err != nil {
				return res, err
			}
			now := e.Now().UTC()
			rec = &store.EpicOpRecord{Kind: store.EpicOpClose, OpID: r.OpID, RequestHash: hash, SessionID: epic.SessionID, Server: epic.Server, HubDirs: hubs, ClosedLinks: closeLinks,
				PersonalHandle: c.personalHandle(), PersonalSessionID: c.personalSession(), State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}
			if err := e.Store.PutEpicOp(*rec); err != nil {
				return res, fmt.Errorf("record the close under --op-id %q: %w", r.OpID, err)
			}
		}
	}
	settle.key = epic
	return e.finishClose(ctx, on, epic, c, rec)
}

func (c closer) personalHandle() core.Handle {
	if c.personal() {
		return c.op.Agent.Handle
	}
	return ""
}

func (c closer) personalSession() string {
	if c.personal() {
		return c.op.Agent.SessionID
	}
	return ""
}

// owns reports whether lc, a closed Epic's tombstone, is one c may answer:
// for a personal agent one of its Hub or its session; for any other
// Operator any, of the --hub Hub when one is named.
func (c closer) owns(lc store.EpicLifecycle) bool {
	if !c.personal() {
		return c.hubDir == "" || lc.Hub.Dir() == c.hubDir
	}
	return lc.Hub != "" && lc.Hub == c.me.Hub || lc.PersonalSessionID != "" && lc.PersonalSessionID == c.op.Agent.SessionID && lc.Server == c.op.Agent.Server
}

// closeTarget is the Epic session c closes: for a personal agent one linked
// to it (linkTo); for any other Operator one the records know, never a
// Hub's session, and of the --hub Hub when one is named.
func (e Env) closeTarget(c closer, session string) (core.SessionKey, error) {
	if c.personal() {
		l, err := e.linkTo(*c.op.Agent, c.me, session)
		return l.Epic, err
	}
	p, hasPolicy, err := e.Store.SessionPolicy(session)
	if err != nil {
		return core.SessionKey{}, err
	}
	if hasPolicy && p.Hub() {
		return core.SessionKey{}, fmt.Errorf("%w: %s is a Hub's Rex session, not an Epic's", ErrRefused, session)
	}
	links, err := e.linksToEpic(session)
	if err != nil {
		return core.SessionKey{}, err
	}
	lc, hasLC, err := e.Store.EpicLifecycle(session)
	if err != nil {
		return core.SessionKey{}, err
	}
	if c.hubDir != "" && !hubAuthorizes(lc, hasLC, links, c.hubDir) {
		return core.SessionKey{}, notOfHub(session, c.hubDir)
	}
	switch {
	case len(links) > 0:
		return links[0].Epic, nil
	case hasLC:
		return core.SessionKey{Server: lc.Server, SessionID: session}, nil
	case hasPolicy && p.Epic():
		return core.SessionKey{Server: p.Server, SessionID: session}, nil
	}
	return core.SessionKey{}, fault.New(fault.NotFound, "%s is no Epic the adapter knows: it has no lifecycle, link, or policy", session).With("kind", "session")
}

// hubAuthorizes reports whether an Epic, by its lifecycle (when hasLC) and
// its links, is of the Hub directory dir: what --hub admits a close by.
func hubAuthorizes(lc store.EpicLifecycle, hasLC bool, links []core.Link, dir string) bool {
	return hasLC && lc.Hub.Dir() == dir || slices.ContainsFunc(links, func(l core.Link) bool { return l.Hub.Dir() == dir })
}

// epicHubDirs are the Hub directories the Epic session is of now, by its
// lifecycle and its links (hubAuthorizes), each once.
func (e Env) epicHubDirs(session string) ([]string, error) {
	lc, hasLC, err := e.Store.EpicLifecycle(session)
	if err != nil {
		return nil, err
	}
	links, err := e.linksToEpic(session)
	if err != nil {
		return nil, err
	}
	var dirs []string
	if hasLC && lc.Hub != "" {
		dirs = append(dirs, lc.Hub.Dir())
	}
	for _, l := range links {
		if l.Hub != "" && !slices.Contains(dirs, l.Hub.Dir()) {
			dirs = append(dirs, l.Hub.Dir())
		}
	}
	return dirs, nil
}

func notOfHub(session, dir string) error {
	return fmt.Errorf("%w: %s is not an Epic of the Hub %s (`rex-amq-adapter epic list --hub %s` lists them)", ErrRefused, session, dir, dir)
}

// linksToEpic is every recorded link to the Epic session.
func (e Env) linksToEpic(session string) ([]core.Link, error) {
	all, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var out []core.Link
	for _, l := range all {
		if l.Epic.SessionID == session {
			out = append(out, l)
		}
	}
	return out, nil
}

// legacyCloseIdentityOf is the identity a close request r from personal
// agent me had before v0.27: its Hub key (its session when it has none), the
// Epic, and --force.
func legacyCloseIdentityOf(me core.Endpoint, r CloseRequest) legacyCloseIdentity {
	id := legacyCloseIdentity{Hub: string(me.Hub), Session: r.Session, Force: r.Force}
	if me.Hub == "" {
		id.Personal = me.Session()
	}
	return id
}

// closedAlready answers a close of an Epic whose lifecycle is closed (its
// tombstone) for a caller of the Hub (or the session) that it was linked to:
// changed false, nothing killed again; with an op id not seen yet it is
// recorded closed, so its replay answers the same. done is false when the
// Epic is not closed, or the tombstone names another Hub. The caller holds
// LockEpicLifecycle and then LockLifecycle.
func (e Env) closedAlready(c closer, r CloseRequest, hash, legacy string, res *CloseResult, settle *closeSettle) (bool, error) {
	lc, ok, err := e.Store.EpicLifecycle(r.Session)
	if err != nil || !ok || lc.State != store.EpicClosed {
		return false, err
	}
	if !c.owns(lc) {
		return false, nil
	}
	res.LifecycleState, res.Parked = lc.State, []core.Handle{}
	// The close is this caller's to settle only once its op id, if any,
	// is its own.
	authorized := func() { settle.key = core.SessionKey{Server: lc.Server, SessionID: r.Session} }
	if r.DryRun || r.OpID == "" {
		res.DryRun = r.DryRun
		if !r.DryRun {
			authorized()
		}
		return true, nil
	}
	prev, found, err := e.Store.EpicOp(store.EpicOpClose, r.OpID)
	if err != nil {
		return true, err
	}
	if found {
		if same, err := e.opHashMatches(&prev, hash, legacy, false); err != nil {
			return true, err
		} else if !same {
			return true, errOpConflict(store.EpicOpClose, r.OpID)
		}
		authorized()
		return true, nil
	}
	now := e.Now().UTC()
	if err := e.Store.PutEpicOp(store.EpicOpRecord{Kind: store.EpicOpClose, OpID: r.OpID, RequestHash: hash, SessionID: r.Session, Server: lc.Server,
		PersonalHandle: c.personalHandle(), PersonalSessionID: c.personalSession(), State: store.EpicClosed, CreatedAt: now, UpdatedAt: now}); err != nil {
		return true, err
	}
	authorized()
	return true, nil
}

// replayClose finishes the close recorded under opID, when there is one: done
// is false when there is none, and the close goes on as a new one. The op id
// of another request (another Hub, another session, another --force) is
// refused.
func (e Env) replayClose(ctx context.Context, c closer, r CloseRequest, hash, legacy string, settle *closeSettle) (res CloseResult, done bool, err error) {
	opID := r.OpID
	unlockEpic, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return res, true, err
	}
	defer unlockEpic()
	op, found, err := e.Store.EpicOp(store.EpicOpClose, opID)
	if err != nil || !found {
		return res, err != nil, err
	}
	if same, err := e.opHashMatches(&op, hash, legacy, r.DryRun); err != nil {
		return CloseResult{OpID: opID}, true, err
	} else if !same {
		return CloseResult{OpID: opID}, true, errOpConflict(store.EpicOpClose, opID)
	}
	// --hub holds for a replay too: a Hub the Epic was of when its close
	// was recorded (HubDirs, which outlive the unlink), else one it is of
	// now by its lifecycle or links (hubAuthorizes), which is added to them
	// before anything can unlink it.
	if c.hubDir != "" && !c.personal() && !slices.Contains(op.HubDirs, c.hubDir) {
		hubs, err := e.epicHubDirs(op.SessionID)
		if err != nil {
			return CloseResult{OpID: opID}, true, err
		}
		if !slices.Contains(hubs, c.hubDir) {
			return CloseResult{OpID: opID}, true, notOfHub(op.SessionID, c.hubDir)
		}
		if !r.DryRun {
			op.HubDirs = append(op.HubDirs, c.hubDir)
			if err := e.Store.PutEpicOp(op); err != nil {
				return CloseResult{OpID: opID}, true, fmt.Errorf("record the close under --op-id %q: %w", opID, err)
			}
		}
	}
	if r.DryRun {
		// What the retry would do, and nothing of it: the record is not
		// advanced, the session not killed.
		res = CloseResult{SessionID: op.SessionID, OpID: opID, Parked: []core.Handle{}, Replayed: true, DryRun: true, Changed: op.State != store.EpicClosed}
		if op.State == store.EpicClosed {
			res.LifecycleState, res.Unlinked = store.EpicClosed, true
		}
		return res, true, nil
	}
	// The op is this caller's own close request: what it closed is settled.
	settle.key = core.SessionKey{Server: op.Server, SessionID: op.SessionID}
	if op.State == store.EpicClosed {
		// Seen through already: the tombstone answers, and nothing is
		// killed, parked, or unlinked again.
		return CloseResult{SessionID: op.SessionID, OpID: opID, Parked: []core.Handle{}, Unlinked: true, LifecycleState: store.EpicClosed, Replayed: true}, true, nil
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, true, err
	}
	defer unlock()
	// Another close (another op id, or a tab of the Hub with none) may have
	// closed the Epic since this one failed: its tombstone answers, so this
	// op is reconciled to closed and nothing is killed, parked, or unlinked
	// again.
	if lc, ok, err := e.Store.EpicLifecycle(op.SessionID); err != nil {
		return res, true, err
	} else if ok && lc.State == store.EpicClosed {
		if op.Advance(store.EpicClosed, e.Now().UTC()) {
			if err := e.Store.PutEpicOp(op); err != nil {
				return res, true, fmt.Errorf("record the close under --op-id %q: %w", opID, err)
			}
		}
		return CloseResult{SessionID: op.SessionID, OpID: opID, Parked: []core.Handle{}, LifecycleState: store.EpicClosed, Replayed: true}, true, nil
	}
	on := e.Rex
	if e.RexOn != nil && op.Server != "" {
		on = e.RexOn(op.Server)
	}
	res, err = e.finishClose(ctx, on, core.SessionKey{Server: op.Server, SessionID: op.SessionID}, c, &op)
	res.Replayed = true
	return res, true, err
}

// linkTo is a link that grants caller (me, its gateway endpoint) the Epic
// session, refused when there is none.
func (e Env) linkTo(caller agent, me core.Endpoint, session string) (core.Link, error) {
	links, err := e.linkedEpics(me)
	if err != nil {
		return core.Link{}, err
	}
	for _, l := range links {
		if l.Epic.SessionID == session {
			return l, nil
		}
	}
	return core.Link{}, fmt.Errorf("%w: %s is not an Epic linked to %s or its Hub (`rex-amq-adapter epic list` lists them); a session you did not open is the user's to close", ErrRefused, session, caller.Handle)
}

// unsettled names each live agent of epic, Stealth ones aside, that is not
// at rest (unrest).
func (e Env) unsettled(ctx context.Context, on Terminal, epic core.SessionKey) ([]string, error) {
	recs, err := e.agents()
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID || core.Stealth(rec.Role) {
			continue
		}
		why, err := e.unrest(ctx, on, rec)
		if err != nil {
			return nil, err
		}
		if why != "" {
			busy = append(busy, why)
		}
	}
	return busy, nil
}

// unrest says, "<handle>: <why>", how rec's agent is not at rest: working,
// waiting on a dialog, with unread mail, or reporting no state, which cannot
// be told from working. It is "" at rest, and when its block is gone, which
// leaves nothing in the way.
func (e Env) unrest(ctx context.Context, on Terminal, rec agent) (string, error) {
	snap, err := e.assessActivity(ctx, activityRequest{Terminal: on, Agent: &rec, Block: rec.BlockID, Policy: LazyScreen})
	if err != nil {
		if errors.Is(err, rex.ErrNotFound) {
			return "", nil
		}
		return fmt.Sprintf("%s: state unknown (%v)", rec.Handle, err), nil
	}
	switch a := snap.Activity.(type) {
	case core.AtPrompt:
		if a.Box == screen.BoxDraft {
			return fmt.Sprintf("%s: draft in input box", rec.Handle), nil
		}
		// When TurnIdle overrides a non-at-rest external status (such as
		// frozen OSC working), require a successful empty-box check before
		// allowing shutdown so a draft or hidden dialog cannot be discarded.
		if a.Source == core.SignalTurnLedger && !snap.External.State.AtRest() && (snap.Surface == nil || a.Box != screen.BoxEmpty) {
			return fmt.Sprintf("%s: cannot verify empty input box", rec.Handle), nil
		}
		// At rest; check unread mail below.
	case core.InTurn:
		return fmt.Sprintf("%s: working", rec.Handle), nil
	case core.AwaitingUser:
		return fmt.Sprintf("%s: blocked", rec.Handle), nil
	case core.Unknown:
		if a.Why == "no_signal" {
			return fmt.Sprintf("%s: reports no state", rec.Handle), nil
		}
		if a.Unrecognized != "" {
			return fmt.Sprintf("%s: %s", rec.Handle, a.Unrecognized), nil
		}
		return fmt.Sprintf("%s: %s", rec.Handle, a.Why), nil
	default:
		return fmt.Sprintf("%s: reports no state", rec.Handle), nil
	}
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return "", err
	}
	if len(unread) > 0 {
		return fmt.Sprintf("%s: %d unread message(s)", rec.Handle, len(unread)), nil
	}
	return "", nil
}

// parkClosed parks each live agent recorded in epic, the caller holding the
// lifecycle lock, as departed for epic_closed. Unlike a departure (park) it
// tells nobody: everyone it would tell was in the session that was just
// killed.
func (e Env) parkClosed(ctx context.Context, epic core.SessionKey) ([]core.Handle, error) {
	recs, err := e.agents()
	if err != nil {
		return nil, err
	}
	var parked []core.Handle
	var errs []error
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID {
			continue
		}
		if _, err := e.departQuietly(rec, store.DepartEpicClosed); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rec.Handle, err))
			continue
		}
		parked = append(parked, rec.Handle)
	}
	return parked, errors.Join(errs...)
}

// departQuietly releases rec's lease as departed for reason, the caller
// holding the lifecycle lock, as unfinished: the caller, once it has released
// the lock, finishes it quietly (finishQuietly), retiring the waker and
// clearing the window notes, and telling nobody (quietDeparture). Waiting
// here for the handle's lock or for amq would make every hook on the host
// wait behind a waker. A departure left unfinished is finished by collection.
func (e Env) departQuietly(rec agent, reason string) (agent, error) {
	return e.releaseLease(rec, store.Departure{Reason: reason, At: e.Now().UTC(), Unfinished: true})
}

// finishQuietly finishes, with no lock held, each departure of recs (as
// they were read) that is a quiet one still unfinished (departQuietly). The
// departures stand whatever happens here: one it cannot finish (its waker
// would not retire) stays unfinished, collection finishes it, and the error
// says so. finishDeparture acts only on the departure each record read.
func (e Env) finishQuietly(ctx context.Context, recs []agent) error {
	var errs []error
	for _, rec := range recs {
		p := rec.departure()
		if p == nil || !p.Unfinished || !quietDeparture(p.DepartReason) {
			continue
		}
		if err := e.finishDeparture(ctx, rec); err != nil {
			errs = append(errs, fmt.Errorf("%s departed, but finishing its departure failed (collection finishes it): %w", rec.Handle, err))
		}
	}
	return errors.Join(errs...)
}
