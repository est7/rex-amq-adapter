package app

import (
	"errors"
	"testing"
)

// An idle agent runs no hooks, so its dead waker leaves new mail unseen
// until its next turn. Listing agents used to repair it on the side; it is
// a pure read now, so the send that needs the doorbell repairs it: after a
// delivered `mail send` (and reply), every live recipient's waker runs. A
// refused send starts nothing, and a sender's own waker is not its concern.
func TestMailSendRepairsWaker(t *testing.T) {
	e, ctx := managedWorld(t)
	if wakerLive(t, e, "codex-01") || wakerLive(t, e, "claude-09") {
		t.Fatal("setup: a recipient's waker already runs")
	}
	if _, _, err := run(t, e, ctx, "claude-02", "send", "--to", "claude-09", "--subject", "s", "--body", "b"); !errors.Is(err, ErrRefused) {
		t.Fatalf("cross-Epic send: %v", err)
	}
	if wakerLive(t, e, "claude-09") {
		t.Error("a refused send started the recipient's waker")
	}
	if out, code, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--subject", "s", "--body", "b", "--json"); code != 0 || err != nil {
		t.Fatalf("send: %d %v %s", code, err, out)
	}
	if !wakerLive(t, e, "codex-01") {
		t.Fatal("a delivered send left the recipient's waker dead")
	}
	if wakerLive(t, e, "claude-02") {
		t.Error("the send started the sender's waker")
	}
	killWaker(t, e, "codex-01")
	if wakerLive(t, e, "codex-01") {
		t.Fatal("setup: the killed waker still reports live")
	}
	if out, code, err := run(t, e, ctx, "claude-02", "send", "--to", "codex-01", "--subject", "again", "--body", "b"); code != 0 || err != nil {
		t.Fatalf("second send: %d %v %s", code, err, out)
	}
	if !wakerLive(t, e, "codex-01") {
		t.Fatal("a send to an agent whose waker died did not bring it back")
	}
}
