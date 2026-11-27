package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An Epic's lifecycle (store.EpicLifecycle) is written at its edges: the
// session created (partial_start), both agents adopted (opened), a hand-back
// recorded (hand_back), and the close (closed). An `epic --op-id` or
// `epic close --op-id` also keeps its own record (store.EpicOpRecord), so a
// retry of the same operation, after a timeout or a crash, finishes what the
// first run started instead of starting it again: a second open reconciles
// the session the first created and never creates another; a second close
// finishes the kill, the parking, and the unlink, even once the link is
// gone.

// maxOpID bounds an operation id, in bytes.
const maxOpID = 256

// ValidOpID checks an --op-id: "" is none; otherwise at most 256 bytes of
// UTF-8 with no control characters and not only blanks. An invalid one is
// ErrUsage.
func ValidOpID(id string) error {
	if id == "" {
		return nil
	}
	if len(id) > maxOpID || !utf8.ValidString(id) || strings.TrimSpace(id) == "" || strings.ContainsFunc(id, unicode.IsControl) {
		return fmt.Errorf("%w: --op-id is at most %d bytes of text with no control characters", ErrUsage, maxOpID)
	}
	return nil
}

// requestHash is "sha256:<hex>" of v's JSON: the identity of a request, so
// an op id reused for another request is told from a retry.
func requestHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // v is a struct of strings
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// openIdentity is what makes two `epic open --op-id` requests the same
// one: the Hub the Epic is linked to (HubDir, whoever of its Operators
// asks), or with no Hub the personal agent's session, and the Epic, where,
// under what grant, as which crew, on which harness.
type openIdentity struct {
	HubDir   string          `json:"hub_dir,omitempty"`
	Personal core.SessionKey `json:"personal,omitzero"`
	Name     string          `json:"name"`
	Dir      string          `json:"dir"`
	Goal     string          `json:"goal"`
	Asked    string          `json:"asked"`
	Crew     string          `json:"crew"`
	Harness  string          `json:"harness"`
	// GoalSHA256 is the digest of a contract piped in (--goal -), which has
	// no path; omitted otherwise.
	GoalSHA256 string `json:"goal_sha256,omitempty"`
}

// closeIdentity is what makes two `epic close --op-id` requests the same
// one: the Hub (a personal agent's own, or its session without one; for any
// other Operator the Hub the Epic's lifecycle records), the Epic, and
// --force (closeHashes). Neither changes under the close's own unlink, so a
// retry after a close that died part way is the same request.
type closeIdentity struct {
	HubDir   string          `json:"hub_dir,omitempty"`
	Personal core.SessionKey `json:"personal,omitzero"`
	Session  string          `json:"session"`
	Force    bool            `json:"force,omitempty"`
}

// The identities before v0.27 named the personal agent (or the user) that
// asked, not the Hub, so an op id one Operator recorded could not be
// replayed by another. An op recorded under one is still the same request
// for the caller that could have made it (opHashMatches).
//
// TODO(v0.28): remove legacyOpenIdentity, legacyCloseIdentity, and
// legacyReviveIdentity, one minor version after the change.
type legacyOpenIdentity struct {
	Personal   core.SessionKey `json:"personal"`
	Name       string          `json:"name"`
	Dir        string          `json:"dir"`
	Goal       string          `json:"goal"`
	Asked      string          `json:"asked"`
	Crew       string          `json:"crew"`
	Harness    string          `json:"harness"`
	GoalSHA256 string          `json:"goal_sha256,omitempty"`
}

type legacyCloseIdentity struct {
	Hub      string          `json:"hub,omitempty"`
	Personal core.SessionKey `json:"personal"`
	Session  string          `json:"session"`
	Force    bool            `json:"force,omitempty"`
}

type legacyReviveIdentity struct {
	Session  string          `json:"session"`
	Personal core.SessionKey `json:"personal,omitzero"`
	Human    bool            `json:"human,omitempty"`
}

// opHashMatches reports whether prev, a recorded op, is the request whose
// identity hashes to hash. One recorded under legacy, the hash the same
// request had before v0.27 ("" when this caller could not have made one),
// is the same request too: unless dryRun, its record is rewritten to hash,
// so its next replay matches on its own. The caller holds
// LockEpicLifecycle when it writes.
func (e Env) opHashMatches(prev *store.EpicOpRecord, hash, legacy string, dryRun bool) (bool, error) {
	if prev.RequestHash == hash {
		return true, nil
	}
	if legacy == "" || prev.RequestHash != legacy {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	prev.RequestHash = hash
	if err := e.Store.PutEpicOp(*prev); err != nil {
		return true, fmt.Errorf("record --op-id %q under its new request hash: %w", prev.OpID, err)
	}
	return true, nil
}

// epicHubDir is the Hub directory session's Epic lifecycle records, "" for
// none.
func (e Env) epicHubDir(session string) (string, error) {
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok {
		return "", err
	}
	return lc.Hub.Dir(), nil
}

// errOpConflict refuses an op id already used for another request.
func errOpConflict(kind, opID string) error {
	return refusal(fault.Conflict, "op_id_reused", "--op-id %q already names another epic %s request; a new request needs a new op id", opID, kind)
}

// advanceEpic moves session seed.SessionID's lifecycle to state, starting it
// from seed when none was recorded and filling in what it lacks from seed,
// and op with it when there is one. The caller holds LockEpicLifecycle.
func (e Env) advanceEpic(seed store.EpicLifecycle, state string, op *store.EpicOpRecord) (store.EpicLifecycle, error) {
	now := e.Now().UTC()
	lc, ok, err := e.Store.EpicLifecycle(seed.SessionID)
	if err != nil {
		return lc, err
	}
	if !ok {
		lc = store.EpicLifecycle{SessionID: seed.SessionID, UpdatedAt: now}
	}
	fill(&lc.Server, seed.Server)
	fill(&lc.Hub, seed.Hub)
	fill(&lc.Name, seed.Name)
	fill(&lc.Dir, seed.Dir)
	fill(&lc.PersonalHandle, seed.PersonalHandle)
	fill(&lc.PersonalSessionID, seed.PersonalSessionID)
	fill(&lc.Crew, seed.Crew)
	fill(&lc.OpenOpID, seed.OpenOpID)
	fill(&lc.CloseOpID, seed.CloseOpID)
	lc.ClosedLinks = mergeLinks(lc.ClosedLinks, seed.ClosedLinks...)
	if lc.OpenedAt.IsZero() && (state == store.EpicPartialStart || state == store.EpicOpened) {
		lc.OpenedAt = now
	}
	lc.Advance(state, now)
	if state == store.EpicClosed {
		// Persist the closed lifecycle tombstone before marking the close op
		// complete so replayClose never returns early before lifecycle.json is
		// durable.
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			return lc, err
		}
		if op != nil && op.Advance(state, now) {
			if err := e.Store.PutEpicOp(*op); err != nil {
				return lc, err
			}
		}
		return lc, nil
	}
	if op != nil && op.Advance(state, now) {
		if err := e.Store.PutEpicOp(*op); err != nil {
			return lc, err
		}
	}
	return lc, e.Store.PutEpicLifecycle(lc)
}

// lockedAdvanceEpic is advanceEpic under LockEpicLifecycle.
func (e Env) lockedAdvanceEpic(ctx context.Context, seed store.EpicLifecycle, state string, op *store.EpicOpRecord) (store.EpicLifecycle, error) {
	unlock, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return store.EpicLifecycle{}, err
	}
	defer unlock()
	return e.advanceEpic(seed, state, op)
}

func fill[T comparable](dst *T, v T) {
	var zero T
	if *dst == zero {
		*dst = v
	}
}

// reconcileOpen is a retry of the `epic --op-id` op recorded: it starts
// nothing. It finds the Epic session the first run created, links it again,
// records the session's unattended policy and crew if the first run could
// not, waits as the first run did for both agents, and moves the lifecycle
// on.
func (e Env) reconcileOpen(ctx context.Context, o opener, op store.EpicOpRecord, seed store.EpicLifecycle, grant epicGrant) (EpicResult, error) {
	res := EpicResult{SessionID: op.SessionID, OpID: op.OpID, Replayed: true, Hub: o.hub.Dir(), NoHub: o.noHub(),
		Lead: EpicLead{Role: epicLead(seed.Crew), EpicAgent: EpicAgent{Block: op.OrchestratorBlock}}, Narrator: EpicAgent{Block: op.NarratorBlock}}
	lc, ok, err := e.Store.EpicLifecycle(op.SessionID)
	if err != nil {
		return res, err
	}
	res.LifecycleState = op.State
	if ok {
		res.LifecycleState = lc.State
	}
	if lc.State == store.EpicClosed {
		return res, refusal(fault.Conflict, "op_id_reused", "--op-id %q opened the Epic %s, which was closed; a new Epic needs a new op id", op.OpID, op.SessionID)
	}
	if _, err := e.Rex.InspectSession(ctx, op.SessionID); errors.Is(err, rex.ErrNotFound) {
		return res, fmt.Errorf("--op-id %q opened the Epic session %s, which Rex no longer has; nothing was started again (a new Epic needs a new op id)", op.OpID, op.SessionID)
	} else if err != nil {
		return res, fmt.Errorf("inspect the Epic session %s that --op-id %q opened: %w", op.SessionID, op.OpID, err)
	}
	var errs []error
	if err := e.ensureEpicPolicy(op, seed.Crew, o.op.By()); err != nil {
		errs = append(errs, fmt.Errorf("record the Epic policy: %w", err))
	}
	if seed.Crew != "" {
		if err := e.ensureSessionCrew(ctx, op.SessionID, seed.Crew, o.op.By()); err != nil {
			errs = append(errs, fmt.Errorf("record the crew: %w", err))
		}
	}
	// The first run's grant stands; one it never wrote is written now.
	if err := e.notarize(op.SessionID, o.op, grant); err != nil {
		errs = append(errs, fmt.Errorf("notarize the grant: %w", err))
	} else {
		res.GoalPath, res.GrantPath = e.grantPaths(op.SessionID)
	}
	if res.Linked, err = e.linkEpic(ctx, o, op.SessionID); err != nil {
		errs = append(errs, fmt.Errorf("link: %w", err))
	}
	errs = append(errs, e.awaitEpicAgents(ctx, &res)...)
	state := store.EpicPartialStart
	if res.Lead.Status == core.StartupAdopted && res.Narrator.Status == core.StartupAdopted {
		state = store.EpicOpened
	}
	if lc, err := e.lockedAdvanceEpic(ctx, seed, state, &op); err != nil {
		errs = append(errs, fmt.Errorf("record the lifecycle: %w", err))
	} else {
		res.LifecycleState = lc.State
	}
	return res, errors.Join(errs...)
}

// dropUnstartedOp removes the intent of op, an open whose session.create
// failed, when Rex lists no session it did not list before: nothing was
// created, so no retry needs the intent, and no other open's session is
// taken for its (adoptCreated). With any doubt the intent stays for the retry
// to settle. The caller holds LockEpicLifecycle.
func (e Env) dropUnstartedOp(ctx context.Context, op store.EpicOpRecord) {
	after, err := e.Rex.ListSessions(ctx)
	if err != nil {
		return
	}
	for _, id := range after {
		if !slices.Contains(op.PriorSessions, id) {
			return
		}
	}
	// Best effort: an intent left behind only makes a retry more careful.
	_ = e.Store.DeleteEpicOp(store.EpicOpOpen, op.OpID)
}

// adoptCreated settles an open op whose first run asked Rex for its session
// and ended before recording which (store.EpicCreating). The session it
// created is the one Rex has now and did not have then (op.PriorSessions),
// labelled name, holding the Epic's narrator and its lead (epicLead: an
// orchestrator, or a pair Epic's implementer), and not another open's Epic
// (recorded in that op, or in a lifecycle it opened): that one is recorded
// in op, and its lifecycle moved to partial_start, and found is true. With
// none, Rex created nothing and found is false: the op may create it. More
// than one, one that is not an Epic's, or one that another unfinished open
// of the same name could have created as well, is an error, and nothing is
// created. The caller holds LockEpicLifecycle.
func (e Env) adoptCreated(ctx context.Context, op *store.EpicOpRecord, seed store.EpicLifecycle, name string) (found bool, err error) {
	ops, err := e.Store.EpicOps(store.EpicOpOpen)
	if err != nil {
		return false, err
	}
	others := slices.DeleteFunc(ops, func(o store.EpicOpRecord) bool { return o.OpID == op.OpID })
	ids, err := e.Rex.ListSessions(ctx)
	if err != nil {
		return false, fmt.Errorf("list Rex's sessions for the one --op-id %q created: %w", op.OpID, err)
	}
	var fresh []string
	for _, id := range ids {
		if slices.Contains(op.PriorSessions, id) {
			continue
		}
		s, err := e.Rex.InspectSession(ctx, id)
		if errors.Is(err, rex.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect %s for the session --op-id %q created: %w", id, op.OpID, err)
		}
		if s.Label != name {
			continue
		}
		if slices.ContainsFunc(others, func(o store.EpicOpRecord) bool { return o.SessionID == id }) {
			continue
		}
		// A lifecycle an open wrote; a hand-back alone writes one too, and
		// says nothing of who opened the session.
		if lc, ok, err := e.Store.EpicLifecycle(id); err != nil {
			return false, err
		} else if ok && lc.OpenOpID != op.OpID && (lc.OpenOpID != "" || !lc.OpenedAt.IsZero()) {
			continue
		}
		fresh = append(fresh, id)
	}
	switch len(fresh) {
	case 0:
		return false, nil
	case 1:
	default:
		return false, fmt.Errorf("--op-id %q: Rex has %d new sessions labelled %q (%s) and nothing tells which one this op created; nothing was started (close the ones that are not its Epic and retry)", op.OpID, len(fresh), name, strings.Join(fresh, ", "))
	}
	for _, o := range others {
		if o.State == store.EpicCreating && (o.Name == "" || o.Name == name) && (o.Server == "" || op.Server == "" || o.Server == op.Server) && !slices.Contains(o.PriorSessions, fresh[0]) {
			return false, refusal(fault.Conflict, "op_id_ambiguous", "--op-id %q cannot tell whether session %s (labelled %q) is its own or unfinished --op-id %q's; nothing was started", op.OpID, fresh[0], name, o.OpID)
		}
	}
	blocks, err := e.Rex.ListBlocks(ctx, fresh[0])
	if err != nil {
		return false, fmt.Errorf("list the blocks of %s, the session --op-id %q created: %w", fresh[0], op.OpID, err)
	}
	lead := epicLead(seed.Crew)
	var narrators, leads []string
	for _, b := range blocks {
		switch core.Role(b.Label) {
		case core.NarratorRole:
			narrators = append(narrators, b.ID)
		case lead:
			leads = append(leads, b.ID)
		}
	}
	if len(narrators) != 1 || len(leads) != 1 {
		return false, fmt.Errorf("--op-id %q: Rex has a new session %s labelled %q, but it does not hold one narrator and one %s, so it may not be this op's Epic; nothing was started", op.OpID, fresh[0], name, lead)
	}
	op.SessionID, op.NarratorBlock, op.OrchestratorBlock, op.PriorSessions = fresh[0], narrators[0], leads[0], nil
	seed.SessionID = fresh[0]
	if _, err := e.advanceEpic(seed, store.EpicPartialStart, op); err != nil {
		return false, fmt.Errorf("record %s, the session --op-id %q created: %w", fresh[0], op.OpID, err)
	}
	return true, nil
}

// ensureEpicPolicy records the unattended policy of op's Epic unless one is:
// a first run that died between creating the session and writing it
// (newEpicSession) leaves its agents' later summons without it.
func (e Env) ensureEpicPolicy(op store.EpicOpRecord, crewName string, by core.Handle) error {
	if _, ok, err := e.Store.SessionPolicy(op.SessionID); err != nil || ok {
		return err
	}
	return e.Store.PutSessionPolicy(epicPolicy(op.SessionID, op.Server, crewName, by, e.Now().UTC()))
}

// ensureSessionCrew records crew as session's crew unless one is.
func (e Env) ensureSessionCrew(ctx context.Context, session, crewName string, by core.Handle) error {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok, err := e.Store.SessionCrew(session); err != nil || ok {
		return err
	}
	return e.Store.PutSessionCrew(session, store.SessionCrew{Crew: crewName, ClaimedBy: by, ClaimedAt: e.Now().UTC()})
}

// awaitEpicAgents waits, until ctx ends, for the Epic's two agents to be
// adopted, and fills in how each startup ended.
func (e Env) awaitEpicAgents(ctx context.Context, res *EpicResult) []error {
	var errs []error
	for _, a := range []*EpicAgent{&res.Lead.EpicAgent, &res.Narrator} {
		s, err := e.AwaitStartup(ctx, a.Block)
		if err != nil {
			errs = append(errs, fmt.Errorf("await %s: %w", a.Block, err))
			continue
		}
		a.Startup = s
	}
	return errs
}

// finishClose kills the Epic's session (one already gone is not an error),
// parks its agents, removes every link to it that grants caller (me), and
// records the Epic closed, with op when there is one; the closed lifecycle
// stays as the close's tombstone. The caller holds LockEpicLifecycle and
// then LockLifecycle.
func (e Env) finishClose(ctx context.Context, on Terminal, epic core.SessionKey, c closer, op *store.EpicOpRecord) (CloseResult, error) {
	res := CloseResult{SessionID: epic.SessionID, Changed: true}
	if op != nil {
		res.OpID = op.OpID
	}
	// Killed first: parking first would retire the wakers of agents that
	// stay alive if the kill fails. The lock keeps the session's watcher
	// and the dying agents' SessionEnd hooks from parking them meanwhile.
	if err := on.KillSession(ctx, epic.SessionID); err != nil && !errors.Is(err, rex.ErrNotFound) {
		return res, fmt.Errorf("kill the Epic's session: %w", err)
	}
	var err error
	if res.Parked, err = e.parkClosed(ctx, epic); err != nil {
		return res, err
	}
	links, err := e.closeLinksFor(c, epic)
	if err != nil {
		return res, err
	}
	lcExisting, hasLC, err := e.Store.EpicLifecycle(epic.SessionID)
	if err != nil {
		return res, err
	}
	var closedLinks []core.Link
	if op != nil {
		closedLinks = mergeLinks(closedLinks, op.ClosedLinks...)
	}
	if hasLC {
		closedLinks = mergeLinks(closedLinks, lcExisting.ClosedLinks...)
	}
	closedLinks = mergeLinks(closedLinks, links...)
	// Persist the unlink-phase grant snapshot durably BEFORE the first
	// DeleteLink so a crash between unlink and the final closed tombstone never
	// loses the closer's archive on replay, while grants revoked before close
	// reached this unlink phase are never restored.
	if op != nil && !slices.Equal(closedLinks, op.ClosedLinks) {
		op.ClosedLinks = slices.Clone(closedLinks)
		if err := e.Store.PutEpicOp(*op); err != nil {
			return res, fmt.Errorf("record the close under --op-id %q: %w", op.OpID, err)
		}
	}
	if hasLC && !slices.Equal(closedLinks, lcExisting.ClosedLinks) {
		lcExisting.ClosedLinks = slices.Clone(closedLinks)
		if err := e.Store.PutEpicLifecycle(lcExisting); err != nil {
			return res, fmt.Errorf("record the Epic's closed grants: %w", err)
		}
	}
	for _, l := range links {
		if _, err := e.Store.DeleteLink(l); err != nil {
			return res, fmt.Errorf("unlink the Epic: %w", err)
		}
	}
	res.Unlinked = true
	seed := store.EpicLifecycle{SessionID: epic.SessionID, Server: epic.Server, Hub: c.me.Hub, PersonalHandle: c.personalHandle(), PersonalSessionID: c.personalSession(), ClosedLinks: closedLinks}
	if op != nil {
		seed.CloseOpID = op.OpID
	}
	lc, err := e.advanceEpic(seed, store.EpicClosed, op)
	if err != nil {
		return res, fmt.Errorf("the Epic is closed; recording it in its lifecycle failed: %w", err)
	}
	res.LifecycleState = lc.State
	return res, nil
}

func (e Env) closeLinksFor(c closer, epic core.SessionKey) ([]core.Link, error) {
	if c.personal() {
		return e.linksFor(c.me, epic)
	}
	return e.linksToEpic(epic.SessionID)
}

func mergeLinks(dst []core.Link, src ...core.Link) []core.Link {
	out := slices.Clone(dst)
	for _, l := range src {
		if !slices.ContainsFunc(out, func(e core.Link) bool { return e.SameGrant(l) }) {
			out = append(out, l)
		}
	}
	return out
}
