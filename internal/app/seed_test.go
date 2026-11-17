package app

import (
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// seed is an agent a test starts from, described the way the scenario reads:
// who it is, where it runs or ran, and how it left. putSeed writes it through
// the store's own operations (PutIdentity, AcquireLease, ReleaseLease), so a
// fixture that breaks an invariant (two handles of one session, two live
// agents in one block) fails the test that planted it.
type seed struct {
	Handle         core.Handle
	Kind           core.Kind
	Server         string
	BlockID        string
	SessionID      string
	WindowID       string
	Generation     int
	PlacedAt       time.Time
	AgentPID       int
	AgentSessionID string
	Role           string
	Task           string
	AdoptedAt      time.Time
	FirstAdoptedAt time.Time
	Cwd            string
	Launched       bool
	Model          string
	Effort         string
	Parent         core.Handle
	Depth          int
	SummonedFor    string
	Unattended     bool
	DepartedAt     *time.Time
	DepartReason   string
	// Adopting: the adoption that leased it has not finished. Departing: its
	// departure still owes notices.
	Adopting  bool
	Departing bool
}

func (s seed) Live() bool { return s.DepartedAt == nil }

func (s seed) identity() store.Identity {
	id := store.Identity{
		Handle: s.Handle, Kind: s.Kind, Role: s.Role, Task: s.Task,
		Parent: s.Parent, Depth: s.Depth, SummonedFor: s.SummonedFor,
		Launched: s.Launched, Model: s.Model, Effort: s.Effort,
		Unattended: s.Unattended, Cwd: s.Cwd, FirstAdoptedAt: s.FirstAdoptedAt,
		Fence: store.Fence{Server: s.Server, SessionID: s.SessionID, Generation: s.Generation, PlacedAt: s.PlacedAt},
	}
	return id.WithSession(s.AgentSessionID)
}

// putSeed writes s as its handle's identity and, unless it departed, lease;
// a departed seed is leased and released, as an agent that ran and left.
func putSeed(st store.Store, s seed) error {
	if err := st.PutIdentity(s.identity()); err != nil {
		return err
	}
	at := s.AdoptedAt
	if at.IsZero() {
		at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	l, err := st.AcquireLease(store.Lease{
		Handle:      s.Handle,
		Place:       store.Place{Server: s.Server, SessionID: s.SessionID, WindowID: s.WindowID, BlockID: s.BlockID},
		Incarnation: store.Incarnation{PID: s.AgentPID},
		AcquiredAt:  s.AdoptedAt,
		Adopting:    s.Adopting,
	}, at)
	if err != nil || s.DepartedAt == nil {
		return err
	}
	_, err = st.ReleaseLease(s.Handle, l.Epoch, store.Departure{Reason: s.DepartReason, At: *s.DepartedAt, Unfinished: s.Departing})
	return err
}

// plant writes s and returns the agent the app reads back for it.
func plant(t testing.TB, e Env, s seed) agent {
	t.Helper()
	if err := putSeed(e.Store, s); err != nil {
		t.Fatal(err)
	}
	a, ok, err := e.agentOf(s.Handle)
	if err != nil || !ok {
		t.Fatalf("read back %s: ok=%v err=%v", s.Handle, ok, err)
	}
	return a
}

// seedOf is a as a seed, for a test that changes a read-back agent and plants
// it again.
func seedOf(a agent) seed {
	s := seed{
		Handle: a.Handle, Kind: a.Kind, Server: a.Server, BlockID: a.BlockID, SessionID: a.SessionID, WindowID: a.WindowID,
		Generation: a.Fence.Generation, PlacedAt: a.Fence.PlacedAt, AgentPID: a.pid(), AgentSessionID: a.Session(),
		Role: a.Role, Task: a.Task, AdoptedAt: a.adoptedAt(), FirstAdoptedAt: a.FirstAdoptedAt, Cwd: a.Cwd,
		Launched: a.Launched, Model: a.Model, Effort: a.Effort, Parent: a.Parent, Depth: a.Depth, SummonedFor: a.SummonedFor,
		Unattended: a.Unattended, DepartedAt: a.departedAt(), DepartReason: a.departReason(),
		Adopting: a.Live() && !a.Adopted(), Departing: a.departure() != nil && a.departure().Unfinished,
	}
	return s
}

// The transitions an agent can be in the middle of (agent.unfinished).
const (
	adopting  = "adoption"
	departing = "departure"
)

// unfinished names the transition a was committed in the middle of: an
// adoption whose lease is still marked, or a departure that owes notices.
func (a agent) unfinished() string {
	switch {
	case a.Live() && !a.Adopted():
		return adopting
	case a.departure() != nil && a.departure().Unfinished:
		return departing
	}
	return ""
}

// unplant removes h from the state a test started from, running or not: its
// lease, then its identity and delivery history.
func unplant(st store.Store, h core.Handle) error {
	if l, live, err := st.LeaseOf(h); err != nil {
		return err
	} else if live {
		if _, err := st.ReleaseLease(h, l.Epoch, store.Departure{Reason: store.DepartSessionEnd, At: time.Now()}); err != nil {
			return err
		}
	}
	return st.ForgetIdentity(h)
}

// asAgent is s as the app would read it back, without a store: for a test
// of a decision that reads an agent and writes nothing.
func (s seed) asAgent() agent {
	id := s.identity()
	if s.DepartedAt != nil {
		id.LastPlacement = &store.Placement{Place: store.Place{Server: s.Server, SessionID: s.SessionID, WindowID: s.WindowID, BlockID: s.BlockID}, AcquiredAt: s.AdoptedAt, DepartReason: s.DepartReason, DepartedAt: *s.DepartedAt, Unfinished: s.Departing}
		return newAgent(id, nil)
	}
	return newAgent(id, &store.Lease{Handle: s.Handle, Place: store.Place{Server: s.Server, SessionID: s.SessionID, WindowID: s.WindowID, BlockID: s.BlockID}, Incarnation: store.Incarnation{PID: s.AgentPID}, AcquiredAt: s.AdoptedAt, Adopting: s.Adopting})
}

// moveThrough moves h's block through sessions in turn (on its server),
// window window in each, as a layout change Rex reports would.
func moveThrough(t testing.TB, e Env, h core.Handle, window string, sessions ...string) agent {
	t.Helper()
	for _, s := range sessions {
		if err := e.Store.Moved(h, s, window, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := e.agentOf(h)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
