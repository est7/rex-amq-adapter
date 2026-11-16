package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// `rex-amq-adapter panel --json` is the human's panel on a Rex session: the
// events stream of the session (Stream) with the session's panel added, its
// label, crew, what waits on each agent, its stored view, and, in a personal
// agent's session, the Epics linked to it. It reads as events does: no lock,
// no mail moved, no receipt, nothing written. It is the human's only (a
// floating pane with no agent in it), never an agent's, so it refuses any
// caller that runs as or under an agent of any role (panelGate) before it
// resolves a scope or asks Rex anything.

// PanelProjection is the panel of a session: the snapshot's panel field, and
// the value of each panel upsert.
type PanelProjection struct {
	// SessionLabel is the Rex session's label (an Epic's name).
	SessionLabel string `json:"session_label"`
	// Crew is the crew the session claimed, if any.
	Crew *PanelCrew `json:"crew,omitempty"`
	// Attention is, for each live agent of the session, narrator or not,
	// what waits on someone in its pane (Env.attention): turn_error@<ts>,
	// waiting_on_the_human@<ts>, stalled@<ts>; [] when nothing does.
	Attention map[core.Handle][]string `json:"attention"`
	// View is the session's stored view with its hand-back and lifecycle as
	// recorded (withStored); before its narrator writes one, the lifecycle
	// or hand-back alone with a no_view gap.
	View *SessionView `json:"view,omitempty"`
	// Cockpit is set only in a session holding a live personal agent: the
	// Epics linked to it ([] when none is).
	Cockpit []PanelEpic `json:"cockpit,omitzero"`
}

// PanelCrew is a session's crew claim, with the lead and members its
// definition declares; those two are left out when the definition does not
// resolve.
type PanelCrew struct {
	store.SessionCrew
	Lead    string   `json:"lead,omitempty"`
	Members []string `json:"members,omitempty"`
}

// PanelEpic is a linked Epic in a personal agent's panel: its cockpit
// summary, its narrator's narrative, how many of its agents are live (the
// narrator and personal agents not counted), and how many attention marks
// and open obligations wait in it.
type PanelEpic struct {
	CockpitEpic
	Narrative       *ViewNarrative `json:"narrative,omitempty"`
	ActiveMembers   int            `json:"active_members"`
	OpenAttention   int            `json:"open_attention"`
	OpenObligations int            `json:"open_obligations"`
}

// panelState is what a panel stream told last.
type panelState struct {
	label string
	// told is the last panel told, as panelKey compares it.
	told []byte
}

// Panel streams the panel of the caller's Rex session, or of session when
// set, to w until ctx ends, for the caller whose environment getenv reads.
func (e Env) Panel(ctx context.Context, w io.Writer, getenv func(string) string, session string, ticks StreamTicks) error {
	if err := e.panelGate(getenv); err != nil {
		return err
	}
	var loc Location
	if session == "" {
		// No harness runs this (panelGate), so the caller is its pane.
		var err error
		if loc, err = here(getenv); err != nil {
			return err
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sc, err := e.ResolveScope(rctx, ScopeSession, loc, session)
	cancel()
	if err != nil {
		return err
	}
	return e.stream(ctx, w, sc, ticks, true)
}

// panelGate refuses, failing closed, every caller but the human's:
//
//  1. a harness session the environment names (sessionVars), adopted or not;
//  2. ME_UNATTENDED=1;
//  3. a REX_BLOCK holding a live agent of any role, personal, orchestrator,
//     and narrator included: the panel runs in a pane of its own;
//  4. this process descending from a live agent of any role, so `env -i`
//     under an agent is refused too.
func (e Env) panelGate(getenv func(string) string) error {
	for _, v := range sessionVars {
		if getenv(v.name) != "" {
			return fmt.Errorf("%w: the panel is the human's, and this command runs in a %s session (%s is set)", ErrRefused, v.kind, v.name)
		}
	}
	if getenv("ME_UNATTENDED") == "1" {
		return fmt.Errorf("%w: the panel is the human's, and this command runs unattended (ME_UNATTENDED=1)", ErrRefused)
	}
	if block := getenv("REX_BLOCK"); block != "" {
		rec, ok, err := e.Store.Record(block)
		if err != nil {
			return err
		}
		if ok && rec.Live() {
			return panelRefused(rec)
		}
	}
	recs, err := e.Store.Records()
	if err != nil {
		return err
	}
	rec, ok, err := e.agentAncestor(recs, func(store.Record) bool { return true })
	if err != nil {
		return fmt.Errorf("tell which agent runs the panel: %w", err)
	}
	if ok {
		return panelRefused(rec)
	}
	return nil
}

func panelRefused(rec store.Record) error {
	role := rec.Role
	if role == "" {
		role = "no role"
	}
	return fmt.Errorf("%w: the panel is the human's, and this command runs as or under %s (%s)", ErrRefused, rec.Handle, role)
}

// panelNow is the panel of the stream's session now; held are its records
// in scope, and s.recs every record (held reread them).
func (s *streamer) panelNow(held []store.Record) (PanelProjection, error) {
	if len(s.v.sessions) > 0 && s.v.sessions[0].described {
		// A session Rex could not describe this time keeps its last label.
		s.panel.label = s.v.sessions[0].label
	}
	p, err := s.e.panelProjection(s.scope, s.panel.label, held, s.recs)
	if err != nil {
		return p, err
	}
	s.panel.told, err = panelKey(p)
	return p, err
}

// panelTick tells the panel when it changed since it was last told. A look
// that fails is reported and tried again at the next tick.
func (s *streamer) panelTick(held []store.Record) error {
	before := s.panel.told
	p, err := s.panelNow(held)
	if err != nil {
		s.panel.told = before
		fmt.Fprintf(Diagnostics, "panel: %v\n", err)
		return nil
	}
	if bytes.Equal(before, s.panel.told) {
		return nil
	}
	return s.emit(entityFrame{envelope: s.next("upsert"), Entity: "panel", Key: s.scope.SessionID, Value: p, Changed: panelChanged(before, s.panel.told)})
}

// panelProjection builds the panel of session sc labeled label: held are
// the records in scope, all every record.
func (e Env) panelProjection(sc Scope, label string, held, all []store.Record) (PanelProjection, error) {
	p := PanelProjection{SessionLabel: label, Attention: map[core.Handle][]string{}}
	var personal *store.Record
	for _, r := range held {
		if !r.Live() {
			continue
		}
		a, err := e.attention(r)
		if err != nil {
			return p, err
		}
		if a == nil {
			a = []string{}
		}
		p.Attention[r.Handle] = a
		if r.Role == core.PersonalRole && personal == nil {
			personal = &r
		}
	}
	var err error
	if p.Crew, err = e.panelCrew(sc.SessionID, all); err != nil {
		return p, err
	}
	if p.View, err = e.panelView(core.SessionKey{Server: sc.Server, SessionID: sc.SessionID}); err != nil {
		return p, err
	}
	if personal != nil {
		if p.Cockpit, err = e.panelCockpit(*personal, all); err != nil {
			return p, err
		}
	}
	return p, nil
}

// panelCrew is session id's crew claim, with its lead and members resolved
// from the crews the claiming agent's directory sees.
func (e Env) panelCrew(id string, all []store.Record) (*PanelCrew, error) {
	c, ok, err := e.Store.SessionCrew(id)
	if err != nil || !ok {
		return nil, err
	}
	out := &PanelCrew{SessionCrew: c}
	if e.Crews == nil {
		return out, nil
	}
	dir := ""
	if i := slices.IndexFunc(all, func(r store.Record) bool { return r.Handle == c.ClaimedBy }); i >= 0 {
		dir = all[i].Cwd
	}
	// A definition that is missing or does not parse leaves the claim
	// alone: the panel shows what was claimed, not why it cannot resolve.
	if def, found, err := e.Crews(dir).Crew(c.Crew); err == nil && found {
		out.Lead, out.Members = def.Lead, def.Members
	}
	return out, nil
}

// panelView is key's view as `view --cockpit --session` reads it
// (cockpitSession), or, for a session with a hand-back but neither a view
// nor a lifecycle, that hand-back alone; nil when it has none of the three.
func (e Env) panelView(key core.SessionKey) (*SessionView, error) {
	b, err := e.cockpitSession(key)
	if errors.Is(err, ErrNoView) {
		return e.handBackView(key)
	}
	if err != nil {
		return nil, err
	}
	var v SessionView
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("decode the view of %s: %w", key.SessionID, err)
	}
	return &v, nil
}

// handBackView is the view of key before its narrator wrote one and with no
// lifecycle recorded: its hand-back alone, built as cockpitSession builds
// the lifecycle's.
func (e Env) handBackView(key core.SessionKey) (*SessionView, error) {
	hb, ok, err := e.Store.HandBack(key.SessionID)
	if err != nil || !ok {
		return nil, err
	}
	return &SessionView{Schema: ViewSchema, Session: ViewSession{Server: key.Server, SessionID: key.SessionID}, HandBack: &hb,
		Members: []ViewMember{}, Obligations: []ViewObligation{}, Activity: []ViewMessage{}, ControlPlane: []ViewMessage{},
		CoverageGaps: []CoverageGap{{Kind: GapNoView, Note: "the narrator has not written the view yet; only the recorded hand-back is shown"}},
		Sources:      []string{"hand-back.json"}}, nil
}

// panelCockpit is the summary of every Epic linked to personal's session,
// sorted by session id, counted from records and stored views: no Epic's
// Rex session is asked.
func (e Env) panelCockpit(personal store.Record, all []store.Record) ([]PanelEpic, error) {
	links, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	out := []PanelEpic{}
	for _, l := range links {
		if l.Personal != personal.Endpoint().Session() {
			continue
		}
		c, err := e.cockpitEpic(l.Epic)
		if err != nil {
			return nil, err
		}
		pe := PanelEpic{CockpitEpic: c}
		if b, ok, err := e.Store.ReadView(l.Epic.SessionID); err != nil {
			return nil, err
		} else if ok {
			var v SessionView
			if err := json.Unmarshal(b, &v); err != nil {
				return nil, fmt.Errorf("decode the view of %s: %w", l.Epic.SessionID, err)
			}
			pe.Narrative, pe.OpenObligations = v.Narrative, len(v.Obligations)
		}
		for _, r := range all {
			if !r.Live() || r.Server != l.Epic.Server || r.SessionID != l.Epic.SessionID {
				continue
			}
			if !core.Stealth(r.Role) {
				pe.ActiveMembers++
			}
			a, err := e.attention(r)
			if err != nil {
				return nil, err
			}
			pe.OpenAttention += len(a)
		}
		out = append(out, pe)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// panelKey is p as compared for a change: the view's as_of cursor and its
// obligations' ages, which move with every narrator refresh and every
// second, are left out.
func panelKey(p PanelProjection) ([]byte, error) {
	if p.View != nil {
		v := *p.View
		v.AsOf = ViewCursor{}
		v.Obligations = slices.Clone(v.Obligations)
		for i := range v.Obligations {
			v.Obligations[i].AgeSeconds = 0
		}
		p.View = &v
	}
	return json.Marshal(p)
}

// panelChanged names the panel's fields that differ between two keys.
func panelChanged(before, after []byte) []string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	var out []string
	for k, v := range b {
		if !bytes.Equal(a[k], v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
