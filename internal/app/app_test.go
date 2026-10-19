package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// fakeTerm is one Codex terminal. Its input box holds box; a write lands in
// the box unless the agent submits it (submit), and ctrl+u empties it.
type fakeTerm struct {
	mu        sync.Mutex
	gone      bool
	fgPID     int
	state     string // "" means no program status record
	box       string
	submit    bool // a written doorbell is submitted: box stays empty, state becomes working
	statusErr bool // ProgramStatus fails after the first write
	writes    []string
	keys      []string
}

func (f *fakeTerm) Whoami(context.Context) (rex.Server, error) {
	return rex.Server{InstanceID: "i", Endpoint: "unix:///fake.sock"}, nil
}

func (f *fakeTerm) Foreground(_ context.Context, block string) (rex.Process, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return rex.Process{}, false, rex.ErrNotFound
	}
	return rex.Process{PID: f.fgPID, Name: "codex"}, true, nil
}

func (f *fakeTerm) ProgramStatus(context.Context, string) (rex.Status, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr && len(f.writes) > 0 {
		return rex.Status{}, false, errors.New("rex timed out")
	}
	return rex.Status{State: f.state}, f.state != "", nil
}

func (f *fakeTerm) CaptureVT(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	box := "\x1b[2mAsk Codex to do anything\x1b[0m"
	if f.box != "" {
		box = f.box
	}
	return "  >_ OpenAI Codex\n\n› " + box + "\n\n  gpt · Context 0% used\n", nil
}

func (f *fakeTerm) Write(_ context.Context, _ string, data string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, data)
	if f.submit {
		if f.state != "" { // an agent without program status stays silent
			f.state = "working"
		}
		return nil
	}
	text := strings.TrimSuffix(strings.TrimPrefix(data, bracketedPasteStart), bracketedPasteEnd+"\r")
	f.box += text
	return nil
}

func (f *fakeTerm) SendKeys(_ context.Context, _ string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, keys...)
	for _, k := range keys {
		if k == "ctrl+u" {
			f.box = ""
		}
	}
	return nil
}

func (f *fakeTerm) InspectBlock(context.Context, string) (rex.Block, error) {
	return rex.Block{ID: "block:a", Label: "shell"}, nil
}

func (f *fakeTerm) RenameBlock(context.Context, string, string) error { return nil }

const block = "block:a"

// fixture builds an Env around term with codex adopted in block:a and the
// given unread message ids in its inbox.
func fixture(t *testing.T, term *fakeTerm, unread ...string) Env {
	t.Helper()
	paths := store.Paths{Home: t.TempDir()}
	e := Env{
		Paths: paths, Store: store.Store{P: paths},
		Rex: term, RexOn: func(string) Terminal { return term },
		SelfBin: "/x/rex-amq-adapter", Now: time.Now,
	}
	if term.fgPID == 0 {
		term.fgPID = os.Getpid()
	}
	if err := e.Store.PutRecord(store.Record{Handle: "codex", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: block, AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(paths.Root(), "agents", "codex", "inbox", "new")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range unread {
		if err := os.WriteFile(filepath.Join(inbox, id+".md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func inject(e Env) core.Outcome {
	return e.Inject(context.Background(), InjectRequest{Block: block, Handle: "codex", Root: e.Paths.Root()})
}

func TestFakeScreenClassifies(t *testing.T) {
	f := &fakeTerm{}
	vt, _ := f.CaptureVT(context.Background(), block)
	if screen.Check("codex", vt) != screen.Empty {
		t.Fatal("fixture: empty box not recognised")
	}
	f.box = "draft"
	vt, _ = f.CaptureVT(context.Background(), block)
	if screen.Check("codex", vt) != screen.Typed || !screen.BoxHolds("codex", vt, "draft") {
		t.Fatal("fixture: draft not recognised")
	}
}

func TestDoorbellToIdleAgentIsConfirmedByWorking(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m1", "m2")
	o := inject(e)
	if o.Progress != core.Accepted {
		t.Fatalf("got %+v", o)
	}
	if len(f.writes) != 1 || !strings.HasPrefix(f.writes[0], bracketedPasteStart) || !strings.HasSuffix(f.writes[0], bracketedPasteEnd+"\r") {
		t.Fatalf("one bracketed write expected: %q", f.writes)
	}
	if !strings.Contains(f.writes[0], "2 unread message(s) for codex") {
		t.Fatalf("doorbell text: %q", f.writes[0])
	}
	ann, _ := e.Store.Announced("codex")
	if !ann.IDs["m1"] || !ann.IDs["m2"] || ann.At.IsZero() {
		t.Fatalf("ledger after accepted doorbell: %+v", ann)
	}
	if _, ok, _ := e.Store.Pending(block); ok {
		t.Fatal("pending must be cleared after acceptance")
	}
}

func TestUnsubmittedDoorbellIsErased(t *testing.T) {
	f := &fakeTerm{state: "idle"}
	e := fixture(t, f, "m1")
	o := inject(e)
	if o.Progress != core.Deferred || o.Code != "not_submitted" {
		t.Fatalf("got %+v", o)
	}
	if strings.Join(f.keys, ",") != "ctrl+u" || f.box != "" {
		t.Fatalf("doorbell not erased: keys=%v box=%q", f.keys, f.box)
	}
	if _, ok, _ := e.Store.Pending(block); ok {
		t.Fatal("pending must be cleared once the doorbell is erased")
	}
}

func TestUserTextIsNeverErased(t *testing.T) {
	f := &fakeTerm{state: "idle", box: "my own draft"}
	e := fixture(t, f, "m1")
	o := inject(e)
	if o.Progress != core.Deferred || o.Code != "draft_in_box" {
		t.Fatalf("got %+v", o)
	}
	if len(f.writes) != 0 || len(f.keys) != 0 || f.box != "my own draft" {
		t.Fatalf("draft touched: writes=%q keys=%v box=%q", f.writes, f.keys, f.box)
	}
}

func TestLeftoverDoorbellIsErasedBeforeTheNextOne(t *testing.T) {
	f := &fakeTerm{state: "idle", submit: true}
	e := fixture(t, f, "m1")
	left := "AMQ doorbell from an attempt that died"
	f.box = left
	if err := e.Store.SetPending(block, left); err != nil {
		t.Fatal(err)
	}
	o := inject(e)
	if o.Progress != core.Accepted || strings.Join(f.keys, ",") != "ctrl+u" {
		t.Fatalf("got %+v keys=%v", o, f.keys)
	}
	if len(f.writes) != 1 || strings.Contains(f.writes[0], left) {
		t.Fatalf("new doorbell merged with the leftover: %q", f.writes)
	}
}

func TestCleanupSurvivesErrorsAndCancellationAfterTheWrite(t *testing.T) {
	// Status reads fail after the write, and the caller's context is already
	// cancelled (the fake ignores it before the write). Confirmation runs on
	// its own budget, so the doorbell is still found, erased, and the erase
	// verified; on the caller's context it would end "cleanup_unverified".
	f := &fakeTerm{state: "idle", statusErr: true}
	e := fixture(t, f, "m1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := e.Inject(ctx, InjectRequest{Block: block, Handle: "codex", Root: e.Paths.Root()})
	if len(f.writes) != 1 {
		t.Fatalf("expected one write, got %q", f.writes)
	}
	if o.Progress != core.Deferred || o.Code != "not_submitted" || f.box != "" {
		t.Fatalf("got %+v box=%q keys=%v", o, f.box, f.keys)
	}
}

func TestWorkingAgentIsNotTypedInto(t *testing.T) {
	f := &fakeTerm{state: "working"}
	e := fixture(t, f, "m1")
	if o := inject(e); o.Progress != core.Deferred || o.Code != "agent_working" || len(f.writes) != 0 {
		t.Fatalf("got %+v writes=%q", o, f.writes)
	}
}

func TestBindingOutcomes(t *testing.T) {
	cases := []struct {
		name string
		term *fakeTerm
		pid  int
		want core.Progress
		code string
	}{
		{"block gone", &fakeTerm{gone: true}, os.Getpid(), core.Failed, "block_gone"},
		{"agent alive behind an editor", &fakeTerm{fgPID: 1, state: "idle"}, os.Getpid(), core.Deferred, "agent_not_foreground"},
		{"agent process exited", &fakeTerm{fgPID: 1, state: "idle"}, 0x3ffffff0, core.Failed, "agent_gone"},
	}
	for _, c := range cases {
		e := fixture(t, c.term, "m1")
		rec, _, _ := e.Store.Record(block)
		rec.AgentPID = c.pid
		if err := e.Store.PutRecord(rec); err != nil {
			t.Fatal(err)
		}
		if o := inject(e); o.Progress != c.want || o.Code != c.code || len(c.term.writes) != 0 {
			t.Errorf("%s: got %+v writes=%q", c.name, o, c.term.writes)
		}
	}
}

func TestNothingUnreadIsAcceptedWithoutTyping(t *testing.T) {
	f := &fakeTerm{state: "idle"}
	e := fixture(t, f)
	if o := inject(e); o.Progress != core.Accepted || len(f.writes) != 0 {
		t.Fatalf("got %+v writes=%q", o, f.writes)
	}
}

func TestAgentWithoutStatusNeedsTwoEmptyPolls(t *testing.T) {
	f := &fakeTerm{submit: true} // no program status at all
	e := fixture(t, f, "m1")
	if o := inject(e); o.Progress != core.Accepted || o.Code != "box_emptied" {
		t.Fatalf("got %+v", o)
	}
}

// hook runs a hook and renders it as the harness would read it.
func hook(t *testing.T, e Env, kind core.Kind, event string, in HookInput) string {
	t.Helper()
	r, err := e.Hook(context.Background(), event, kind, Location{Block: block}, in)
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	out, err := Render(kind, event, r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHookOutputShapes(t *testing.T) {
	f := &fakeTerm{state: "working"}
	e := fixture(t, f, "m1")
	out := hook(t, e, core.Codex, EventPostTool, HookInput{})
	var post struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &post); err != nil || post.HookSpecificOutput.HookEventName != "PostToolUse" || !strings.Contains(post.HookSpecificOutput.AdditionalContext, "1 unread") {
		t.Fatalf("post-tool output %q: %v", out, err)
	}
	if out := hook(t, e, core.Codex, EventPostTool, HookInput{}); out != "" {
		t.Fatalf("announced mail repeated: %q", out)
	}
	out = hook(t, e, core.Codex, EventStop, HookInput{})
	var stop struct{ Decision, Reason string }
	if json.Unmarshal([]byte(out), &stop) != nil || stop.Decision != "block" || !strings.Contains(stop.Reason, "before ending your turn") {
		t.Fatalf("stop output %q", out)
	}
	if out := hook(t, e, core.Codex, EventStop, HookInput{StopHookActive: true}); out != "" {
		t.Fatalf("stop looped on the same mail: %q", out)
	}
}

func TestAgyHookOutputShapes(t *testing.T) {
	f := &fakeTerm{state: "working"}
	e := fixture(t, f, "m1")
	rec, _, _ := e.Store.Record(block)
	rec.Kind = core.Agy
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	// PreInvocation announces new mail as an ephemeral injected step.
	out := hook(t, e, core.Agy, EventPreInvocation, HookInput{})
	var pre struct {
		InjectSteps []struct {
			EphemeralMessage string `json:"ephemeralMessage"`
		} `json:"injectSteps"`
	}
	if json.Unmarshal([]byte(out), &pre) != nil || len(pre.InjectSteps) != 1 || !strings.Contains(pre.InjectSteps[0].EphemeralMessage, "1 unread") {
		t.Fatalf("pre-invocation output %q", out)
	}
	// Antigravity always reads JSON; nothing to say is {}.
	if out := hook(t, e, core.Agy, EventPreInvocation, HookInput{}); out != "{}\n" {
		t.Fatalf("repeated or non-JSON output: %q", out)
	}
	// No stop_hook_active: announced mail never blocks a stop, so it cannot loop.
	if out := hook(t, e, core.Agy, EventStop, HookInput{TerminationReason: "NO_TOOL_CALL"}); out != "{}\n" {
		t.Fatalf("stop blocked on announced mail: %q", out)
	}
	if err := os.WriteFile(filepath.Join(e.Paths.Root(), "agents", "codex", "inbox", "new", "m2.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A user's cancel is never overridden, even with new mail.
	if out := hook(t, e, core.Agy, EventStop, HookInput{TerminationReason: "USER_CANCELED"}); out != "{}\n" {
		t.Fatalf("cancelled stop overridden: %q", out)
	}
	out = hook(t, e, core.Agy, EventStop, HookInput{TerminationReason: "NO_TOOL_CALL"})
	var stop struct{ Decision, Reason string }
	if json.Unmarshal([]byte(out), &stop) != nil || stop.Decision != "continue" || !strings.Contains(stop.Reason, "(1 new)") {
		t.Fatalf("stop with new mail: %q", out)
	}
	ss, err := Render(core.Agy, EventSessionStart, Reply{Notice: "you are codex"})
	var start struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err != nil || json.Unmarshal([]byte(ss), &start) != nil || len(start.InjectSteps) != 1 || start.InjectSteps[0].SystemMessage.SystemMessage != "you are codex" {
		t.Fatalf("session-start output %q", ss)
	}
}

func TestFailedAdoptionBacksOff(t *testing.T) {
	f := &fakeTerm{}
	e := fixture(t, f)
	if err := e.Store.DeleteRecord(block); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetAdoptFailed(block, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Adopting would need amq (AmqBin is empty here); within the back-off the
	// hook must not try.
	r, err := e.Hook(context.Background(), EventPostTool, core.Codex, Location{Block: block}, HookInput{})
	if err != nil || r.Notice != "" {
		t.Fatalf("hook adopted during back-off: %+v %v", r, err)
	}
	if err := e.Store.SetAdoptFailed(block, time.Now().Add(-2*AdoptRetryAfter)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(context.Background(), EventPostTool, core.Codex, Location{Block: block}, HookInput{}); err == nil {
		t.Fatal("after the back-off the hook must try to adopt again (and fail without amq)")
	}
}

func TestPiHookFlow(t *testing.T) {
	f := &fakeTerm{state: "idle"}
	e := fixture(t, f, "m1")
	rec, _, _ := e.Store.Record(block)
	rec.Kind = core.Pi
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	var out struct{ Notice, Inbox string }
	render := func(event string, r Reply) {
		t.Helper()
		s, err := Render(core.Pi, event, r)
		out.Notice, out.Inbox = "", ""
		if err != nil || json.Unmarshal([]byte(s), &out) != nil {
			t.Fatalf("%s render %q: %v", event, s, err)
		}
	}
	// Mail already waiting stays unannounced, so the extension's first scan
	// delivers it with a turn of its own.
	render(EventMail, Reply{})
	if out.Notice != "" {
		t.Fatal("empty reply must render as {}")
	}
	if s, _ := Render(core.Pi, EventSessionStart, Reply{Notice: "you are codex", Inbox: "/r/agents/codex/inbox/new"}); !strings.Contains(s, `"inbox":"/r/agents/codex/inbox/new"`) {
		t.Fatalf("session-start render: %q", s)
	}
	r, err := e.Hook(context.Background(), EventMail, core.Pi, Location{Block: block}, HookInput{})
	if err != nil {
		t.Fatal(err)
	}
	render(EventMail, r)
	if !strings.Contains(out.Notice, "1 unread") {
		t.Fatalf("first scan must announce waiting mail: %+v", out)
	}
	if r, _ := e.Hook(context.Background(), EventMail, core.Pi, Location{Block: block}, HookInput{Idle: true}); r.Notice != "" {
		t.Fatalf("a second scan repeated the announcement: %q", r.Notice)
	}
	// Announced, never drained, Pi idle: after RemindAfter it is told again,
	// once; a busy Pi is left alone.
	later := time.Now().Add(core.RemindAfter + time.Minute)
	e.Now = func() time.Time { return later }
	if r, _ := e.Hook(context.Background(), EventMail, core.Pi, Location{Block: block}, HookInput{}); r.Notice != "" {
		t.Fatalf("a busy Pi was reminded: %q", r.Notice)
	}
	if r, _ := e.Hook(context.Background(), EventMail, core.Pi, Location{Block: block}, HookInput{Idle: true}); !strings.Contains(r.Notice, "1 unread") {
		t.Fatalf("an idle Pi with undrained mail was not reminded: %q", r.Notice)
	}
	if r, _ := e.Hook(context.Background(), EventMail, core.Pi, Location{Block: block}, HookInput{Idle: true}); r.Notice != "" {
		t.Fatalf("the reminder repeated at once: %q", r.Notice)
	}
	e.Now = time.Now
	// Settling with the mail still unread continues the run once.
	if r, _ := e.Hook(context.Background(), EventSettle, core.Pi, Location{Block: block}, HookInput{}); !strings.Contains(r.Notice, "before ending your turn") {
		t.Fatalf("settle must remind once: %q", r.Notice)
	}
	if r, _ := e.Hook(context.Background(), EventSettle, core.Pi, Location{Block: block}, HookInput{StopHookActive: true}); r.Notice != "" {
		t.Fatalf("settle looped: %q", r.Notice)
	}
	if len(f.writes) != 0 || len(f.keys) != 0 {
		t.Fatalf("nothing may be typed into Pi: writes=%q keys=%v", f.writes, f.keys)
	}
}
