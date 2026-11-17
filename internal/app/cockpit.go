package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The cockpit is the human's read of a personal agent's Epics, from the
// personal agent's own pane or from outside Rex (`view --cockpit`). It reads
// only: no mail moves, no receipt is written, nothing is started. Its scope
// is one live personal agent and the Epics linked to it, never another
// Epic; and it is not a way around the view's own access rule, so anything
// running as, or under, an agent that is not a personal one is refused,
// however its environment was trimmed (cockpitScope). A pane or a shell with
// no agent in it is not trusted for being one: it names the personal agent
// (--personal) or the hub it works in (--hub).

// CockpitViewRequest is what `view --cockpit` asks: Session, one linked Epic's
// view; without it, a summary of every linked Epic. Personal and Hub name
// the personal agent for a caller that is not one.
type CockpitViewRequest struct {
	Session  string
	Personal string
	Hub      string
}

// CockpitView is the summary of a personal agent's linked Epics.
type CockpitView struct {
	Schema    int           `json:"schema"`
	Personal  core.Handle   `json:"personal"`
	SessionID string        `json:"session_id"`
	Epics     []CockpitEpic `json:"epics"`
}

// CockpitEpic is one linked Epic in the summary. HasView says its narrator
// has written its view (read it with --session).
type CockpitEpic struct {
	SessionID string               `json:"session_id"`
	Server    string               `json:"server,omitempty"`
	Name      string               `json:"name,omitempty"`
	Dir       string               `json:"dir,omitempty"`
	Lifecycle *store.EpicLifecycle `json:"lifecycle,omitempty"`
	HandBack  *store.HandBack      `json:"hand_back,omitempty"`
	HasView   bool                 `json:"has_view"`
}

// ReadCockpitView returns, for the caller whose environment getenv reads,
// the view of req.Session, an Epic linked to the personal agent in scope, or
// without req.Session the summary of every Epic linked to it.
func (e Env) ReadCockpitView(ctx context.Context, getenv func(string) string, req CockpitViewRequest) (json.RawMessage, error) {
	personal, err := e.cockpitScope(ctx, getenv, req)
	if err != nil {
		return nil, err
	}
	links, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var linked []core.Link
	for _, l := range links {
		if l.Personal == personal.Endpoint().Session() {
			linked = append(linked, l)
		}
	}
	if req.Session != "" {
		for _, l := range linked {
			if l.Epic.SessionID == req.Session {
				return e.cockpitSession(l.Epic)
			}
		}
		return nil, refused(fmt.Sprintf("the cockpit of %s reading the view of %s", personal.Handle, req.Session), core.Decision{Reason: core.ReasonNotLinked})
	}
	out := CockpitView{Schema: ViewSchema, Personal: personal.Handle, SessionID: personal.SessionID, Epics: []CockpitEpic{}}
	for _, l := range linked {
		c, err := e.cockpitEpic(l.Epic)
		if err != nil {
			return nil, err
		}
		out.Epics = append(out.Epics, c)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return append(b, '\n'), err
}

// cockpitScope is the live personal agent whose linked Epics the caller may
// read, after every gate:
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
// A personal caller reads its own scope, and naming another is refused;
// anyone else names exactly one live personal agent by --personal, --hub, or
// both.
func (e Env) cockpitScope(ctx context.Context, getenv func(string) string, req CockpitViewRequest) (agent, error) {
	recs, err := e.agents()
	if err != nil {
		return agent{}, err
	}
	for _, v := range sessionVars {
		id := getenv(v.name)
		if id == "" {
			continue
		}
		// One no hook adopted is Caller's to refuse, below.
		rec, ok, err := e.liveRecordOf(v.kind, id)
		if err != nil {
			return agent{}, err
		}
		if ok && rec.Role != core.PersonalRole {
			return agent{}, cockpitRefused(rec)
		}
	}
	var caller agent
	isPersonal := false
	loc, err := e.Caller(ctx, getenv)
	switch {
	case errors.Is(err, ErrNotInRex):
	case err != nil:
		return agent{}, fmt.Errorf("%w: the cockpit's caller: %v", ErrRefused, err)
	default:
		rec, ok, err := e.agentAt(loc.Block)
		if err != nil {
			return agent{}, err
		}
		if ok && rec.Live() {
			if rec.Role != core.PersonalRole {
				return agent{}, cockpitRefused(rec)
			}
			caller, isPersonal = rec, true
		}
	}
	ancestors, err := e.ancestors()
	if err != nil {
		return agent{}, fmt.Errorf("tell which agent runs the cockpit: %w", err)
	}
	if r, ok := agentAncestor(ancestors, recs, func(r agent) bool { return r.Role != core.PersonalRole }); ok {
		return agent{}, cockpitRefused(r)
	}
	if getenv("ME_UNATTENDED") == "1" && !isPersonal {
		return agent{}, fmt.Errorf("%w: the cockpit is the human's or a personal agent's, and this command runs unattended (ME_UNATTENDED=1)", ErrRefused)
	}
	if isPersonal {
		if req.Personal != "" && core.Handle(req.Personal) != caller.Handle {
			return agent{}, fmt.Errorf("%w: %s reads its own cockpit, not %s's", ErrRefused, caller.Handle, req.Personal)
		}
		if req.Hub != "" && !dirWithin(caller.Cwd, req.Hub) {
			return agent{}, fmt.Errorf("%w: %s does not work in %s", ErrRefused, caller.Handle, req.Hub)
		}
		return caller, nil
	}
	if req.Personal == "" && req.Hub == "" {
		return agent{}, fmt.Errorf("%w: name the personal agent whose Epics to read (--personal HANDLE or --hub DIR)", ErrUsage)
	}
	var found []agent
	for _, r := range recs {
		if !r.Live() || r.Role != core.PersonalRole || !placed(r) {
			continue
		}
		if (req.Personal == "" || r.Handle == core.Handle(req.Personal)) && (req.Hub == "" || dirWithin(r.Cwd, req.Hub)) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return agent{}, fmt.Errorf("%w: no live %s agent matches %s", ErrRefused, core.PersonalRole, cockpitTarget(req))
	case 1:
		return found[0], nil
	}
	handles := make([]string, len(found))
	for i, r := range found {
		handles[i] = string(r.Handle)
	}
	return agent{}, fmt.Errorf("%w: %d live %s agents match %s (%s); name one with --personal", ErrUsage, len(found), core.PersonalRole, cockpitTarget(req), strings.Join(handles, ", "))
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
