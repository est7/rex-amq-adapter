package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// frozenAgyTerm models antigravity-cli#1915: an agy pane whose OSC 7501 status
// stays stuck at working (with frozen UpdatedAt) after Stop because a
// background command or schedule timer is still active, while its ruled > box
// is visible and empty at the prompt.
type frozenAgyTerm struct {
	*fakeTerm
	block        string
	st           rex.Status
	empty        string // VT of empty agy > box
	boxVal       string // text currently in the agy > box
	submit       bool   // when true, Write submits and leaves box empty; when false, doorbell stays in box
	captureErr   error
	captureCalls int
	onCapture    func()
	onWrite      func(block, data string)
	fmu          sync.Mutex
}

func (a *frozenAgyTerm) ProgramStatus(ctx context.Context, block string) (rex.Status, bool, error) {
	if block != a.block {
		return a.fakeTerm.ProgramStatus(ctx, block)
	}
	a.fmu.Lock()
	defer a.fmu.Unlock()
	return a.st, a.st.State != "", nil
}

func (a *frozenAgyTerm) CaptureVT(ctx context.Context, block string) (string, error) {
	if block != a.block {
		return a.fakeTerm.CaptureVT(ctx, block)
	}
	a.fmu.Lock()
	a.captureCalls++
	cerr := a.captureErr
	val := a.boxVal
	empty := a.empty
	cb := a.onCapture
	a.fmu.Unlock()
	if cb != nil {
		cb()
	}
	if cerr != nil {
		return "", cerr
	}
	if val == "" {
		return empty, nil
	}
	rule := strings.Repeat("─", 40)
	return rule + "\n> " + val + "\n" + rule + "\n", nil
}

func (a *frozenAgyTerm) Write(ctx context.Context, block, data string) error {
	if block != a.block {
		return a.fakeTerm.Write(ctx, block, data)
	}
	a.fmu.Lock()
	a.writes = append(a.writes, data)
	if !a.submit {
		text := strings.TrimSuffix(strings.TrimPrefix(data, bracketedPasteStart), bracketedPasteEnd+"\r")
		a.boxVal += text
	}
	cb := a.onWrite
	a.fmu.Unlock()
	if cb != nil {
		cb(block, data)
	}
	return nil
}

func (a *frozenAgyTerm) SendKeys(ctx context.Context, block string, keys ...string) error {
	if block != a.block {
		return a.fakeTerm.SendKeys(ctx, block, keys...)
	}
	a.fmu.Lock()
	defer a.fmu.Unlock()
	a.keys = append(a.keys, keys...)
	for _, k := range keys {
		if k == "ctrl+u" {
			a.boxVal = ""
		}
	}
	return nil
}

func frozenAgyWorld(t *testing.T, submit bool) (Env, *frozenAgyTerm, agent, Session, *time.Time) {
	t.Helper()
	e, term, _, s, now, _ := healthWorld(t)
	ft := &frozenAgyTerm{
		fakeTerm: term,
		block:    "block:agy",
		st: rex.Status{
			State:        "working",
			App:          "antigravity",
			EffectiveApp: "antigravity",
			UpdatedAt:    *now,
		},
		empty:  agyFixture(t, "rex-agy-empty"),
		submit: submit,
	}
	e.Rex, e.RexOn = ft, func(string) Terminal { return ft }
	term.windows["block:agy"] = "window:1"
	term.fgs = map[string]rex.Process{"block:agy": {PID: os.Getpid(), Name: "agy"}}
	rec := seed{
		Handle: "agy-42", Kind: core.Agy, Server: "unix:///fake.sock",
		BlockID: "block:agy", SessionID: "session:s", WindowID: "window:1",
		AgentPID: os.Getpid(), AgentSessionID: "conv-42",
		AdoptedAt: *now, FirstAdoptedAt: *now,
	}
	planted := plant(t, e, rec)
	if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), "agy-42"); err != nil {
		t.Fatal(err)
	}
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	return e, ft, planted, s, now
}

// Reproduces agy-42/agy-15 (antigravity-cli#1915): PreInvocation -> Stop while
// OSC 7501 remains frozen at working. With the turn ledger and edge
// confirmation, doorbell injection succeeds on a causal edge, catches
// unsubmitted leftover text instead of false-positive Accepted, health avoids
// false stalled alerts at 15m, close/retire unrest allows shutdown, and
// Esc-cancel with a newer OSC idle heals TurnWorking to AtPrompt.
func TestAgyTurnLedgerFixesFrozenOSCDeadlock(t *testing.T) {
	loc := Location{Server: "unix:///fake.sock", Block: "block:agy"}

	t.Run("doorbell injects and confirms on PreInvocation turn-seq edge despite frozen OSC working", func(t *testing.T) {
		e, ft, _, _, now := frozenAgyWorld(t, true)
		// Turn 1 opens and finishes normally, while OSC stays frozen at working.
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		// Simulate agy starting Turn 2 right after the doorbell is submitted.
		ft.onWrite = func(_, _ string) {
			*now = now.Add(time.Second)
			_, _ = e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"})
		}
		o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()})
		if o.Progress != core.Accepted || o.Code != "" {
			t.Fatalf("expected causal turn-seq acceptance, got %+v", o)
		}
		if len(ft.writes) != 1 {
			t.Fatalf("writes=%d, want 1", len(ft.writes))
		}
	})

	t.Run("frozen OSC working never false-accepts unsubmitted leftover doorbell", func(t *testing.T) {
		e, ft, _, _, now := frozenAgyWorld(t, false)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()})
		if o.Progress != core.Deferred || o.Code != "not_submitted" {
			t.Fatalf("unsubmitted doorbell under frozen working OSC: got %+v, want deferred/not_submitted", o)
		}
		if strings.Join(ft.keys, ",") != "ctrl+u" || ft.boxVal != "" {
			t.Fatalf("unsubmitted doorbell not erased: keys=%v boxVal=%q", ft.keys, ft.boxVal)
		}
	})

	t.Run("frozen OSC working accepts via EmptiedTwice when prompt clears before hook", func(t *testing.T) {
		e, _, _, _, now := frozenAgyWorld(t, true)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()})
		if o.Progress != core.Accepted || o.Code != "box_emptied" {
			t.Fatalf("expected EmptiedTwice acceptance under frozen working OSC, got %+v", o)
		}
	})

	t.Run("health does not false-report stalled at 15m after Stop with frozen OSC working", func(t *testing.T) {
		if _, err := exec.LookPath("sqlite3"); err != nil {
			t.Skip("sqlite3 not installed")
		}
		e, _, rec, s, now := frozenAgyWorld(t, true)
		orch := recordNamed(t, e, "orch-01")
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		// Write a real agy conversation SQLite DB older than StalledFor (15m).
		db := filepath.Join(e.TranscriptHome, ".gemini/antigravity-cli/conversations/conv-42.db")
		if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
			t.Fatal(err)
		}
		sql := "create table steps (idx integer primary key, step_type integer, status integer, error_details blob); insert into steps values (1, 15, 3, null);"
		if out, err := exec.Command("sqlite3", db, sql).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		old := now.Add(-20 * time.Minute)
		if err := os.Chtimes(db, old, old); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(20 * time.Minute)
		if err := e.health(context.Background(), orch, s, rec); err != nil {
			t.Fatal(err)
		}
		if inbox := inboxOf(t, e, "orch-01"); len(inbox) != 0 {
			t.Fatalf("false stalled alert after Stop with frozen OSC working: %v", inbox)
		}
	})

	t.Run("close and retire unrest allows empty box and refuses draft or capture error under frozen OSC working", func(t *testing.T) {
		e, ft, rec, _, now := frozenAgyWorld(t, true)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		// Empty prompt box allows shutdown.
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || why != "" {
			t.Fatalf("unrest with empty box after Stop + frozen working OSC: why=%q err=%v, want at rest", why, err)
		}
		// Unsent user draft must refuse shutdown even when TurnIdle overrides frozen working OSC.
		ft.fmu.Lock()
		ft.boxVal = "half-written prompt"
		ft.fmu.Unlock()
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || !strings.Contains(why, "draft") {
			t.Fatalf("unrest with user draft under frozen working OSC: why=%q err=%v, want draft refusal", why, err)
		}
		// Failed VT capture under frozen working OSC must refuse shutdown.
		ft.fmu.Lock()
		ft.boxVal = ""
		ft.captureErr = errors.New("vt capture unavailable")
		ft.fmu.Unlock()
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || !strings.Contains(why, "empty input box") {
			t.Fatalf("unrest with capture error under frozen working OSC: why=%q err=%v, want refusal", why, err)
		}
		// Ordinary idle OSC preserves the zero-capture LazyScreen path.
		ft.fmu.Lock()
		ft.captureErr = nil
		ft.captureCalls = 0
		ft.st.State = "idle"
		ft.fmu.Unlock()
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || why != "" {
			t.Fatalf("unrest with ordinary idle OSC: why=%q err=%v, want at rest", why, err)
		}
		ft.fmu.Lock()
		calls := ft.captureCalls
		ft.fmu.Unlock()
		if calls != 0 {
			t.Fatalf("ordinary idle OSC captured VT %d times, want 0", calls)
		}
	})

	t.Run("Esc-cancel with newer OSC idle heals TurnWorking to AtPrompt", func(t *testing.T) {
		e, ft, rec, _, now := frozenAgyWorld(t, true)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		// Stale OSC idle older than Turn.At must NOT heal an active turn.
		ft.fmu.Lock()
		ft.st = rex.Status{State: "idle", App: "antigravity", UpdatedAt: now.Add(-5 * time.Second)}
		ft.fmu.Unlock()
		snap, err := e.assessActivity(context.Background(), activityRequest{Agent: &rec, Block: rec.BlockID, Policy: FullScreen})
		if err != nil {
			t.Fatal(err)
		}
		if it, ok := snap.Activity.(core.InTurn); !ok || !it.Backed {
			t.Fatalf("older OSC idle must not heal TurnWorking: got %#v", snap.Activity)
		}
		// Newer OSC idle after Turn.At heals TurnWorking -> AtPrompt.
		ft.fmu.Lock()
		ft.st = rex.Status{State: "idle", App: "antigravity", UpdatedAt: now.Add(5 * time.Second)}
		ft.fmu.Unlock()
		snap, err = e.assessActivity(context.Background(), activityRequest{Agent: &rec, Block: rec.BlockID, Policy: FullScreen})
		if err != nil {
			t.Fatal(err)
		}
		if ap, ok := snap.Activity.(core.AtPrompt); !ok || ap.Box != screen.BoxEmpty {
			t.Fatalf("newer OSC idle must heal TurnWorking to AtPrompt(empty): got %#v", snap.Activity)
		}
	})
}

func TestTurnLedgerHookFiltersContinuingAndIncarnation(t *testing.T) {
	loc := Location{Server: "unix:///fake.sock", Block: "block:agy"}

	t.Run("SessionStart does not write turn ledger; Stop continuing for mail keeps working", func(t *testing.T) {
		e, _, _, _, now := frozenAgyWorld(t, true)
		// SessionStart must not create a turn record so CLI-prompt startup falls
		// back cleanly to ExternalSignal.
		if _, err := e.Hook(context.Background(), EventSessionStart, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || ok {
			t.Fatalf("SessionStart wrote turn ledger: %+v ok=%v err=%v", tr, ok, err)
		}

		// PreInvocation opens turn 1; Stop with unread mail continues turn (stays working).
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "unread"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		r, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"})
		if err != nil || r.Notice == "" {
			t.Fatalf("expected continuing Stop notice, got r=%+v err=%v", r, err)
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
			t.Fatalf("continuing Stop must keep Phase=working Seq=1: got %+v ok=%v err=%v", tr, ok, err)
		}

		// Subsequent Stop without new mail settles to idle keeping Seq=1.
		*now = now.Add(time.Second)
		r, err = e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"})
		if err != nil || r.Notice != "" {
			t.Fatalf("expected quiet Stop, got r=%+v err=%v", r, err)
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || !ok || tr.Phase != core.TurnIdle || tr.Seq != 1 {
			t.Fatalf("quiet Stop must write Phase=idle Seq=1: got %+v ok=%v err=%v", tr, ok, err)
		}
	})

	t.Run("incarnation mismatch discards stale turn ledger and falls back to ExternalSignal", func(t *testing.T) {
		e, _, rec, _, now := frozenAgyWorld(t, true)
		// Write a TurnIdle record from a different PID (dead incarnation).
		if err := e.Store.PutTurn("agy-42", store.TurnRecord{
			Seq:         5,
			Phase:       core.TurnIdle,
			At:          now.Add(-time.Second),
			Incarnation: store.Incarnation{PID: os.Getpid() + 9999},
			SessionID:   "conv-42",
		}); err != nil {
			t.Fatal(err)
		}
		snap, err := e.assessActivity(context.Background(), activityRequest{Agent: &rec, Block: rec.BlockID, Policy: NoScreen})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Turn != nil {
			t.Fatalf("mismatched incarnation ledger was not discarded: %+v", snap.Turn)
		}
		if it, ok := snap.Activity.(core.InTurn); !ok || it.Backed {
			t.Fatalf("with discarded ledger, must fall back to unbacked ExternalSignal working: got %#v", snap.Activity)
		}
	})

	t.Run("agySubagent and nested startedUnderOwner hooks do not mutate pane turn ledger", func(t *testing.T) {
		e, _, _, _, now := frozenAgyWorld(t, true)
		e.AgyBrainDir = t.TempDir()
		subDir := filepath.Join(e.AgyBrainDir, "conv-42", ".system_generated", "subagents")
		if err := os.MkdirAll(subDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "sub-1.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		// Subagent Stop must be ignored before writing turn ledger.
		*now = now.Add(time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "sub-1", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking {
			t.Fatalf("agySubagent Stop mutated parent turn ledger: %+v", tr)
		}
		// Nested `claude -p` under agy must be ignored by startedUnderOwner.
		eNested := e
		eNested.Ancestors = func() ([]proc, error) {
			return []proc{{os.Getpid() + 50, "rex-amq-adapter"}, {os.Getpid() + 40, "sh"}, {os.Getpid() + 30, "claude"}, {os.Getpid() + 20, "bash"}, {os.Getpid(), "agy"}}, nil
		}
		if _, err := eNested.Hook(context.Background(), EventStopFailure, core.Claude, loc, HookInput{SessionID: "nested-c", AgentPID: os.Getpid() + 30, Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking {
			t.Fatalf("nested startedUnderOwner hook mutated parent turn ledger: %+v", tr)
		}
	})

	t.Run("StopFailure writes idle; foreign recordFit does not mutate turn ledger", func(t *testing.T) {
		e, _, _, _, now := frozenAgyWorld(t, true)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		// Foreign hooks (another kind, or a session held by another identity)
		// must not overwrite agy-42's ledger.
		if err := putSeed(e.Store, seed{
			Handle: "agy-99", Kind: core.Agy, Server: "unix:///fake.sock",
			BlockID: "block:other", SessionID: "session:s", WindowID: "window:1",
			AgentPID: os.Getpid() + 1, AgentSessionID: "conv-other",
			AdoptedAt: *now, FirstAdoptedAt: *now,
		}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		if _, err := e.Hook(context.Background(), EventStopFailure, core.Agy, loc, HookInput{ConversationID: "conv-other", Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
			t.Fatalf("foreign session StopFailure mutated agy-42 ledger: %+v", tr)
		}
		if _, err := e.Hook(context.Background(), EventStopFailure, core.Codex, loc, HookInput{SessionID: "codex-other", Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
			t.Fatalf("foreign kind StopFailure mutated agy-42 ledger: %+v", tr)
		}
		// Matching StopFailure settles the turn to idle.
		*now = now.Add(time.Second)
		if _, err := e.Hook(context.Background(), EventStopFailure, core.Agy, loc, HookInput{ConversationID: "conv-42", Error: "rate_limit"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnIdle || tr.Seq != 1 {
			t.Fatalf("matching StopFailure did not write idle: %+v", tr)
		}
		// Subsequent InvocationNum=0 opens Seq=2; InvocationNum=1 keeps Seq=2.
		zero, one := 0, 1
		*now = now.Add(time.Second)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42", InvocationNum: &zero}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 2 {
			t.Fatalf("InvocationNum=0 did not open Seq=2: %+v", tr)
		}
		*now = now.Add(time.Second)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42", InvocationNum: &one}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 2 {
			t.Fatalf("InvocationNum=1 incremented Seq unexpectedly: %+v", tr)
		}
	})

	t.Run("Pi prompt and settle hooks update turn ledger", func(t *testing.T) {
		e, _, _, _, now, _ := healthWorld(t)
		e.Rex.(*fakeTerm).windows["block:pi"] = "window:1"
		piRec := seed{
			Handle: "pi-07", Kind: core.Pi, Server: "unix:///fake.sock",
			BlockID: "block:pi", SessionID: "session:s", WindowID: "window:1",
			AgentPID: os.Getpid(), AgentSessionID: "sid-pi7", AdoptedAt: *now, FirstAdoptedAt: *now,
		}
		if err := putSeed(e.Store, piRec); err != nil {
			t.Fatal(err)
		}
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), "pi-07"); err != nil {
			t.Fatal(err)
		}
		piLoc := Location{Server: "unix:///fake.sock", Block: "block:pi"}
		if _, err := e.Hook(context.Background(), EventPrompt, core.Pi, piLoc, HookInput{SessionID: "sid-pi7", AgentPID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, err := e.Store.Turn("pi-07"); err != nil || !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
			t.Fatalf("Pi prompt: %+v ok=%v err=%v", tr, ok, err)
		}
		// Continuing settle (with unread mail) keeps working.
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "pi-07", "task", "go"); err != nil {
			t.Fatal(err)
		}
		if r, err := e.Hook(context.Background(), EventSettle, core.Pi, piLoc, HookInput{SessionID: "sid-pi7", AgentPID: os.Getpid(), Outcome: "completed"}); err != nil || r.Notice == "" {
			t.Fatalf("Pi continuing settle: r=%+v err=%v", r, err)
		}
		if tr, ok, _ := e.Store.Turn("pi-07"); !ok || tr.Phase != core.TurnWorking {
			t.Fatalf("Pi continuing settle dropped working: %+v", tr)
		}
		// Aborted or settled without notice writes idle.
		if _, err := e.Hook(context.Background(), EventSettle, core.Pi, piLoc, HookInput{SessionID: "sid-pi7", AgentPID: os.Getpid(), Outcome: "aborted"}); err != nil {
			t.Fatal(err)
		}
		if tr, ok, _ := e.Store.Turn("pi-07"); !ok || tr.Phase != core.TurnIdle || tr.Seq != 1 {
			t.Fatalf("Pi aborted settle: %+v", tr)
		}
	})

	t.Run("competing turn opened before or during Write never false-accepts unsubmitted doorbell", func(t *testing.T) {
		// Case 1: Competing prompt opens after assessActivity and before Rex.Write ->
		// write-boundary check aborts before typing a single byte.
		e, ft, _, _, now := frozenAgyWorld(t, false)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		fired := false
		ft.onCapture = func() {
			if fired {
				return
			}
			fired = true
			*now = now.Add(time.Second)
			_, _ = e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"})
		}
		o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()})
		if o.Progress != core.Deferred || o.Code != core.CodeAgentWorking {
			t.Fatalf("pre-write competing turn: got %+v, want deferred/agent_working", o)
		}
		if len(ft.writes) != 0 {
			t.Fatalf("pre-write competing turn still wrote %d times", len(ft.writes))
		}
		if pending, ok, err := e.Store.Pending("block:agy"); err != nil || ok {
			t.Fatalf("aborted write left pending=%q ok=%v err=%v", pending, ok, err)
		}

		// Case 2: Competing turn increments Seq concurrently with Rex.Write while
		// the doorbell stays unsubmitted in the box -> confirm must NOT accept.
		e2, ft2, _, _, now2 := frozenAgyWorld(t, false)
		if _, err := e2.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		*now2 = now2.Add(2 * time.Second)
		if _, err := e2.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err != nil {
			t.Fatal(err)
		}
		if err := amq.Send(context.Background(), e2.AMQ, e2.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		ft2.onWrite = func(_, _ string) {
			*now2 = now2.Add(time.Second)
			_, _ = e2.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"})
		}
		o2 := e2.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e2.Paths.Root()})
		if o2.Progress != core.Deferred || o2.Code != "not_submitted" {
			t.Fatalf("concurrent competing turn with unsubmitted doorbell: got %+v, want deferred/not_submitted", o2)
		}
		if strings.Join(ft2.keys, ",") != "ctrl+u" || ft2.boxVal != "" {
			t.Fatalf("leftover doorbell not cleared: keys=%v boxVal=%q", ft2.keys, ft2.boxVal)
		}
		if pending, ok, err := e2.Store.Pending("block:agy"); err != nil || ok {
			t.Fatalf("cleared doorbell left pending=%q ok=%v err=%v", pending, ok, err)
		}
		if hist, err := e2.Store.InjectHistory("agy-42"); err != nil || len(hist) == 0 || hist[len(hist)-1].Progress != core.Deferred || hist[len(hist)-1].Code != "not_submitted" {
			t.Fatalf("inject history did not record deferred/not_submitted: %+v err=%v", hist, err)
		}
	})

	t.Run("Stop that fails mail announcement still records idle so restored I/O is not deadlocked", func(t *testing.T) {
		e, ft, rec, _, now := frozenAgyWorld(t, true)
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		// Break inbox/new so announceMail fails os.ReadDir while Stop fires.
		inboxNew := amq.Inbox(e.Paths.Root(), "agy-42")
		if err := os.RemoveAll(inboxNew); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(inboxNew, []byte("not-a-dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
		if _, err := e.Hook(context.Background(), EventStop, core.Agy, loc, HookInput{ConversationID: "conv-42", TerminationReason: "NO_TOOL_CALL"}); err == nil {
			t.Fatal("expected Stop announcement error on broken inbox/new")
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || !ok || tr.Phase != core.TurnIdle {
			t.Fatalf("failed Stop announcement left ledger non-idle: %+v ok=%v err=%v", tr, ok, err)
		}
		// Restore inbox/new while OSC remains frozen working: unrest and Inject recover cleanly.
		if err := os.Remove(inboxNew); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(inboxNew, 0o700); err != nil {
			t.Fatal(err)
		}
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || why != "" {
			t.Fatalf("unrest after restored I/O with frozen working OSC: why=%q err=%v, want at rest", why, err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		ft.onWrite = func(_, _ string) {
			*now = now.Add(time.Second)
			_, _ = e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"})
		}
		if o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()}); o.Progress != core.Accepted {
			t.Fatalf("inject after failed-Stop recovery: got %+v, want accepted", o)
		}
	})

	t.Run("PaneTitle agent_state=idle heals stale agy TurnWorking on Esc with active background task", func(t *testing.T) {
		e, ft, rec, _, now := frozenAgyWorld(t, true)
		e.Ancestors = func() ([]proc, error) { return []proc{{os.Getpid(), "agy"}}, nil }
		// Startup idle before any turn must NOT fabricate a turn ledger.
		if got := e.PaneTitle(loc, core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: "conv-42"}); got != "agy-42" {
			t.Fatalf("PaneTitle before turn: %q", got)
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || ok {
			t.Fatalf("PaneTitle fabricated ledger before turn: %+v ok=%v err=%v", tr, ok, err)
		}

		// Open Turn 1; simulate Esc with an active schedule timer (no Stop hook,
		// OSC 7501 remains frozen at working).
		if _, err := e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"}); err != nil {
			t.Fatal(err)
		}
		// Non-idle, empty conversation_id, subagent conversation_id, wrong kind,
		// and non-descendant process must NOT heal TurnWorking.
		for _, bad := range []struct {
			kind core.Kind
			in   PaneTitleInput
		}{
			{core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "working", ConversationID: "conv-42"}},
			{core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: ""}},
			{core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: "subagent-99"}},
			{core.Codex, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: "conv-42"}},
		} {
			_ = e.PaneTitle(loc, bad.kind, bad.in)
			if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
				t.Fatalf("invalid PaneTitle(%s, %+v) mutated ledger: %+v", bad.kind, bad.in, tr)
			}
		}
		eForeign := e
		eForeign.Ancestors = func() ([]proc, error) { return []proc{{os.Getpid() + 777, "agy"}}, nil }
		_ = eForeign.PaneTitle(loc, core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: "conv-42"})
		if tr, ok, _ := e.Store.Turn("agy-42"); !ok || tr.Phase != core.TurnWorking || tr.Seq != 1 {
			t.Fatalf("non-descendant PaneTitle mutated ledger: %+v", tr)
		}

		// Matching agy PaneTitle(idle, conv-42) heals TurnWorking -> TurnIdle preserving Seq=1.
		*now = now.Add(3 * time.Second)
		if got := e.PaneTitle(loc, core.Agy, PaneTitleInput{Cwd: "/w/proj", AgentState: "idle", ConversationID: "conv-42"}); got != "agy-42" {
			t.Fatalf("PaneTitle on Esc idle: %q", got)
		}
		if tr, ok, err := e.Store.Turn("agy-42"); err != nil || !ok || tr.Phase != core.TurnIdle || tr.Seq != 1 {
			t.Fatalf("PaneTitle did not heal TurnWorking to TurnIdle(Seq=1): %+v ok=%v err=%v", tr, ok, err)
		}
		// Pane is now unblocked for both unrest/shutdown and doorbell delivery.
		if why, err := e.unrest(context.Background(), ft, rec); err != nil || why != "" {
			t.Fatalf("unrest after PaneTitle idle heal: why=%q err=%v, want at rest", why, err)
		}
		if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "orch-01", "agy-42", "task", "ping"); err != nil {
			t.Fatal(err)
		}
		ft.onWrite = func(_, _ string) {
			*now = now.Add(time.Second)
			_, _ = e.Hook(context.Background(), EventPreInvocation, core.Agy, loc, HookInput{ConversationID: "conv-42"})
		}
		if o := e.Inject(context.Background(), InjectRequest{Handle: "agy-42", Root: e.Paths.Root()}); o.Progress != core.Accepted {
			t.Fatalf("inject after PaneTitle idle heal: got %+v, want accepted", o)
		}
	})
}
