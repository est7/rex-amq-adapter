package app

import (
	"context"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/guide"
)

// epicWorld is a terminal whose block:a sits in a Rex session labelled epic.
func epicWorld(epic string) *fakeTerm {
	return &fakeTerm{label: "reviewer", state: "working", windows: map[string]string{block: "window:1"}, sessionLabel: epic}
}

// titleState is the tracked conversation title of the agent in block.
func titleState(t *testing.T, e Env) core.TitleState {
	t.Helper()
	rec, ok, err := e.Store.Record(block)
	if err != nil || !ok {
		t.Fatalf("no record in %s: %v", block, err)
	}
	st, ok, err := e.Store.TitleState(rec.Handle)
	if err != nil || !ok {
		t.Fatalf("no title state for %s: %v", rec.Handle, err)
	}
	return st
}

func wantState(t *testing.T, e Env, step string, want core.TitleState) {
	t.Helper()
	if got := titleState(t, e); got != want {
		t.Errorf("%s: title state %+v, want %+v", step, got, want)
	}
}

// Claude Code is given "<role>(<handle>) | [<epic>] <task>" when its session
// names an Epic. A task set between turns is desired until the next prompt
// offers it; PostToolUse cannot carry it. Renaming the Epic retitles the
// conversation at the next prompt, while the pane label never shows it.
func TestEpicTitleClaudeConversationTitle(t *testing.T) {
	term := epicWorld("auth-refresh")
	e := realAmqEnv(t, term)
	in := HookInput{SessionID: "s1"}
	if got := hookTitle(t, e, core.Claude, EventSessionStart, in); got != "reviewer(claude-01) | [auth-refresh]" {
		t.Fatalf("start: title %q", got)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	wantState(t, e, "start", core.TitleState{Status: core.TitleOffered, Desired: "reviewer(claude-01) | [auth-refresh]", Offered: "reviewer(claude-01) | [auth-refresh]"})

	if label := setTask(t, e, block, "fix login"); label != "reviewer(claude-01) | fix login" {
		t.Errorf("the pane label carries the Epic: %q", label)
	}
	desired := core.TitleState{Status: core.TitleDesired, Desired: "reviewer(claude-01) | [auth-refresh] fix login", Offered: "reviewer(claude-01) | [auth-refresh]"}
	wantState(t, e, "task set", desired)
	if got := hookTitle(t, e, core.Claude, EventPostTool, in); got != "" {
		t.Errorf("PostToolUse carried title %q", got)
	}
	wantState(t, e, "after PostToolUse", desired)
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "reviewer(claude-01) | [auth-refresh] fix login" {
		t.Errorf("prompt after the task changed: title %q", got)
	}
	wantState(t, e, "prompt", core.TitleState{Status: core.TitleOffered, Desired: "reviewer(claude-01) | [auth-refresh] fix login", Offered: "reviewer(claude-01) | [auth-refresh] fix login"})
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "" {
		t.Errorf("the same title was given twice: %q", got)
	}

	term.mu.Lock()
	term.sessionLabel = "auth v2"
	term.mu.Unlock()
	if got := hookTitle(t, e, core.Claude, EventPrompt, in); got != "reviewer(claude-01) | [auth v2] fix login" {
		t.Errorf("prompt after the Epic was renamed: title %q", got)
	}
	if term.label != "reviewer(claude-01) | fix login" {
		t.Errorf("the pane label changed with the Epic: %q", term.label)
	}
}

// A session Rex named itself ("Session <8 hex>") or nobody named names no
// Epic: the conversation title is the pane label.
func TestEpicTitlePlaceholderSessionGivesThePaneLabel(t *testing.T) {
	for _, epic := range []string{"", "Session 01a1cad4"} {
		e := realAmqEnv(t, epicWorld(epic))
		if got := hookTitle(t, e, core.Claude, EventSessionStart, HookInput{SessionID: "s1"}); got != "reviewer(claude-01)" {
			t.Errorf("session %q: title %q", epic, got)
		}
		setTask(t, e, block, "fix login")
		if got := hookTitle(t, e, core.Claude, EventPrompt, HookInput{SessionID: "s1"}); got != "reviewer(claude-01) | fix login" {
			t.Errorf("session %q: title %q", epic, got)
		}
		_ = e.Depart(context.Background(), Location{Block: block}, HookInput{})
	}
}

// Pi is offered its title on any hook; a role taken with join is desired
// until then.
func TestEpicTitlePiOffersOnAnyHook(t *testing.T) {
	e, term := joinWorld(t, "Window 1a2b")
	term.mu.Lock()
	term.sessionLabel = "auth-refresh"
	term.mu.Unlock()
	if _, err := e.Join(context.Background(), Location{Block: block}, "", "reviewer"); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.Record(block)
	want := "reviewer(" + string(rec.Handle) + ") | [auth-refresh]"
	if st := titleState(t, e); st.Status != core.TitleDesired || st.Desired != want || st.Offered != string(rec.Handle) {
		t.Errorf("after join: title state %+v, want desired %q over offered %q", st, want, rec.Handle)
	}
	if got := hookTitle(t, e, core.Pi, EventMail, HookInput{SessionID: "s1"}); got != want {
		t.Errorf("mail after join: title %q, want %q", got, want)
	}
	wantState(t, e, "mail", core.TitleState{Status: core.TitleOffered, Desired: want, Offered: want})
}

// Codex and agy take no conversation title: they are tracked unsupported with
// the title they would have, and agy's terminal window title, which shows the
// pane label, never counts as one offered.
func TestEpicTitleUnsupportedForCodexAndAgy(t *testing.T) {
	for _, c := range []struct {
		kind core.Kind
		in   HookInput
	}{
		{core.Codex, HookInput{SessionID: "x1", TranscriptPath: []byte(`"/tmp/x1.jsonl"`)}},
		{core.Agy, HookInput{ConversationID: "a1"}},
	} {
		e := realAmqEnv(t, epicWorld("auth-refresh"))
		if got := hookTitle(t, e, c.kind, EventSessionStart, c.in); got != "" {
			t.Errorf("%s start carried title %q", c.kind, got)
		}
		rec, _, _ := e.Store.Record(block)
		head := core.PaneHead(rec.Handle, rec.Role)
		wantState(t, e, string(c.kind)+" start", core.TitleState{Status: core.TitleUnsupported, Desired: head + " | [auth-refresh]"})
		setTask(t, e, block, "fix login")
		wantState(t, e, string(c.kind)+" task set", core.TitleState{Status: core.TitleUnsupported, Desired: head + " | [auth-refresh] fix login"})
		if c.kind == core.Agy {
			e.Ancestors = func() ([]proc, error) { return []proc{{rec.AgentPID, "agy"}}, nil }
			if got := e.PaneTitle(Location{Block: block}, core.Agy, "/w/proj"); got != head+" | fix login" {
				t.Errorf("agy's window title %q is not its pane label", got)
			}
		}
		for _, event := range []string{EventPrompt, EventPreInvocation, EventPostTool} {
			if got := hookTitle(t, e, c.kind, event, c.in); got != "" {
				t.Errorf("%s %s carried title %q", c.kind, event, got)
			}
		}
		wantState(t, e, string(c.kind)+" after its hooks", core.TitleState{Status: core.TitleUnsupported, Desired: head + " | [auth-refresh] fix login"})
		_ = e.Depart(context.Background(), Location{Block: block}, HookInput{})
	}
}

// The guide agents read keeps the Rex session label for the human's Epic.
func TestEpicTitleGuideForbidsSessionRename(t *testing.T) {
	text := strings.Join(strings.Fields(guide.Skill), " ")
	for _, rule := range []string{"one Rex session is one Epic", "Never run `rex session rename` for your own task"} {
		if !strings.Contains(text, rule) {
			t.Errorf("the guide does not say %q", rule)
		}
	}
}
