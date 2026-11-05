package app

import (
	"context"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func promptNotice(t *testing.T, e Env, kind core.Kind, event string, in HookInput) string {
	t.Helper()
	r, err := e.Hook(context.Background(), event, kind, Location{Block: block}, in)
	if err != nil {
		t.Fatal(err)
	}
	return r.Notice
}

// Claude Code runs SessionStart before the user asks anything, so its first
// prompt repeats the request to show a task, once per session, and only
// while the pane shows none.
func TestClaudesFirstPromptAsksForATaskOnce(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	s1 := HookInput{SessionID: "s1"}
	if n := promptNotice(t, e, core.Claude, EventSessionStart, s1); !strings.Contains(n, "Pane label:") {
		t.Fatalf("SessionStart does not ask for a task: %q", n)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	if n := promptNotice(t, e, core.Claude, EventPrompt, s1); !strings.Contains(n, "Pane label:") || !strings.Contains(n, e.SelfBin+` title "<task in a few words>"`) {
		t.Errorf("the first prompt was not asked for a task: %q", n)
	}
	if n := promptNotice(t, e, core.Claude, EventPrompt, s1); strings.Contains(n, "Pane label:") {
		t.Errorf("the second prompt was asked again: %q", n)
	}
	// A new session that keeps the handle (/clear) is asked at its first prompt.
	s2 := HookInput{SessionID: "s2"}
	promptNotice(t, e, core.Claude, EventSessionStart, s2)
	if n := promptNotice(t, e, core.Claude, EventPrompt, s2); !strings.Contains(n, "Pane label:") {
		t.Errorf("a new session was not asked: %q", n)
	}
	// Once it shows a task, no request.
	s3 := HookInput{SessionID: "s3"}
	promptNotice(t, e, core.Claude, EventSessionStart, s3)
	setTask(t, e, block, "fix login")
	if n := promptNotice(t, e, core.Claude, EventPrompt, s3); strings.Contains(n, "Pane label:") {
		t.Errorf("an agent with a task was asked for one: %q", n)
	}
}

// Codex and agy run SessionStart with the first prompt, so its request is in
// time and their prompts add none.
func TestOnlyClaudeIsAskedAgainAtItsFirstPrompt(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	in := HookInput{SessionID: "c1"}
	promptNotice(t, e, core.Codex, EventSessionStart, in)
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	if n := promptNotice(t, e, core.Codex, EventPrompt, in); strings.Contains(n, "Pane label:") {
		t.Errorf("Codex was asked again at its prompt: %q", n)
	}
}

// A Claude session first adopted at a prompt (the adapter was installed
// mid-session) is asked for its task once in that notice, not twice.
func TestAClaudeAdoptedAtAPromptIsAskedOnce(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	n := promptNotice(t, e, core.Claude, EventPrompt, HookInput{SessionID: "s1"})
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: block}, HookInput{}) })
	if c := strings.Count(n, "Pane label:"); c != 1 {
		t.Errorf("asked %d times in one notice: %q", c, n)
	}
	if n := promptNotice(t, e, core.Claude, EventPrompt, HookInput{SessionID: "s1"}); strings.Contains(n, "Pane label:") {
		t.Errorf("asked again at the next prompt: %q", n)
	}
}
