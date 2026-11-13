package amq

import (
	"context"
	"io"
	"time"
)

// CLI runs the amq binary at Bin. It is the production Queue the adapter's
// entry points use; every method is the package function of the same name
// with Bin as its binary. Reading the mailbox files needs no binary and stays
// in the package functions (Unread, ReadHeader, IDs, ...).
type CLI struct{ Bin string }

func (c CLI) EnsureMailbox(ctx context.Context, root, handle string) error {
	return EnsureMailbox(ctx, c.Bin, root, handle)
}

func (c CLI) Ensure(ctx context.Context, w Waker, logDir string) (WakeState, WakeDecision, error) {
	return Ensure(ctx, c.Bin, w, logDir)
}

func (c CLI) RetireFor(ctx context.Context, root, handle, block string) (bool, error) {
	return RetireFor(ctx, c.Bin, root, handle, block)
}

func (c CLI) RetireHandle(ctx context.Context, root, handle string) (bool, error) {
	return RetireHandle(ctx, c.Bin, root, handle)
}

func (c CLI) ConfigureHolds(ctx context.Context, root, handle string, normal, low time.Duration) error {
	return ConfigureHolds(ctx, c.Bin, root, handle, normal, low)
}

func (c CLI) Post(ctx context.Context, root, verb string, args ...string) (Sent, []byte, error) {
	return Post(ctx, c.Bin, root, verb, args...)
}

func (c CLI) ThreadOpen(ctx context.Context, root, me string, handles []string) (OpenObligations, error) {
	return ThreadOpen(ctx, c.Bin, root, me, handles)
}

func (c CLI) ThreadRead(ctx context.Context, root, me, thread string, agents, ids []string) ([]ThreadMessage, error) {
	return ThreadRead(ctx, c.Bin, root, me, thread, agents, ids)
}

func (c CLI) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	return Run(ctx, c.Bin, stdin, stdout, stderr, args...)
}
