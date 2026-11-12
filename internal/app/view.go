package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A session's view is what its narrator knows of it, kept in view.json for
// the linked personal agent to read without mailing anyone. The narrator
// rebuilds it in two tiers, neither of which moves a message, writes a
// receipt, or reads a mailbox as its owner:
//
//  1. Envelopes: the session's agents and their mail headers (what events
//     shows) and the open obligations amq finds without reading a body
//     (amq thread open).
//  2. Bodies, by exact id (amq thread --message-ids): only messages whose
//     send-time provenance, as the adapter recorded it, places sender and
//     every recipient in this session. By default those are the
//     orchestrator's control-plane mail; other mail is read only when the
//     narrator names it (ReadSessionMessages), and what was not read is
//     listed as a coverage gap, never guessed.

// ViewSchema is the version of view.json.
const ViewSchema = 1

// View limits: how many recent messages the view lists, and how much of a
// body it quotes.
const (
	viewMessages = 30
	viewExcerpt  = 1200
)

// SessionView is view.json.
type SessionView struct {
	Schema       int              `json:"schema"`
	AsOf         ViewCursor       `json:"as_of"`
	Session      ViewSession      `json:"session"`
	Narrator     core.Handle      `json:"narrator"`
	Narrative    *ViewNarrative   `json:"narrative,omitempty"`
	Members      []ViewMember     `json:"members"`
	Obligations  []ViewObligation `json:"obligations"`
	Activity     []ViewMessage    `json:"activity"`
	ControlPlane []ViewMessage    `json:"control_plane"`
	CoverageGaps []CoverageGap    `json:"coverage_gaps"`
	Limits       ViewLimits       `json:"limits"`
	Sources      []string         `json:"sources"`
}

// ViewCursor is how current a view is: when it was built, the newest message
// it saw, and the session's pulse then (core.SessionPulse.Digest).
type ViewCursor struct {
	At          time.Time `json:"at"`
	LastMessage string    `json:"last_message,omitempty"`
	Pulse       string    `json:"pulse"`
}

// ViewSession names the session: an Epic is the Rex session's label.
type ViewSession struct {
	Server    string `json:"server"`
	SessionID string `json:"session_id"`
	Epic      string `json:"epic,omitempty"`
}

// ViewNarrative is the narrator's own reading of the session.
type ViewNarrative struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// ViewMember is one agent of the session.
type ViewMember struct {
	Handle       core.Handle `json:"handle"`
	Kind         core.Kind   `json:"kind"`
	Role         string      `json:"role,omitempty"`
	Task         string      `json:"task,omitempty"`
	Live         bool        `json:"live"`
	DepartedAt   *time.Time  `json:"departed_at,omitempty"`
	DepartReason string      `json:"depart_reason,omitempty"`
	Attention    []string    `json:"attention,omitempty"`
}

// ViewObligation is an open request, question, or todo; Ref is its opener.
type ViewObligation struct {
	Ref        string `json:"ref"`
	Thread     string `json:"thread"`
	Kind       string `json:"kind"`
	Subject    string `json:"subject"`
	OwedBy     string `json:"owed_by"`
	OwedTo     string `json:"owed_to"`
	State      string `json:"state"`
	AgeSeconds int64  `json:"age_seconds"`
	// Provenance is "adapter" when the adapter recorded where the opener
	// was sent from, "unverified" when it did not (sent with amq directly).
	Provenance string `json:"provenance"`
}

// ViewMessage is a message the view lists: its envelope, and for one whose
// body was read, the body up to the excerpt limit.
type ViewMessage struct {
	Ref       string    `json:"ref"`
	From      string    `json:"from"`
	To        []string  `json:"to"`
	Thread    string    `json:"thread"`
	Subject   string    `json:"subject"`
	Created   time.Time `json:"created"`
	Kind      string    `json:"kind,omitempty"`
	Priority  string    `json:"priority,omitempty"`
	Body      string    `json:"body,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Coverage gap kinds: what the view could not say, and why.
const (
	GapUnverified   = "unverified_provenance"   // no adapter record of where it was sent from
	GapPeerBody     = "peer_mail_body_not_read" // in the session, not the orchestrator's; read on demand
	GapOtherSession = "other_session"           // sent from or to another session
	GapNotFound     = "not_found"               // amq could not read it
	GapObligations  = "obligations_truncated"   // amq left obligations out
	GapMessages     = "messages_truncated"      // older than the view's window
)

// CoverageGap is a set of messages the view does not cover, by reason.
type CoverageGap struct {
	Kind string   `json:"kind"`
	Refs []string `json:"refs,omitempty"`
	Note string   `json:"note,omitempty"`
}

// ViewLimits are the bounds the view was built under.
type ViewLimits struct {
	RecentMessages          int   `json:"recent_messages"`
	MessagesOmitted         int   `json:"messages_omitted"`
	ExcerptChars            int   `json:"excerpt_chars"`
	ObligationsOmitted      int   `json:"obligations_omitted"`
	ObligationMaxAgeSeconds int64 `json:"obligation_max_age_seconds,omitempty"`
}

// ErrNoView is a read of a session whose narrator has not written its view.
var ErrNoView = errors.New("no view yet: the session's narrator writes it with `rex-amq-adapter view refresh`")

// narratorOf is the live narrator in loc, which alone writes its session's
// view and reads bodies for it.
func (e Env) narratorOf(loc Location) (store.Record, core.SessionKey, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return rec, core.SessionKey{}, err
	}
	if !ok || !rec.Live() || rec.Role != core.NarratorRole || !placed(rec) {
		return rec, core.SessionKey{}, fmt.Errorf("%w: only a session's narrator writes its view or reads its mail", ErrRefused)
	}
	return rec, core.SessionKey{Server: rec.Server, SessionID: rec.SessionID}, nil
}

// viewReader decides which session's view the caller in loc may read: its
// own, for the session's narrator or orchestrator, or for a pane with no
// agent in it (the user's own); a linked Epic's, for a personal agent
// (session names it, or the one Epic it is linked to). Anyone else is
// refused.
func (e Env) viewReader(ctx context.Context, loc Location, session string) (string, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return "", err
	}
	if !ok || !rec.Live() {
		s, err := e.FindSession(ctx, loc.Block)
		if err != nil {
			return "", err
		}
		if session != "" && session != s.id {
			return "", fmt.Errorf("%w: a pane reads the view of its own session only", ErrRefused)
		}
		return s.id, nil
	}
	switch rec.Role {
	case core.NarratorRole, core.OrchestratorRole:
		if session != "" && session != rec.SessionID {
			return "", refused(fmt.Sprintf("%s reading the view of %s", rec.Handle, session), core.Decision{Reason: core.ReasonCrossEpic})
		}
		return rec.SessionID, nil
	case core.PersonalRole:
		links, err := e.Store.Links()
		if err != nil {
			return "", err
		}
		var linked []string
		for _, l := range links {
			if l.Personal == rec.Endpoint().Session() && l.Epic.Server == rec.Server {
				linked = append(linked, l.Epic.SessionID)
			}
		}
		if session == "" && len(linked) == 1 {
			session = linked[0]
		}
		if session == "" {
			return "", fmt.Errorf("%w: name the Epic session (--session); linked: %v", ErrUsage, linked)
		}
		if !slices.Contains(linked, session) {
			return "", refused(fmt.Sprintf("%s reading the view of %s", rec.Handle, session), core.Decision{Reason: core.ReasonNotLinked})
		}
		return session, nil
	}
	return "", fmt.Errorf("%w: the view of a session is its narrator's, its orchestrator's, and a linked personal agent's to read; %s is %q", ErrRefused, rec.Handle, rec.Role)
}

// ReadSessionView returns the view.json the caller in loc may read.
func (e Env) ReadSessionView(ctx context.Context, loc Location, session string) (json.RawMessage, error) {
	id, err := e.viewReader(ctx, loc, session)
	if err != nil {
		return nil, err
	}
	b, ok, err := e.Store.ReadView(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoView
	}
	return b, nil
}

// RefreshSessionView rebuilds the view of the narrator's session in loc and
// writes it whole. summary, when set, replaces the narrator's narrative;
// otherwise the last one is kept.
func (e Env) RefreshSessionView(ctx context.Context, loc Location, summary string) (SessionView, error) {
	narr, key, err := e.narratorOf(loc)
	if err != nil {
		return SessionView{}, err
	}
	v, err := e.buildView(ctx, narr, key)
	if err != nil {
		return v, err
	}
	switch {
	case summary != "":
		v.Narrative = &ViewNarrative{Text: summary, At: v.AsOf.At}
	default:
		if b, ok, err := e.Store.ReadView(key.SessionID); err == nil && ok {
			var prev SessionView
			if json.Unmarshal(b, &prev) == nil {
				v.Narrative = prev.Narrative
			}
		}
	}
	return v, e.Store.PutView(key.SessionID, v)
}

// sessionMembers are the records of key's session other than the stealth
// ones, sorted by handle.
func (e Env) sessionMembers(key core.SessionKey) ([]store.Record, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, err
	}
	var out []store.Record
	for _, r := range recs {
		if r.Server == key.Server && r.SessionID == key.SessionID && !core.Stealth(r.Role) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// inSession reports whether p, a message's send-time provenance, places its
// sender and every recipient in key.
func inSession(p store.Provenance, key core.SessionKey) bool {
	if p.From.Session() != key || len(p.To) == 0 {
		return false
	}
	for _, t := range p.To {
		if t.Session() != key {
			return false
		}
	}
	return true
}

// controlPlane reports whether the orchestrator sent or received p's message.
func controlPlane(p store.Provenance) bool {
	if p.From.Role == core.OrchestratorRole {
		return true
	}
	for _, t := range p.To {
		if t.Role == core.OrchestratorRole {
			return true
		}
	}
	return false
}

// provenanceOf is h's provenance as the adapter recorded it at sending.
func (e Env) provenanceOf(h amq.Header) (store.Provenance, bool, error) {
	var ac adapterContext
	h.ContextOf(store.ProvenanceContextKey, &ac)
	return e.Store.ProvenanceOf(h.ID, ac.Token, core.Handle(h.From), h.To)
}

// gaps collects coverage gaps by kind, in first-seen order.
type gaps struct {
	order []string
	refs  map[string][]string
	notes map[string]string
}

func (g *gaps) add(kind, note string, refs ...string) {
	if g.refs == nil {
		g.refs, g.notes = map[string][]string{}, map[string]string{}
	}
	if _, ok := g.refs[kind]; !ok {
		g.order = append(g.order, kind)
	}
	g.refs[kind] = append(g.refs[kind], refs...)
	if note != "" {
		g.notes[kind] = note
	}
}

func (g *gaps) list() []CoverageGap {
	out := []CoverageGap{}
	for _, k := range g.order {
		out = append(out, CoverageGap{Kind: k, Refs: g.refs[k], Note: g.notes[k]})
	}
	return out
}

func (e Env) buildView(ctx context.Context, narr store.Record, key core.SessionKey) (SessionView, error) {
	v := SessionView{
		Schema: ViewSchema, Session: ViewSession{Server: key.Server, SessionID: key.SessionID}, Narrator: narr.Handle,
		Members: []ViewMember{}, Obligations: []ViewObligation{}, Activity: []ViewMessage{}, ControlPlane: []ViewMessage{},
		Limits:  ViewLimits{RecentMessages: viewMessages, ExcerptChars: viewExcerpt},
		Sources: []string{"adapter records", "mailbox envelopes", "amq thread open", "amq thread --message-ids"},
	}
	v.AsOf.At = e.Now().UTC()
	if rs, err := e.Rex.InspectSession(ctx, key.SessionID); err == nil {
		v.Session.Epic = rs.Label
	} else {
		fmt.Fprintf(Diagnostics, "read the label of %s: %v\n", key.SessionID, err)
	}
	members, err := e.sessionMembers(key)
	if err != nil {
		return v, err
	}
	var handles []core.Handle
	for _, r := range members {
		m := ViewMember{Handle: r.Handle, Kind: r.Kind, Role: r.Role, Task: r.Task, Live: r.Live(), DepartedAt: r.DepartedAt, DepartReason: r.DepartReason}
		if r.Live() {
			if m.Attention, err = e.attention(r); err != nil {
				return v, err
			}
		}
		v.Members = append(v.Members, m)
		handles = append(handles, r.Handle)
	}
	pulse, err := e.sessionPulse(key)
	if err != nil {
		return v, err
	}
	v.AsOf.Pulse = pulse.Digest()
	var g gaps

	// Tier 1: obligations.
	agents := make([]string, len(handles))
	for i, h := range handles {
		agents[i] = string(h)
	}
	open, err := e.AMQ.ThreadOpen(ctx, e.Paths.Root(), string(narr.Handle), agents)
	if err != nil {
		return v, err
	}
	for _, ob := range open.Obligations {
		o := ViewObligation{Ref: ob.Opener.ID, Thread: ob.Thread, Kind: ob.Opener.Kind, Subject: ob.Opener.Subject, OwedBy: ob.OwedBy.Handle, OwedTo: ob.OwedTo.Handle, State: ob.State, AgeSeconds: ob.AgeSeconds, Provenance: "adapter"}
		p, ok, err := e.Store.ProvenanceOf(ob.Opener.ID, "", "", nil)
		switch {
		case err != nil:
			return v, err
		case ok && !inSession(p, key):
			g.add(GapOtherSession, "sent from or to another session", ob.Opener.ID)
			continue
		case !ok:
			o.Provenance = "unverified"
			g.add(GapUnverified, "sent without the adapter, so where it was sent from is not known", ob.Opener.ID)
		}
		v.Obligations = append(v.Obligations, o)
	}
	if n := open.Coverage.TruncatedByLimit.Omitted; n > 0 {
		v.Limits.ObligationsOmitted = n
		g.add(GapObligations, fmt.Sprintf("amq left %d obligations out", n))
	}
	v.Limits.ObligationMaxAgeSeconds = open.Coverage.MaxAgeSeconds

	// Tier 1: envelopes, newest first.
	cache := headerCache{}
	msgs, err := e.observeMessages(handles, members, cache)
	if err != nil {
		return v, err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID > msgs[j].ID })
	if len(msgs) > 0 {
		v.AsOf.LastMessage = msgs[0].ID
	}
	if len(msgs) > viewMessages {
		v.Limits.MessagesOmitted = len(msgs) - viewMessages
		g.add(GapMessages, fmt.Sprintf("%d older messages are not listed", len(msgs)-viewMessages))
		msgs = msgs[:viewMessages]
	}
	var control []amq.Header
	for _, m := range msgs {
		h := cache[m.ID]
		p, ok, err := e.provenanceOf(h)
		if err != nil {
			return v, err
		}
		switch {
		case !ok:
			g.add(GapUnverified, "sent without the adapter, so where it was sent from is not known", m.ID)
		case !inSession(p, key):
			g.add(GapOtherSession, "sent from or to another session", m.ID)
			continue
		case controlPlane(p):
			control = append(control, h)
		default:
			g.add(GapPeerBody, "peer mail that bypassed the orchestrator; read it with `rex-amq-adapter view read --ids`", m.ID)
		}
		v.Activity = append(v.Activity, ViewMessage{Ref: m.ID, From: m.From, To: m.To, Thread: m.Thread, Subject: m.Subject, Created: m.Created, Kind: h.Kind, Priority: h.Priority})
	}

	// Tier 2: the orchestrator's control-plane bodies.
	read, missing := e.readBodies(ctx, narr.Handle, agents, control)
	v.ControlPlane = read
	if len(missing) > 0 {
		g.add(GapNotFound, "amq could not read these", missing...)
	}
	v.CoverageGaps = g.list()
	return v, nil
}

// readBodies reads the bodies of hs by exact id, thread by thread, from the
// copies agents hold, newest first. A thread whose read fails is read id by
// id, so one missing message costs only itself.
func (e Env) readBodies(ctx context.Context, me core.Handle, agents []string, hs []amq.Header) (read []ViewMessage, missing []string) {
	byThread := map[string][]string{}
	var threads []string
	for _, h := range hs {
		if _, ok := byThread[h.Thread]; !ok {
			threads = append(threads, h.Thread)
		}
		byThread[h.Thread] = append(byThread[h.Thread], h.ID)
	}
	for _, th := range threads {
		ids := byThread[th]
		msgs, err := e.AMQ.ThreadRead(ctx, e.Paths.Root(), string(me), th, agents, ids)
		if err != nil {
			msgs = nil
			for _, id := range ids {
				one, err := e.AMQ.ThreadRead(ctx, e.Paths.Root(), string(me), th, agents, []string{id})
				if err != nil {
					missing = append(missing, id)
					continue
				}
				msgs = append(msgs, one...)
			}
		}
		got := map[string]bool{}
		for _, m := range msgs {
			got[m.ID] = true
			read = append(read, excerpt(m))
		}
		for _, id := range ids {
			if !got[id] && !slices.Contains(missing, id) {
				missing = append(missing, id)
			}
		}
	}
	sort.Slice(read, func(i, j int) bool { return read[i].Ref > read[j].Ref })
	return read, missing
}

func excerpt(m amq.ThreadMessage) ViewMessage {
	v := ViewMessage{Ref: m.ID, From: m.From, To: m.To, Thread: m.Thread, Subject: m.Subject, Created: m.Created, Kind: m.Kind, Priority: m.Priority, Body: m.Body}
	if r := []rune(m.Body); len(r) > viewExcerpt {
		v.Body, v.Truncated = string(r[:viewExcerpt]), true
	}
	return v
}

// MessagesRead is what a narrator's read by id returns.
type MessagesRead struct {
	Messages     []ViewMessage `json:"messages"`
	CoverageGaps []CoverageGap `json:"coverage_gaps"`
}

// ReadSessionMessages reads, for the narrator in loc, the bodies of ids: the
// ones whose send-time provenance places sender and every recipient in its
// session. The rest are reported as gaps, unread. Nothing is moved and no
// receipt is written.
func (e Env) ReadSessionMessages(ctx context.Context, loc Location, ids []string) (MessagesRead, error) {
	out := MessagesRead{Messages: []ViewMessage{}}
	narr, key, err := e.narratorOf(loc)
	if err != nil {
		return out, err
	}
	members, err := e.sessionMembers(key)
	if err != nil {
		return out, err
	}
	agents := make([]string, len(members))
	for i, r := range members {
		agents[i] = string(r.Handle)
	}
	var g gaps
	var authorized []amq.Header
	for _, id := range ids {
		// An id names a file: one that could name another directory is
		// nobody's message.
		var h amq.Header
		found := false
		if id != "" && !strings.ContainsAny(id, "/\\") && !strings.HasPrefix(id, ".") {
			h, found = e.findInSession(members, id)
		}
		if !found {
			g.add(GapNotFound, "no agent of this session holds it", id)
			continue
		}
		p, ok, err := e.provenanceOf(h)
		switch {
		case err != nil:
			return out, err
		case !ok:
			g.add(GapUnverified, "sent without the adapter, so it is not read", id)
		case !inSession(p, key):
			g.add(GapOtherSession, "sent from or to another session, so it is not read", id)
		default:
			authorized = append(authorized, h)
		}
	}
	read, missing := e.readBodies(ctx, narr.Handle, agents, authorized)
	out.Messages = read
	if len(missing) > 0 {
		g.add(GapNotFound, "amq could not read these", missing...)
	}
	out.CoverageGaps = g.list()
	return out, nil
}

// findInSession finds message id's header in a mailbox of members: a copy
// received (read or not) or sent.
func (e Env) findInSession(members []store.Record, id string) (amq.Header, bool) {
	for _, r := range members {
		for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained, amq.BoxSent} {
			if h, err := amq.ReadHeader(e.Paths.Root(), string(r.Handle), box, id); err == nil {
				return h, true
			}
		}
	}
	return amq.Header{}, false
}
