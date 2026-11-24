package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Conversation ids of the revive world's agents, shaped as Pi writes them.
const (
	orchSID    = "01a1f23a-0000-7000-8000-00000000000a"
	narrSID    = "01a1f23a-0000-7000-8000-00000000000b"
	workerSID  = "01a1f23a-0000-7000-8000-00000000000c"
	retiredSID = "01a1f23a-0000-7000-8000-00000000000d"
	reviveSess = "session:epic"
	shellPID   = 424242
	deadPID    = 2147480000 // no such process: an agent that died with the server
)

// reviveWorld is an Epic (session:epic, linked to the personal agent
// claude-01 in session:me) right after a Rex restart: its orchestrator
// (block:o) and narrator (block:n) still hold leases of processes that are
// gone, its implementer (block:w) was parked agent_gone, and a reviewer
// (block:r) was retired before the restart. Every block shows an idle fish
// prompt. A command typed into a block adopts the Pi conversation it
// resumes there, as the resumed harness's first hook would.
type reviveWorld struct {
	t    *testing.T
	e    Env
	term *fakeTerm
	dir  string
	env  map[string]string
	diag *bytes.Buffer
	mu   sync.Mutex
	sids map[string]string // block -> the conversation a write there resumes
}

func newReviveWorld(t *testing.T) *reviveWorld {
	t.Helper()
	fish := rex.Process{PID: shellPID, Name: "-fish"}
	prompt := "\x1b[1m~/app\x1b[0m at 21:38\r\n\x1b[1m❯\x1b[0m \r\n\r\n"
	term := &fakeTerm{state: "idle",
		windows:  map[string]string{"block:me": "window:me", "block:o": "window:o", "block:n": "window:n", "block:w": "window:w", "block:r": "window:r"},
		sessions: map[string]string{"window:me": "session:me", "window:o": reviveSess, "window:n": reviveSess, "window:w": reviveSess, "window:r": reviveSess},
		tabs:     map[string]string{"window:me": "me", "window:o": placeholder, "window:n": placeholder, "window:w": placeholder, "window:r": placeholder},
		fgs:      map[string]rex.Process{"block:o": fish, "block:n": fish, "block:w": fish, "block:r": fish},
		screens:  map[string]string{"block:o": prompt, "block:n": prompt, "block:w": prompt, "block:r": prompt},
	}
	e := realAmqEnv(t, term)
	e.TranscriptHome = t.TempDir()
	var diag bytes.Buffer
	old := Diagnostics
	Diagnostics = &diag
	t.Cleanup(func() { Diagnostics = old })
	w := &reviveWorld{t: t, e: e, term: term, dir: workDir(t, e, "app"), diag: &diag,
		env:  map[string]string{"REX_BLOCK": "block:me", "REX_SERVER": fakeServer},
		sids: map[string]string{"block:o": orchSID, "block:n": narrSID, "block:w": workerSID, "block:r": retiredSID}}
	long := time.Now().Add(-time.Hour)
	put(t, e,
		seed{Handle: "claude-01", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me", SessionID: "session:me", WindowID: "window:me", AgentPID: os.Getpid(), AdoptedAt: time.Now()},
		seed{Handle: "pi-01", Kind: core.Pi, Role: core.OrchestratorRole, BlockID: "block:o", SessionID: reviveSess, WindowID: "window:o", AgentPID: deadPID, AgentSessionID: orchSID, Cwd: w.dir, Unattended: true, Launched: true, Model: "anthropic/claude-opus", Effort: "high", AdoptedAt: long},
		seed{Handle: "pi-02", Kind: core.Pi, Role: core.NarratorRole, BlockID: "block:n", SessionID: reviveSess, WindowID: "window:n", AgentPID: deadPID, AgentSessionID: narrSID, Cwd: w.dir, Unattended: true, AdoptedAt: long},
		parkedFor(seed{Handle: "pi-03", Kind: core.Pi, Role: "implementer", BlockID: "block:w", SessionID: reviveSess, WindowID: "window:w", AgentPID: deadPID, AgentSessionID: workerSID, Cwd: w.dir, Unattended: true, AdoptedAt: long}, "agent_gone"),
		parkedFor(seed{Handle: "pi-04", Kind: core.Pi, Role: "reviewer", BlockID: "block:r", SessionID: reviveSess, WindowID: "window:r", AgentPID: deadPID, AgentSessionID: retiredSID, Cwd: w.dir, Unattended: true, AdoptedAt: long}, store.DepartRetired),
	)
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyEpic, SessionID: reviveSess, Server: fakeServer, LeadRole: core.OrchestratorRole, LeadHandle: "pi-01", Unattended: true, CreatedAt: long}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: reviveSess, Server: fakeServer, Name: "login revamp", Dir: w.dir, PersonalHandle: "claude-01", State: store.EpicOpened, UpdatedAt: long}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:me"}, Epic: core.SessionKey{Server: fakeServer, SessionID: reviveSess}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	term.wrote = func(block, data string) {
		w.mu.Lock()
		sid := w.sids[block]
		w.mu.Unlock()
		if sid == "" || !strings.HasPrefix(data, "/bin/sh ") {
			return
		}
		// Adopted in the background: the revive holds the lifecycle lock
		// while it types, as the real resumed harness's hook would wait.
		wg.Add(1)
		go func() { defer wg.Done(); w.adopt(block, sid) }()
	}
	term.started = func(l launch) {
		sid := launchedSession(l.Command)
		if sid == "" {
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); w.adopt(l.Block, sid) }()
	}
	return w
}

// launchedSession is the Pi conversation a tab's command resumes: the
// revive's launcher it runs (`/bin/sh <script>`), read as the shell would,
// or a bare `pi --session <id>`. It claims the launcher as running it would,
// so a retry sees the launch started.
func launchedSession(cmd []string) string {
	if len(cmd) == 2 && cmd[0] == "/bin/sh" {
		b, err := os.ReadFile(cmd[1])
		if err != nil {
			return ""
		}
		_ = os.Remove(cmd[1])
		_, rest, _ := strings.Cut(string(b), "'--session' '")
		sid, _, _ := strings.Cut(rest, "'")
		return sid
	}
	if i := slices.Index(cmd, "--session"); i >= 0 && i+1 < len(cmd) {
		return cmd[i+1]
	}
	return ""
}

// parkedFor is s parked for reason a while ago.
func parkedFor(s seed, reason string) seed {
	at := time.Now().Add(-30 * time.Minute)
	s.DepartedAt, s.DepartReason = &at, reason
	return s
}

func (w *reviveWorld) adopt(block, sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.e.Hook(ctx, EventSessionStart, core.Pi, Location{Block: block}, HookInput{SessionID: sid, AgentPID: 1}); err != nil {
		w.t.Errorf("adopt %s in %s: %v", sid, block, err)
	}
}

func (w *reviveWorld) revive(r ReviveRequest) (ReviveResult, error) {
	w.t.Helper()
	if r.Session == "" {
		r.Session = reviveSess
	}
	if r.Timeout == 0 {
		r.Timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.e.ReviveEpic(ctx, func(k string) string { return w.env[k] }, r)
}

func (w *reviveWorld) writes(block string) []string {
	w.term.mu.Lock()
	defer w.term.mu.Unlock()
	var out []string
	for _, x := range w.term.writesTo {
		if b, data, _ := strings.Cut(x, "<-"); b == block {
			out = append(out, data)
		}
	}
	return out
}

func byHandle(res ReviveResult) map[core.Handle]ReviveAgent {
	m := map[core.Handle]ReviveAgent{}
	for _, a := range res.Agents {
		m[a.Handle] = a
	}
	return m
}

// A revive resumes the lead, the narrator, and the implementer that died
// with the server, each in its own block under the same handle and its
// latest conversation, and leaves the retired reviewer down. Run again it
// finds them live and types nothing (already_live), and the same op id
// replays; the op id for another request is refused.
func TestReviveResumesLeadNarratorAndWorkerInPlaceAndIsIdempotent(t *testing.T) {
	w := newReviveWorld(t)
	res, err := w.revive(ReviveRequest{OpID: "me:revive#1", Asked: "把 Epic 恢复起来"})
	if err != nil {
		t.Fatalf("revive: %v\n%s", err, w.diag)
	}
	got := byHandle(res)
	if len(got) != 3 || !res.Ready() || res.LifecycleState != store.EpicOpened || !res.Linked {
		t.Fatalf("result %+v\n%s", res, w.diag)
	}
	for h, block := range map[core.Handle]string{"pi-01": "block:o", "pi-02": "block:n", "pi-03": "block:w"} {
		a := got[h]
		if a.Action != ReviveResumedInPlace || a.Status != ReviveReady || a.Block != block {
			t.Errorf("%s: %+v", h, a)
		}
		rec := recordOf(t, w.e, h)
		if !rec.Live() || rec.BlockID != block {
			t.Errorf("%s after the revive: live=%v block=%s", h, rec.Live(), rec.BlockID)
		}
		if n := len(w.writes(block)); n != 1 {
			t.Errorf("%s's block got %d writes, want 1", h, n)
		}
	}
	if _, ok := got["pi-04"]; ok || recordOf(t, w.e, "pi-04").Live() || len(w.writes("block:r")) != 0 {
		t.Errorf("the retired reviewer was revived: %+v", got["pi-04"])
	}
	if a := got["pi-01"]; a.AgentSessionID != orchSID || a.Kind != core.Pi || a.Role != core.OrchestratorRole || a.Cwd != w.dir {
		t.Errorf("the lead's row: %+v", a)
	}

	again, err := w.revive(ReviveRequest{OpID: "me:revive#1"})
	if err != nil || !again.Replayed || !again.Ready() {
		t.Fatalf("replay: %+v %v", again, err)
	}
	for _, a := range again.Agents {
		if a.Action != ReviveAlreadyLive || a.Status != ReviveReady {
			t.Errorf("replay of %s: %+v", a.Handle, a)
		}
	}
	for _, b := range []string{"block:o", "block:n", "block:w"} {
		if n := len(w.writes(b)); n != 1 {
			t.Errorf("a retry typed into %s again: %d writes", b, n)
		}
	}
	w.env["REX_BLOCK"] = "" // the user, from a terminal of their own
	w.e.Ancestors = func() ([]proc, error) { return []proc{{PID: 1, Name: "launchd"}}, nil }
	if _, err := w.revive(ReviveRequest{OpID: "me:revive#1"}); !isCode(err, fault.Conflict) {
		t.Errorf("the op id for another caller: %v", err)
	}
	if res, err := w.revive(ReviveRequest{}); err != nil || len(res.Agents) != 3 {
		t.Errorf("the user reviving it: %+v %v", res, err)
	}
}

// A dry run reports who would resume where and changes nothing: no lease
// is released, no block is typed into, no op id recorded.
func TestReviveDryRunTouchesNothing(t *testing.T) {
	w := newReviveWorld(t)
	res, err := w.revive(ReviveRequest{DryRun: true, OpID: "dry"})
	if err != nil {
		t.Fatal(err)
	}
	got := byHandle(res)
	if !res.DryRun || len(got) != 3 || got["pi-01"].Action != reviveInPlacePlan || got["pi-02"].Action != reviveInPlacePlan || got["pi-03"].Action != reviveInPlacePlan {
		t.Fatalf("plan %+v", res)
	}
	if got["pi-01"].Block != "block:o" || got["pi-01"].AgentSessionID != orchSID {
		t.Errorf("lead row %+v", got["pi-01"])
	}
	w.term.mu.Lock()
	writes, launches := len(w.term.writesTo), len(w.term.launches)
	w.term.mu.Unlock()
	if writes != 0 || launches != 0 {
		t.Errorf("a dry run typed %d times and opened %d tabs", writes, launches)
	}
	if l, ok, _ := w.e.Store.LeaseOf("pi-01"); !ok || l.Incarnation.PID != deadPID {
		t.Errorf("a dry run released the stale lease: %+v %v", l, ok)
	}
	if _, found, _ := w.e.Store.EpicOp(store.EpicOpRevive, "dry"); found {
		t.Error("a dry run recorded its op id")
	}
}

// A closed Epic is never revived, a session with no lifecycle is no Epic,
// and one Rex no longer has has nothing to revive in; nothing is typed.
func TestReviveRefusesClosedMissingAndGoneEpics(t *testing.T) {
	w := newReviveWorld(t)
	if err := w.e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:me"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:nope"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.revive(ReviveRequest{Session: "session:nope"}); !isCode(err, fault.NotFound) {
		t.Errorf("no lifecycle: %v", err)
	}
	w.term.mu.Lock()
	w.term.killed = append(w.term.killed, reviveSess)
	w.term.mu.Unlock()
	if _, err := w.revive(ReviveRequest{}); !isCode(err, fault.NotFound) {
		t.Errorf("session gone: %v", err)
	}
	if err := w.e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: reviveSess, Server: fakeServer, State: store.EpicClosed, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err := w.revive(ReviveRequest{})
	if f, ok := fault.As(err); !ok || f.Code != fault.Conflict || f.Details["reason"] != "epic_closed" {
		t.Errorf("closed: %v", err)
	}
	if len(w.term.writesTo) != 0 {
		t.Errorf("typed %v", w.term.writesTo)
	}
}

// Only the user or a personal agent of a Hub the Epic is linked to revives
// it: an Epic's own agent, and a personal agent it is not linked to, are
// refused before anything is reconciled.
func TestReviveRefusesWhoMayNot(t *testing.T) {
	w := newReviveWorld(t)
	put(t, w.e, seed{Handle: "claude-09", Kind: core.Claude, Role: "implementer", BlockID: "block:x", SessionID: "session:other", AgentPID: 1, AdoptedAt: time.Now()})
	w.env["REX_BLOCK"] = "block:x"
	if _, err := w.revive(ReviveRequest{}); err == nil || !isCode(err, fault.GuardDenied) {
		t.Errorf("a worker: %v", err)
	}
	if _, err := w.e.Store.DeleteLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:me"}, Epic: core.SessionKey{Server: fakeServer, SessionID: reviveSess}}); err != nil {
		t.Fatal(err)
	}
	w.env["REX_BLOCK"] = "block:me"
	if _, err := w.revive(ReviveRequest{}); !isCode(err, fault.PermissionDenied) {
		t.Errorf("an unlinked personal agent: %v", err)
	}
	if l, ok, _ := w.e.Store.LeaseOf("pi-01"); !ok || l.Incarnation.PID != deadPID {
		t.Errorf("a refused revive reconciled the Epic: %+v %v", l, ok)
	}
}

// A block whose prompt holds someone's unsent input, or that runs another
// program, is never typed into: that agent is refused with its reason, the
// others are resumed, and the result is not ready (the CLI's exit 7).
func TestReviveNeverClobbersADraftOrAProgram(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	w.term.screens["block:w"] = "\x1b[1m❯\x1b[0m git push --force\r\n"
	w.term.fgs["block:n"] = rex.Process{PID: 77, Name: "vim"}
	w.term.mu.Unlock()
	res, err := w.revive(ReviveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := byHandle(res)
	if a := got["pi-03"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "no fresh shell's empty prompt") {
		t.Errorf("the draft's block: %+v", a)
	}
	if a := got["pi-02"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "runs vim") {
		t.Errorf("the program's block: %+v", a)
	}
	if a := got["pi-01"]; a.Action != ReviveResumedInPlace || a.Status != ReviveReady {
		t.Errorf("the lead: %+v", a)
	}
	if res.Ready() {
		t.Error("a partial revive reads as ready")
	}
	if len(w.writes("block:w")) != 0 || len(w.writes("block:n")) != 0 {
		t.Errorf("typed into a refused block: %v %v", w.writes("block:w"), w.writes("block:n"))
	}
}

// A lead whose block was closed while its session stayed gets a tab of its
// own running the same resume; a worker whose block is gone is refused.
func TestReviveRespawnsAClosedLeadBlockInANewTab(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	delete(w.term.windows, "block:o")
	delete(w.term.windows, "block:w")
	w.term.mu.Unlock()
	res, err := w.revive(ReviveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := byHandle(res)
	lead := got["pi-01"]
	if lead.Action != ReviveRespawned || lead.Status != ReviveReady || lead.Block != "block:launch-1" {
		t.Fatalf("the lead: %+v\n%s", lead, w.diag)
	}
	if rec := recordOf(t, w.e, "pi-01"); !rec.Live() || rec.BlockID != "block:launch-1" || rec.Session() != orchSID {
		t.Errorf("the lead's record: %+v", rec)
	}
	w.term.mu.Lock()
	cmd := w.term.launches[0].Command
	w.term.mu.Unlock()
	if len(cmd) != 2 || cmd[0] != "/bin/sh" || !strings.HasPrefix(cmd[1], w.e.Paths.Home) {
		t.Errorf("the respawned command runs no launcher: %q", cmd)
	}
	if a := got["pi-03"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "no longer in the session") {
		t.Errorf("the worker whose block is gone: %+v", a)
	}
}

// The command typed in place runs a launcher script whose exec is the
// harness's resume with the model and effort the agent was launched on,
// the launch env adoption records, and ME_UNATTENDED=1.
func TestReviveLauncherCarriesUnattendedAndLaunchFlags(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	w.term.wrote = nil // nothing adopts: the launcher is read before it would run
	w.term.mu.Unlock()
	res, err := w.revive(ReviveRequest{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(res)["pi-01"]; a.Action != ReviveResumedInPlace || a.Status != string(core.StartupTimeout) || res.Ready() {
		t.Fatalf("an agent never adopted: %+v", a)
	}
	writes := w.writes("block:o")
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "/bin/sh '") || !strings.HasSuffix(writes[0], "'\r") {
		t.Fatalf("typed %q", writes)
	}
	script := strings.TrimSuffix(strings.TrimPrefix(writes[0], "/bin/sh '"), "'\r")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	want := "exec 'env' 'ME_UNATTENDED=1' 'REX_AMQ_MODEL=anthropic/claude-opus' 'REX_AMQ_EFFORT=high' 'pi' '--model' 'anthropic/claude-opus' '--thinking' 'high' '--session' '" + orchSID + "' "
	if !strings.Contains(body, want) || !strings.Contains(body, "cd -- '"+w.dir+"' || exit 1") || !strings.Contains(body, "rm -- '"+script+"' 2>/dev/null || exit 0") {
		t.Errorf("launcher:\n%s\nwant %s", body, want)
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("launcher mode: %v %v", info, err)
	}
}

// reviveCommand puts each harness's model and effort flags where its CLI
// takes them on a resume, records them for the adoption, and marks the
// agent unattended in its own command.
func TestReviveCommandPerHarness(t *testing.T) {
	cases := []struct {
		kind   core.Kind
		launch core.Launch
		want   []string
	}{
		{core.Claude, core.Launch{Model: "opus", Effort: "high"}, []string{"claude", "--model", "opus", "--effort", "high", "--resume", "s-1", "P"}},
		{core.Codex, core.Launch{Model: "gpt-5", Effort: "off"}, []string{"codex", "-m", "gpt-5", "-c", "model_reasoning_effort=none", "--no-daemon", "resume", "s-1", "P"}},
		{core.Agy, core.Launch{Model: "gemini-4-hydrogen-low"}, []string{"agy", "--model", "gemini-4-hydrogen-low", "--dangerously-skip-permissions", "--mode", "auto", "--conversation", "s-1", "-i", "P"}},
		{core.Pi, core.Launch{Effort: "max"}, []string{"pi", "--thinking", "max", "--session", "s-1", "P"}},
	}
	for _, c := range cases {
		rec := agent{Identity: store.Identity{Kind: c.kind, Sessions: []string{"old", "s-1"}}}
		cmd, ok := reviveCommand(rec, c.launch, "P", true, slices.Clip)
		if !ok {
			t.Fatalf("%s: not resumable", c.kind)
		}
		env := []string{"env", "ME_UNATTENDED=1", LaunchModelEnv + "=" + c.launch.Model, LaunchEffortEnv + "=" + c.launch.Effort}
		if !slices.Equal(cmd, append(env, c.want...)) {
			t.Errorf("%s: %q", c.kind, cmd)
		}
		if cmd, _ := reviveCommand(rec, c.launch, "P", false, slices.Clip); slices.Contains(cmd, "ME_UNATTENDED=1") {
			t.Errorf("%s attended: %q", c.kind, cmd)
		}
	}
}

// A conversation id read from disk goes on a command line only when it is
// one: a leading dash would be a flag, and quotes or spaces no id at all.
func TestReviveRefusesAMalformedConversationID(t *testing.T) {
	for _, bad := range []string{"", "-rf", "a b c d e f", "x'; rm -rf ~", "a.b"} {
		if core.ValidateResumeSessionID(core.Pi, bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{orchSID, "f0ee617e-1dca-4766-af95-f8c451ccb7d7"} {
		if err := core.ValidateResumeSessionID(core.Claude, good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	w := newReviveWorld(t)
	rec := recordOf(t, w.e, "pi-01")
	s := seedOf(rec)
	s.AgentSessionID = "-bad"
	if err := unplant(w.e.Store, "pi-01"); err != nil {
		t.Fatal(err)
	}
	put(t, w.e, s)
	res, err := w.revive(ReviveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(res)["pi-01"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "conversation id") || len(w.writes("block:o")) != 0 {
		t.Errorf("a malformed id: %+v", a)
	}
}

func isCode(err error, c fault.Code) bool {
	f, ok := fault.As(err)
	return ok && f.Code == c
}

// A draft that ends in a prompt glyph is still a draft: typing the resume
// after it would submit it (`echo pending # /bin/sh …` runs echo). Nothing
// is typed, and the agent is refused (codex-27's round-1 S1).
func TestReviveRefusesADraftEndingInAPromptGlyph(t *testing.T) {
	for _, draft := range []string{"❯ echo pending # \r\n", "user@host:~$ echo $\r\n", "user@host:~$ echo \"a\r\n> \r\n"} {
		w := newReviveWorld(t)
		w.term.mu.Lock()
		w.term.screens["block:w"] = draft
		w.term.mu.Unlock()
		res, err := w.revive(ReviveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if a := byHandle(res)["pi-03"]; a.Action != ReviveRefused || len(w.writes("block:w")) != 0 {
			t.Errorf("%q: %+v, writes %q", draft, a, w.writes("block:w"))
		}
	}
}

// A launch still starting when the revive's wait ran out is recorded, so
// a retry (the same op id, or none) waits for that launch instead of
// starting the conversation again in another tab or typing into the pane
// it runs in; once it is adopted the retry reports it ready and forgets it
// (codex-27's round-1 S2).
func TestReviveRetryWaitsForALaunchStillStarting(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	delete(w.term.windows, "block:o")
	started := w.term.started
	w.term.started = nil // the replacement harness runs, its first hook not yet
	w.term.mu.Unlock()
	first, err := w.revive(ReviveRequest{OpID: "pending", Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(first)["pi-01"]; a.Action != ReviveRespawned || a.Status != string(core.StartupTimeout) {
		t.Fatalf("first: %+v", a)
	}
	if p, _ := w.e.Store.PendingRevives(reviveSess); p["pi-01"].Block != "block:launch-1" || p["pi-01"].AgentSessionID != orchSID {
		t.Fatalf("pending after a timeout: %+v", p)
	}
	for _, op := range []string{"pending", ""} {
		again, err := w.revive(ReviveRequest{OpID: op, Timeout: 30 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if a := byHandle(again)["pi-01"]; a.Action != ReviveRespawned || a.Block != "block:launch-1" {
			t.Errorf("retry %q: %+v", op, a)
		}
	}
	w.term.mu.Lock()
	n := len(w.term.launches)
	w.term.mu.Unlock()
	if n != 1 {
		t.Fatalf("the conversation was started %d times", n)
	}
	started(launch{Block: "block:launch-1", Command: []string{"pi", "--session", orchSID}}) // its first hook runs
	done, err := w.revive(ReviveRequest{})
	if err != nil || byHandle(done)["pi-01"].Status != ReviveReady {
		t.Fatalf("after its adoption: %+v %v", done, err)
	}
	if p, _ := w.e.Store.PendingRevives(reviveSess); len(p) != 0 {
		t.Errorf("pending launches left: %+v", p)
	}
}

// A project the user trusts through the adapter's grant passes the
// preflight, so the resumed agent must carry that session trust too: Pi's
// --approve and Antigravity's --settings go on the harness's own argv,
// before the env wrapper (codex-27's round-1 S3).
func TestReviveCarriesTheSessionTrustOfAGrantedProject(t *testing.T) {
	w := newReviveWorld(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(w.dir, ".pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, ".pi", "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.trustGate().Grant(ctx, w.dir, "explicit"); err != nil {
		t.Fatal(err)
	}
	key := core.SessionKey{Server: fakeServer, SessionID: reviveSess}
	s, err := w.e.loadSession(ctx, key.Server, key.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordOf(t, w.e, "pi-03")
	p := revivePlan{rec: rec, ReviveAgent: ReviveAgent{Cwd: rec.Cwd}}
	if refusal, err := w.e.planReviveAgent(ctx, key, s, agent{}, false, &p); err != nil || refusal != "" {
		t.Fatalf("plan: %s %v", refusal, err)
	}
	if i := slices.Index(p.cmd, "pi"); i < 0 || !slices.Contains(p.cmd[i:], "--approve") {
		t.Errorf("pi: %q", p.cmd)
	}
	agy := agent{Identity: store.Identity{Kind: core.Agy, Sessions: []string{workerSID}}}
	cmd, _ := reviveCommand(agy, core.Launch{}, "P", true, func(argv []string) []string { return w.e.trustGate().Args(w.dir, argv) })
	if i := slices.Index(cmd, "agy"); i < 0 || cmd[i+1] != "--settings" {
		t.Errorf("agy: %q", cmd)
	}
}

// An unrecognised prompt is never taken for an empty one, whatever its
// line ends in: `app: echo pending # ` (PS1='app: ') may be a draft
// (codex-27's round-2 S1).
func TestReviveRefusesAnUnrecognisedPrompt(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	w.term.screens["block:w"] = "app: echo pending # \r\n"
	w.term.mu.Unlock()
	res, err := w.revive(ReviveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(res)["pi-03"]; a.Action != ReviveRefused || len(w.writes("block:w")) != 0 {
		t.Errorf("%+v, writes %q", a, w.writes("block:w"))
	}
}

// lostReply is a Rex whose reply to a new tab is lost after the tab was
// made, as a request that times out once the server finished it.
type lostReply struct {
	Terminal
	term *fakeTerm
}

func (r lostReply) NewWindow(ctx context.Context, session, tab, label, dir string, cmd []string) (string, string, error) {
	if _, _, err := r.term.NewWindow(ctx, session, tab, label, dir, cmd); err != nil {
		return "", "", err
	}
	return "", "", errors.New("rex: request timed out")
}

// A tab whose reply was lost is found again, by its label among the blocks
// that were not there before, and waited for: a retry under the same op id
// opens no second tab, and its adoption makes the agent ready. Two new tabs
// of that label cannot be told apart, and nothing more is started
// (codex-27's round-2 S2).
func TestReviveFindsATabWhoseReplyWasLost(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	delete(w.term.windows, "block:o")
	started := w.term.started
	w.term.started = nil
	w.term.mu.Unlock()
	w.e.Rex = lostReply{Terminal: w.e.Rex, term: w.term}
	w.e.RexOn = func(string) Terminal { return w.e.Rex }
	first, err := w.revive(ReviveRequest{OpID: "lost", Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(first)["pi-01"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "a retry tells") {
		t.Fatalf("first: %+v", a)
	}
	again, err := w.revive(ReviveRequest{OpID: "lost", Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(again)["pi-01"]; a.Action != ReviveRespawned || a.Block != "block:launch-1" {
		t.Fatalf("retry: %+v", a)
	}
	w.term.mu.Lock()
	cmd, n := w.term.launches[0].Command, len(w.term.launches)
	w.term.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d tabs opened", n)
	}
	started(launch{Block: "block:launch-1", Command: cmd})
	if done, err := w.revive(ReviveRequest{}); err != nil || byHandle(done)["pi-01"].Status != ReviveReady {
		t.Fatalf("after its adoption: %+v %v", done, err)
	}

	w2 := newReviveWorld(t)
	w2.term.mu.Lock()
	delete(w2.term.windows, "block:o")
	w2.term.started = nil
	w2.term.mu.Unlock()
	w2.e.Rex = lostReply{Terminal: w2.e.Rex, term: w2.term}
	w2.e.RexOn = func(string) Terminal { return w2.e.Rex }
	if _, err := w2.revive(ReviveRequest{Timeout: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w2.term.NewWindow(context.Background(), reviveSess, "x", core.PaneLabel("pi-01", core.OrchestratorRole, ""), w2.dir, nil); err != nil {
		t.Fatal(err)
	}
	res, err := w2.revive(ReviveRequest{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	w2.term.mu.Lock()
	n = len(w2.term.launches)
	w2.term.mu.Unlock()
	if a := byHandle(res)["pi-01"]; a.Action != ReviveRefused || !strings.Contains(a.Reason, "cannot be told apart") || n != 2 {
		t.Errorf("two candidate tabs: %+v, %d tabs", a, n)
	}
}

// A launch typed into its shell is waited for while its harness starts,
// even when the pane shows a shell again before the harness execs (the
// login shell runs the launcher); a launcher that never ran (the typed text
// lost) is cancelled and the agent resumed once, in the same pane
// (codex-27's round-2 S2).
func TestReviveReconcilesAnInPlaceLaunch(t *testing.T) {
	w := newReviveWorld(t)
	w.term.mu.Lock()
	w.term.wrote = nil
	w.term.mu.Unlock()
	if _, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	p, _ := w.e.Store.PendingRevives(reviveSess)
	script := p["pi-01"].Script
	if _, err := os.Stat(script); err != nil || p["pi-01"].ShellPID != shellPID {
		t.Fatalf("pending %+v: %v", p["pi-01"], err)
	}
	// The text was lost: its launcher never ran. A retry cancels it and types again.
	again, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("the lost launcher was not cancelled: %v", err)
	}
	if a := byHandle(again)["pi-01"]; a.Action != ReviveResumedInPlace || len(w.writes("block:o")) != 2 {
		t.Fatalf("after a lost write: %+v, %d writes", a, len(w.writes("block:o")))
	}
	// This time it runs: its launcher is claimed, and the pane's foreground
	// is a shell (the launcher's) other than the one it was typed into.
	p, _ = w.e.Store.PendingRevives(reviveSess)
	_ = os.Remove(p["pi-01"].Script)
	w.term.mu.Lock()
	w.term.fgs["block:o"] = rex.Process{PID: 999, Name: "sh"}
	w.term.mu.Unlock()
	third, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if a := byHandle(third)["pi-01"]; a.Action != ReviveResumedInPlace || len(w.writes("block:o")) != 2 || !strings.Contains(a.Note, "still starting") {
		t.Errorf("while it starts: %+v, %d writes", a, len(w.writes("block:o")))
	}
	// Its agent exited to the shell it was typed into: planned anew.
	w.term.mu.Lock()
	w.term.fgs["block:o"] = rex.Process{PID: shellPID, Name: "-fish"}
	w.term.mu.Unlock()
	if fourth, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond}); err != nil || len(w.writes("block:o")) != 3 {
		t.Errorf("after its agent left: %+v %v, %d writes", byHandle(fourth)["pi-01"], err, len(w.writes("block:o")))
	}
}

// A launch recorded on a server instance that restarted since died with it,
// whether its launcher had run or not: its block, kept, holds a fresh
// login shell, and the next revive resumes the agent there (in its own
// block, or in the tab an earlier revive opened) after the usual pane
// checks, never waiting on it forever; a pane holding a draft still refuses
// (codex-27's round-3 S2).
func TestReviveReplansALaunchARestartKilled(t *testing.T) {
	for _, respawn := range []bool{false, true} {
		w := newReviveWorld(t)
		w.term.mu.Lock()
		w.term.wrote, w.term.started = nil, nil
		if respawn {
			delete(w.term.windows, "block:o")
		}
		w.term.mu.Unlock()
		if _, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		pending, _ := w.e.Store.PendingRevives(reviveSess)
		pl := pending["pi-01"]
		if pl.ServerInstance != "i" || pl.Block == "" {
			t.Fatalf("respawn=%v: pending %+v", respawn, pl)
		}
		_ = os.Remove(pl.Script) // it ran, then the server restarted before its adoption
		w.term.mu.Lock()
		w.term.fgs[pl.Block] = rex.Process{PID: shellPID + 1, Name: "-fish"}
		w.term.screens[pl.Block] = "\x1b[1m❯\x1b[0m echo half-typed\r\n"
		w.term.mu.Unlock()
		w.e.Rex = restartedTerm{w.term, "i-after-restart"}
		w.e.RexOn = func(string) Terminal { return w.e.Rex }
		before := len(w.writes(pl.Block))
		res, err := w.revive(ReviveRequest{Timeout: 30 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if a := byHandle(res)["pi-01"]; a.Action != ReviveRefused || len(w.writes(pl.Block)) != before {
			t.Errorf("respawn=%v: a draft in the restored pane: %+v", respawn, a)
		}
		w.term.mu.Lock()
		w.term.screens[pl.Block] = "\x1b[1m❯\x1b[0m \r\n"
		w.term.mu.Unlock()
		res, err = w.revive(ReviveRequest{Timeout: 30 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		a := byHandle(res)["pi-01"]
		if a.Action != ReviveResumedInPlace || a.Block != pl.Block || len(w.writes(pl.Block)) != before+1 {
			t.Errorf("respawn=%v: %+v, %d writes", respawn, a, len(w.writes(pl.Block)))
		}
		w.term.mu.Lock()
		tabs := len(w.term.launches)
		w.term.mu.Unlock()
		if want := map[bool]int{false: 0, true: 1}[respawn]; tabs != want {
			t.Errorf("respawn=%v: %d tabs opened", respawn, tabs)
		}
		if p, _ := w.e.Store.PendingRevives(reviveSess); p["pi-01"].ServerInstance != "i-after-restart" {
			t.Errorf("respawn=%v: the new launch's record %+v", respawn, p["pi-01"])
		}
	}
}
