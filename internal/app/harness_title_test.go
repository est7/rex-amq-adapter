package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func hookTitle(t *testing.T, e Env, kind core.Kind, event string, in HookInput) string {
	t.Helper()
	r, err := e.Hook(context.Background(), event, kind, Location{Block: block}, in)
	if err != nil {
		t.Fatal(err)
	}
	return r.Title
}

// Claude Code is given its pane label as its conversation title when it
// starts, and again only after the task changes, so a /rename by hand holds
// until then.
func TestClaudeIsGivenItsTitleOnceAndAgainWhenTheTaskChanges(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{label: "reviewer", state: "working"})
	in := HookInput{SessionID: "s1"}
	if got := hookTitle(t, e, core.Claude, EventSessionStart, in); got != "claude-reviewer-01" {
		t.Fatalf("start: title %q", got)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "" {
		t.Errorf("an unchanged title was given again: %q", got)
	}
	setTask(t, e, block, "fix login")
	if got := hookTitle(t, e, core.Claude, EventPostTool, in); got != "" {
		t.Errorf("PostToolUse cannot carry a title, got %q", got)
	}
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "claude-reviewer-01 | fix login" {
		t.Errorf("after the task changed: title %q", got)
	}
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "" {
		t.Errorf("the new title was given twice: %q", got)
	}
}

// Pi's extension applies a title from any reply.
func TestPiIsGivenItsTitleOnAnyHook(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{label: "reviewer"})
	in := HookInput{SessionID: "p1", AgentPID: 0}
	if got := hookTitle(t, e, core.Pi, EventSessionStart, in); got != "pi-reviewer-01" {
		t.Fatalf("start: title %q", got)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	setTask(t, e, block, "write docs")
	if got := hookTitle(t, e, core.Pi, EventMail, in); got != "pi-reviewer-01 | write docs" {
		t.Errorf("mail after the task changed: title %q", got)
	}
}

// Codex has no title it can be given without its daemon, and agy asks for
// its own (PaneTitle): their hooks carry none.
func TestOnlyHarnessesThatTakeATitleAreGivenOne(t *testing.T) {
	for _, c := range []struct {
		kind core.Kind
		in   HookInput
	}{
		{core.Codex, HookInput{SessionID: "x1", TranscriptPath: []byte(`"/tmp/x1.jsonl"`)}},
		{core.Agy, HookInput{ConversationID: "a1"}},
	} {
		e := realAmqEnv(t, &fakeTerm{state: "working"})
		for _, event := range []string{EventSessionStart, EventPrompt, EventPreInvocation} {
			if got := hookTitle(t, e, c.kind, event, c.in); got != "" {
				t.Errorf("%s %s carried title %q", c.kind, event, got)
			}
		}
		_ = e.Depart(context.Background(), Location{Block: block}, HookInput{})
	}
}

// A hook that is not the record's agent, waiting out a failed adoption, is
// never given the record's title. Pi's mail hook takes a title and waits out
// the back-off.
func TestAnotherAgentWaitingToBeAdoptedIsGivenNoTitle(t *testing.T) {
	e := fixture(t, &fakeTerm{state: "working"}) // a codex record in block
	if err := e.Store.SetAdoptFailed(block, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := hookTitle(t, e, core.Pi, EventMail, HookInput{SessionID: "p1"}); got != "" {
		t.Errorf("another agent was given the record's title %q", got)
	}
}

func TestRenderCarriesTheTitle(t *testing.T) {
	decode := func(s string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return v
	}
	out, _ := Render(core.Claude, EventSessionStart, Reply{Notice: "you are claude-01", Title: "claude-01"})
	h := decode(out)["hookSpecificOutput"].(map[string]any)
	if h["hookEventName"] != "SessionStart" || h["additionalContext"] != "you are claude-01" || h["sessionTitle"] != "claude-01" {
		t.Errorf("Claude SessionStart: %s", out)
	}
	if out, _ := Render(core.Claude, EventSessionStart, Reply{Notice: "you are claude-01"}); out != "you are claude-01\n" {
		t.Errorf("SessionStart without a title stays plain text: %q", out)
	}
	out, _ = Render(core.Claude, EventPrompt, Reply{Title: "claude-01 | fix"})
	h = decode(out)["hookSpecificOutput"].(map[string]any)
	if h["hookEventName"] != "UserPromptSubmit" || h["sessionTitle"] != "claude-01 | fix" || h["additionalContext"] != nil {
		t.Errorf("a prompt carrying only a title: %s", out)
	}
	if out, _ := Render(core.Pi, EventMail, Reply{Title: "pi-01"}); decode(out)["title"] != "pi-01" {
		t.Errorf("Pi: %s", out)
	}
}

// agy asks for its window title: the pane label of its own agent, never
// another kind's, and the directory when there is none.
func TestPaneTitleNamesOnlyAnAgentOfThatKind(t *testing.T) {
	e := fixture(t, &fakeTerm{}) // a codex record in block
	loc := Location{Block: block}
	if got := e.PaneTitle(loc, core.Agy, "/w/proj"); got != "proj" {
		t.Errorf("a Codex record titled agy's window %q", got)
	}
	if err := e.Store.PutRecord(store.Record{Handle: "agy-01", Kind: core.Agy, Server: "unix:///fake.sock", BlockID: block, Task: "fix login"}); err != nil {
		t.Fatal(err)
	}
	if got := e.PaneTitle(loc, core.Agy, "/w/proj"); got != "agy-01 | fix login" {
		t.Errorf("agy's own record: %q", got)
	}
	if got := e.PaneTitle(Location{Block: "block:none"}, core.Agy, "/w/proj"); got != "proj" {
		t.Errorf("no record: %q", got)
	}
}

// A new session in the same process (Pi's /new) keeps the handle but starts
// unnamed, so it is given the pane label although the handle's previous
// session already had it.
func TestNewSessionInTheProcessIsGivenItsTitle(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{label: "reviewer"})
	first := hookTitle(t, e, core.Pi, EventSessionStart, HookInput{SessionID: "p1"})
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	second := hookTitle(t, e, core.Pi, EventSessionStart, HookInput{SessionID: "p2"})
	if rec, _, _ := e.Store.Record(block); rec.AgentSessionID != "p2" || first == "" || second != first {
		t.Fatalf("new session %q given title %q, first %q", rec.AgentSessionID, second, first)
	}
	if got := hookTitle(t, e, core.Pi, EventSessionStart, HookInput{SessionID: "p2"}); got != "" {
		t.Errorf("the same session restarted was given its title again: %q", got)
	}
}

// agy has no SessionEnd, so a new agy in the pane finds the old one's record
// still live until its first turn. Its title must not name the old agent.
func TestPaneTitleIgnoresAnotherProcessesRecord(t *testing.T) {
	e := fixture(t, &fakeTerm{})
	if err := e.Store.PutRecord(store.Record{Handle: "agy-old-01", Kind: core.Agy, Server: "unix:///fake.sock", BlockID: block, AgentPID: 12345, AgentSessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	e.Ancestors = func() ([]int, error) { return []int{20000, 19999}, nil }
	if got := e.PaneTitle(Location{Block: block}, core.Agy, "/w/proj"); got != "proj" {
		t.Errorf("a new agy process was titled with another agent's label %q", got)
	}
}
