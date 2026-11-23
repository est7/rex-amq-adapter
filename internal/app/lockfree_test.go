package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// The lifecycle lock serialises every adoption, departure, and collection on
// the host, and each hook waits for it inside a budget of a few seconds. A
// Rex or amq call made under it (amq init --force alone took 8-10 s under
// write load) made every other pane's hook time out, so the lock holds local
// file reads and writes only.

func assertNoCallsUnderLock(t *testing.T, w *lockWatch, what string) {
	t.Helper()
	if v := w.violations(); len(v) > 0 {
		t.Fatalf("%s called Rex or amq under the lifecycle lock: %v", what, v)
	}
}

func TestAdoptCallsNoRexOrAmqUnderTheLifecycleLock(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	plain := e // reads the fake amq
	w := watchLocks(&e)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := e.Hook(ctx, EventSessionStart, core.Codex, Location{Block: block}, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := e.agentAt(block)
	if err != nil || !ok || !rec.Adopted() || rec.Handle != "codex-01" || r.Notice == "" {
		t.Fatalf("adoption: %+v ok=%v err=%v notice=%q", rec, ok, err, r.Notice)
	}
	if !wakerLive(t, plain, "codex-01") {
		t.Fatal("the adopted agent has no live waker")
	}
	assertNoCallsUnderLock(t, w, "adoption")
}

func TestDepartureCallsNoRexOrAmqUnderTheLifecycleLock(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	plain := e // reads the fake amq
	w := watchLocks(&e)
	if err := e.Depart(ctx, Location{Block: block}, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := e.agentAt(block); rec.Live() || wakerLive(t, plain, "codex-01") {
		t.Fatalf("after departure: live=%v waker live=%v", rec.Live(), wakerLive(t, plain, "codex-01"))
	}
	assertNoCallsUnderLock(t, w, "departure")
}

func TestCollectionCallsNoRexOrAmqUnderTheLifecycleLock(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	plain := e // reads the fake amq
	w := watchLocks(&e)
	term.gone = true
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := e.agentAt(block); rec.Live() || wakerLive(t, plain, "codex-01") {
		t.Fatalf("after collection: live=%v waker live=%v", rec.Live(), wakerLive(t, plain, "codex-01"))
	}
	assertNoCallsUnderLock(t, w, "collection")
}

func TestStopSupervisionCallsNoAmqUnderTheLifecycleLock(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	killWaker(t, e, "codex-01")
	plain := e // reads the fake amq
	w := watchLocks(&e)
	if err := e.SuperviseWaker(ctx, Location{Block: block}); err != nil {
		t.Fatal(err)
	}
	if !wakerLive(t, plain, "codex-01") {
		t.Fatal("supervision did not bring the waker back")
	}
	assertNoCallsUnderLock(t, w, "supervision")
}

func TestOpenEpicCreatesItsSessionOutsideTheLifecycleLock(t *testing.T) {
	e, _, loc, goal := openEpicWorld(t)
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	w := watchLocks(&e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "login revamp", Dir: filepath.Dir(goal), Goal: goal}); err != nil {
		t.Fatal(err)
	}
	assertNoCallsUnderLock(t, w, "opening an Epic")
}

func TestSetTaskRenamesThePaneOutsideTheLifecycleLock(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := realAmqEnv(t, term)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	w := watchLocks(&e)
	label, err := e.SetTask(ctx, Location{Block: block}, "fix login")
	if err != nil || term.renamed[block] != label || !strings.Contains(label, "fix login") {
		t.Fatalf("label %q (pane shows %q): %v", label, term.renamed[block], err)
	}
	assertNoCallsUnderLock(t, w, "agent title")
}
