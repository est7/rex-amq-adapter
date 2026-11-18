package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The cockpit is the human's read of a Hub's Epics, from a personal agent's
// own pane or from outside Rex (`epic list`, `epic show`). It reads only: no
// mail moves, no receipt is written, nothing is started. Its scope is the
// Epics linked to one Hub (core.LinkMatchesPersonal), never another Epic,
// and it does not depend on which of the Hub's personal agents are live, or
// how many; and it is not a way around the view's own access rule, so
// anything running as, or under, an agent that is not a personal one is
// refused, however its environment was trimmed (cockpitScope). A pane or a
// shell with no agent in it is not trusted for being one: it reads the Hub
// it names (--hub), the Hub of the personal agent it names (--personal), or
// the default Hub.

// CockpitViewRequest is what `epic list` and `epic show` from outside Rex
// ask: Session, one linked Epic's view; without it, a summary of every
// linked Epic. Hub names the Hub directory for a caller that is not a
// personal agent; Personal narrows to the Epics that personal agent opened
// or prefers (and names its Hub when Hub is not given).
type CockpitViewRequest struct {
	Session  string
	Personal string
	Hub      string
}

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

// CockpitEpic is one linked Epic in the summary. HasView says its narrator
// has written its view (read it with --session). Obligation is what the
// Epic still asks of its Hub, from its lifecycle alone, whatever mail was
// read: hand_back from the lead's hand-back until the Epic is closed.
type CockpitEpic struct {
	SessionID         string               `json:"session_id"`
	Server            string               `json:"server,omitempty"`
	Name              string               `json:"name,omitempty"`
	Dir               string               `json:"dir,omitempty"`
	PreferredPersonal core.Handle          `json:"preferred_personal,omitempty"`
	Obligation        string               `json:"obligation,omitempty"`
	Lifecycle         *store.EpicLifecycle `json:"lifecycle,omitempty"`
	HandBack          *store.HandBack      `json:"hand_back,omitempty"`
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
	scope, err := e.cockpitScope(ctx, getenv, req)
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
	for _, l := range linked {
		c, err := e.cockpitEpic(l.Epic)
		if err != nil {
			return nil, err
		}
		c.PreferredPersonal = l.PreferredPersonal
		out.Epics = append(out.Epics, c)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), err
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

// cockpitScope is the Hub whose linked Epics the caller may read, after
// every gate:
//
//  1. No harness session the environment names (sessionVars) is a live
//     agent's of another role, even beside a personal agent's.
//  2. The caller (Caller) resolves: a harness session no hook adopted (a
//     worker's nested `claude -p`) is refused; its pane, when it holds a
//     live agent, holds a personal one.
//  3. No ancestor of this process is the agent of a live record of another
//     role, so `env -i` under a worker or an orchestrator is still refused.
//  4. ME_UNATTENDED=1 is refused unless the caller is a personal agent (which
//     may carry it inherited).
//
// A personal caller reads its own Hub, by its own endpoint alone
// (core.LinkMatchesPersonal); --personal only narrows that to the Epics the
// named personal agent opened or prefers, live or not (§4.2(8)), so naming
// one outside the scope reads nothing; a directory it does not work in is
// refused. Anyone else reads the Hub
// --hub names, else the Hub of the live personal agent --personal names,
// else the default Hub; with none of them, it names one. However many of
// the Hub's personal agents are live, none, one, or several, the scope is
// the same Hub.
func (e Env) cockpitScope(ctx context.Context, getenv func(string) string, req CockpitViewRequest) (cockpitScope, error) {
	recs, err := e.agents()
	if err != nil {
		return cockpitScope{}, err
	}
	for _, v := range sessionVars {
		id := getenv(v.name)
		if id == "" {
			continue
		}
		// One no hook adopted is Caller's to refuse, below.
		rec, ok, err := e.liveRecordOf(v.kind, id)
		if err != nil {
			return cockpitScope{}, err
		}
		if ok && rec.Role != core.PersonalRole {
			return cockpitScope{}, cockpitRefused(rec)
		}
	}
	var caller agent
	isPersonal := false
	loc, err := e.Caller(ctx, getenv)
	switch {
	case errors.Is(err, ErrNotInRex):
	case err != nil:
		return cockpitScope{}, fmt.Errorf("%w: the cockpit's caller: %v", ErrRefused, err)
	default:
		rec, ok, err := e.agentIn(loc)
		if err != nil {
			return cockpitScope{}, err
		}
		if ok && rec.Live() {
			if rec.Role != core.PersonalRole {
				return cockpitScope{}, cockpitRefused(rec)
			}
			caller, isPersonal = rec, true
		}
	}
	ancestors, err := e.ancestors()
	if err != nil {
		return cockpitScope{}, fmt.Errorf("tell which agent runs the cockpit: %w", err)
	}
	if r, ok := agentAncestor(ancestors, recs, func(r agent) bool { return r.Role != core.PersonalRole }); ok {
		return cockpitScope{}, cockpitRefused(r)
	}
	if getenv("ME_UNATTENDED") == "1" && !isPersonal {
		return cockpitScope{}, fmt.Errorf("%w: the cockpit is the human's or a personal agent's, and this command runs unattended (ME_UNATTENDED=1)", ErrRefused)
	}
	if isPersonal {
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

func cockpitRefused(rec agent) error {
	role := rec.Role
	if role == "" {
		role = "no role"
	}
	return fmt.Errorf("%w: the cockpit is the human's or a personal agent's to read, and this command runs as or under %s (%s)", ErrRefused, rec.Handle, role)
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

// cockpitEpic is the summary of the linked Epic epic.
func (e Env) cockpitEpic(epic core.SessionKey) (CockpitEpic, error) {
	c := CockpitEpic{SessionID: epic.SessionID, Server: epic.Server}
	if lc, ok, err := e.Store.EpicLifecycle(epic.SessionID); err != nil {
		return c, err
	} else if ok {
		c.Lifecycle, c.Name, c.Dir = &lc, lc.Name, lc.Dir
		if lc.State == store.EpicHandBack {
			c.Obligation = ObligationHandBack
		}
	}
	if hb, ok, err := e.Store.HandBack(epic.SessionID); err != nil {
		return c, err
	} else if ok {
		c.HandBack = &hb
	}
	b, ok, err := e.Store.ReadView(epic.SessionID)
	if err != nil {
		return c, err
	}
	c.HasView = ok
	if ok && c.Name == "" {
		var v SessionView
		if json.Unmarshal(b, &v) == nil {
			c.Name = v.Session.Epic
		}
	}
	return c, nil
}

// dirWithin reports whether dir is hub or inside it, symbolic links
// resolved; a directory that cannot be resolved is in no hub
// (sameOrWithin, which notify-personal's --hub uses too).
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
