package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A launcher (agent summon, epic open, epic revive) knows the handle its
// agent will take, or names it now, and makes the handle's mailbox before the
// agent starts: `amq init --add` runs outside the hook budgets, and the
// agent's first hook finds the handle registered and its mailbox made
// (EnsureMailbox's fast path, which takes no lock and runs no amq). A
// resumed agent's handle is its own; a new agent's is reserved
// (store.Reservation) under a token its launch carries in ReservationEnv,
// which its adoption takes the handle with. A mailbox
// that cannot be made stops the launch (ErrNotProvisioned). A reservation is
// given back only when its launch surely did not start its agent; after an
// ambiguous reply from Rex it stays until it expires, so a pane Rex did start
// still takes its name.

// ReservationEnv is the environment variable that names a launched agent's
// handle reservation.
const ReservationEnv = "REX_AMQ_RESERVATION"

// reserveHandle names a new handle of kind for an agent about to be
// launched, under the lifecycle lock (so no adoption or other launch takes
// the name meanwhile), records it under a fresh token, and then makes its
// mailbox with no lock held. A mailbox that cannot be made fails it, with
// the reservation given back: the launch must not happen, or its agent's
// first hook would run amq init inside its budget.
func (e Env) reserveHandle(ctx context.Context, kind core.Kind) (string, core.Handle, error) {
	token, err := newToken()
	if err != nil {
		return "", "", err
	}
	h, err := e.recordReservation(ctx, token, kind)
	if err != nil {
		return "", "", err
	}
	if err := e.provisionMailbox(ctx, h); err != nil {
		e.dropReservation(token)
		return "", "", err
	}
	return token, h, nil
}

func (e Env) recordReservation(ctx context.Context, token string, kind core.Kind) (core.Handle, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	recs, err := e.agents()
	if err != nil {
		return "", err
	}
	taken, err := e.takenHandles(recs, "")
	if err != nil {
		return "", err
	}
	h := core.NameHandle(kind, taken)
	return h, e.Store.PutReservation(token, store.Reservation{Handle: h, Kind: kind, At: e.Now().UTC()})
}

// ErrNotProvisioned is a launch that did not happen because its agent's
// mailbox could not be made first: retrying it is safe.
var ErrNotProvisioned = errors.New("the agent's mailbox could not be made, so it was not launched; try again")

// provisionMailbox makes h's mailbox before its agent starts, under h's
// lock, so a roster sweep (which removes a mailbox only under that lock)
// never removes it halfway through being made; a sweep that runs after it,
// before the agent's adoption, is repaired by the adoption (provisionHeld).
// The callers hold no lock. An error is ErrNotProvisioned, and the launch
// does not happen.
func (e Env) provisionMailbox(ctx context.Context, h core.Handle) error {
	unlock, err := e.Store.LockHandle(ctx, h)
	if err != nil {
		return fmt.Errorf("%w (%s: %v)", ErrNotProvisioned, h, err)
	}
	defer unlock()
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), string(h)); err != nil {
		return fmt.Errorf("%w (%s: %v)", ErrNotProvisioned, h, err)
	}
	return nil
}

// dropReservation gives back the reservation under token, when its launch
// did not happen.
func (e Env) dropReservation(token string) {
	if err := e.Store.DeleteReservation(token); err != nil {
		fmt.Fprintf(Diagnostics, "drop the reservation %s: %v\n", token, err)
	}
}

// withReservation adds token to cmd's environment as ReservationEnv; no
// token leaves cmd as it is.
func withReservation(token string, cmd []string) []string {
	if token == "" {
		return cmd
	}
	if len(cmd) == 0 || cmd[0] != "env" {
		return append([]string{"env", ReservationEnv + "=" + token}, cmd...)
	}
	return append([]string{"env", ReservationEnv + "=" + token}, cmd[1:]...)
}

// reservedHandle is the handle the reservation under token keeps for an agent
// of kind, when it is still unexpired, of that kind, and not taken by
// anything else (taken leaves this reservation out).
func (e Env) reservedHandle(token string, kind core.Kind, taken map[core.Handle]bool) (core.Handle, bool, error) {
	r, ok, err := e.Store.ReservationOf(token)
	if err != nil || !ok || r.Kind != kind || r.Expired(e.Now()) || taken[r.Handle] {
		return "", false, err
	}
	return r.Handle, true, nil
}
