package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/transcript"
)

// Summon on send (core/summon.go for the guards): an agent addresses a peer
// by handle or role; the adapter delivers to a live one, resumes a departed
// one of this session in a pane beside the caller, or starts one, then
// delivers. Mail goes out first in every case that has a mailbox, so a
// failed resume or start leaves it waiting.

// SummonRequest is what the summoner asks.
type SummonRequest struct {
	Target  string // a handle or a role
	Kind    string // an amq message kind; requests may summon
	Subject string
	Body    string
	// Grant is the user's grant file the brief cites, Asked the user's own
	// words asking for a new agent (self-attested, logged); starting a new
	// agent needs one of them.
	Grant, Asked string
	Harness      string // overrides the role's default harness for a start
}

// SummonResult is what happened. Block is set when an agent was launched
// (resume or start): the caller awaits its adoption and then delivers.
type SummonResult struct {
	Action  string        `json:"action"` // deliver, resume, start, refused
	Handles []core.Handle `json:"handles,omitempty"`
	Block   string        `json:"block,omitempty"`
	Label   string        `json:"label,omitempty"`
	Reason  string        `json:"reason,omitempty"`
	Parent  core.Handle   `json:"parent,omitempty"`
	Depth   int           `json:"depth"`
	Mailed  bool          `json:"mailed"` // the message is in a mailbox already
}

// Summon resolves r.Target in the caller's session and acts.
func (e Env) Summon(ctx context.Context, loc Location, r SummonRequest) (SummonResult, error) {
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return SummonResult{}, err
	}
	if !ok || !caller.Live() {
		return SummonResult{}, errors.New("no AMQ identity for this pane, so nothing can summon")
	}
	if r.Kind == "" {
		r.Kind = "question"
	}
	s, err := e.FindSession(ctx, loc.Block)
	if err != nil {
		return SummonResult{}, err
	}
	recs, err := e.Store.Records()
	if err != nil {
		return SummonResult{}, err
	}
	var live []store.Record
	var departed *store.Record
	liveCount := 0
	for i, rec := range recs {
		if rec.Live() && s.holds(rec) {
			liveCount++
		}
		if !matches(rec, r.Target) {
			continue
		}
		switch {
		case rec.Live() && s.holds(rec):
			live = append(live, rec)
		case !rec.Live() && s.recorded(rec) && (departed == nil || rec.AdoptedAt.After(departed.AdoptedAt)):
			departed = &recs[i]
		}
	}
	res := SummonResult{Parent: caller.Handle, Depth: caller.Depth + 1}
	if len(live) > 0 {
		for _, rec := range live {
			if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(rec.Handle), r.Subject, r.Body); err != nil {
				return res, err
			}
			res.Handles = append(res.Handles, rec.Handle)
		}
		res.Action, res.Mailed = "deliver", true
		return res, e.summonLog(caller, r, res)
	}
	chain := e.chain(recs, caller)
	if departed != nil {
		// Its mailbox waits for a resume whatever the guards say.
		if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(departed.Handle), r.Subject, r.Body); err != nil {
			return res, err
		}
		res.Mailed = true
		res.Handles = []core.Handle{departed.Handle}
	}
	if g := core.GuardSummon(r.Kind, chain, caller.Depth, r.Target, liveCount); g.Refused {
		res.Action, res.Reason = "refused", g.Reason
		if departed != nil {
			res.Reason += "; the message waits in " + string(departed.Handle) + "'s mailbox for a resume"
		}
		return res, e.summonLog(caller, r, res)
	}
	if departed != nil {
		return e.resume(ctx, caller, s, r, *departed, res)
	}
	return e.start(ctx, caller, s, r, res)
}

// matches reports whether rec is addressed by target: its handle, or its role.
func matches(rec store.Record, target string) bool {
	return string(rec.Handle) == target || rec.Role != "" && rec.Role == target
}

// chain is the summon chain from the user's agent down to rec, rec last.
func (e Env) chain(recs []store.Record, rec store.Record) []core.Peer {
	byHandle := map[core.Handle]store.Record{}
	for _, r := range recs {
		byHandle[r.Handle] = r
	}
	var chain []core.Peer
	for cur, ok := rec, true; ok && len(chain) < 64; cur, ok = byHandle[cur.Parent] {
		chain = append([]core.Peer{peerOf(cur)}, chain...)
		if cur.Parent == "" {
			break
		}
	}
	return chain
}

// resume starts the departed agent's native session again in a pane beside
// the caller, in the directory it ran in. Its record keeps its handle,
// lineage and mail.
func (e Env) resume(ctx context.Context, caller store.Record, s Session, r SummonRequest, departed store.Record, res SummonResult) (SummonResult, error) {
	cwd := departed.Cwd
	if cwd == "" {
		cwd = e.transcriptCwd(ctx, departed)
	}
	if cwd == "" {
		res.Action, res.Reason = "refused", fmt.Sprintf("%s cannot be resumed: its working directory is not recorded and its transcript does not name one; the message waits in its mailbox", departed.Handle)
		return res, e.summonLog(caller, r, res)
	}
	cmd, ok := resumeCommand(departed.Kind, departed.AgentSessionID)
	if !ok {
		res.Action, res.Reason = "refused", fmt.Sprintf("%s (%s) cannot be resumed from outside; the message waits in its mailbox", departed.Handle, departed.Kind)
		return res, e.summonLog(caller, r, res)
	}
	label := core.PaneLabel(departed.Handle, departed.Role, departed.Task)
	block, err := e.launch(ctx, caller, s, label, cwd, cmd)
	if err != nil {
		return res, err
	}
	res.Action, res.Block, res.Label = "resume", block, label
	return res, e.summonLog(caller, r, res)
}

// start launches a new agent for a role beside the caller, with the message
// as its brief. A handle that names no agent cannot be started.
func (e Env) start(ctx context.Context, caller store.Record, s Session, r SummonRequest, res SummonResult) (SummonResult, error) {
	if core.LooksLikeHandle(r.Target) {
		res.Action, res.Reason = "refused", fmt.Sprintf("no agent %s in this session, live or departed; a new agent is started by role, not by handle", r.Target)
		return res, e.summonLog(caller, r, res)
	}
	if r.Grant == "" && r.Asked == "" {
		res.Action, res.Reason = "refused", "starting a new agent needs the user's own words in this session asking for it (--asked \"<their words>\") or a brief that cites their grant file (--grant <file>)"
		return res, e.summonLog(caller, r, res)
	}
	kind := core.DefaultHarness(r.Target)
	if r.Harness != "" {
		var err error
		if kind, err = core.ParseKind(r.Harness); err != nil {
			return res, err
		}
	}
	brief := r.Body + "\n\nYou were started by " + string(caller.Handle) + " (summon on send). Report back with `amq send --to " + string(caller.Handle) + "`; the same message waits in your mailbox so you can `amq reply` to it."
	label := r.Target + " | " + core.Task(r.Subject)
	if err := e.Store.PutSummon(s.id, label, store.Summon{Parent: caller.Handle, Depth: caller.Depth + 1, For: r.Subject, At: e.Now()}); err != nil {
		return res, err
	}
	block, err := e.launch(ctx, caller, s, label, caller.Cwd, startCommand(kind, brief))
	if err != nil {
		return res, err
	}
	res.Action, res.Block, res.Label = "start", block, label
	return res, e.summonLog(caller, r, res)
}

// launch opens the pane beside the caller, or in a new tab when the
// caller's pane is too small to split.
func (e Env) launch(ctx context.Context, caller store.Record, s Session, label, cwd string, cmd []string) (string, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if size, err := e.Rex.Size(ctx, caller.BlockID); err == nil && size.Rows > 0 && size.Rows < 2*core.TinyRows {
		window, shell, err := e.Rex.NewTab(ctx, s.id, label)
		if err != nil {
			return "", err
		}
		block, err := e.Rex.NewSplit(ctx, s.id, shell, label, cwd, cmd)
		if err != nil {
			return "", err
		}
		_ = e.Rex.CloseBlock(ctx, shell)
		_ = window
		return block, nil
	}
	return e.Rex.NewSplit(ctx, s.id, caller.BlockID, label, cwd, cmd)
}

func startCommand(kind core.Kind, brief string) []string {
	switch kind {
	case core.Codex:
		return []string{"codex", "--no-daemon", brief}
	case core.Agy:
		return []string{"agy", "--dangerously-skip-permissions", "-i", brief}
	case core.Pi:
		return []string{"pi", brief}
	}
	return []string{"claude", brief}
}

func resumeCommand(kind core.Kind, session string) ([]string, bool) {
	if session == "" {
		return nil, false
	}
	switch kind {
	case core.Claude:
		return []string{"claude", "--resume", session}, true
	case core.Codex:
		return []string{"codex", "--no-daemon", "resume", session}, true
	case core.Agy:
		return []string{"agy", "--dangerously-skip-permissions", "--conversation", session}, true
	case core.Pi:
		return []string{"pi", "--session", session}, true
	}
	return nil, false
}

// transcriptCwd reads the working directory a departed agent's transcript
// names, for records written before cwd was recorded.
func (e Env) transcriptCwd(ctx context.Context, rec store.Record) string {
	return transcript.Cwd(ctx, e.TranscriptHome, rec.Kind, rec.AgentSessionID)
}

// summonLog appends one JSON line per summon to logs/summon.log.
func (e Env) summonLog(caller store.Record, r SummonRequest, res SummonResult) error {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "summoner": caller.Handle, "parent": caller.Parent, "depth": caller.Depth, "target": r.Target, "kind": r.Kind, "subject": r.Subject, "action": res.Action, "handles": res.Handles, "block": res.Block, "reason": res.Reason, "grant": r.Grant, "asked": r.Asked}
	b, _ := json.Marshal(entry)
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "summon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Summons lists the summons a handle made, newest last, for its hand-back.
func (e Env) Summons(h core.Handle) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(e.Paths.Logs(), "summon.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["summoner"] != string(h) {
			continue
		}
		var hs []string
		if arr, ok := m["handles"].([]any); ok {
			for _, x := range arr {
				hs = append(hs, fmt.Sprint(x))
			}
		}
		sort.Strings(hs)
		out = append(out, fmt.Sprintf("%s %s %s -> %s: %s", m["at"], m["action"], m["target"], strings.Join(hs, ","), m["subject"]))
	}
	return out, nil
}

// Deliver mails body to h in the caller's name.
func (e Env) Deliver(ctx context.Context, loc Location, h core.Handle, subject, body string) error {
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return err
	}
	if !ok || !caller.Live() {
		return errors.New("no AMQ identity for this pane")
	}
	return amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(h), subject, body)
}
