package app

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// lockWatch records every Rex or amq call made while lifecycle.lock is held.
// The lock is a flock, so a probe through a fresh descriptor fails while any
// descriptor (this process's included) holds it. Tests that use it run one
// lifecycle operation at a time, so a held lock is the caller's own.
type lockWatch struct {
	st   store.Store
	mu   sync.Mutex
	held []string
}

func (w *lockWatch) note(op string) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled context tries the lock once
	unlock, err := w.st.LockLifecycle(ctx)
	if err != nil {
		w.mu.Lock()
		w.held = append(w.held, op)
		w.mu.Unlock()
		return
	}
	unlock()
}

// violations is the calls made under the lifecycle lock so far.
func (w *lockWatch) violations() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.held...)
}

// watchLocks wraps e's Rex and amq so every call is checked against the
// lifecycle lock.
func watchLocks(e *Env) *lockWatch {
	w := &lockWatch{st: e.Store}
	rexOn := e.RexOn
	e.Rex = watchedTerm{e.Rex, w}
	if rexOn != nil {
		e.RexOn = func(server string) Terminal { return watchedTerm{rexOn(server), w} }
	}
	e.AMQ = watchedQueue{e.AMQ, w}
	return w
}

type watchedTerm struct {
	t Terminal
	w *lockWatch
}

func (m watchedTerm) Whoami(ctx context.Context) (rex.Server, error) {
	m.w.note("rex Whoami")
	return m.t.Whoami(ctx)
}
func (m watchedTerm) Foreground(ctx context.Context, block string) (rex.Process, bool, error) {
	m.w.note("rex Foreground")
	return m.t.Foreground(ctx, block)
}
func (m watchedTerm) ProgramStatus(ctx context.Context, block string) (rex.Status, bool, error) {
	m.w.note("rex ProgramStatus")
	return m.t.ProgramStatus(ctx, block)
}
func (m watchedTerm) CaptureVT(ctx context.Context, block string) (string, error) {
	m.w.note("rex CaptureVT")
	return m.t.CaptureVT(ctx, block)
}
func (m watchedTerm) Write(ctx context.Context, block, data string) error {
	m.w.note("rex Write")
	return m.t.Write(ctx, block, data)
}
func (m watchedTerm) SendKeys(ctx context.Context, block string, keys ...string) error {
	m.w.note("rex SendKeys")
	return m.t.SendKeys(ctx, block, keys...)
}
func (m watchedTerm) InspectBlock(ctx context.Context, block string) (rex.Block, error) {
	m.w.note("rex InspectBlock")
	return m.t.InspectBlock(ctx, block)
}
func (m watchedTerm) InspectSession(ctx context.Context, session string) (rex.Session, error) {
	m.w.note("rex InspectSession")
	return m.t.InspectSession(ctx, session)
}
func (m watchedTerm) ListBlocks(ctx context.Context, session string) ([]rex.Block, error) {
	m.w.note("rex ListBlocks")
	return m.t.ListBlocks(ctx, session)
}
func (m watchedTerm) RenameBlock(ctx context.Context, block, label string) error {
	m.w.note("rex RenameBlock")
	return m.t.RenameBlock(ctx, block, label)
}
func (m watchedTerm) RenameWindow(ctx context.Context, window, label string) error {
	m.w.note("rex RenameWindow")
	return m.t.RenameWindow(ctx, window, label)
}
func (m watchedTerm) Title(ctx context.Context, block string) (string, error) {
	m.w.note("rex Title")
	return m.t.Title(ctx, block)
}
func (m watchedTerm) ListSessions(ctx context.Context) ([]string, error) {
	m.w.note("rex ListSessions")
	return m.t.ListSessions(ctx)
}
func (m watchedTerm) NewSplit(ctx context.Context, session, anchor, label, dir string, command []string) (string, error) {
	m.w.note("rex NewSplit")
	return m.t.NewSplit(ctx, session, anchor, label, dir, command)
}
func (m watchedTerm) NewWindow(ctx context.Context, session, tab, label, dir string, command []string) (string, string, error) {
	m.w.note("rex NewWindow")
	return m.t.NewWindow(ctx, session, tab, label, dir, command)
}
func (m watchedTerm) NewSession(ctx context.Context, label string, panes []rex.Pane) (string, []string, []string, error) {
	m.w.note("rex NewSession")
	return m.t.NewSession(ctx, label, panes)
}
func (m watchedTerm) Size(ctx context.Context, block string) (rex.Size, error) {
	m.w.note("rex Size")
	return m.t.Size(ctx, block)
}
func (m watchedTerm) DetachBlock(ctx context.Context, session, block string) error {
	m.w.note("rex DetachBlock")
	return m.t.DetachBlock(ctx, session, block)
}
func (m watchedTerm) ResizeBlock(ctx context.Context, block string, columns, rows int) error {
	m.w.note("rex ResizeBlock")
	return m.t.ResizeBlock(ctx, block, columns, rows)
}
func (m watchedTerm) NewTab(ctx context.Context, session, label string) (string, string, error) {
	m.w.note("rex NewTab")
	return m.t.NewTab(ctx, session, label)
}
func (m watchedTerm) MoveBlock(ctx context.Context, session, block, anchor string) error {
	m.w.note("rex MoveBlock")
	return m.t.MoveBlock(ctx, session, block, anchor)
}
func (m watchedTerm) CloseBlock(ctx context.Context, block string) error {
	m.w.note("rex CloseBlock")
	return m.t.CloseBlock(ctx, block)
}
func (m watchedTerm) KillSession(ctx context.Context, session string) error {
	m.w.note("rex KillSession")
	return m.t.KillSession(ctx, session)
}

type watchedQueue struct {
	q Queue
	w *lockWatch
}

func (m watchedQueue) EnsureMailbox(ctx context.Context, root, handle string) error {
	m.w.note("amq EnsureMailbox")
	return m.q.EnsureMailbox(ctx, root, handle)
}
func (m watchedQueue) RemoveMailbox(ctx context.Context, root, handle string) (amq.MailboxRemoval, error) {
	m.w.note("amq RemoveMailbox")
	return m.q.RemoveMailbox(ctx, root, handle)
}
func (m watchedQueue) Ensure(ctx context.Context, wk amq.Waker, logDir string) (amq.WakeState, amq.WakeDecision, error) {
	m.w.note("amq Ensure")
	return m.q.Ensure(ctx, wk, logDir)
}
func (m watchedQueue) RetireHandle(ctx context.Context, root, handle string) (bool, error) {
	m.w.note("amq RetireHandle")
	return m.q.RetireHandle(ctx, root, handle)
}
func (m watchedQueue) ConfigureHolds(ctx context.Context, root, handle string, normal, low time.Duration) error {
	m.w.note("amq ConfigureHolds")
	return m.q.ConfigureHolds(ctx, root, handle, normal, low)
}
func (m watchedQueue) Post(ctx context.Context, root, verb string, args ...string) (amq.Sent, []byte, error) {
	m.w.note("amq Post")
	return m.q.Post(ctx, root, verb, args...)
}
func (m watchedQueue) ThreadOpen(ctx context.Context, root, me string, handles []string) (amq.OpenObligations, error) {
	m.w.note("amq ThreadOpen")
	return m.q.ThreadOpen(ctx, root, me, handles)
}
func (m watchedQueue) ThreadRead(ctx context.Context, root, me, thread string, agents, ids []string) ([]amq.ThreadMessage, error) {
	m.w.note("amq ThreadRead")
	return m.q.ThreadRead(ctx, root, me, thread, agents, ids)
}
func (m watchedQueue) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	m.w.note("amq Run")
	return m.q.Run(ctx, stdin, stdout, stderr, args...)
}
