package core

import (
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/screen"
)

func TestHarnessProfilesDeclareExpectedInvariants(t *testing.T) {
	for _, k := range []Kind{Claude, Codex, Agy, Pi} {
		p := ProfileFor(k)
		if p.Kind != k {
			t.Errorf("ProfileFor(%q).Kind = %q", k, p.Kind)
		}
		if p.MaxQuiet <= 0 {
			t.Errorf("ProfileFor(%q).MaxQuiet = %v, want > 0", k, p.MaxQuiet)
		}
		if len(p.TurnOpenHooks) == 0 || len(p.TurnCloseHooks) == 0 {
			t.Errorf("ProfileFor(%q) missing hook mappings: open=%v close=%v", k, p.TurnOpenHooks, p.TurnCloseHooks)
		}
	}
	if ProfileFor(Agy).OSCWorkingIsTurn {
		t.Error("Agy.OSCWorkingIsTurn must be false (antigravity-cli#1915)")
	}
	for _, k := range []Kind{Claude, Codex, Pi} {
		if !ProfileFor(k).OSCWorkingIsTurn {
			t.Errorf("%s.OSCWorkingIsTurn must be true", k)
		}
	}
	if !ProfileFor(Claude).HasSelfDenyingCountdown {
		t.Error("Claude must enable HasSelfDenyingCountdown")
	}
	if ProfileFor(Codex).HasSelfDenyingCountdown || ProfileFor(Agy).HasSelfDenyingCountdown {
		t.Error("non-Claude harnesses must not enable HasSelfDenyingCountdown")
	}
}

func TestProfileForSignalInfersAgyBeforeAdoption(t *testing.T) {
	sig := ExternalSignal{Source: SignalRexStatus, State: StateBlocked, Kind: "permission", App: "antigravity", Msg: "Approve plan"}
	if p := ProfileForSignal("", sig); p.Kind != Agy || !p.IsPassiveBanner(sig) {
		t.Fatalf("App=antigravity: got %+v", p)
	}
	sigEff := ExternalSignal{Source: SignalRexStatus, State: StateBlocked, Kind: "permission", EffectiveApp: "antigravity", Msg: "Approve plan"}
	if p := ProfileForSignal("", sigEff); p.Kind != Agy || !p.IsPassiveBanner(sigEff) {
		t.Fatalf("EffectiveApp=antigravity: got %+v", p)
	}
	sigOther := ExternalSignal{Source: SignalRexStatus, State: StateBlocked, Kind: "permission", Msg: "Approve plan"}
	if p := ProfileForSignal("", sigOther); p.IsPassiveBanner(sigOther) {
		t.Fatalf("unadopted without antigravity app must not match passive banner: %+v", p)
	}
	if p := ProfileForSignal(Claude, sig); p.Kind != Claude || p.IsPassiveBanner(sig) {
		t.Fatalf("explicit Claude kind must not treat Approve plan as passive: %+v", p)
	}
}

func TestAssessActivity(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	approvePlan := ExternalSignal{
		Source: SignalRexStatus, State: StateBlocked, Kind: "permission",
		App: "antigravity", EffectiveApp: "antigravity", Msg: "Approve plan",
	}

	t.Run("agy passive Approve plan resolves to AtPrompt for empty and draft boxes", func(t *testing.T) {
		for _, box := range []screen.BoxState{screen.BoxEmpty, screen.BoxDraft} {
			got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
				External: approvePlan,
				Surface:  &screen.Surface{Box: box},
			})
			ap, ok := got.(AtPrompt)
			if !ok || ap.Box != box || ap.RawState != StateIdle {
				t.Errorf("box=%s: got %#v, want AtPrompt{Box:%s, RawState:idle}", box, got, box)
			}
		}
	})

	t.Run("agy Approve plan stays AwaitingUser when box missing, trust up, or screen uncaptured", func(t *testing.T) {
		for name, surf := range map[string]*screen.Surface{
			"artifact viewer (missing box)": {Box: screen.BoxMissing},
			"trust dialog":                  {Box: screen.BoxMissing, TrustPhrase: "Trust this folder?"},
			"no screen captured":            nil,
		} {
			got := AssessActivity(ProfileFor(Agy), ActivityEvidence{External: approvePlan, Surface: surf})
			au, ok := got.(AwaitingUser)
			if !ok || au.Kind != "permission" {
				t.Errorf("%s: got %#v, want AwaitingUser(permission)", name, got)
			}
		}
	})

	t.Run("self-denying countdown honored only for Claude", func(t *testing.T) {
		sig := ExternalSignal{Source: SignalRexStatus, State: StateBlocked, Kind: "permission"}
		surf := &screen.Surface{Box: screen.BoxMissing, SelfDenyingCountdown: true}
		cl := AssessActivity(ProfileFor(Claude), ActivityEvidence{External: sig, Surface: surf})
		if au, ok := cl.(AwaitingUser); !ok || !au.SelfDenying {
			t.Fatalf("claude: got %#v, want AwaitingUser{SelfDenying:true}", cl)
		}
		cx := AssessActivity(ProfileFor(Codex), ActivityEvidence{External: sig, Surface: surf})
		if au, ok := cx.(AwaitingUser); !ok || au.SelfDenying {
			t.Fatalf("codex: got %#v, want AwaitingUser{SelfDenying:false}", cx)
		}
	})

	t.Run("supervisor signal is backed and authoritative", func(t *testing.T) {
		working := AssessActivity(ProfileFor(Claude), ActivityEvidence{
			External: ExternalSignal{Source: SignalSupervisor, State: StateWorking, UpdatedAt: t0},
		})
		if it, ok := working.(InTurn); !ok || !it.Backed || it.Source != SignalSupervisor {
			t.Fatalf("supervisor working: got %#v", working)
		}
		idle := AssessActivity(ProfileFor(Claude), ActivityEvidence{
			External: ExternalSignal{Source: SignalSupervisor, State: StateIdle},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		})
		if ap, ok := idle.(AtPrompt); !ok || ap.Box != screen.BoxEmpty || ap.Source != SignalSupervisor {
			t.Fatalf("supervisor idle: got %#v", idle)
		}
	})

	t.Run("trust dialog outranks working and absent status", func(t *testing.T) {
		for _, st := range []AgentState{StateWorking, StateIdle, StateAbsent} {
			sig := ExternalSignal{State: st}
			if st != StateAbsent {
				sig.Source = SignalRexStatus
			}
			got := AssessActivity(ProfileFor(Codex), ActivityEvidence{
				External: sig,
				Surface:  &screen.Surface{Box: screen.BoxMissing, TrustPhrase: HooksReviewPhrase},
			})
			au, ok := got.(AwaitingUser)
			if !ok || au.TrustPhrase != HooksReviewPhrase || au.Kind != "trust_screen" {
				t.Errorf("state=%q: got %#v, want AwaitingUser(trust_screen)", st, got)
			}
		}
	})

	t.Run("absent and unrecognized states yield Unknown", func(t *testing.T) {
		absent := AssessActivity(ProfileFor(Codex), ActivityEvidence{
			Surface: &screen.Surface{Box: screen.BoxEmpty},
		})
		if u, ok := absent.(Unknown); !ok || u.Why != "no_signal" || u.Box != screen.BoxEmpty {
			t.Fatalf("absent: got %#v", absent)
		}
		weird := AssessActivity(ProfileFor(Codex), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: "thinking"},
		})
		if u, ok := weird.(Unknown); !ok || u.Unrecognized != "thinking" {
			t.Fatalf("unrecognized: got %#v", weird)
		}
	})

	t.Run("ActivityObservationFrom keeps wire states backwards-compatible", func(t *testing.T) {
		for _, tc := range []struct {
			act      AgentActivity
			wantSt   AgentState
			wantKind string
			wantSrc  string
		}{
			{InTurn{Source: SignalRexStatus}, StateWorking, "", "rex_status"},
			{InTurn{Backed: true, Source: SignalTurnLedger}, StateWorking, "", "turn_ledger"},
			{InTurn{Backed: true, Source: SignalSupervisor}, StateWorking, "", "supervisor"},
			{AtPrompt{Source: SignalRexStatus, RawState: StateDone}, StateDone, "", "rex_status"},
			{AtPrompt{Source: SignalSupervisor, RawState: StateIdle}, StateIdle, "", "supervisor"},
			{AtPrompt{Source: SignalTurnLedger, RawState: StateIdle}, StateIdle, "", "turn_ledger"},
			{AwaitingUser{Kind: "permission", Source: SignalRexStatus}, StateBlocked, "permission", "rex_status"},
			{Unknown{Why: "no_signal"}, StateAbsent, "", "rex_status"},
			{Unknown{Why: "unknown_state:thinking", Unrecognized: "thinking", Source: SignalRexStatus}, "thinking", "", "rex_status"},
		} {
			obs := ActivityObservationFrom(tc.act, t0)
			if obs.State != tc.wantSt || obs.Kind != tc.wantKind || obs.Source != tc.wantSrc {
				t.Errorf("%#v -> %+v, want state=%q kind=%q src=%q", tc.act, obs, tc.wantSt, tc.wantKind, tc.wantSrc)
			}
		}
	})

	t.Run("TurnLedger precedence rules", func(t *testing.T) {
		t1 := t0.Add(10 * time.Second)
		t2 := t0.Add(20 * time.Second)

		// Rule 1: supervisor and modal blocked / trust outrank TurnWorking and TurnIdle.
		if got := AssessActivity(ProfileFor(Claude), ActivityEvidence{
			External: ExternalSignal{Source: SignalSupervisor, State: StateWorking, UpdatedAt: t1},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnIdle, At: t2},
		}); func() bool { it, ok := got.(InTurn); return !ok || !it.Backed || it.Source != SignalSupervisor }() {
			t.Errorf("supervisor must outrank TurnIdle: got %#v", got)
		}
		if got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateBlocked, Kind: "permission", Msg: "Approve bash"},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { _, ok := got.(AwaitingUser); return !ok }() {
			t.Errorf("modal blocked must outrank TurnWorking: got %#v", got)
		}
		if got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t0},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxMissing, TrustPhrase: "Trust this folder?"},
		}); func() bool { _, ok := got.(AwaitingUser); return !ok }() {
			t.Errorf("trust screen must outrank TurnWorking: got %#v", got)
		}

		// Rule 2: TurnWorking -> InTurn{Backed:true}, healed to AtPrompt only by newer at-rest OSC.
		if got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateIdle, UpdatedAt: t0},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { it, ok := got.(InTurn); return !ok || !it.Backed || it.Source != SignalTurnLedger }() {
			t.Errorf("older OSC idle must not heal TurnWorking: got %#v", got)
		}
		if got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateIdle, UpdatedAt: t2},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { ap, ok := got.(AtPrompt); return !ok || ap.Box != screen.BoxEmpty }() {
			t.Errorf("newer OSC idle must heal TurnWorking to AtPrompt: got %#v", got)
		}

		// Rule 3: TurnIdle -> AtPrompt for agy even with newer OSC working; for Codex/Claude newer OSC working yields unbacked InTurn.
		if got := AssessActivity(ProfileFor(Agy), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t2},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnIdle, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { ap, ok := got.(AtPrompt); return !ok || ap.Box != screen.BoxEmpty }() {
			t.Errorf("agy TurnIdle must ignore OSC working (OSCWorkingIsTurn=false): got %#v", got)
		}
		if got := AssessActivity(ProfileFor(Codex), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t2},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnIdle, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { it, ok := got.(InTurn); return !ok || it.Backed || it.Source != SignalRexStatus }() {
			t.Errorf("codex TurnIdle with newer OSC working must return unbacked InTurn: got %#v", got)
		}
		if got := AssessActivity(ProfileFor(Codex), ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t0},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnIdle, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
		}); func() bool { ap, ok := got.(AtPrompt); return !ok || ap.Box != screen.BoxEmpty }() {
			t.Errorf("codex TurnIdle with older OSC working must remain AtPrompt: got %#v", got)
		}

		// Rule 2b (MaxQuiet): TurnWorking quiet past Profile.MaxQuiet degrades
		// to Unknown{Why:"stale_quiet"} and projects to backwards-compatible absent.
		agyProf := ProfileFor(Agy)
		if got := AssessActivity(agyProf, ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t0},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
			Now:      t1.Add(agyProf.MaxQuiet - time.Second),
		}); func() bool { it, ok := got.(InTurn); return !ok || !it.Backed }() {
			t.Errorf("TurnWorking before MaxQuiet must stay backed InTurn: got %#v", got)
		}
		stale := AssessActivity(agyProf, ActivityEvidence{
			External: ExternalSignal{Source: SignalRexStatus, State: StateWorking, UpdatedAt: t0},
			Turn:     &TurnLedger{Seq: 1, Phase: TurnWorking, At: t1},
			Surface:  &screen.Surface{Box: screen.BoxEmpty},
			Now:      t1.Add(agyProf.MaxQuiet),
		})
		if u, ok := stale.(Unknown); !ok || u.Why != "stale_quiet" || u.Source != SignalTurnLedger {
			t.Errorf("TurnWorking at MaxQuiet must degrade to Unknown(stale_quiet): got %#v", stale)
		}
		if obs := ActivityObservationFrom(stale, t1.Add(agyProf.MaxQuiet)); obs.State != StateAbsent {
			t.Errorf("stale_quiet wire state=%q, want StateAbsent", obs.State)
		}
	})
}
