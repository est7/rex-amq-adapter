package app

import (
	"errors"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/hub"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// HubDirs resolves the user's Hub directories (internal/hub): ME_HUB, then
// config.toml hub_dirs, then $HOME/me, each canonical. An Env without
// Getenv (a test's) has none.
func (e Env) HubDirs() ([]hub.Dir, error) {
	if e.Getenv == nil {
		return nil, nil
	}
	return hub.Dirs(e.Getenv)
}

// hubOf is the Hub a personal agent working in cwd on server belongs to:
// the Hub directory cwd is in (symlinks resolved, so a repository linked
// under the Hub is not in it), else the first Hub directory; in is whether
// cwd is inside one. With no Hub directory at all the key is "": the agent
// is a personal agent of no Hub, whose links stay keyed by its session.
func (e Env) hubOf(server, cwd string) (key core.HubKey, in bool, err error) {
	dirs, err := e.HubDirs()
	if err != nil || len(dirs) == 0 {
		return "", false, err
	}
	for _, d := range dirs {
		if sameOrWithin(cwd, d.Path) {
			return core.NewHubKey(server, d.Path), true, nil
		}
	}
	return core.NewHubKey(server, dirs[0].Path), false, nil
}

// defaultHubDir is the Hub directory a command outside Rex reads when it
// names none: the first resolved one; "" when there is none.
func (e Env) defaultHubDir() (string, error) {
	dirs, err := e.HubDirs()
	if err != nil || len(dirs) == 0 {
		return "", err
	}
	return dirs[0].Path, nil
}

// sessionFacts is what admission reads of session (core.SessionFacts),
// leaving out the agent self, with the session's policy. The caller holds
// LockLifecycle so the facts stand until it acts on them.
func (e Env) sessionFacts(session string, recs []agent, self core.Handle) (core.SessionFacts, store.SessionPolicy, error) {
	var f core.SessionFacts
	if session == "" {
		return f, store.SessionPolicy{}, nil
	}
	p, ok, err := e.Store.SessionPolicy(session)
	if err != nil {
		return f, p, err
	}
	if ok {
		f.HubPolicy, f.EpicPolicy = p.Hub(), p.Epic()
	}
	if _, f.Crew, err = e.Store.SessionCrew(session); err != nil {
		return f, p, err
	}
	for _, r := range recs {
		if !r.Live() || r.Handle == self || r.SessionID != session {
			continue
		}
		switch {
		case r.Role == core.PersonalRole:
			f.LivePersonal = true
		default:
			f.LiveOther = true
			f.LiveWorker = f.LiveWorker || !core.Stealth(r.Role)
		}
	}
	return f, p, nil
}

// withSelf is f with the live agent of role counted in.
func withSelf(f core.SessionFacts, role string) core.SessionFacts {
	if role == core.PersonalRole {
		f.LivePersonal = true
	} else {
		f.LiveOther, f.LiveWorker = true, f.LiveWorker || !core.Stealth(role)
	}
	return f
}

// admissionRefused is the guard refusal of an agent from (its role, ""
// for a new one) taking role to in a session with facts f (the agent itself
// counted in, so a session it already mixes reports the conflict).
func admissionRefused(f core.SessionFacts, h core.Handle, from, to, reason string) *fault.Error {
	var msg string
	switch reason {
	case core.ReasonRoleLocked:
		msg = fmt.Sprintf("%s is a %s agent, and that role is for good: it never becomes %q", h, from, to)
	case core.ReasonSessionPure:
		msg = "this Rex session is a Hub, for personal agents only; multi-agent work opens an Epic"
	case core.ReasonNoPersonal:
		msg = "this Rex session is an Epic's (its policy, its crew, or a live worker), and a personal agent lives only in a Hub session"
	}
	err := fault.New(fault.GuardDenied, "%s: %s", ErrRefused, msg).WithCause(ErrRefused).With("reason", reason)
	if f.Conflict() {
		err.With("policy_conflict", true)
	}
	if reason == core.ReasonSessionPure {
		err.WithNext(`rex-amq-adapter epic open <name> --cwd <dir> --asked "<the user's words>"`)
	}
	return err
}

// warnConflict reports a session admission found both a Hub's and an
// Epic's: it admits nothing new and changes no role until it is one again,
// and nothing in it is killed or changed.
func warnConflict(session string, f core.SessionFacts) {
	if f.Conflict() {
		fmt.Fprintf(Diagnostics, "policy_conflict: Rex session %s holds personal agents beside other agents; it admits no new agent and no role change until it is one or the other\n", session)
	}
}

// ensureHubPolicy records session as a Hub's, the caller holding
// LockLifecycle, unless it has a policy: the first personal agent a session
// adopts makes it a Hub session, which it stays after that agent leaves.
func (e Env) ensureHubPolicy(session, server string, key core.HubKey, by core.Handle) error {
	if session == "" {
		return nil
	}
	if _, ok, err := e.Store.SessionPolicy(session); err != nil || ok {
		return err
	}
	return e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: session, Server: server, HubKey: key, CreatedBy: by, CreatedAt: e.Now().UTC()})
}

// hubOfSession is the Hub of session's policy; "" when it is no Hub's.
func (e Env) hubOfSession(session string) (core.HubKey, error) {
	p, ok, err := e.Store.SessionPolicy(session)
	if err != nil || !ok || !p.Hub() {
		return "", err
	}
	return p.HubKey, nil
}

// ErrHubRefused is an adoption refused because the session is a Hub's (or
// an Epic's, for a personal agent): the agent gets no identity, mailbox, or
// lease, and is told why (HubRefusal.Notice).
var ErrHubRefused = errors.New("refused admission to this Rex session")

// HubRefusal is an adoption admission refused, with the notice its hook
// replies.
type HubRefusal struct {
	Reason string
	Notice string
}

func (r *HubRefusal) Error() string { return ErrHubRefused.Error() + ": " + r.Reason }
func (r *HubRefusal) Unwrap() error { return ErrHubRefused }
