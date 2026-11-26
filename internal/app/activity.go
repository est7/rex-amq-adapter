package app

import (
	"context"
	"fmt"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/screen"
)

// ScreenPolicy controls whether assessActivity captures the terminal screen.
type ScreenPolicy int

const (
	// NoScreen skips VT capture; used when only external status is needed.
	NoScreen ScreenPolicy = iota
	// LazyScreen captures VT only when the external signal matches a passive
	// banner rule (e.g. agy "Approve plan"), keeping polling paths cheap.
	LazyScreen
	// FullScreen always captures VT and inspects the full surface snapshot.
	FullScreen
)

type activityRequest struct {
	Terminal Terminal // optional; defaults to e.Rex
	Block    string   // required when Agent is nil; defaults to Agent.BlockID
	Agent    *agent   // nil before adoption
	Policy   ScreenPolicy
	Ours     string // optional expected doorbell text for Surface.OursInBox
}

type activitySnapshot struct {
	Activity core.AgentActivity
	External core.ExternalSignal
	Turn     *core.TurnLedger
	Surface  *screen.Surface
	VT       string
}

// jobStateError marks a failure reading Claude Code's local supervisor files,
// distinct from a Rex terminal error.
type jobStateError struct{ err error }

func (e jobStateError) Error() string { return e.err.Error() }
func (e jobStateError) Unwrap() error { return e.err }

// assessActivity is the sole production boundary that reads Rex.ProgramStatus,
// Claude background-job supervisor status, and screen.Inspect, and feeds them
// into core.AssessActivity.
func (e Env) assessActivity(ctx context.Context, req activityRequest) (activitySnapshot, error) {
	term := req.Terminal
	if term == nil {
		term = e.Rex
	}
	block := req.Block
	var kind core.Kind
	if req.Agent != nil {
		if block == "" {
			block = req.Agent.BlockID
		}
		kind = req.Agent.Kind
	}

	var sig core.ExternalSignal
	isJob := false
	if req.Agent != nil && (req.Agent.Server == "" || localServer(req.Agent.Server)) {
		var err error
		sig, isJob, err = e.jobStatus(*req.Agent)
		if err != nil {
			return activitySnapshot{}, jobStateError{err: err}
		}
	}
	if !isJob {
		st, ok, err := term.ProgramStatus(ctx, block)
		if err != nil {
			return activitySnapshot{}, err
		}
		if ok {
			sig = core.ExternalSignal{
				Source:       core.SignalRexStatus,
				State:        core.AgentState(st.State),
				Kind:         st.Kind,
				App:          st.App,
				EffectiveApp: st.EffectiveApp,
				Msg:          st.Msg,
				UpdatedAt:    st.UpdatedAt,
			}
		}
	}

	profile := core.ProfileForSignal(kind, sig)
	var turn *core.TurnLedger
	if req.Agent != nil {
		turn = e.readTurn(*req.Agent)
	}
	var (
		surf *screen.Surface
		vt   string
	)
	switch req.Policy {
	case FullScreen:
		var err error
		vt, err = term.CaptureVT(ctx, block)
		if err != nil {
			return activitySnapshot{}, err
		}
		s := screen.Inspect(string(profile.Kind), vt, req.Ours)
		surf = &s
	case LazyScreen:
		needCapture := profile.IsPassiveBanner(sig) || (turn != nil && turn.Phase == core.TurnIdle && !sig.State.AtRest())
		if needCapture {
			if captured, err := term.CaptureVT(ctx, block); err == nil {
				vt = captured
				s := screen.Inspect(string(profile.Kind), vt, req.Ours)
				surf = &s
			}
		}
	}

	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	act := core.AssessActivity(profile, core.ActivityEvidence{
		External: sig,
		Turn:     turn,
		Surface:  surf,
		Now:      now,
	})
	return activitySnapshot{
		Activity: act,
		External: sig,
		Turn:     turn,
		Surface:  surf,
		VT:       vt,
	}, nil
}

// readTurn reads rec's hook turn ledger without locking and discards any
// record whose Incarnation does not match rec's live lease incarnation.
func (e Env) readTurn(rec agent) *core.TurnLedger {
	if rec.lease == nil {
		return nil
	}
	tr, ok, err := e.Store.Turn(rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read turn ledger of %s: %v\n", rec.Handle, err)
		return nil
	}
	if !ok || !tr.Incarnation.Same(rec.lease.Incarnation) {
		return nil
	}
	return &core.TurnLedger{Seq: tr.Seq, Phase: tr.Phase, At: tr.At}
}

// inspectSurface captures block's screen and returns its Surface snapshot
// without querying status (used by post-write cleanup when status reads fail).
func (e Env) inspectSurface(ctx context.Context, term Terminal, kind core.Kind, block, ours string) (screen.Surface, string, error) {
	if term == nil {
		term = e.Rex
	}
	vt, err := term.CaptureVT(ctx, block)
	if err != nil {
		return screen.Surface{}, "", err
	}
	return screen.Inspect(string(kind), vt, ours), vt, nil
}
