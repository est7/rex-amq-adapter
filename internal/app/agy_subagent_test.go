package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// agy runs a subagent as a conversation of its own in the parent's process,
// and every hook (SessionStart, PreInvocation, Stop) runs for it with the
// subagent's conversationId; agy lists the subagent under its parent's
// brain directory before its first hook (agy's plan review, 2026-11-05: a
// subagent of agy-18 was told "you are agy-18"). A subagent's hooks must do
// nothing: no identity, no mail of its parent's, no held Stop, no record.
func TestAnAgySubagentsHooksDoNothing(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "working"})
	e.AgyBrainDir = t.TempDir()
	sub := filepath.Join(e.AgyBrainDir, "parent", ".system_generated", "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "child.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	loc := Location{Block: block}
	if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "parent"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
	parent, _, _ := e.Store.Record(block)
	for _, event := range []string{EventSessionStart, EventPreInvocation, EventStop} {
		r, err := e.Hook(ctx, event, core.Agy, loc, HookInput{ConversationID: "child", TerminationReason: "NO_TOOL_CALL"})
		if err != nil || r.Notice != "" {
			t.Errorf("%s of a subagent: notice %q, err %v", event, r.Notice, err)
		}
		if now, _, _ := e.Store.Record(block); now != parent {
			t.Fatalf("%s of a subagent changed the parent's record: %+v, was %+v", event, now, parent)
		}
	}
	// The parent itself is not taken for a subagent.
	if r, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "parent"}); err != nil || r.Notice == "" {
		t.Errorf("the parent's own SessionStart: %q %v", r.Notice, err)
	}
}
