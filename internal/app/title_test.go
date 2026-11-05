package app

import (
	"context"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func setTask(t *testing.T, e Env, b, task string) string {
	t.Helper()
	label, err := e.SetTask(context.Background(), Location{Block: b}, task)
	if err != nil {
		t.Fatal(err)
	}
	return label
}

// The agent says what it is doing and the pane shows it after the handle; a
// blank task clears it again.
func TestTitleShowsTheTaskAfterTheHandle(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	a := adoptIn(t, e, block, "s1", 0)
	if label := setTask(t, e, block, "  fix login\nredirect "); label != "codex-reviewer-01 | fix login redirect" || term.label != label {
		t.Fatalf("label %q, pane %q", label, term.label)
	}
	if rec := recordOf(t, e, a.Handle); rec.Task != "fix login redirect" {
		t.Errorf("task not recorded: %+v", rec)
	}
	if label := setTask(t, e, block, ""); label != "codex-reviewer-01" || term.label != label {
		t.Errorf("a blank task left %q, pane %q", label, term.label)
	}
}

// Adoption draws the label from the record, so a repeated SessionStart or a
// compaction keeps the task instead of resetting the label to the handle.
func TestReadoptionKeepsTheTask(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	adoptIn(t, e, block, "s1", 0)
	want := setTask(t, e, block, "fix login")
	adoptIn(t, e, block, "s1", 0)
	if term.label != want {
		t.Errorf("re-adoption changed the label to %q, want %q", term.label, want)
	}
}

// The task follows the handle: a session resumed in a new pane shows it there.
func TestResumedAgentShowsItsTaskInANewPane(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	a := adoptIn(t, e, block, "s1", 0)
	want := setTask(t, e, block, "fix login")
	depart(t, e, block, "s1")
	term.mu.Lock()
	term.label = "shell" // the new pane
	term.mu.Unlock()
	resumed := adoptIn(t, e, "block:b", "s1", 0)
	if resumed.Handle != a.Handle || term.label != want {
		t.Errorf("resumed as %s with label %q, want %s with %q", resumed.Handle, term.label, a.Handle, want)
	}
}

// A new agent started in a pane whose label still shows the last agent's
// task is named for the role alone and starts without a task.
func TestNewAgentInADescribedPaneKeepsOnlyTheRole(t *testing.T) {
	term := &fakeTerm{label: "reviewer"}
	e := realAmqEnv(t, term)
	adoptIn(t, e, block, "s1", 0)
	setTask(t, e, block, "fix login")
	depart(t, e, block, "s1")
	next := adoptIn(t, e, block, "s2", 0)
	if next.Handle != "codex-reviewer-02" || next.Task != "" || term.label != "codex-reviewer-02" {
		t.Errorf("new agent %s, task %q, label %q", next.Handle, next.Task, term.label)
	}
}

func TestTitleNeedsAnIdentity(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	if _, err := e.SetTask(context.Background(), Location{Block: "block:none"}, "x"); err == nil {
		t.Fatal("a block without an agent got a task")
	}
}

// The agent learns the command with its identity.
func TestIdentityNoticeNamesTheTitleCommand(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	loc := Location{Block: block}
	r, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	if !strings.Contains(r.Notice, e.SelfBin+` title "<task in a few words>"`) {
		t.Errorf("identity notice does not name the title command: %q", r.Notice)
	}
}
