package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// Summon on send (core/summon.go for the guards): an agent addresses a peer
// by handle or role; the adapter delivers to a live one, resumes a departed
// one of this session in a pane beside the caller, or starts one, then
// delivers. Mail goes out first in every case that has a mailbox, so a
// failed resume or start leaves it waiting. A launch carries a token in its
// environment (SummonEnv); the adoption that sees it takes the lineage, and
// until then the token reserves a place in the session's cap.

// SummonEnv is the environment variable a launched agent carries.
const SummonEnv = "REX_AMQ_SUMMON"

// SummonRequest is what the summoner asks.
type SummonRequest struct {
	Target  string // a handle or a role
	Kind    string // an amq message kind; requests may summon
	Subject string
	Body    string
	// Grant is the user's grant file the brief cites, Asked the user's own
	// words asking for a new agent (self-attested, logged); starting a new
	// agent needs one of them. Neither is verified here.
	Grant, Asked string
	Harness      string // overrides the role's default harness for a start
	// Cwd is the working directory of a started agent (a Slice worktree);
	// empty means the caller's. A resume keeps the departed agent's own.
	Cwd string
}

// SummonResult is what happened. Block is set when an agent was launched
// (resume or start): the caller awaits its adoption and then delivers.
type SummonResult struct {
	Action  string        `json:"action"` // deliver, resume, start, refused
	Handles []core.Handle `json:"handles,omitempty"`
	Block   string        `json:"block,omitempty"`
	Label   string        `json:"label,omitempty"`
	Token   string        `json:"-"`
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
	// A target is a handle, or a role in the form adoption records it.
	target, isHandle := r.Target, core.LooksLikeHandle(r.Target)
	if !isHandle {
		if target = core.Role(r.Target); target == "" {
			return SummonResult{}, fmt.Errorf("%q is neither a handle (<kind>-<NN>) nor a role", r.Target)
		}
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
	for i, rec := range recs {
		if rec.Handle == caller.Handle || !matches(rec, target, isHandle) {
			continue // nobody summons themselves
		}
		switch {
		case rec.Live() && s.holds(rec):
			live = append(live, rec)
		case !rec.Live() && s.recorded(rec) && (departed == nil || rec.AdoptedAt.After(departed.AdoptedAt)):
			departed = &recs[i]
		}
	}
	res := SummonResult{Parent: caller.Handle, Depth: caller.Depth + 1}
	if r.Cwd != "" {
		// --cwd always means a new agent in that directory: a live one would get
		// the mail in its own directory and a departed one would resume in its
		// own, so neither may silently stand in. A second agent of a live role
		// starts, unless the live one already works in that directory.
		switch {
		case isHandle:
			res.Action, res.Reason = "refused", "--cwd starts a new agent for a role; "+r.Target+" names an existing agent, which keeps its own directory"
			e.summonLog(caller, r, res)
			return res, nil
		}
		for _, rec := range live {
			if sameDir(rec.Cwd, r.Cwd) {
				res.Action, res.Reason = "refused", fmt.Sprintf("a live %s (%s) already works in %s; two agents of one role in one directory would edit the same files", target, rec.Handle, r.Cwd)
				e.summonLog(caller, r, res)
				return res, nil
			}
		}
		live, departed = nil, nil // a second agent of the role, in its own directory
	}
	if len(live) > 1 {
		names := make([]string, len(live))
		for i, rec := range live {
			names[i] = string(rec.Handle)
		}
		res.Action, res.Reason = "refused", fmt.Sprintf("%d live %s agents (%s) would all get this; name one by handle, or pass --cwd to start a new one", len(live), target, strings.Join(names, ", "))
		e.summonLog(caller, r, res)
		return res, nil
	}
	if len(live) > 0 {
		for _, rec := range live {
			if err := amq.SendKind(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(rec.Handle), r.Kind, r.Subject, r.Body); err != nil {
				return res, err
			}
			res.Handles = append(res.Handles, rec.Handle)
		}
		res.Action, res.Mailed = "deliver", true
		e.summonLog(caller, r, res)
		return res, nil
	}
	if departed != nil {
		// Its mailbox waits for a resume whatever the guards say.
		if err := amq.SendKind(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(departed.Handle), r.Kind, r.Subject, r.Body); err != nil {
			return res, err
		}
		res.Mailed = true
		res.Handles = []core.Handle{departed.Handle}
	}
	chain, complete := e.chain(recs, caller)
	if !complete {
		res.Action, res.Reason = "refused", "your summon chain cannot be checked: an agent that summoned you is no longer recorded, so no new agent is started or resumed"
		return e.refused(caller, r, res, departed)
	}
	// The guards and the reservation, together under the lifecycle lock:
	// two summoners racing the cap both see the other's reservation.
	token, err := e.reserve(ctx, caller, s, r.Kind, chain, target, departed)
	if err != nil {
		var refusal core.SummonCheck
		if errors.As(err, &refusal) {
			res.Action, res.Reason = "refused", refusal.Reason
			return e.refused(caller, r, res, departed)
		}
		return res, err
	}
	res.Token = token
	if departed != nil {
		return e.resume(ctx, caller, s, r, *departed, res)
	}
	return e.start(ctx, caller, s, r, target, res)
}

func (e Env) refused(caller store.Record, r SummonRequest, res SummonResult, departed *store.Record) (SummonResult, error) {
	if departed != nil {
		res.Reason += "; the message waits in " + string(departed.Handle) + "'s mailbox for a resume"
	}
	e.summonLog(caller, r, res)
	return res, nil
}

// matches reports whether rec is addressed by target: its handle, or its
// role (in adoption's canonical form).
func matches(rec store.Record, target string, isHandle bool) bool {
	if isHandle {
		return string(rec.Handle) == target
	}
	return rec.Role != "" && rec.Role == target
}

// chain is the summon chain from the user's agent down to rec, rec last.
// complete is false when an ancestor is no longer recorded (collected after
// its seven days), so the chain cannot be proven.
func (e Env) chain(recs []store.Record, rec store.Record) (chain []core.Peer, complete bool) {
	byHandle := map[core.Handle]store.Record{}
	for _, r := range recs {
		byHandle[r.Handle] = r
	}
	cur := rec
	for len(chain) < 64 {
		chain = append([]core.Peer{peerOf(cur)}, chain...)
		if cur.Parent == "" {
			return chain, true
		}
		next, ok := byHandle[cur.Parent]
		if !ok {
			return chain, false
		}
		cur = next
	}
	return chain, false
}

// reserve runs the guards against the session's live agents plus its
// pending launches, and on success writes the summon under a fresh token,
// which reserves a place until adoption consumes it or it expires.
func (e Env) reserve(ctx context.Context, caller store.Record, s Session, kind string, chain []core.Peer, target string, departed *store.Record) (string, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	recs, err := e.Store.Records()
	if err != nil {
		return "", err
	}
	live := 0
	for _, rec := range recs {
		if rec.Live() && s.holds(rec) {
			live++
		}
	}
	pending, err := e.Store.PendingSummons(s.id, e.Now())
	if err != nil {
		return "", err
	}
	if g := core.GuardSummon(kind, chain, caller.Depth, target, live+pending); g.Refused {
		return "", g
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	sm := store.Summon{Parent: caller.Handle, Depth: caller.Depth + 1, For: target, Session: s.id, At: e.Now()}
	if departed != nil {
		// A resumed agent keeps its own lineage; the token only holds its place.
		sm.Parent, sm.Depth = departed.Parent, departed.Depth
	}
	return token, e.Store.PutSummon(token, sm)
}

func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resume starts the departed agent's native session again in a pane beside
// the caller, in the directory it ran in, with a fixed first prompt that
// makes it read its mailbox: a resumed session runs no start hook, so the
// prompt is what re-adopts it and delivers the waiting mail. Its record
// keeps its handle, lineage and mail.
func (e Env) resume(ctx context.Context, caller store.Record, s Session, r SummonRequest, departed store.Record, res SummonResult) (SummonResult, error) {
	cwd := departed.Cwd
	if cwd == "" {
		cwd = transcript.Cwd(ctx, e.harnessDirs(), departed.Kind, departed.AgentSessionID)
	}
	if cwd == "" {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", fmt.Sprintf("%s cannot be resumed: its working directory is not recorded and its transcript does not name one; the message waits in its mailbox", departed.Handle)
		e.summonLog(caller, r, res)
		return res, nil
	}
	bootstrap := fmt.Sprintf("Resumed by %s (summon on send): mail waits for you. Run eval \"$(%s)\" && amq drain --include-body and act on it.", caller.Handle, e.EnvCommand())
	cmd, ok := resumeCommand(departed.Kind, departed.AgentSessionID, bootstrap)
	if !ok {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", fmt.Sprintf("%s (%s) cannot be resumed from outside; the message waits in its mailbox", departed.Handle, departed.Kind)
		e.summonLog(caller, r, res)
		return res, nil
	}
	label := core.PaneLabel(departed.Handle, departed.Role, departed.Task)
	block, err := e.launch(ctx, caller, s, label, cwd, res.Token, cmd)
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	res.Action, res.Block, res.Label = "resume", block, label
	e.summonLog(caller, r, res)
	return res, nil
}

// start launches a new agent for a role beside the caller, with the message
// as its brief. A handle that names no agent cannot be started.
func (e Env) start(ctx context.Context, caller store.Record, s Session, r SummonRequest, role string, res SummonResult) (SummonResult, error) {
	refuse := func(reason string) (SummonResult, error) {
		_ = e.Store.DeleteSummon(res.Token)
		res.Action, res.Reason = "refused", reason
		e.summonLog(caller, r, res)
		return res, nil
	}
	if core.LooksLikeHandle(r.Target) {
		return refuse(fmt.Sprintf("no agent %s in this session, live or departed; a new agent is started by role, not by handle", r.Target))
	}
	if r.Grant != "" && r.Asked == "" {
		if _, err := os.Stat(r.Grant); err != nil {
			return refuse("the grant file " + r.Grant + " is not readable, so it authorizes nothing: " + err.Error())
		}
	}
	if r.Grant == "" && r.Asked == "" {
		return refuse("starting a new agent needs the user's own words in this session asking for it (--asked \"<their words>\", your attestation, logged) or a brief that cites their grant file (--grant <file>, which you verified and the brief names)")
	}
	kind := core.DefaultHarness(role)
	if r.Harness != "" {
		var err error
		if kind, err = core.ParseKind(r.Harness); err != nil {
			return refuse(err.Error())
		}
	}
	brief := r.Body + "\n\nYou were started by " + string(caller.Handle) + " (summon on send). Report back with `amq send --to " + string(caller.Handle) + "`; the same request waits in your mailbox once you are adopted, so you can `amq reply` to it."
	label := role + " | " + core.Task(r.Subject)
	cwd := caller.Cwd
	if r.Cwd != "" {
		info, err := os.Stat(r.Cwd)
		if err != nil || !info.IsDir() {
			return refuse("--cwd " + r.Cwd + " is not a directory, so the agent was not started")
		}
		cwd = r.Cwd
	}
	block, err := e.launch(ctx, caller, s, label, cwd, res.Token, startCommand(kind, brief))
	if err != nil {
		_ = e.Store.DeleteSummon(res.Token)
		return res, err
	}
	res.Action, res.Block, res.Label = "start", block, label
	e.summonLog(caller, r, res)
	return res, nil
}

// launch opens the pane beside the caller, or in a new tab when the
// caller's pane is too small to split, running cmd with the token in its
// environment.
func (e Env) launch(ctx context.Context, caller store.Record, s Session, label, cwd, token string, cmd []string) (string, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// An Antigravity peer in a linked worktree of a repository the user
	// trusts meets no folder dialog: the trust goes in for its session only.
	if t, err := trust.Derive(e.TranscriptHome, cwd); err == nil {
		cmd = trust.Args(cmd, t.Settings)
	}
	cmd = append([]string{"env", SummonEnv + "=" + token}, cmd...)
	if size, err := e.Rex.Size(ctx, caller.BlockID); err == nil && size.Rows > 0 && size.Rows < 2*core.TinyRows {
		_, shell, err := e.Rex.NewTab(ctx, s.id, label)
		if err != nil {
			return "", err
		}
		block, err := e.Rex.NewSplit(ctx, s.id, shell, label, cwd, cmd)
		if err != nil {
			return "", err
		}
		_ = e.Rex.CloseBlock(ctx, shell)
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

// resumeCommand resumes session in its harness with a first prompt: every
// CLI here takes one beside its resume flag.
func resumeCommand(kind core.Kind, session, prompt string) ([]string, bool) {
	if session == "" {
		return nil, false
	}
	switch kind {
	case core.Claude:
		return []string{"claude", "--resume", session, prompt}, true
	case core.Codex:
		return []string{"codex", "--no-daemon", "resume", session, prompt}, true
	case core.Agy:
		return []string{"agy", "--dangerously-skip-permissions", "--conversation", session, "-i", prompt}, true
	case core.Pi:
		return []string{"pi", "--session", session, prompt}, true
	}
	return nil, false
}

// Deliver mails body to h in the caller's name.
func (e Env) Deliver(ctx context.Context, loc Location, h core.Handle, kind, subject, body string) error {
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return err
	}
	if !ok || !caller.Live() {
		return errors.New("no AMQ identity for this pane")
	}
	return amq.SendKind(ctx, e.AmqBin, e.Paths.Root(), string(caller.Handle), string(h), kind, subject, body)
}

// summonLog appends one JSON line per summon to logs/summon.log. It is an
// audit trail: a failure is reported to Diagnostics and never fails the
// summon, whose mail or pane already exists.
func (e Env) summonLog(caller store.Record, r SummonRequest, res SummonResult) {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "summoner": caller.Handle, "parent": caller.Parent, "depth": caller.Depth, "target": r.Target, "kind": r.Kind, "subject": r.Subject, "action": res.Action, "handles": res.Handles, "block": res.Block, "reason": res.Reason, "grant": r.Grant, "asked": r.Asked}
	b, _ := json.Marshal(entry)
	write := func() error {
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
	if err := write(); err != nil {
		fmt.Fprintf(Diagnostics, "summon log: %v\n", err)
	}
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

// sameDir reports whether two paths name one directory; an unknown path matches nothing.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}
