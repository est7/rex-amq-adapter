package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// cmdEnv is the value cmd's env prefix gives name; "" when none.
func cmdEnv(cmd []string, name string) string {
	if len(cmd) == 0 || cmd[0] != "env" {
		return ""
	}
	for _, a := range cmd[1:] {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			break
		}
		if k == name {
			return v
		}
	}
	return ""
}

// countInits makes e's amq count `amq init` (EnsureMailbox) calls.
func countInits(e *Env) *int {
	n := new(int)
	e.AMQ = e.AMQ.(*amq.Fake).WithFail(func(op string, _ []string) error {
		if op == "init" {
			*n++
		}
		return nil
	})
	return n
}

// mailboxMade reports whether h is registered in the root's roster and has
// its mailbox: what `amq init` makes, which a send to h alone does not.
func mailboxMade(e Env, h core.Handle) bool {
	agents, _ := amq.Agents(e.Paths.Root())
	st, err := os.Stat(amq.Inbox(e.Paths.Root(), string(h)))
	return err == nil && st.IsDir() && slices.Contains(agents, string(h))
}

// unregister rewrites the root's roster without hs, as a pruned roster is
// (a test fixture: the adapter itself never writes amq's config).
func unregister(t *testing.T, e Env, hs ...string) {
	t.Helper()
	path := filepath.Join(e.Paths.Root(), "meta", "config.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		b = []byte(`{"agents":[]}`)
	} else if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	var keep []string
	for _, a := range cfg["agents"].([]any) {
		if !slices.Contains(hs, a.(string)) {
			keep = append(keep, a.(string))
		}
	}
	cfg["agents"] = keep
	b, _ = json.Marshal(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if err := os.RemoveAll(filepath.Join(e.Paths.Root(), "agents", h)); err != nil {
			t.Fatal(err)
		}
	}
}

// A summon that starts an agent reserves its handle and makes its mailbox
// before the pane opens; the agent's first hook takes that handle and finds
// the mailbox made, so it runs no `amq init` inside its budget.
func TestASummonedStartFindsItsHandleAndMailboxMade(t *testing.T) {
	e, term, loc := summonWorld(t)
	var madeAtLaunch bool
	term.started = func(l launch) { madeAtLaunch = mailboxMade(e, reservedIn(t, e, l.Command)) }
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "开个 codex 审一下" })
	if res.Action != "start" {
		t.Fatalf("%+v", res)
	}
	l := term.launches[0]
	h := reservedIn(t, e, l.Command)
	if !madeAtLaunch {
		t.Fatalf("the mailbox of %s was not made before its pane opened", h)
	}
	inits := countInits(&e)
	term.mu.Lock()
	term.label = l.Label
	term.mu.Unlock()
	a, err := e.Adopt(context.Background(), Location{Block: l.Block}, core.Codex, HookInput{SessionID: "thread-9", SummonToken: tokenOf(l), Reservation: cmdEnv(l.Command, ReservationEnv)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.AMQ.RetireHandle(context.Background(), e.Paths.Root(), string(a.Agent.Handle)) })
	if a.Agent.Handle != h || *inits != 0 {
		t.Fatalf("adopted as %s with %d amq init(s); want %s and none", a.Agent.Handle, *inits, h)
	}
	if _, ok, _ := e.Store.ReservationOf(cmdEnv(l.Command, ReservationEnv)); ok {
		t.Fatal("the adoption did not consume its reservation")
	}
}

// reservedIn is the handle the reservation cmd carries keeps.
func reservedIn(t *testing.T, e Env, cmd []string) core.Handle {
	t.Helper()
	r, ok, err := e.Store.ReservationOf(cmdEnv(cmd, ReservationEnv))
	if err != nil || !ok {
		t.Fatalf("no reservation in %q: %v", cmd, err)
	}
	return r.Handle
}

// A summon that resumes a departed agent makes its mailbox again before the
// pane opens (its roster entry may have been pruned), and carries no
// reservation: the resume takes its own handle.
func TestASummonedResumeFindsItsMailboxMade(t *testing.T) {
	e, term, loc := summonWorld(t)
	add(t, e, departed(seed{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", AdoptedAt: e.Now().Add(-time.Hour), Cwd: workDir(t, e, "app")}, e.Now().Add(-time.Minute)))
	unregister(t, e, "codex-02")
	var madeAtLaunch bool
	term.started = func(launch) { madeAtLaunch = mailboxMade(e, "codex-02") }
	res := summon(t, e, loc, "reviewer")
	if res.Action != "resume" || !madeAtLaunch || cmdEnv(term.launches[0].Command, ReservationEnv) != "" {
		t.Fatalf("%+v mailbox made at launch=%v command=%q", res, madeAtLaunch, term.launches[0].Command)
	}
}

// An Epic's two agents start with reserved handles whose mailboxes exist
// when the session is created, and their first hooks take them.
func TestAnEpicsAgentsStartWithTheirMailboxesMade(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	fake := &amq.Fake{}
	e.AMQ = fake
	var made []bool
	var cmds [][]string
	term.sessionStarted = func(_ string, _ []string) {
		for _, p := range term.sessions_[len(term.sessions_)-1].panes {
			cmds = append(cmds, p.Command)
			r, ok, _ := e.Store.ReservationOf(cmdEnv(p.Command, ReservationEnv))
			made = append(made, ok && mailboxMade(e, r.Handle))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = e.OpenEpic(ctx, loc, EpicRequest{Name: "login revamp", Dir: filepath.Dir(goal), Goal: goal})
	if len(made) != 2 || !made[0] || !made[1] {
		t.Fatalf("mailboxes made when the session started: %v (commands %q)", made, cmds)
	}
	narr, lead := reservedIn(t, e, cmds[0]), reservedIn(t, e, cmds[1])
	if narr == lead || narr == "" || lead == "" {
		t.Fatalf("reserved %q and %q", narr, lead)
	}
}

// A reservation names a handle only for an agent of its kind, while it is
// unexpired and nothing else took the name; other new agents avoid it.
func TestAReservationKeepsItsNameForItsLaunchOnly(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	tok, h, err := e.reserveHandle(ctx, core.Codex)
	if err != nil || h != "codex-01" || !mailboxMade(e, h) {
		t.Fatalf("reserved %q (mailbox %v): %v", h, mailboxMade(e, h), err)
	}
	other, err := e.Adopt(ctx, Location{Block: "block:x"}, core.Codex, HookInput{SessionID: "other"})
	if err != nil || other.Agent.Handle == h {
		t.Fatalf("a new agent without the reservation took %s: %+v %v", h, other.Agent, err)
	}
	if got, ok, _ := e.reservedHandle(tok, core.Claude, map[core.Handle]bool{}); ok {
		t.Fatalf("a claude adoption took a codex reservation: %s", got)
	}
	expired := e
	expired.Now = func() time.Time { return time.Now().Add(store.ReservationFor + time.Minute) }
	if got, ok, _ := expired.reservedHandle(tok, core.Codex, map[core.Handle]bool{}); ok {
		t.Fatalf("an expired reservation named %s", got)
	}
	if got, ok, _ := e.reservedHandle(tok, core.Codex, map[core.Handle]bool{h: true}); ok {
		t.Fatalf("a reservation whose name was taken named %s", got)
	}
	// A lower name freed meanwhile does not take the reserved one's place.
	tok2, h2, err := e.reserveHandle(ctx, core.Codex)
	if err != nil {
		t.Fatal(err)
	}
	e.dropReservation(tok)
	a, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "mine", Reservation: tok2})
	if err != nil || a.Agent.Handle != h2 || h2 == h {
		t.Fatalf("the reserved launch adopted as %+v, reserved %s (%s freed): %v", a.Agent, h2, h, err)
	}
}

// A revive makes each resumed handle's mailbox again before it types the
// resume into its pane (its roster entry may have been pruned since).
func TestReviveMakesEachResumedMailboxFirst(t *testing.T) {
	w := newReviveWorld(t)
	unregister(t, w.e, "pi-01", "pi-02", "pi-03")
	handleOf := map[string]core.Handle{"block:o": "pi-01", "block:n": "pi-02", "block:w": "pi-03"}
	var mu sync.Mutex
	madeAtResume := map[core.Handle]bool{}
	term := w.e.Rex.(*fakeTerm)
	resumed := term.wrote
	term.wrote = func(block, data string) {
		if h, ok := handleOf[block]; ok && strings.HasPrefix(data, "/bin/sh ") {
			mu.Lock()
			madeAtResume[h] = mailboxMade(w.e, h)
			mu.Unlock()
		}
		resumed(block, data)
	}
	if _, err := w.revive(ReviveRequest{Asked: "把 Epic 恢复起来"}); err != nil {
		t.Fatalf("revive: %v\n%s", err, w.diag)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range []core.Handle{"pi-01", "pi-02", "pi-03"} {
		if made, typed := madeAtResume[h]; !typed || !made {
			t.Errorf("%s was resumed (typed %v) without its mailbox", h, typed)
		}
	}
	if mailboxMade(w.e, "pi-04") {
		t.Error("the retired reviewer, not revived, got a mailbox")
	}
}

// failInit makes e's amq fail every `amq init` (EnsureMailbox).
func failInit(e *Env) {
	e.AMQ = e.AMQ.(*amq.Fake).WithFail(func(op string, _ []string) error {
		if op == "init" {
			return errors.New("amq init: signal: killed")
		}
		return nil
	})
}

// A launcher whose agent's mailbox cannot be made does not launch it: its
// first hook would run amq init inside its budget. It says the launch can be
// tried again, and gives the reserved name back.
func TestALauncherThatCannotMakeTheMailboxLaunchesNothing(t *testing.T) {
	e, term, loc := summonWorld(t)
	add(t, e, departed(seed{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", AdoptedAt: e.Now().Add(-time.Hour), Cwd: workDir(t, e, "app")}, e.Now().Add(-time.Minute)))
	unregister(t, e, "codex-02")
	failInit(&e)
	for _, r := range []SummonRequest{
		{Target: "writer", Kind: "question", Subject: "s", Body: "b", Asked: "start one"},
		{Target: "reviewer", Kind: "question", Subject: "s", Body: "b"},
	} {
		if _, err := e.Summon(context.Background(), loc, r); !errors.Is(err, ErrNotProvisioned) {
			t.Errorf("summon %s: %v, want ErrNotProvisioned", r.Target, err)
		}
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched %d panes with no mailbox", len(term.launches))
	}
	if rs, _ := e.Store.Reservations(); len(rs) != 0 {
		t.Fatalf("reservations left: %v", rs)
	}
}

func TestAnEpicWhoseMailboxesCannotBeMadeDoesNotOpen(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	failInit(&e)
	if _, err := e.OpenEpic(context.Background(), loc, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal}); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("open: %v, want ErrNotProvisioned", err)
	}
	if len(term.sessions_) != 0 {
		t.Fatal("Rex was asked for the session")
	}
	if rs, _ := e.Store.Reservations(); len(rs) != 0 {
		t.Fatalf("reservations left: %v", rs)
	}
}

// A revive resumes no agent whose mailbox cannot be made; it says so for that
// agent and resumes the others.
func TestReviveSkipsAnAgentWhoseMailboxCannotBeMade(t *testing.T) {
	w := newReviveWorld(t)
	unregister(t, w.e, "pi-01", "pi-02", "pi-03")
	w.e.AMQ = w.e.AMQ.(*amq.Fake).WithFail(func(op string, args []string) error {
		if op == "init" && slices.Contains(args, "pi-03") {
			return errors.New("amq init: signal: killed")
		}
		return nil
	})
	res, _ := w.revive(ReviveRequest{Asked: "把 Epic 恢复起来"})
	got := byHandle(res)
	if a := got["pi-03"]; a.Status != ReviveRefused || !strings.Contains(a.Reason, "mailbox") || len(w.writes("block:w")) != 0 {
		t.Errorf("pi-03: %+v (writes %d)", a, len(w.writes("block:w")))
	}
	for _, h := range []core.Handle{"pi-01", "pi-02"} {
		if got[h].Status != ReviveReady {
			t.Errorf("%s was not resumed: %+v", h, got[h])
		}
	}
}

// After an ambiguous reply (Rex may have started the pane), the reservation
// stays, so the agent Rex did start still takes its reserved name.
func TestAnAmbiguousLaunchKeepsItsReservation(t *testing.T) {
	e, term, loc := summonWorld(t)
	term.launchThenFail = errors.New("rex: context deadline exceeded")
	if _, err := e.Summon(context.Background(), loc, SummonRequest{Target: "writer", Kind: "question", Subject: "s", Body: "b", Asked: "start one"}); err == nil {
		t.Fatal("setup: the launch should report the lost reply")
	}
	l := term.launches[0]
	if _, ok, _ := e.Store.ReservationOf(cmdEnv(l.Command, ReservationEnv)); !ok {
		t.Fatal("the reservation of a pane Rex may have started was dropped")
	}
	e2, term2, loc2, goal := openEpicWorld(t)
	term2.createThenFail = errors.New("rex: context deadline exceeded")
	_, _ = e2.OpenEpic(context.Background(), loc2, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal})
	for _, p := range term2.sessions_[0].panes {
		if _, ok, _ := e2.Store.ReservationOf(cmdEnv(p.Command, ReservationEnv)); !ok {
			t.Fatal("the reservation of an Epic pane Rex may have started was dropped")
		}
	}
}
