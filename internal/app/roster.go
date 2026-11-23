package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A handle whose agent will never be resumed leaves the amq root, so the
// roster every `amq init --force` rewrites and fsyncs stays the size of the
// agents that may still run (internal/amq/roster.go). Unresumable is a
// terminal departure: an Epic closed (`epic revive` never revives a closed
// Epic) or a peer retired as finished. Each leaves on its event (Epic close,
// agent retire) and a sweep (agent collect, and the session watchers every
// rosterSweepEvery) takes whatever is left, the handles of identities
// collection forgot included. A personal agent's or a free session's
// departure stays reserved for a resume (ReserveFor), and its roster entry
// with it, until collection forgets it; the sweep then takes the entry.

// rosterSweepEvery is how often a session watcher sweeps the roster.
const rosterSweepEvery = 10 * time.Minute

// rosterChunk bounds the handle locks one step of a retirement holds.
const rosterChunk = 32

// RosterReport is what a roster retirement did.
type RosterReport struct {
	// Pruned left the roster, their mailboxes removed.
	Pruned []core.Handle `json:"pruned"`
	// Forgotten lost their identity records too.
	Forgotten []core.Handle `json:"forgotten"`
	// Kept stayed, each with why: its mailbox holds mail (amq's reason), it
	// runs again, its departure is unfinished, or a launch reserved it.
	Kept map[core.Handle]string `json:"kept,omitempty"`
}

func (r *RosterReport) keep(h core.Handle, why string) {
	if r.Kept == nil {
		r.Kept = map[core.Handle]string{}
	}
	r.Kept[h] = why
}

// retireRoster takes hs out of the amq root: for each, under its handle's
// lock (which every mailbox maker of an adoption holds), it checks that the
// handle still may leave (unresumable, departure finished, not reserved),
// removes its mailbox through amq (which keeps one holding mail), and takes
// the removed ones out of the roster; then, under the lifecycle lock, it
// forgets the identity of each removed handle forget selects, if the record
// is still the one read. A handle with no identity (forgotten already) leaves
// unless a launch reserved it.
func (e Env) retireRoster(ctx context.Context, hs []core.Handle, forget func(agent) bool) (RosterReport, error) {
	var rep RosterReport
	hs = slices.Clone(hs)
	slices.Sort(hs)
	hs = slices.Compact(hs)
	var errs []error
	for len(hs) > 0 {
		n := min(rosterChunk, len(hs))
		errs = append(errs, e.retireChunk(ctx, hs[:n], forget, &rep))
		hs = hs[n:]
	}
	return rep, errors.Join(errs...)
}

func (e Env) retireChunk(ctx context.Context, hs []core.Handle, forget func(agent) bool, rep *RosterReport) error {
	// Sorted, so two retirements never wait for each other's locks in turn.
	for _, h := range hs {
		unlock, err := e.Store.LockHandle(ctx, h)
		if err != nil {
			return err
		}
		defer unlock()
	}
	reserved, err := e.reservedNames()
	if err != nil {
		return err
	}
	var removed []string
	cleared := map[core.Handle]bool{} // a mailbox this run removed
	read := map[core.Handle]agent{}
	var errs []error
	for _, h := range hs {
		rec, ok, err := e.agentOf(h)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
			continue
		}
		if why := mayLeave(rec, ok, reserved[h]); why != "" {
			rep.keep(h, why)
			continue
		}
		r, err := e.AMQ.RemoveMailbox(ctx, e.Paths.Root(), string(h))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
			continue
		}
		if !r.Gone() {
			rep.keep(h, r.Refusal)
			continue
		}
		removed = append(removed, string(h))
		cleared[h] = r.Removed
		if ok {
			read[h] = rec
		}
	}
	if len(removed) == 0 {
		return errors.Join(errs...)
	}
	pruned, err := e.AMQ.PruneAgents(ctx, e.Paths.Root(), removed)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, h := range removed {
		if cleared[core.Handle(h)] || slices.Contains(pruned, h) {
			rep.Pruned = append(rep.Pruned, core.Handle(h))
		}
	}
	gone, err := e.forgetRetired(ctx, read, forget)
	rep.Forgotten = append(rep.Forgotten, gone...)
	return errors.Join(append(errs, err)...)
}

// mayLeave is why h, read as rec (ok false: no identity), stays in the
// roster; "" when it may leave.
func mayLeave(rec agent, ok, reserved bool) string {
	switch {
	case reserved:
		return "reserved for a launch"
	case !ok:
		return ""
	case rec.Live():
		return "running"
	}
	p := rec.departure()
	switch {
	case p == nil:
		return "never placed"
	case p.Unfinished:
		return "departure unfinished"
	case !quietDeparture(p.DepartReason):
		return "reserved for a resume"
	}
	return ""
}

// forgetRetired forgets, under the lifecycle lock, the identity of each
// record of read that forget selects and that is still departed from the
// same placement.
func (e Env) forgetRetired(ctx context.Context, read map[core.Handle]agent, forget func(agent) bool) ([]core.Handle, error) {
	if forget == nil || len(read) == 0 {
		return nil, nil
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var gone []core.Handle
	var errs []error
	for h, rec := range read {
		if !forget(rec) {
			continue
		}
		now, ok, err := e.agentOf(h)
		if err != nil || !ok || !sameDeparture(now, rec) {
			errs = append(errs, err)
			continue
		}
		if err := e.Store.ForgetIdentity(h); err != nil {
			errs = append(errs, err)
			continue
		}
		gone = append(gone, h)
	}
	slices.Sort(gone)
	return gone, errors.Join(errs...)
}

// reservedNames are the handles unexpired launch reservations keep.
func (e Env) reservedNames() (map[core.Handle]bool, error) {
	rs, err := e.Store.Reservations()
	if err != nil {
		return nil, err
	}
	out := map[core.Handle]bool{}
	for _, r := range rs {
		if !r.Expired(e.Now()) {
			out[r.Handle] = true
		}
	}
	return out, nil
}

// retireEpicRoster takes the handles of the closed Epic session out of the
// root, forgetting their identities: every agent of it that departed for
// epic_closed or retired.
func (e Env) retireEpicRoster(ctx context.Context, epic core.SessionKey) (RosterReport, error) {
	recs, err := e.agents()
	if err != nil {
		return RosterReport{}, err
	}
	var hs []core.Handle
	for _, r := range recs {
		if p := r.departure(); p != nil && r.Server == epic.Server && r.SessionID == epic.SessionID && quietDeparture(p.DepartReason) {
			hs = append(hs, r.Handle)
		}
	}
	return e.retireRoster(ctx, hs, func(agent) bool { return true })
}

// SweepRoster takes every handle that will never be resumed out of the root:
// the records departed for epic_closed (identities forgotten), those retired
// (identities forgotten once their Epic is closed, kept for its narrator's
// pulse until then), and roster entries of handles with no identity that the
// adapter named (core.LooksLikeHandle). Entries of other names are left.
func (e Env) SweepRoster(ctx context.Context) (RosterReport, error) {
	recs, err := e.agents()
	if err != nil {
		return RosterReport{}, err
	}
	agents, err := amq.Agents(e.Paths.Root())
	if err != nil {
		return RosterReport{}, err
	}
	var hs []core.Handle
	known := map[core.Handle]bool{}
	for _, r := range recs {
		known[r.Handle] = true
		p := r.departure()
		if p == nil || !quietDeparture(p.DepartReason) {
			continue
		}
		// One already out of the root (a peer retired from an Epic still
		// open) is looked at again only to forget it.
		if slices.Contains(agents, string(r.Handle)) || mailboxExists(e.Paths.Root(), r.Handle) || e.epicOver(r) {
			hs = append(hs, r.Handle)
		}
	}
	for _, a := range agents {
		if h := core.Handle(a); !known[h] && core.LooksLikeHandle(a) {
			hs = append(hs, h)
		}
	}
	return e.retireRoster(ctx, hs, e.epicOver)
}

// mailboxExists reports whether h has a mailbox directory under root.
func mailboxExists(root string, h core.Handle) bool {
	_, err := os.Stat(filepath.Join(root, "agents", string(h)))
	return err == nil
}

// epicOver reports whether rec's departure ends its identity: an Epic closed
// with it, or a retirement from an Epic since closed.
func (e Env) epicOver(rec agent) bool {
	p := rec.departure()
	if p == nil {
		return false
	}
	if p.DepartReason == store.DepartEpicClosed {
		return true
	}
	lc, ok, err := e.Store.EpicLifecycle(rec.SessionID)
	return err == nil && ok && lc.State == store.EpicClosed
}

// sweepIfDue sweeps the roster when no sweep ran for rosterSweepEvery and
// no other process sweeps now; the session watchers call it on each look.
func (e Env) sweepIfDue(ctx context.Context) {
	stamp := filepath.Join(e.Paths.Home, "roster-swept")
	if st, err := os.Stat(stamp); err == nil && e.Now().Sub(st.ModTime()) < rosterSweepEvery {
		return
	}
	once, cancel := context.WithCancel(ctx)
	cancel() // a cancelled context tries the lock once
	unlock, err := fsutil.Flock(once, filepath.Join(e.Paths.Home, "roster-sweep.lock"))
	if err != nil {
		return // another process sweeps
	}
	defer unlock()
	if err := fsutil.WriteAtomic(stamp, []byte(e.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		fmt.Fprintf(Diagnostics, "sweep the roster: %v\n", err)
		return
	}
	if rep, err := e.SweepRoster(ctx); err != nil || len(rep.Pruned) > 0 {
		fmt.Fprintf(Diagnostics, "swept the roster: pruned %d, forgot %d, kept %d: %v\n", len(rep.Pruned), len(rep.Forgotten), len(rep.Kept), err)
	}
}
