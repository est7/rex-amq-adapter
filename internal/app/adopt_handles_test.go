package app

import (
	"context"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
)

// A mailbox no record names is reused for a new agent only when it holds no
// unread mail: mail sent to a handle that left is never read by a stranger.
func TestANewAgentSkipsAHandleWithUnreadMail(t *testing.T) {
	for _, c := range []struct {
		name   string
		unread bool
		want   core.Handle
	}{
		{"unread mail", true, "agy-02"},
		{"empty inbox", false, "agy-01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := realAmqEnv(t, &fakeTerm{state: "working"})
			ctx := context.Background()
			if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), "agy-01"); err != nil {
				t.Fatal(err)
			}
			if c.unread {
				if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "alice", "agy-01", "hi", "for the agent that left"); err != nil {
					t.Fatal(err)
				}
			}
			loc := Location{Block: block}
			if _, err := e.Hook(ctx, EventSessionStart, core.Agy, loc, HookInput{ConversationID: "c"}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Depart(ctx, loc, HookInput{}) })
			if rec, _, _ := e.Store.Record(block); rec.Handle != c.want {
				t.Errorf("handle %q, want %q", rec.Handle, c.want)
			}
		})
	}
}
