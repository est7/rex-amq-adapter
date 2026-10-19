package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
)

// realAmqEnv is fixture plus the real amq binary and an injector script, for
// the lifecycle paths that start and retire wakers.
func realAmqEnv(t *testing.T, term *fakeTerm) Env {
	t.Helper()
	amqBin, err := amq.ResolveBin()
	if err != nil {
		t.Skip(err)
	}
	e := fixture(t, term)
	if err := e.Store.DeleteRecord(block); err != nil {
		t.Fatal(err)
	}
	injector := filepath.Join(t.TempDir(), "inject.sh")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\necho AMQ_INJECT_PROGRESS=deferred >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.AmqBin, e.SelfBin = amqBin, injector
	return e
}

func wakeState(t *testing.T, e Env, h string) amq.WakeState {
	t.Helper()
	st, err := amq.WakeCheck(context.Background(), e.AmqBin, e.Paths.Root(), h)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func wakerLive(t *testing.T, e Env, h string) bool { return wakeState(t, e, h).Live }

func TestAdoptThenDepartRetiresTheWaker(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "codex", block) })
	first := wakeState(t, e, "codex")
	if a.Record.Handle != "codex" || !first.Live || first.Generation == "" {
		t.Fatalf("adoption: %+v waker %+v", a.Record, first)
	}
	again, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"})
	if err != nil || again.Decision != amq.DecisionKeep || wakeState(t, e, "codex").Generation != first.Generation {
		t.Fatalf("re-adopting the same agent must keep its waker: %+v %v", again, err)
	}
	if err := e.Depart(ctx, Location{Block: block}, HookInput{SessionID: "another-session"}); err != nil {
		t.Fatal(err)
	}
	if !wakerLive(t, e, "codex") {
		t.Fatal("a SessionEnd from another agent session must change nothing")
	}
	if err := e.Depart(ctx, Location{Block: block}, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.Record(block)
	if rec.Live() || wakerLive(t, e, "codex") {
		t.Fatalf("after departure: live=%v waker live=%v", rec.Live(), wakerLive(t, e, "codex"))
	}
}

// The record is written before the waker starts, so an adoption that fails
// while starting it still leaves the record departure and collection use.
func TestFailedWakerStartLeavesTheRecord(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	// Make the waker unstartable: the injector wrapper's directory is a file.
	if err := os.MkdirAll(e.Paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(e.injector()), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{}); err == nil {
		t.Fatal("adoption with a missing injector succeeded")
	}
	if rec, ok, err := e.Store.Record(block); err != nil || !ok || !rec.Live() {
		t.Fatalf("no record left by a failed adoption: %+v %v %v", rec, ok, err)
	}
}

func TestCollectParksOnlyProvablyGoneAgents(t *testing.T) {
	term := &fakeTerm{}
	e := realAmqEnv(t, term)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "codex", block) })

	term.fgPID = 1 // an editor in front of a live agent
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := e.Store.Record(block); !rec.Live() || !wakerLive(t, e, "codex") {
		t.Fatal("a live agent behind another process was collected")
	}

	term.gone = true
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := e.Store.Record(block); rec.Live() || wakerLive(t, e, "codex") {
		t.Fatal("an agent whose block is gone was not collected")
	}

	// The reservation ends after ReserveFor.
	e.Now = func() time.Time { return time.Now().Add(ReserveFor + time.Hour) }
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Store.Record(block); ok {
		t.Fatal("an expired reservation was kept")
	}
}

// Pi delivers in-process: adopting it starts no waker, and retires one an
// earlier agent left in the same block so nothing types into Pi.
func TestPiAdoptionHasNoWaker(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "codex", block) })
	if !wakerLive(t, e, "codex") {
		t.Fatal("setup: codex waker not live")
	}
	a, err := e.Adopt(ctx, Location{Block: block}, core.Pi, HookInput{AgentPID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	if a.Record.Handle != "codex" || a.Record.Kind != core.Pi || a.Record.AgentPID != 4242 {
		t.Fatalf("pi record: %+v", a.Record)
	}
	if wakerLive(t, e, "codex") {
		t.Fatal("the previous agent's waker survived; it would type into Pi")
	}
}

// amq wake has no supervisor of its own. An agent ending its turn is the
// moment it starts depending on its waker, so Stop brings a dead one back.
func TestStopRevivesADeadWaker(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "codex", block) })
	dead := wakeState(t, e, "codex")
	if err := syscall.Kill(dead.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50 && wakerLive(t, e, "codex"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if wakerLive(t, e, "codex") {
		t.Fatal("setup: the killed waker still reports live")
	}
	if _, err := e.Hook(ctx, EventStop, core.Codex, Location{Block: block}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if st := wakeState(t, e, "codex"); !st.Live || st.PID == dead.PID {
		t.Fatalf("Stop left the waker dead: %+v", st)
	}
}

// Pi has no waker; ending its run must not start one.
func TestStopStartsNoWakerForPi(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Pi, HookInput{AgentPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Hook(ctx, EventSettle, core.Pi, Location{Block: block}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if wakerLive(t, e, "pi") {
		_, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "pi", block)
		t.Fatal("a waker was started for Pi")
	}
}

// Homebrew installs into group-writable directories (Cellar is 0775), and
// amq refuses an --inject-via under one. Adoption must still start a waker:
// amq runs a wrapper from the private state directory instead.
func TestWakerStartsWhenTheBinaryLivesInAGroupWritableDir(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	cellar := filepath.Join(t.TempDir(), "Cellar")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cellar, 0o775); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(cellar, "inject.sh")
	script, err := os.ReadFile(e.SelfBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = bin
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{}); err != nil {
		t.Fatalf("adoption with the binary under a group-writable dir: %v", err)
	}
	t.Cleanup(func() { _, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), "codex", block) })
	st := wakeState(t, e, "codex")
	if !st.Live {
		t.Fatalf("waker not live: %+v", st)
	}
	// The binary moves (go install to brew, or an upgrade): the waker's
	// target stays the wrapper, so supervision keeps the same waker.
	moved := filepath.Join(t.TempDir(), "moved.sh")
	if err := os.WriteFile(moved, script, 0o755); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = moved
	if err := e.SuperviseWaker(ctx, Location{Block: block}); err != nil {
		t.Fatal(err)
	}
	if again := wakeState(t, e, "codex"); !again.Live || again.Generation != st.Generation {
		t.Fatalf("a moved binary replaced the waker: %+v -> %+v", st, again)
	}
	w, err := os.ReadFile(e.injector())
	if err != nil || !strings.Contains(string(w), moved) {
		t.Fatalf("the wrapper does not run the moved binary: %q %v", w, err)
	}
}
