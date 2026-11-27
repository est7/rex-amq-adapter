package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A handle whose agent will never be resumed loses its mailbox, so a mailbox
// directory exists only for agents that may still run (internal/amq/roster.go).
// Its roster entry in meta/config.json stays: amq owns that file, `amq init
// --add` never reads the entries it was not asked about, and a handle with
// no identity can be mailed by no one (core.CanExchange). Unresumable is a
// terminal departure: an Epic closed (`epic revive` never revives a closed
// Epic) or a peer retired as finished. Each leaves on its event (Epic close,
// agent retire) and a sweep (agent collect, and the session watchers every
// rosterSweepEvery) takes whatever is left, the mailboxes of identities
// collection forgot included. A personal agent's or a free session's
// departure stays reserved for a resume (ReserveFor), and its mailbox with
// it, until collection forgets it; the sweep then takes the mailbox.

// rosterSweepEvery is how often a session watcher sweeps the roster.
const rosterSweepEvery = 10 * time.Minute

// RosterReport is what a roster retirement did.
type RosterReport struct {
	// Pruned had their mailboxes removed by this retirement.
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

// retireRoster takes hs out of the amq root one handle at a time: under its
// handle's lock (which every mailbox maker holds), it checks that the handle
// still may leave (unresumable, departure finished, not reserved) and
// removes its mailbox through amq (which keeps one holding mail); once the
// mailbox is gone it forgets, under the lifecycle lock, the identity forget
// selects, if the record is still the one read. A handle with no identity
// (forgotten already) leaves unless a launch reserved it.
func (e Env) retireRoster(ctx context.Context, hs []core.Handle, forget func(agent) bool) (RosterReport, error) {
	var rep RosterReport
	hs = slices.Clone(hs)
	slices.Sort(hs)
	hs = slices.Compact(hs)
	var errs []error
	for _, h := range hs {
		if err := e.retireHandle(ctx, h, forget, &rep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
		}
	}
	slices.Sort(rep.Forgotten)
	return rep, errors.Join(errs...)
}

func (e Env) retireHandle(ctx context.Context, h core.Handle, forget func(agent) bool, rep *RosterReport) error {
	unlock, err := e.Store.LockHandle(ctx, h)
	if err != nil {
		return err
	}
	defer unlock()
	reserved, err := e.reservedNames()
	if err != nil {
		return err
	}
	rec, ok, err := e.agentOf(h)
	if err != nil {
		return err
	}
	if why := mayLeave(rec, ok, reserved[h]); why != "" {
		rep.keep(h, why)
		return nil
	}
	r, err := e.AMQ.RemoveMailbox(ctx, e.Paths.Root(), string(h))
	if err != nil {
		return err
	}
	if !r.Gone() {
		rep.keep(h, r.Refusal)
		return nil
	}
	if r.Removed {
		rep.Pruned = append(rep.Pruned, h)
	}
	if !ok {
		return nil
	}
	gone, err := e.forgetRetired(ctx, map[core.Handle]agent{h: rec}, forget)
	rep.Forgotten = append(rep.Forgotten, gone...)
	return err
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
// pulse until then), and mailboxes of handles with no identity that the
// adapter named (core.LooksLikeHandle). Mailboxes of other names are left.
func (e Env) SweepRoster(ctx context.Context) (RosterReport, error) {
	recs, err := e.agents()
	if err != nil {
		return RosterReport{}, err
	}
	boxes, err := mailboxDirs(e.Paths.Root())
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
		// One whose mailbox is gone already (a peer retired from an Epic
		// still open) is looked at again only to forget it.
		if mailboxExists(e.Paths.Root(), r.Handle) || e.epicOver(r) {
			hs = append(hs, r.Handle)
		}
	}
	for _, b := range boxes {
		if h := core.Handle(b); !known[h] && core.LooksLikeHandle(b) {
			hs = append(hs, h)
		}
	}
	return e.retireRoster(ctx, hs, e.epicOver)
}

// mailboxDirs lists the names of the mailbox directories under root
// (agents/<handle>), whatever meta/config.json lists: a mailbox amq made for
// a handle the roster does not name holds mail all the same. An
// uninitialised root has none; a symlink is not a mailbox.
func mailboxDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "agents"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out, nil
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
