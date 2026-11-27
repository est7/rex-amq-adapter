package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The cockpit is the Operator's read of a Hub's Epics (`epic list`, `epic
// show`, ResolveOperator): the user's, from outside Rex, a pane with no
// agent, or a Desktop agent no hook adopted; a personal agent's, from its
// own pane; or a top-level standalone agent's. It reads only: no mail
// moves, no receipt is written, nothing is started. Its scope is the Epics
// linked to one Hub (core.LinkMatchesPersonal), never another Epic, and it
// does not depend on which of the Hub's personal agents are live, or how
// many. A caller that is no Operator, however its environment was trimmed,
// is refused by ResolveOperator. An Operator that is no personal agent
// reads the Hub it names (--hub), the Hub of the personal agent it names
// (--personal), or the default Hub.

// CockpitViewRequest is what `epic list` and `epic show` from outside Rex
// ask: Session, one linked Epic's view; without it, a summary of every
// linked Epic. Hub names the Hub directory for a caller that is not a
// personal agent; Personal narrows to the Epics that personal agent opened
// or prefers (and names its Hub when Hub is not given).
type CockpitViewRequest struct {
	Session       string
	Personal      string
	Hub           string
	IncludeClosed bool
	ClosedWithin  time.Duration
}

// DefaultClosedWithin is the lookback window for `epic list --include-closed`
// when --closed-within is omitted.
const DefaultClosedWithin = 24 * time.Hour

// CockpitView is the summary of a Hub's linked Epics. Personal and
// SessionID are the personal agent read for, when there is one: the caller,
// or the one live personal agent the request matched.
type CockpitView struct {
	Schema    int           `json:"schema"`
	Hub       string        `json:"hub,omitempty"`
	Personal  core.Handle   `json:"personal"`
	SessionID string        `json:"session_id"`
	Epics     []CockpitEpic `json:"epics"`
}

// CockpitEpic is one linked (or recently closed, under --include-closed) Epic
// in the summary. HasView says its narrator has written its view (read it with
// --session). Progress is nil (serialized as null, never fabricated 0%) when
// the narrator has not recorded Slice progress. Obligation is what the Epic
// still asks of its Hub, from its lifecycle alone, whatever mail was read:
// hand_back from the lead's hand-back until the Epic is closed.
type CockpitEpic struct {
	SessionID         string               `json:"session_id"`
	Server            string               `json:"server,omitempty"`
	Name              string               `json:"name,omitempty"`
	Dir               string               `json:"dir,omitempty"`
	PreferredPersonal core.Handle          `json:"preferred_personal,omitempty"`
	Obligation        string               `json:"obligation,omitempty"`
	Lifecycle         *store.EpicLifecycle `json:"lifecycle,omitempty"`
	HandBack          *store.HandBack      `json:"hand_back,omitempty"`
	GoalPath          string               `json:"goal_path,omitempty"`
	Narrative         *ViewNarrative       `json:"narrative,omitempty"`
	Progress          *store.EpicProgress  `json:"progress"`
	HasView           bool                 `json:"has_view"`
}

// ObligationHandBack is a hand-back the Hub has not closed its Epic on.
const ObligationHandBack = "hand_back"

// cockpitScope is what a cockpit read covers: the links of a Hub, by its
// members' endpoints and its directory, narrowed to one personal agent's
// Epics when one is named.
type cockpitScope struct {
	members []core.Endpoint // live personal agents of the Hub read
	hubDir  string          // the Hub directory; "" for a personal agent of no Hub
	// strict is set for an authenticated personal caller inside Rex: its
	// own endpoint (members) is the whole grant (core.LinkMatchesPersonal),
	// and no link is read as the Hub's by its directory or by the Hub its
	// personal session has now.
	strict   bool
	personal core.Handle     // --personal: a filter
	named    []core.Endpoint // the endpoint --personal names, for a session-keyed link
	self     *agent          // the personal caller or the one matched
}

// matches reports whether link l is in the scope.
func (c cockpitScope) matches(e Env, l core.Link) bool {
	in := slices.ContainsFunc(c.members, func(m core.Endpoint) bool { return core.LinkMatchesPersonal(l, m) })
	if !in && !c.strict && c.hubDir != "" {
		if l.Hub != "" {
			in = l.Hub.Dir() == c.hubDir
		} else if key, err := e.hubOfSession(l.Personal.SessionID); err == nil && key != "" {
			in = key.Dir() == c.hubDir
		}
	}
	if !in || c.personal == "" {
		return in
	}
	if l.PreferredPersonal != "" {
		return l.PreferredPersonal == c.personal
	}
	return slices.ContainsFunc(c.named, func(m core.Endpoint) bool { return m.Handle == c.personal && m.Session() == l.Personal })
}

// ReadCockpitView returns, for the caller whose environment getenv reads,
// the view of req.Session, an Epic linked to the Hub in scope, or without
// req.Session the summary of every Epic linked to it.
func (e Env) ReadCockpitView(ctx context.Context, getenv func(string) string, req CockpitViewRequest) (json.RawMessage, error) {
	if req.ClosedWithin != 0 && !req.IncludeClosed {
		return nil, fmt.Errorf("%w: --closed-within requires --include-closed", ErrUsage)
	}
	if req.ClosedWithin < 0 {
		return nil, fault.New(fault.InvalidInput, "--closed-within must be positive")
	}
	op, err := e.ResolveOperator(getenv)
	if err != nil {
		return nil, err
	}
	if !op.Personal() && req.Session != "" && req.Hub == "" && req.Personal == "" {
		// An Epic of no Hub (opened with none configured, NoHub) is no
		// Hub's to scope: the Operator names it.
		if key, ok, err := e.unlinkedEpic(req.Session); err != nil || ok {
			if err != nil {
				return nil, err
			}
			return e.cockpitSession(key)
		}
	}
	scope, err := e.cockpitScope(op, req)
	if err != nil {
		return nil, err
	}
	links, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var linked []core.Link
	seen := map[core.SessionKey]bool{}
	for _, l := range links {
		if !seen[l.Epic] && scope.matches(e, l) {
			seen[l.Epic] = true
			linked = append(linked, l)
		}
	}
	if req.Session != "" {
		for _, l := range linked {
			if l.Epic.SessionID == req.Session {
				return e.cockpitSession(l.Epic)
			}
		}
		return nil, refused(fmt.Sprintf("the cockpit of %s reading the view of %s", cockpitWhose(scope), req.Session), core.Decision{Reason: core.ReasonNotLinked})
	}
	out := CockpitView{Schema: ViewSchema, Hub: scope.hubDir, Epics: []CockpitEpic{}}
	if scope.self != nil {
		out.Personal, out.SessionID = scope.self.Handle, scope.self.SessionID
	}
	var cutoff time.Time
	if req.IncludeClosed {
		within := req.ClosedWithin
		if within <= 0 {
			within = DefaultClosedWithin
		}
		now := time.Now().UTC()
		if e.Now != nil {
			now = e.Now().UTC()
		}
		cutoff = now.Add(-within)
	}
	type closedCandidate struct {
		epic     CockpitEpic
		closedAt time.Time
	}
	seenLive := map[string]bool{}
	seenClosed := map[string]bool{}
	var closedMatched []closedCandidate
	for _, l := range linked {
		c, err := e.cockpitEpic(l.Epic)
		if err != nil {
			return nil, err
		}
		c.PreferredPersonal = l.PreferredPersonal
		// A closed Epic that still carries a link from another Hub/session is
		// no longer live: exclude it from default `epic list` and apply the
		// same --include-closed + --closed-within gate while honoring its link.
		if c.Lifecycle != nil && c.Lifecycle.State == store.EpicClosed {
			if req.IncludeClosed && !seenClosed[l.Epic.SessionID] {
				closedAt := closedTimestamp(*c.Lifecycle)
				if !closedAt.IsZero() && !closedAt.Before(cutoff) {
					seenClosed[l.Epic.SessionID] = true
					closedMatched = append(closedMatched, closedCandidate{epic: c, closedAt: closedAt})
				}
			}
			continue
		}
		seenLive[l.Epic.SessionID] = true
		out.Epics = append(out.Epics, c)
	}
	if req.IncludeClosed {
		archived, err := e.archivedClosedEpics(scope, seenLive, seenClosed, cutoff)
		if err != nil {
			return nil, err
		}
		for _, a := range archived {
			closedMatched = append(closedMatched, closedCandidate{epic: a.epic, closedAt: a.closedAt})
		}
		sort.Slice(closedMatched, func(i, j int) bool {
			if !closedMatched[i].closedAt.Equal(closedMatched[j].closedAt) {
				return closedMatched[i].closedAt.After(closedMatched[j].closedAt)
			}
			return closedMatched[i].epic.SessionID < closedMatched[j].epic.SessionID
		})
		for _, m := range closedMatched {
			out.Epics = append(out.Epics, m.epic)
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), err
}

type archivedClosedEpic struct {
	epic     CockpitEpic
	closedAt time.Time
}

func (e Env) archivedClosedEpics(scope cockpitScope, seenLive, seenClosed map[string]bool, cutoff time.Time) ([]archivedClosedEpic, error) {
	lcs, err := e.Store.EpicLifecycles()
	if err != nil {
		return nil, err
	}
	var out []archivedClosedEpic
	for _, lc := range lcs {
		if lc.SessionID == "" || lc.State != store.EpicClosed || seenLive[lc.SessionID] || seenClosed[lc.SessionID] {
			continue
		}
		closedAt := closedTimestamp(lc)
		if closedAt.IsZero() || closedAt.Before(cutoff) {
			continue
		}
		matched, ok := e.closedGrantMatch(scope, lc)
		if !ok {
			continue
		}
		seenClosed[lc.SessionID] = true
		c, err := e.cockpitEpic(matched.Epic)
		if err != nil {
			return nil, err
		}
		c.PreferredPersonal = matched.PreferredPersonal
		out = append(out, archivedClosedEpic{epic: c, closedAt: closedAt})
	}
	return out, nil
}

// closedGrantMatch checks whether scope was authorized for lc when lc closed.
// Only positive grant evidence recorded in lc.ClosedLinks authorizes an
// unlinked closed Epic; pre-upgrade tombstones without ClosedLinks cannot prove
// an opener's grant was still effective at close, so they remain visible only
// through surviving links (handled in ReadCockpitView's linked loop).
func (e Env) closedGrantMatch(scope cockpitScope, lc store.EpicLifecycle) (core.Link, bool) {
	for _, l := range lc.ClosedLinks {
		if scope.matches(e, l) {
			return l, true
		}
	}
	return core.Link{}, false
}

func closedTimestamp(lc store.EpicLifecycle) time.Time {
	if lc.ClosedAt != nil && !lc.ClosedAt.IsZero() {
		return lc.ClosedAt.UTC()
	}
	return lc.UpdatedAt.UTC()
}

func cockpitWhose(s cockpitScope) string {
	switch {
	case s.self != nil:
		return string(s.self.Handle)
	case s.hubDir != "":
		return "the Hub " + s.hubDir
	}
	return "this caller"
}

// cockpitScope is the Hub whose linked Epics op, the Operator
// (ResolveOperator), may read.
//
// A personal agent reads its own Hub, by its own endpoint alone
// (core.LinkMatchesPersonal); --personal only narrows that to the Epics the
// named personal agent opened or prefers, live or not (§4.2(8)), so naming
// one outside the scope reads nothing; a directory it does not work in is
// refused. Any other Operator reads the Hub --hub names, else the Hub of
// the live personal agent --personal names, else the default Hub; with none
// of them, it names one. However many of the Hub's personal agents are
// live, none, one, or several, the scope is the same Hub.
func (e Env) cockpitScope(op Operator, req CockpitViewRequest) (cockpitScope, error) {
	recs, err := e.agents()
	if err != nil {
		return cockpitScope{}, err
	}
	if op.Personal() {
		caller := *op.Agent
		if req.Hub != "" && !dirWithin(caller.Cwd, req.Hub) {
			return cockpitScope{}, fmt.Errorf("%w: %s does not work in %s", ErrRefused, caller.Handle, req.Hub)
		}
		me, err := e.gatewayEndpoint(caller)
		if err != nil {
			return cockpitScope{}, err
		}
		scope := cockpitScope{members: []core.Endpoint{me}, hubDir: me.Hub.Dir(), strict: true, self: &caller}
		if req.Personal != "" {
			// A filter inside the caller's own scope (§4.2(8)): members
			// stays the caller alone, so naming anyone, live, departed, or
			// unknown, only narrows what the caller may read already.
			scope.personal, scope.named = core.Handle(req.Personal), personalRecord(recs, caller, me, core.Handle(req.Personal))
		}
		return scope, nil
	}
	scope := cockpitScope{personal: core.Handle(req.Personal)}
	if req.Hub != "" {
		if scope.hubDir, err = resolvedDir(req.Hub); err != nil {
			return cockpitScope{}, fmt.Errorf("%w: --hub %s: %v", ErrUsage, req.Hub, err)
		}
	}
	var named *agent
	if req.Personal != "" {
		for i, r := range recs {
			if r.Live() && r.Role == core.PersonalRole && placed(r) && r.Handle == core.Handle(req.Personal) {
				named = &recs[i]
			}
		}
		if named == nil {
			return cockpitScope{}, fmt.Errorf("%w: no live %s agent is %s", ErrRefused, core.PersonalRole, req.Personal)
		}
		ep, err := e.gatewayEndpoint(*named)
		if err != nil {
			return cockpitScope{}, err
		}
		if scope.hubDir != "" && ep.Hub.Dir() != scope.hubDir && !dirWithin(named.Cwd, scope.hubDir) {
			return cockpitScope{}, fmt.Errorf("%w: no live %s agent matches %s", ErrRefused, core.PersonalRole, cockpitTarget(req))
		}
		if scope.hubDir == "" {
			scope.hubDir = ep.Hub.Dir()
		}
		scope.members, scope.named, scope.self = []core.Endpoint{ep}, []core.Endpoint{ep}, named
		return scope, nil
	}
	if scope.hubDir == "" {
		if scope.hubDir, err = e.defaultHubDir(); err != nil {
			return cockpitScope{}, err
		}
	}
	if scope.hubDir == "" {
		return cockpitScope{}, fmt.Errorf("%w: name the Hub whose Epics to read (--hub DIR) or one of its personal agents (--personal HANDLE); no Hub directory is configured", ErrUsage)
	}
	// The Hub's live personal agents, so the session-keyed links of those
	// that predate Hub links are read too.
	for i, r := range recs {
		if !r.Live() || r.Role != core.PersonalRole || !placed(r) {
			continue
		}
		ep, err := e.gatewayEndpoint(r)
		if err != nil {
			return cockpitScope{}, err
		}
		if ep.Hub.Dir() == scope.hubDir || ep.Hub == "" && dirWithin(r.Cwd, scope.hubDir) {
			scope.members = append(scope.members, ep)
			scope.self = &recs[i]
		}
	}
	if len(scope.members) != 1 {
		scope.self = nil
	}
	return scope, nil
}

// personalRecord is the endpoint a --personal filter of the personal caller
// (me, its endpoint) names, for matching a session-keyed link by its
// session: the caller itself, or the recorded personal agent h, live or
// departed. None for a handle with no such record: only a link preferring h
// then passes the filter.
func personalRecord(recs []agent, caller agent, me core.Endpoint, h core.Handle) []core.Endpoint {
	if h == caller.Handle {
		return []core.Endpoint{me}
	}
	for _, r := range recs {
		if r.Handle == h && r.Role == core.PersonalRole && placed(r) {
			return []core.Endpoint{r.Endpoint()}
		}
	}
	return nil
}

// agentAncestor is the live record among recs, of those match accepts, whose
// agent is one of ancestors (this process and its ancestors, nearest first):
// a command an agent runs, even with its environment stripped (`env -i`),
// descends from that agent's process.
func agentAncestor(ancestors []proc, recs []agent, match func(agent) bool) (agent, bool) {
	for _, p := range ancestors {
		for _, r := range recs {
			if r.Live() && r.pid() > 1 && r.pid() == p.PID && match(r) {
				return r, true
			}
		}
	}
	return agent{}, false
}

func cockpitTarget(req CockpitViewRequest) string {
	var parts []string
	if req.Personal != "" {
		parts = append(parts, "--personal "+req.Personal)
	}
	if req.Hub != "" {
		parts = append(parts, "--hub "+req.Hub)
	}
	return strings.Join(parts, " ")
}

// unlinkedEpic is the Epic session whose lifecycle records no Hub and
// that nothing links to; ok is false for any other session.
func (e Env) unlinkedEpic(session string) (core.SessionKey, bool, error) {
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok || lc.Hub != "" || lc.PersonalSessionID != "" {
		return core.SessionKey{}, false, err
	}
	links, err := e.linksToEpic(session)
	if err != nil || len(links) > 0 {
		return core.SessionKey{}, false, err
	}
	return core.SessionKey{Server: lc.Server, SessionID: session}, true, nil
}

// cockpitSession is the view of the linked Epic epic, with its stored
// hand-back and lifecycle. An Epic whose narrator has not written a view yet
// but whose lifecycle is recorded reads as that lifecycle alone.
func (e Env) cockpitSession(epic core.SessionKey) (json.RawMessage, error) {
	b, ok, err := e.Store.ReadView(epic.SessionID)
	if err != nil {
		return nil, err
	}
	if ok {
		return e.withStored(epic.SessionID, b)
	}
	lc, hasLC, err := e.Store.EpicLifecycle(epic.SessionID)
	if err != nil {
		return nil, err
	}
	if !hasLC {
		return nil, ErrNoView
	}
	v := SessionView{Schema: ViewSchema, Session: ViewSession{Server: epic.Server, SessionID: epic.SessionID, Epic: lc.Name}, Lifecycle: &lc,
		Members: []ViewMember{}, Obligations: []ViewObligation{}, Activity: []ViewMessage{}, ControlPlane: []ViewMessage{},
		CoverageGaps: []CoverageGap{{Kind: GapNoView, Note: "the narrator has not written the view yet; only the recorded lifecycle is shown"}},
		Sources:      []string{"lifecycle.json"}}
	if hb, ok, err := e.Store.HandBack(epic.SessionID); err != nil {
		return nil, err
	} else if ok {
		v.HandBack = &hb
		v.Sources = append(v.Sources, "hand-back.json")
	}
	out, err := json.Marshal(v)
	return append(out, '\n'), err
}

// cockpitEpic is the summary of the linked (or recently closed) Epic epic.
func (e Env) cockpitEpic(epic core.SessionKey) (CockpitEpic, error) {
	c := CockpitEpic{SessionID: epic.SessionID, Server: epic.Server}
	if lc, ok, err := e.Store.EpicLifecycle(epic.SessionID); err != nil {
		return c, err
	} else if ok {
		c.Lifecycle, c.Name, c.Dir = &lc, lc.Name, lc.Dir
		if c.Server == "" {
			c.Server = lc.Server
		}
		if lc.State == store.EpicHandBack {
			c.Obligation = ObligationHandBack
		}
	}
	if hb, ok, err := e.Store.HandBack(epic.SessionID); err != nil {
		return c, err
	} else if ok {
		c.HandBack = &hb
	}
	if g, ok, err := e.Store.Grant(epic.SessionID); err != nil {
		return c, err
	} else if ok && g.Contract != "" {
		c.GoalPath = g.Contract
	}
	b, ok, err := e.Store.ReadView(epic.SessionID)
	if err != nil {
		return c, err
	}
	c.HasView = ok
	if ok {
		var v SessionView
		if json.Unmarshal(b, &v) == nil {
			if c.Name == "" {
				c.Name = v.Session.Epic
			}
			if c.GoalPath == "" {
				c.GoalPath = v.GoalPath
			}
			c.Narrative = v.Narrative
			c.Progress = v.Progress
		}
	}
	return c, nil
}

// dirWithin reports whether dir is hub or inside it, symbolic links
// resolved; a directory that cannot be resolved is in no hub
// (sameOrWithin, which hub notify's --hub uses too).
func dirWithin(dir, hub string) bool {
	if hub == "" {
		return false
	}
	h, err := resolvedDir(hub)
	if err != nil {
		return false
	}
	return sameOrWithin(dir, h)
}
