package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/core"
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

// validOpID checks an --op-id: "" is none; otherwise at most maxOpID bytes of
// UTF-8 with no control characters and not only blanks.
func validOpID(id string) error {
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

// openIdentity is what makes two `epic` requests the same one: who asks, for
// which Epic, where, under what grant, as which crew, on which harness.
type openIdentity struct {
	Personal core.SessionKey `json:"personal"`
	Name     string          `json:"name"`
	Dir      string          `json:"dir"`
	Goal     string          `json:"goal"`
	Asked    string          `json:"asked"`
	Crew     string          `json:"crew"`
	Harness  string          `json:"harness"`
}

// closeIdentity is what makes two `epic close` requests the same one.
type closeIdentity struct {
	Personal core.SessionKey `json:"personal"`
	Session  string          `json:"session"`
}

// errOpConflict refuses an op id already used for another request.
func errOpConflict(kind, opID string) error {
	return fmt.Errorf("%w: --op-id %q already names another epic %s request; a new request needs a new op id", ErrRefused, opID, kind)
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
	fill(&lc.Name, seed.Name)
	fill(&lc.Dir, seed.Dir)
	fill(&lc.PersonalHandle, seed.PersonalHandle)
	fill(&lc.PersonalSessionID, seed.PersonalSessionID)
	fill(&lc.Crew, seed.Crew)
	fill(&lc.OpenOpID, seed.OpenOpID)
	fill(&lc.CloseOpID, seed.CloseOpID)
	if lc.OpenedAt.IsZero() && (state == store.EpicPartialStart || state == store.EpicOpened) {
		lc.OpenedAt = now
	}
	lc.Advance(state, now)
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
// records the session's crew if the first run could not, waits as the first
// run did for both agents, and moves the lifecycle on.
func (e Env) reconcileOpen(ctx context.Context, loc Location, op store.EpicOpRecord, seed store.EpicLifecycle) (EpicResult, error) {
	res := EpicResult{SessionID: op.SessionID, OpID: op.OpID, Replayed: true,
		Orchestrator: EpicAgent{Block: op.OrchestratorBlock}, Narrator: EpicAgent{Block: op.NarratorBlock}}
	lc, ok, err := e.Store.EpicLifecycle(op.SessionID)
	if err != nil {
		return res, err
	}
	res.LifecycleState = op.State
	if ok {
		res.LifecycleState = lc.State
	}
	if lc.State == store.EpicClosed {
		return res, fmt.Errorf("%w: --op-id %q opened the Epic %s, which was closed; a new Epic needs a new op id", ErrRefused, op.OpID, op.SessionID)
	}
	if _, err := e.Rex.InspectSession(ctx, op.SessionID); errors.Is(err, rex.ErrNotFound) {
		return res, fmt.Errorf("--op-id %q opened the Epic session %s, which Rex no longer has; nothing was started again (a new Epic needs a new op id)", op.OpID, op.SessionID)
	} else if err != nil {
		return res, fmt.Errorf("inspect the Epic session %s that --op-id %q opened: %w", op.SessionID, op.OpID, err)
	}
	var errs []error
	if seed.Crew != "" {
		if err := e.ensureSessionCrew(ctx, op.SessionID, seed.Crew, op.PersonalHandle); err != nil {
			errs = append(errs, fmt.Errorf("record the crew: %w", err))
		}
	}
	if _, _, err := e.Link(ctx, loc, op.SessionID, true); err != nil {
		errs = append(errs, fmt.Errorf("link: %w", err))
	} else {
		res.Linked = true
	}
	errs = append(errs, e.awaitEpicAgents(ctx, &res)...)
	state := store.EpicPartialStart
	if res.Orchestrator.Status == core.StartupAdopted && res.Narrator.Status == core.StartupAdopted {
		state = store.EpicOpened
	}
	if lc, err := e.lockedAdvanceEpic(ctx, seed, state, &op); err != nil {
		errs = append(errs, fmt.Errorf("record the lifecycle: %w", err))
	} else {
		res.LifecycleState = lc.State
	}
	return res, errors.Join(errs...)
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
	for _, a := range []*EpicAgent{&res.Orchestrator, &res.Narrator} {
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
// parks its agents, removes link, and records the Epic closed, with op when
// there is one. The caller holds LockEpicLifecycle and then LockLifecycle.
func (e Env) finishClose(ctx context.Context, on Terminal, link core.Link, caller store.Record, op *store.EpicOpRecord) (CloseResult, error) {
	res := CloseResult{SessionID: link.Epic.SessionID}
	if op != nil {
		res.OpID = op.OpID
	}
	// Killed first: parking first would retire the wakers of agents that
	// stay alive if the kill fails. The lock keeps the session's watcher
	// and the dying agents' SessionEnd hooks from parking them meanwhile.
	if err := on.KillSession(ctx, link.Epic.SessionID); err != nil && !errors.Is(err, rex.ErrNotFound) {
		return res, fmt.Errorf("kill the Epic's session: %w", err)
	}
	var err error
	if res.Parked, err = e.parkClosed(ctx, link.Epic); err != nil {
		return res, err
	}
	if _, err := e.Store.DeleteLink(link); err != nil {
		return res, fmt.Errorf("unlink the Epic: %w", err)
	}
	res.Unlinked = true
	seed := store.EpicLifecycle{SessionID: link.Epic.SessionID, Server: link.Epic.Server, PersonalHandle: caller.Handle, PersonalSessionID: caller.SessionID}
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
