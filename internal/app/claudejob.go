package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A Claude Code background session ("agent view", code.claude.com/docs/en/
// agent-view.md) runs under Claude Code's supervisor, which the first terminal
// that needed it started. Every session it hosts inherits that terminal's
// environment, so REX_BLOCK and REX_SESSION name the starting terminal's pane,
// not the one showing the session; and a terminal client can switch the
// session it shows in agent view. The pane showing a job is the one whose
// terminal title is the job's name (docs/research/claude-background-sessions.md).

// claudeJob is a background session: its short id, current name, and
// state ("working", or at rest: "done", "blocked" waiting for the user's
// reply, "failed", "stopped"; observed in ~/.claude/jobs/*/state.json).
type claudeJob struct {
	Short, Name, State string
}

// claudeJob reports whether session runs under Claude Code's supervisor,
// from the supervisor's roster (~/.claude/daemon/roster.json, documented as
// the list of running background sessions), and the job's current name
// (~/.claude/jobs/<id>/state.json), which follows a rename.
func (e Env) claudeJob(session string) (claudeJob, bool, error) {
	if session == "" || e.ClaudeDir == "" {
		return claudeJob{}, false, nil
	}
	b, err := os.ReadFile(filepath.Join(e.ClaudeDir, "daemon", "roster.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return claudeJob{}, false, nil
	}
	if err != nil {
		return claudeJob{}, false, err
	}
	var roster struct {
		Workers map[string]struct {
			SessionID string `json:"sessionId"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(b, &roster); err != nil {
		return claudeJob{}, false, fmt.Errorf("read Claude Code's supervisor roster: %w", err)
	}
	for short, w := range roster.Workers {
		if w.SessionID != session {
			continue
		}
		job := claudeJob{Short: short}
		b, err := os.ReadFile(filepath.Join(e.ClaudeDir, "jobs", short, "state.json"))
		if err != nil {
			return job, true, err
		}
		var state struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(b, &state); err != nil {
			return job, true, fmt.Errorf("read the state of job %s: %w", short, err)
		}
		job.Name, job.State = state.Name, state.State
		return job, true, nil
	}
	return claudeJob{}, false, nil
}

// ErrJobNotShown: no pane, or more than one, shows the job by its title. The
// job has no pane until one does, and its inherited Rex location names
// another pane.
var ErrJobNotShown = errors.New("no single Rex pane shows this Claude Code background session")

// titleNames is the binding invariant for Claude Code panes: a pane shows an
// agent when its terminal title, without its state glyph, names it. Once
// adopted, the agent's title is its pane label (syncTitle), whose handle
// names it whatever task the harness last drew; before that, a background
// job's title is its current name. Which Claude Code path wrote which file
// does not matter: either counts.
func titleNames(title string, rec store.Record, job claudeJob) bool {
	t := core.ShownTitle(title)
	return t != "" && (core.TitleNamesHandle(t, rec.Handle) || job.Name != "" && t == core.ShownTitle(job.Name))
}

// showsJob reports whether block's foreground is a Claude Code terminal
// client whose title names the job (and rec, its record, if it has one).
func (e Env) showsJob(ctx context.Context, block string, rec store.Record, job claudeJob) (bool, error) {
	fg, ok, err := e.Rex.Foreground(ctx, block)
	if err != nil || !ok || fg.Name != "claude" {
		return false, err
	}
	title, err := e.Rex.Title(ctx, block)
	if err != nil {
		return false, err
	}
	return titleNames(title, rec, job), nil
}

// locateJob finds the one block that shows the job. Every block of every
// session is asked, so this is for hooks that run once a turn, not for every
// tool call. Only a block whose foreground is a Claude Code client can match,
// so a shell or an editor with the same title never does.
func (e Env) locateJob(ctx context.Context, rec store.Record, job claudeJob) (string, error) {
	sessions, err := e.Rex.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	var found []string
	for _, s := range sessions {
		blocks, err := e.Rex.ListBlocks(ctx, s)
		if err != nil {
			return "", err
		}
		for _, b := range blocks {
			if shown, err := e.showsJob(ctx, b.ID, rec, job); err == nil && shown {
				found = append(found, b.ID)
			}
		}
	}
	if len(found) != 1 {
		return "", ErrJobNotShown
	}
	return found[0], nil
}

// namesAnother reports whether title names a Claude conversation other than
// rec's: a background job by its current name, or another record by its
// handle.
func (e Env) namesAnother(title string, rec store.Record) (bool, error) {
	names, err := e.jobNames()
	if err != nil {
		return false, err
	}
	if names[core.ShownTitle(title)] {
		return true, nil
	}
	recs, err := e.Store.Records()
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		// A departed record counts too: a job whose pane closed keeps running.
		if r.Kind == core.Claude && r.Handle != rec.Handle && core.TitleNamesHandle(title, r.Handle) {
			return true, nil
		}
	}
	return false, nil
}

// jobNames are the current names of every background session the supervisor
// runs: a terminal client titled with one of them shows that session.
func (e Env) jobNames() (map[string]bool, error) {
	names := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(e.ClaudeDir, "daemon", "roster.json"))
	if errors.Is(err, fs.ErrNotExist) || e.ClaudeDir == "" {
		return names, nil
	}
	if err != nil {
		return nil, err
	}
	var roster struct {
		Workers map[string]struct {
			SessionID string `json:"sessionId"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(b, &roster); err != nil {
		return nil, err
	}
	for _, w := range roster.Workers {
		if job, ok, err := e.claudeJob(w.SessionID); err == nil && ok && job.Name != "" {
			names[core.ShownTitle(job.Name)] = true
		}
	}
	return names, nil
}

// claudeBinding fills in what a Claude Code record's pane title says. A
// terminal session's process stays its pane's foreground after it moved its
// conversation to the background (`←`), so a matching pid alone proves
// nothing: agent view in the title means the conversation left the pane, and
// another job's name means the pane shows that job. A background job is never
// its pane's foreground; the title alone says whether the pane shows it.
func (e Env) claudeBinding(ctx context.Context, rec store.Record, b *core.Binding) error {
	title, err := e.Rex.Title(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	job, isJob, err := e.claudeJob(rec.AgentSessionID)
	if err != nil {
		return err
	}
	if b.ForegroundPID == b.RecordPID {
		// A terminal session's process that shows agent view, or another
		// conversation, is a client now: its own conversation moved to the
		// background for good (`←`), and it departs.
		switch {
		case core.AgentViewTitle(title):
			b.LeftPane = true
		case !titleNames(title, rec, job):
			other, err := e.namesAnother(title, rec)
			if err != nil {
				return err
			}
			b.LeftPane = other
		}
		return nil
	}
	if isJob && !b.AgentGone {
		b.JobShown, err = e.showsJob(ctx, rec.BlockID, rec, job)
	}
	return err
}

// liveRecordOf is the live record of session, the identity a background
// job's tool shell has when its inherited REX_BLOCK names another pane.
func (e Env) liveRecordOf(kind core.Kind, session string) (store.Record, bool, error) {
	if session == "" {
		return store.Record{}, false, nil
	}
	recs, err := e.Store.Records()
	if err != nil {
		return store.Record{}, false, err
	}
	for _, r := range recs {
		if r.Live() && r.Kind == kind && r.AgentSessionID == session {
			return r, true, nil
		}
	}
	return store.Record{}, false, nil
}

// jobLocation is where the agent of a Claude hook is, and whether it is a
// background job (in.Job). A terminal session is at loc, its REX_BLOCK. A job
// is in the block of its live record while that block shows it; hooks that
// run once a turn check that and otherwise look for the block that does,
// where the job is then adopted again. shown is false when no block is known,
// and the hook then does nothing: REX_BLOCK names another agent's pane.
// SessionEnd finds the record by session first: the supervisor may have
// dropped the job from its roster already. A roster that cannot be read
// leaves loc as it was, and is reported.
func (e Env) jobLocation(ctx context.Context, event string, loc Location, in HookInput) (Location, HookInput, bool) {
	rec, hasRec, err := e.liveRecordOf(core.Claude, in.SessionID)
	if err != nil {
		fmt.Fprintf(Diagnostics, "records of %s: %v\n", in.SessionID, err)
	}
	at := func(block string) Location { return Location{Server: loc.Server, Block: block} }
	job, isJob, err := e.claudeJob(in.SessionID)
	if err != nil {
		fmt.Fprintf(Diagnostics, "Claude Code background session %s: %v\n", in.SessionID, err)
	}
	if event == EventSessionEnd && hasRec {
		return at(rec.BlockID), in, true
	}
	if !isJob {
		return loc, in, event != EventSessionEnd || !hasRec
	}
	in.Job = true
	// A tool call trusts the record of its own adopted process: looking
	// through every pane on each call would cost too much. A record that is
	// not, such as an older adapter's bound to another pane, is checked and
	// moved like at a prompt, so the hook never adopts, or records a failed
	// adoption, in a pane that does not show the job.
	if hasRec && !onceATurn(event) && rec.Adopted() && e.sameProcess(rec, in) {
		return at(rec.BlockID), in, true
	}
	if !onceATurn(event) && !hasRec {
		return loc, in, false
	}
	if hasRec {
		if shown, err := e.showsJob(ctx, rec.BlockID, rec, job); err == nil && shown {
			return at(rec.BlockID), in, true
		}
	} else if event == EventStop {
		// A first adoption tells the agent who it is; a Stop hook would
		// drop that when it has no mail to hold the turn for.
		return loc, in, false
	}
	block, err := e.locateJob(ctx, rec, job)
	if err != nil {
		fmt.Fprintf(Diagnostics, "Claude Code background session %s (%q): %v\n", job.Short, job.Name, err)
		return loc, in, false
	}
	return at(block), in, true
}

// onceATurn: hooks that run at most once a turn, where looking through every
// pane for a background job costs little.
func onceATurn(event string) bool {
	return event == EventSessionStart || event == EventPrompt || event == EventStop
}

// Caller is where the agent running a command is. A command runs in the
// agent's tool shell, where Claude Code sets CLAUDE_CODE_SESSION_ID afresh;
// REX_BLOCK there is inherited, and in a background job it names the pane of
// whichever terminal started the supervisor. So a Claude session with a live
// record is where that record is, and for a background job only while that
// pane shows it; a job without such a pane is refused rather than given
// another agent's identity.
func (e Env) Caller(ctx context.Context, loc Location, claudeSession string) (Location, error) {
	rec, hasRec, err := e.liveRecordOf(core.Claude, claudeSession)
	if err != nil {
		return loc, err
	}
	job, isJob, err := e.claudeJob(claudeSession)
	if err != nil {
		return loc, err
	}
	switch {
	case isJob && hasRec:
		if shown, err := e.showsJob(ctx, rec.BlockID, rec, job); err != nil || !shown {
			return loc, fmt.Errorf("%w now (it was last in %s), so it has no AMQ identity until one does", ErrJobNotShown, rec.BlockID)
		}
		return Location{Server: loc.Server, Block: rec.BlockID}, nil
	case isJob:
		return loc, fmt.Errorf("%w yet, so it has no AMQ identity; it gets one at its next prompt once a pane shows it", ErrJobNotShown)
	case hasRec:
		return Location{Server: loc.Server, Block: rec.BlockID}, nil
	case claudeSession != "":
		// A Claude Code session no hook adopted: a `claude -p` that another
		// program in the pane runs, or one that has not started a turn. The
		// pane's identity is not its.
		return loc, fmt.Errorf("this Claude Code session (%s) has no AMQ identity: its hooks have not adopted it, and the pane's agent is another", claudeSession)
	}
	return loc, nil
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

// jobStatus is the program status a doorbell for rec reads. The terminal
// client showing a background job reports none of its own, or a stale one
// from before its conversation moved to the background, so a job's status is
// its supervisor's state: working, or at rest. reports is false: an emptied
// box, not a status, confirms the doorbell (core.DecideConfirm).
func (e Env) jobStatus(rec store.Record) (status rex.Status, reports, isJob bool, err error) {
	if rec.Kind != core.Claude {
		return rex.Status{}, false, false, nil
	}
	job, isJob, err := e.claudeJob(rec.AgentSessionID)
	if err != nil || !isJob {
		return rex.Status{}, false, false, err
	}
	if job.State == "working" {
		return rex.Status{State: string(core.StateWorking)}, false, true, nil
	}
	return rex.Status{State: string(core.StateIdle)}, false, true, nil
}
