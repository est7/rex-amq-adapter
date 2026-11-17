package app

import (
	"fmt"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// agent is one handle as a decision reads it: its Identity, and its Lease
// while its agent runs. Place is where the agent runs (its lease's) or last
// ran (its last placement's). An agent is read, never written back: every
// change goes through the store's operations (PutIdentity, AcquireLease,
// ReleaseLease, Moved), which hold the invariants.
type agent struct {
	store.Identity
	store.Place
	lease *store.Lease
}

func newAgent(id store.Identity, l *store.Lease) agent {
	a := agent{Identity: id, lease: l}
	switch {
	case l != nil:
		a.Place = l.Place
	case id.LastPlacement != nil:
		a.Place = id.LastPlacement.Place
	}
	return a
}

// Live reports whether the agent runs: its handle holds a lease.
func (a agent) Live() bool { return a.lease != nil }

// Adopted reports whether the agent finished adoption: it has its waker and
// was told who it is. A live agent that has not has a mailbox, and no more;
// a handle never placed (its adoption died before its lease) has not either.
func (a agent) Adopted() bool { return a.lease != nil && !a.lease.Adopting }

// pid is the process the agent runs as; 0 when it is not running.
func (a agent) pid() int {
	if a.lease == nil {
		return 0
	}
	return a.lease.Incarnation.PID
}

// adoptedAt is when its current lease, else its last one, was acquired.
func (a agent) adoptedAt() time.Time {
	switch {
	case a.lease != nil:
		return a.lease.AcquiredAt
	case a.LastPlacement != nil:
		return a.LastPlacement.AcquiredAt
	}
	return time.Time{}
}

// departure is how the agent's last lease ended; nil while it runs.
func (a agent) departure() *store.Placement {
	if a.lease != nil {
		return nil
	}
	return a.LastPlacement
}

// departedAt is when the agent left; nil while it runs.
func (a agent) departedAt() *time.Time {
	if p := a.departure(); p != nil {
		at := p.DepartedAt
		return &at
	}
	return nil
}

// departReason is why the agent left; "" while it runs.
func (a agent) departReason() string {
	if p := a.departure(); p != nil {
		return p.DepartReason
	}
	return ""
}

// InitialAdoptedAt is when the handle was first adopted, preserved across
// resumes so tab-ownership order stays monotonic.
func (a agent) InitialAdoptedAt() time.Time {
	if !a.FirstAdoptedAt.IsZero() {
		return a.FirstAdoptedAt
	}
	return a.adoptedAt()
}

// agents lists every handle, sorted by handle, with its lease. A lease
// without an identity is an error: nothing could say whose agent it is.
// Leases are read first: every writer creates an identity before its lease
// and removes a lease before its identity, so a reader that holds no lock
// and races an adoption or a collection never sees a lease alone.
func (e Env) agents() ([]agent, error) {
	leases, err := e.Store.Leases()
	if err != nil {
		return nil, err
	}
	ids, err := e.Store.Identities()
	if err != nil {
		return nil, err
	}
	byHandle := make(map[core.Handle]*store.Lease, len(leases))
	for i := range leases {
		byHandle[leases[i].Handle] = &leases[i]
	}
	out := make([]agent, 0, len(ids))
	for _, id := range ids {
		out = append(out, newAgent(id, byHandle[id.Handle]))
		delete(byHandle, id.Handle)
	}
	for h := range byHandle {
		return nil, fmt.Errorf("the lease of %s has no identity", h)
	}
	return out, nil
}

// agentOf returns handle h; ok is false when it has no identity.
func (e Env) agentOf(h core.Handle) (agent, bool, error) {
	id, ok, err := e.Store.IdentityOf(h)
	if err != nil || !ok {
		return agent{}, false, err
	}
	l, live, err := e.Store.LeaseOf(h)
	if err != nil {
		return agent{}, false, err
	}
	if !live {
		return newAgent(id, nil), true, nil
	}
	return newAgent(id, &l), true, nil
}

// agentAt returns the agent of block on any server: for a caller that knows
// only the block (agentIn).
func (e Env) agentAt(block string) (agent, bool, error) {
	return e.agentIn(Location{Block: block})
}

// agentIn returns the agent of loc's block on loc's server: the one that runs
// there, else the one that left it last and runs nowhere now. ok is false
// when there is none. A block id is one server's (I3); an empty server
// matches any.
func (e Env) agentIn(loc Location) (agent, bool, error) {
	l, live, err := e.Store.LeaseAt(loc.Server, loc.Block)
	if err != nil {
		return agent{}, false, err
	}
	if live {
		id, ok, err := e.Store.IdentityOf(l.Handle)
		if err != nil {
			return agent{}, false, err
		}
		if !ok {
			return agent{}, false, fmt.Errorf("the lease of %s has no identity", l.Handle)
		}
		return newAgent(id, &l), true, nil
	}
	id, ok, err := e.Store.DepartedFrom(loc.Server, loc.Block)
	if err != nil || !ok {
		return agent{}, false, err
	}
	return newAgent(id, nil), true, nil
}

// liveAt returns the agent running in block; ok is false when none is.
func (e Env) liveAt(block string) (agent, bool, error) {
	a, ok, err := e.agentAt(block)
	if err != nil || !ok || !a.Live() {
		return agent{}, false, err
	}
	return a, true, nil
}

// releaseLease ends a's lease for d (I4: only the lease a was read with), and
// returns a as departed.
func (e Env) releaseLease(a agent, d store.Departure) (agent, error) {
	if a.lease == nil {
		return a, nil
	}
	released, err := e.Store.ReleaseLease(a.Handle, a.lease.Epoch, d)
	if err != nil {
		return a, err
	}
	if !released {
		return a, fmt.Errorf("the lease of %s changed before it was released", a.Handle)
	}
	p := store.Placement{Place: a.Place, Epoch: a.lease.Epoch, AcquiredAt: a.lease.AcquiredAt, DepartReason: d.Reason, DepartedAt: d.At.UTC(), Unfinished: d.Unfinished}
	a.LastPlacement, a.lease = &p, nil
	return a, nil
}

// incarnation is pid as a lease records it: with its start time when it
// runs on this machine, so a reused pid is not taken for it. A start time
// that cannot be read is left unknown.
func incarnation(server string, pid int) store.Incarnation {
	inc := store.Incarnation{PID: pid}
	if pid > 0 && localServer(server) {
		if st, err := processStartTime(pid); err == nil {
			inc.StartTime = st
		}
	}
	return inc
}
