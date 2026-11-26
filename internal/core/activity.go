package core

import (
	"time"

	"github.com/est7/rex-amq-adapter/internal/screen"
)

// SignalSource names where an external or ledger activity signal originated.
type SignalSource string

const (
	SignalNone       SignalSource = ""
	SignalRexStatus  SignalSource = "rex_status"
	SignalSupervisor SignalSource = "supervisor"
	SignalTurnLedger SignalSource = "turn_ledger"
)

// ExternalSignal is an agent's external status report: either Rex's OSC 7501
// program_status record or Claude Code's background-job supervisor state.
type ExternalSignal struct {
	Source       SignalSource
	State        AgentState
	Kind         string
	App          string
	EffectiveApp string
	Msg          string
	UpdatedAt    time.Time
}

// Present reports whether an external status signal was reported.
func (s ExternalSignal) Present() bool {
	return s.Source != SignalNone && s.State != StateAbsent
}

// ReportsStatus reports whether the signal comes from Rex's OSC 7501 status
// (which flips on terminal writes for confirmation polling; Claude Code's
// supervisor file does not).
func (s ExternalSignal) ReportsStatus() bool {
	return s.Source == SignalRexStatus && s.State != StateAbsent
}

// PassiveBannerRule matches an OSC 7501 blocked status that is a passive UI
// banner whenever the agent's input box is on screen (empty or with a user
// draft) and no trust dialog is shown.
type PassiveBannerRule struct {
	App  string // matches ExternalSignal.App or EffectiveApp before adoption
	Kind string // matches ExternalSignal.Kind
	Msg  string // matches ExternalSignal.Msg
}

// HarnessProfile declares how one harness reports activity and turn boundaries.
type HarnessProfile struct {
	Kind Kind
	// OSCWorkingIsTurn reports whether OSC 7501 state=working implies an active
	// foreground turn even after a turn-closing hook recorded idle. True for
	// Claude and Codex; false for Antigravity (agy), whose OSC 7501 holds
	// working while non-daemon background commands or schedule timers remain
	// active after Stop (antigravity-cli#1915).
	OSCWorkingIsTurn bool
	// MaxQuiet is how long a ledger-backed in-turn state may stay quiet before
	// degrading to Unknown / entering watchdog visibility.
	MaxQuiet time.Duration
	// HasSelfDenyingCountdown reports whether the harness draws an unattended
	// permission dialog that self-denies when its timer expires (Claude Code).
	HasSelfDenyingCountdown bool
	// PassiveBanners lists OSC blocked statuses that are passive banners while
	// the input box is on screen.
	PassiveBanners []PassiveBannerRule
	// TurnOpenHooks lists hook events that open or confirm an active turn.
	TurnOpenHooks []string
	// TurnCloseHooks lists hook events that close a turn when not continuing.
	TurnCloseHooks []string
}

var harnessProfiles = map[Kind]HarnessProfile{
	Claude: {
		Kind:                    Claude,
		OSCWorkingIsTurn:        true,
		MaxQuiet:                StalledFor,
		HasSelfDenyingCountdown: true,
		TurnOpenHooks:           []string{"user-prompt-submit", "post-tool-use"},
		TurnCloseHooks:          []string{"stop", "stop-failure"},
	},
	Codex: {
		Kind:             Codex,
		OSCWorkingIsTurn: true,
		MaxQuiet:         StalledFor,
		TurnOpenHooks:    []string{"user-prompt-submit", "post-tool-use"},
		TurnCloseHooks:   []string{"stop", "stop-failure"},
	},
	Agy: {
		Kind:             Agy,
		OSCWorkingIsTurn: false,
		MaxQuiet:         StalledFor,
		PassiveBanners: []PassiveBannerRule{
			// agy reports "Approve plan" as a blocked permission at every turn
			// end after writing an artifact under brain/<conversation>/, while
			// its keys only go to the review when the full-screen /artifact
			// viewer is open (no ruled > box on screen).
			{App: "antigravity", Kind: "permission", Msg: "Approve plan"},
		},
		TurnOpenHooks:  []string{"user-prompt-submit", "pre-invocation", "post-tool-use"},
		TurnCloseHooks: []string{"stop", "stop-failure"},
	},
	Pi: {
		Kind:             Pi,
		OSCWorkingIsTurn: true,
		MaxQuiet:         StalledFor,
		TurnOpenHooks:    []string{"user-prompt-submit"},
		TurnCloseHooks:   []string{"settle", "stop-failure"},
	},
}

var defaultHarnessProfile = HarnessProfile{
	OSCWorkingIsTurn: true,
	MaxQuiet:         StalledFor,
}

// ProfileFor returns the activity profile for kind, or a conservative default
// when kind is empty or unknown.
func ProfileFor(kind Kind) HarnessProfile {
	if p, ok := harnessProfiles[kind]; ok {
		return p
	}
	return defaultHarnessProfile
}

// ProfileForSignal returns kind's profile, or infers a matching profile from
// sig's App / EffectiveApp when kind is empty (e.g., before adoption).
func ProfileForSignal(kind Kind, sig ExternalSignal) HarnessProfile {
	if kind != "" {
		return ProfileFor(kind)
	}
	for _, p := range harnessProfiles {
		for _, rule := range p.PassiveBanners {
			if rule.App != "" && (sig.App == rule.App || sig.EffectiveApp == rule.App) {
				return p
			}
		}
	}
	return defaultHarnessProfile
}

// IsPassiveBanner reports whether sig matches a passive banner rule of p.
func (p HarnessProfile) IsPassiveBanner(sig ExternalSignal) bool {
	if sig.State != StateBlocked {
		return false
	}
	for _, r := range p.PassiveBanners {
		if r.Kind == sig.Kind && r.Msg == sig.Msg {
			return true
		}
	}
	return false
}

// AgentActivity is the orthogonal verdict of what an agent is doing right now.
// It is a closed sum type: InTurn, AtPrompt, AwaitingUser, or Unknown.
type AgentActivity interface {
	agentActivity()
}

// InTurn means the agent is actively executing a turn.
type InTurn struct {
	Since  time.Time
	Backed bool
	Source SignalSource
}

func (InTurn) agentActivity() {}

// AtPrompt means the agent is at rest at its prompt.
type AtPrompt struct {
	Box      screen.BoxState
	Source   SignalSource
	RawState AgentState // "idle" | "done" | "error"; empty projects as "idle"
}

func (AtPrompt) agentActivity() {}

// AwaitingUser means the agent waits on the user (a permission/question modal
// or a trust/hook-review screen).
type AwaitingUser struct {
	What        string
	Kind        string // OSC blocked kind, or "trust_screen" when screen-only
	TrustPhrase string // non-empty when a trust/hook-review dialog is on screen
	SelfDenying bool   // Claude Code unattended countdown dialog that self-denies
	Source      SignalSource
}

func (AwaitingUser) agentActivity() {}

// Unknown means activity could not be determined as in-turn, at-prompt, or
// awaiting-user (e.g., no external signal, unrecognized OSC state, or quiet
// beyond MaxQuiet).
type Unknown struct {
	Why          string
	Unrecognized AgentState
	Box          screen.BoxState
	Source       SignalSource
}

func (Unknown) agentActivity() {}

// TurnPhase is whether an agent's hooks last reported a turn open or settled.
type TurnPhase string

const (
	TurnWorking TurnPhase = "working"
	TurnIdle    TurnPhase = "idle"
)

// TurnLedger is the incarnation-verified hook turn ledger snapshot for one agent.
type TurnLedger struct {
	Seq   uint64
	Phase TurnPhase
	At    time.Time
}

// ActivityEvidence is everything observed about an agent at one moment.
type ActivityEvidence struct {
	External ExternalSignal
	Turn     *TurnLedger     // nil when absent or discarded on incarnation mismatch
	Surface  *screen.Surface // nil when the screen was not captured
	Now      time.Time
}

// AssessActivity evaluates ev under profile and returns one orthogonal
// AgentActivity verdict.
func AssessActivity(profile HarnessProfile, ev ActivityEvidence) AgentActivity {
	box := screen.BoxMissing
	trustPhrase := ""
	selfDenying := false
	if ev.Surface != nil {
		box = ev.Surface.Box
		trustPhrase = ev.Surface.TrustPhrase
		selfDenying = profile.HasSelfDenyingCountdown && ev.Surface.SelfDenyingCountdown
	}

	// 1. Claude job supervisor is authoritative when present.
	if ev.External.Source == SignalSupervisor {
		if ev.External.State == StateWorking {
			return InTurn{Since: ev.External.UpdatedAt, Backed: true, Source: SignalSupervisor}
		}
		if trustPhrase != "" {
			return AwaitingUser{
				What:        "a trust screen is up: " + trustPhrase,
				Kind:        "trust_screen",
				TrustPhrase: trustPhrase,
				Source:      SignalSupervisor,
			}
		}
		return AtPrompt{Box: box, Source: SignalSupervisor, RawState: ev.External.State}
	}

	// 2. Modal OSC blocked outranks working and the turn ledger, except for a
	// passive banner over an on-screen input box (empty or user draft) with no
	// trust dialog, which normalizes to an at-rest external signal.
	ext := ev.External
	if ext.State == StateBlocked {
		if profile.IsPassiveBanner(ext) && ev.Surface != nil && trustPhrase == "" && (box == screen.BoxEmpty || box == screen.BoxDraft) {
			ext.State = StateIdle
			ext.Kind = ""
			ext.Msg = ""
		} else {
			what := "its status reports blocked"
			if ext.Kind != "" {
				what += " (" + ext.Kind + ")"
			}
			return AwaitingUser{
				What:        what,
				Kind:        ext.Kind,
				TrustPhrase: trustPhrase,
				SelfDenying: selfDenying,
				Source:      ext.Source,
			}
		}
	}

	// 3. A trust dialog on screen outranks working, idle, and absent status.
	// screen.Inspect suppresses TrustPhrase whenever Box == BoxEmpty, so quoted
	// transcript text above an empty prompt box never triggers this.
	if trustPhrase != "" {
		return AwaitingUser{
			What:        "a trust screen is up: " + trustPhrase,
			Kind:        "trust_screen",
			TrustPhrase: trustPhrase,
			Source:      ext.Source,
		}
	}

	// 4. Hook turn ledger (when present for the live lease's incarnation).
	if ev.Turn != nil {
		switch ev.Turn.Phase {
		case TurnWorking:
			// An at-rest external signal strictly newer than Turn.At heals a
			// turn that ended without a Stop hook (e.g. Esc cancel).
			if ext.Present() && ext.State.AtRest() && !ext.UpdatedAt.IsZero() && ext.UpdatedAt.After(ev.Turn.At) {
				return AtPrompt{Box: box, Source: ext.Source, RawState: ext.State}
			}
			// A ledger-backed turn that stays quiet past Profile.MaxQuiet
			// degrades to Unknown so watchdog visibility is never silenced
			// indefinitely when a Stop hook was missed.
			if profile.MaxQuiet > 0 && !ev.Now.IsZero() && !ev.Turn.At.IsZero() && ev.Now.Sub(ev.Turn.At) >= profile.MaxQuiet {
				return Unknown{Why: "stale_quiet", Box: box, Source: SignalTurnLedger}
			}
			return InTurn{Since: ev.Turn.At, Backed: true, Source: SignalTurnLedger}
		case TurnIdle:
			// For harnesses whose OSC working implies an active foreground turn
			// (Claude/Codex, never agy), a strictly newer working signal overrides
			// idle before the next hook writes working.
			if profile.OSCWorkingIsTurn && ext.Present() && ext.State == StateWorking && !ext.UpdatedAt.IsZero() && ext.UpdatedAt.After(ev.Turn.At) {
				return InTurn{Since: ext.UpdatedAt, Backed: false, Source: ext.Source}
			}
			raw := StateIdle
			if ext.Present() && ext.State.AtRest() {
				raw = ext.State
			}
			return AtPrompt{Box: box, Source: SignalTurnLedger, RawState: raw}
		}
	}

	// 5. Nil ledger: fall back to ExternalSignal with Backed=false.
	switch ext.State {
	case StateWorking:
		return InTurn{Since: ext.UpdatedAt, Backed: false, Source: ext.Source}
	case StateIdle, StateDone, StateError:
		return AtPrompt{Box: box, Source: ext.Source, RawState: ext.State}
	case StateAbsent:
		return Unknown{Why: "no_signal", Box: box, Source: ext.Source}
	default:
		return Unknown{
			Why:          "unknown_state:" + string(ext.State),
			Unrecognized: ext.State,
			Box:          box,
			Source:       ext.Source,
		}
	}
}

// ActivityObservationFrom projects an assessed AgentActivity onto the
// ActivityObservation consumed by Project (agent list and stream events),
// preserving backwards-compatible wire activity.state values.
func ActivityObservationFrom(act AgentActivity, at time.Time) *ActivityObservation {
	src := func(s SignalSource) string {
		if s == SignalNone {
			return string(SignalRexStatus)
		}
		return string(s)
	}
	switch a := act.(type) {
	case InTurn:
		return &ActivityObservation{State: StateWorking, Source: src(a.Source), At: at}
	case AtPrompt:
		st := a.RawState
		if st == StateAbsent {
			st = StateIdle
		}
		return &ActivityObservation{State: st, Source: src(a.Source), At: at}
	case AwaitingUser:
		return &ActivityObservation{State: StateBlocked, Kind: a.Kind, Source: src(a.Source), At: at}
	case Unknown:
		st := StateAbsent
		if a.Unrecognized != "" {
			st = a.Unrecognized
		}
		return &ActivityObservation{State: st, Source: src(a.Source), At: at}
	default:
		return &ActivityObservation{State: StateAbsent, Source: string(SignalRexStatus), At: at}
	}
}
