package main

import (
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// testAgent is an agent a test starts from: who it is, where it runs or ran.
// putAgent writes it through the store's own operations, so a fixture that
// breaks an invariant fails the test that planted it.
type testAgent struct {
	Handle         core.Handle
	Kind           core.Kind
	Role           string
	Server         string
	BlockID        string
	SessionID      string
	WindowID       string
	AgentPID       int
	AgentSessionID string
	AdoptedAt      time.Time
	FirstAdoptedAt time.Time
	DepartedAt     *time.Time
}

func putAgent(s store.Store, a testAgent) error {
	id := store.Identity{Handle: a.Handle, Kind: a.Kind, Role: a.Role, FirstAdoptedAt: a.FirstAdoptedAt}
	if err := s.PutIdentity(id.WithSession(a.AgentSessionID)); err != nil {
		return err
	}
	l, err := s.AcquireLease(store.Lease{
		Handle:      a.Handle,
		Place:       store.Place{Server: a.Server, SessionID: a.SessionID, WindowID: a.WindowID, BlockID: a.BlockID},
		Incarnation: store.Incarnation{PID: a.AgentPID},
		AcquiredAt:  a.AdoptedAt,
	}, a.AdoptedAt)
	if err != nil || a.DepartedAt == nil {
		return err
	}
	_, err = s.ReleaseLease(a.Handle, l.Epoch, store.Departure{Reason: store.DepartSessionEnd, At: *a.DepartedAt})
	return err
}

// placed is where h runs, or last ran, and whether it runs now.
func placed(s store.Store, h core.Handle) (store.Place, bool, error) {
	if l, live, err := s.LeaseOf(h); err != nil || live {
		return l.Place, live, err
	}
	id, ok, err := s.IdentityOf(h)
	if err != nil || !ok || id.LastPlacement == nil {
		return store.Place{}, false, err
	}
	return id.LastPlacement.Place, false, nil
}
