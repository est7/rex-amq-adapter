package app

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An observer reads the adapter's state and Rex's layout, and writes nothing:
// no record, ledger, injector wrapper, or lock. It never collects or
// supervises, so watching an agent cannot change what happens to it.

// Scope kinds.
const (
	ScopeSession = "session"
	ScopeTab     = "tab"
	ScopeAll     = "all"
)

// Scope is what an observer watches: one Rex session, one tab of it, or every
// agent of a server.
type Scope struct {
	Kind      string `json:"kind"`
	Server    string `json:"server"`
	SessionID string `json:"session_id,omitempty"`
	WindowID  string `json:"window_id,omitempty"`
}

// view is a scope with the layout it was last seen in.
type view struct {
	scope    Scope
	sessions []Session
}

// place is where the layout puts r's block now: its session, window, and
// whether it is there at all.
func (v view) place(r store.Record) (session, window string, placed bool) {
	for _, s := range v.sessions {
		if w, ok := s.windowOf[r.BlockID]; ok && r.Server == s.server {
			return s.id, w, true
		}
	}
	return r.SessionID, r.WindowID, false
}

// tab is how window w of session is shown.
func (v view) tab(session, window string) string {
	for _, s := range v.sessions {
		if s.id == session {
			return s.tab(window)
		}
	}
	return window
}

// holds reports whether r is in the scope: for a session, as discovery
// decides; for a tab, an agent the session holds whose block is there now or,
// once departed, was recorded there; for all, every record of the server.
func (v view) holds(r store.Record) bool {
	switch v.scope.Kind {
	case ScopeAll:
		return r.Server == v.scope.Server
	case ScopeTab:
		if len(v.sessions) == 0 || !v.sessions[0].holds(r) {
			return false
		}
		_, window, placed := v.place(r)
		if !placed || !r.Live() {
			window = r.WindowID
		}
		return window == v.scope.WindowID
	}
	return len(v.sessions) > 0 && v.sessions[0].holds(r)
}

// ResolveScope finds the scope an observer of kind watches: the caller's
// session or tab (loc), a session named by id, or the whole server.
func (e Env) ResolveScope(ctx context.Context, kind string, loc Location, session string) (Scope, error) {
	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return Scope{}, err
	}
	if srv.Endpoint == "" {
		return Scope{}, fmt.Errorf("the Rex server reports no endpoint")
	}
	sc := Scope{Kind: kind, Server: srv.Endpoint, SessionID: session}
	switch {
	case kind == ScopeAll:
		sc.SessionID = ""
		return sc, nil
	case session != "":
		return sc, nil
	}
	s, err := e.FindSession(ctx, loc.Block)
	if err != nil {
		return Scope{}, err
	}
	sc.SessionID = s.id
	if kind == ScopeTab {
		sc.WindowID = s.windowOf[loc.Block]
	}
	return sc, nil
}

// loadView reads the layout sc covers now.
func (e Env) loadView(ctx context.Context, sc Scope) (view, error) {
	v := view{scope: sc}
	ids := []string{sc.SessionID}
	if sc.Kind == ScopeAll {
		var err error
		if ids, err = e.Rex.ListSessions(ctx); err != nil {
			return v, err
		}
	}
	for _, id := range ids {
		s, err := e.loadSession(ctx, sc.Server, id)
		if err != nil {
			return v, fmt.Errorf("read the layout of %s: %w", id, err)
		}
		v.sessions = append(v.sessions, s)
	}
	return v, nil
}

// observeAgents observes the agents of recs: their records, where v places
// them, and their mail. activity, when not nil, is what each live agent last
// reported (observeActivity).
func (e Env) observeAgents(v view, recs []store.Record, activity map[core.Handle]*core.ActivityObservation) ([]core.AgentObservation, error) {
	out := make([]core.AgentObservation, 0, len(recs))
	for _, r := range recs {
		session, window, placed := v.place(r)
		if !r.Live() {
			session, window = r.SessionID, r.WindowID
		}
		o := core.AgentObservation{
			Handle: r.Handle, Kind: r.Kind, Server: r.Server, SessionID: session, WindowID: window,
			Tab: v.tab(session, window), BlockID: r.BlockID, Placed: placed, Task: r.Task, Role: r.Role,
			Adopting: r.Live() && r.Unfinished == store.Adopting, Departing: !r.Live() && r.Unfinished == store.Departing,
			AdoptedAt: r.AdoptedAt, DepartedAt: r.DepartedAt, DepartReason: r.DepartReason,
		}
		if r.Live() {
			o.Activity = activity[r.Handle]
		}
		var err error
		if o.Unread, err = amq.Unread(e.Paths.Root(), string(r.Handle)); err != nil {
			return nil, err
		}
		ledger, err := e.Store.Announced(r.Handle)
		if err != nil {
			return nil, err
		}
		o.Announced = ledger.IDs
		if !r.Kind.InProcess() {
			if o.Doorbells, err = e.Store.InjectHistory(r.Handle); err != nil {
				return nil, err
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// observeActivity asks what each live agent of recs is doing: a Claude Code
// background job's supervisor, else Rex's program status of its block. A
// failed look is reported as such, not as a state.
func (e Env) observeActivity(ctx context.Context, recs []store.Record) map[core.Handle]*core.ActivityObservation {
	out := map[core.Handle]*core.ActivityObservation{}
	for _, r := range recs {
		if !r.Live() {
			continue
		}
		a := &core.ActivityObservation{At: e.Now().UTC()}
		if st, _, isJob, err := e.jobStatus(r); err == nil && isJob {
			a.State, a.Source = core.AgentState(st.State), "supervisor"
			out[r.Handle] = a
			continue
		}
		a.Source = "rex_status"
		st, ok, err := e.RexOn(r.Server).ProgramStatus(ctx, r.BlockID)
		switch {
		case err != nil:
			a.Err = true
		case ok:
			a.State, a.Kind = core.AgentState(st.State), st.Kind
		}
		out[r.Handle] = a
	}
	return out
}

// recentMessages is how many drained or sent messages an observer keeps, the
// newest; undrained and dead-lettered ones are kept whatever their number.
const recentMessages = 50

// headerCache keeps the headers already read: a message's header never
// changes, it only moves between boxes.
type headerCache map[string]amq.Header

// observeMessages observes the mail of the scope's agents (handles): what
// they hold, unread, drained, or dead-lettered, and what they sent. A
// recipient outside the scope is not looked at, so its copy counts for
// nothing; mail neither sent nor received in the scope is not seen.
func (e Env) observeMessages(handles []core.Handle, recs []store.Record, cache headerCache) ([]core.MessageObservation, error) {
	root := e.Paths.Root()
	departed := map[string]bool{}
	for _, r := range recs {
		if !r.Live() {
			departed[string(r.Handle)] = true
		}
	}
	type copyAt struct {
		handle string
		box    amq.Box
	}
	copies := map[string][]copyAt{} // id → where it is
	keep := map[string]bool{}
	var settled []string
	announced := map[string]map[string]bool{}
	for _, h := range handles {
		hs := string(h)
		ledger, err := e.Store.Announced(h)
		if err != nil {
			return nil, err
		}
		announced[hs] = ledger.IDs
		for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained, amq.BoxDLQNew, amq.BoxDLQCur, amq.BoxSent} {
			ids, err := amq.IDs(root, hs, box)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				copies[id] = append(copies[id], copyAt{hs, box})
				switch box {
				case amq.BoxUnread, amq.BoxDLQNew, amq.BoxDLQCur:
					keep[id] = true
				default:
					settled = append(settled, id)
				}
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(settled)))
	settled = slices.Compact(settled)
	for n, i := 0, 0; i < len(settled) && n < recentMessages; i++ {
		if id := settled[i]; !keep[id] {
			keep[id] = true
			n++
		}
	}
	var out []core.MessageObservation
	for id := range keep {
		h, ok := cache[id]
		if !ok {
			var err error
			at := copies[id][0]
			if h, err = amq.ReadHeader(root, at.handle, at.box, id); err != nil {
				fmt.Fprintf(Diagnostics, "read %s in %s: %v\n", id, at.handle, err)
				continue
			}
			cache[id] = h
		}
		m := core.MessageObservation{ID: id, From: h.From, To: h.To, Thread: h.Thread, Subject: h.Subject, Created: h.Created}
		for _, c := range copies[id] {
			var b core.Box
			switch c.box {
			case amq.BoxUnread:
				b = core.BoxUnread
			case amq.BoxDrained:
				b = core.BoxRead
			case amq.BoxDLQNew, amq.BoxDLQCur:
				b = core.BoxDead
			default:
				continue // the sender's copy says nothing of delivery
			}
			m.Recipients = append(m.Recipients, core.RecipientObservation{Handle: c.handle, Box: b, Announced: announced[c.handle][id], Departed: departed[c.handle]})
		}
		out = append(out, m)
	}
	return out, nil
}
