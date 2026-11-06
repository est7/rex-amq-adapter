package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// sessionVars are the variables each harness sets afresh in its tool shells,
// naming the session that runs the command: an inherited value is
// overwritten. REX_BLOCK there is inherited, so it names whichever pane
// started the process that hosts the session: for a Claude Code background
// job the supervisor's first terminal, for a command run by a nested agent
// the outer agent's pane.
var sessionVars = []struct {
	name string
	kind core.Kind
}{
	{"CLAUDE_CODE_SESSION_ID", core.Claude},
	{"CODEX_THREAD_ID", core.Codex},
	{"PI_SESSION_ID", core.Pi},
	{"ANTIGRAVITY_CONVERSATION_ID", core.Agy},
}

// Caller is where the agent running a command is, from getenv, the command's
// environment. A command that runs in a harness's tool shell is that
// session's: it is where the session's live record is, and for a Claude Code
// background job only while that pane shows it. Every harness session the
// environment names must have one: a session no hook adopted (a `claude -p`
// another agent runs, an Antigravity subagent) or a job no pane shows has no
// identity, and is refused rather than given the outer agent's or REX_BLOCK's.
// When several adopted sessions are named (an agent another agent started),
// the command is the nearest one's. Only a command no harness runs is located
// by REX_BLOCK.
func (e Env) Caller(ctx context.Context, getenv func(string) string) (Location, error) {
	type named struct {
		kind    core.Kind
		session string
	}
	var sessions []named
	for _, v := range sessionVars {
		if id := getenv(v.name); id != "" {
			sessions = append(sessions, named{v.kind, id})
		}
	}
	if len(sessions) == 0 {
		return here(getenv)
	}
	var found []store.Record
	for _, s := range sessions {
		rec, err := e.sessionRecord(ctx, s.kind, s.session)
		if err != nil {
			return Location{}, err
		}
		found = append(found, rec)
	}
	rec := found[0]
	if len(found) > 1 {
		var err error
		if rec, err = e.nearest(found); err != nil {
			return Location{}, err
		}
	}
	return Location{Server: rec.Server, Block: rec.BlockID}, nil
}

// sessionRecord is the live record of a harness session a command's
// environment names.
func (e Env) sessionRecord(ctx context.Context, kind core.Kind, session string) (store.Record, error) {
	rec, hasRec, err := e.liveRecordOf(kind, session)
	if err != nil {
		return store.Record{}, err
	}
	if kind == core.Claude {
		job, isJob, err := e.claudeJob(session)
		if err != nil {
			return store.Record{}, err
		}
		switch {
		case isJob && hasRec:
			if shown, err := e.showsJob(ctx, rec.BlockID, rec, job); err != nil || !shown {
				return store.Record{}, fmt.Errorf("%w now (it was last in %s), so it has no AMQ identity until one does", ErrJobNotShown, rec.BlockID)
			}
		case isJob:
			return store.Record{}, fmt.Errorf("%w yet, so it has no AMQ identity; it gets one at its next prompt once a pane shows it", ErrJobNotShown)
		}
	}
	if !hasRec {
		return store.Record{}, fmt.Errorf("this %s session (%s) has no AMQ identity: its hooks have not adopted it, and no other agent's identity is its own", kind, session)
	}
	return rec, nil
}

// nearest is the record in recs of the agent nearest this process: the
// command runs in that agent's tool shell, and the others are agents it was
// started under.
func (e Env) nearest(recs []store.Record) (store.Record, error) {
	ancestors, err := e.ancestors()
	if err != nil {
		return store.Record{}, fmt.Errorf("tell which of several agents runs this command: %w", err)
	}
	for _, p := range ancestors {
		if i := slices.IndexFunc(recs, func(r store.Record) bool { return r.AgentPID == p.PID }); i >= 0 {
			return recs[i], nil
		}
	}
	handles := make([]string, len(recs))
	for i, r := range recs {
		handles[i] = string(r.Handle)
	}
	return store.Record{}, fmt.Errorf("this command's environment names the sessions of %v, and it descends from none of their agents", handles)
}

// ErrNotInRex: the caller's terminal is not a Rex block.
var ErrNotInRex = errors.New("not running in a Rex terminal (REX_BLOCK is unset)")

// Location is the Rex terminal the caller runs in.
type Location struct {
	Server string
	Block  string
}

// Here reads the caller's Rex location from the environment Rex sets.
func Here() (Location, error) { return here(os.Getenv) }

func here(getenv func(string) string) (Location, error) {
	l := Location{Server: getenv("REX_SERVER"), Block: getenv("REX_BLOCK")}
	if l.Block == "" {
		return Location{}, ErrNotInRex
	}
	return l, nil
}

// RexExports is the shell text that points rex at block and its session, for
// a caller whose inherited REX_BLOCK names another pane.
func (e Env) RexExports(ctx context.Context, block string) (string, error) {
	b, err := e.Rex.InspectBlock(ctx, block)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("export REX_BLOCK=%s REX_SESSION=%s\n", shellQuote(block), shellQuote(b.SessionID)), nil
}
