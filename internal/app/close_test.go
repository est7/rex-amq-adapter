package app

import (
	"context"
	"errors"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// closeWorld is a personal agent (claude-01, session:me) linked to the Epic
// session:epic, whose orchestrator (block:o), worker (block:w), and
// narrator (block:n) are adopted Pi agents at rest; block:x is a worker of
// session:other, which no close may touch.
func closeWorld(t *testing.T) (Env, *fakeTerm, Location) {
	t.Helper()
	term := &fakeTerm{state: "idle", statusGone: true,
		windows:  map[string]string{"block:me": "window:me", "block:o": "window:o", "block:w": "window:w", "block:n": "window:n", "block:x": "window:x"},
		sessions: map[string]string{"window:me": "session:me", "window:o": "session:epic", "window:w": "session:epic", "window:n": "session:epic", "window:x": "session:other"},
		tabs:     map[string]string{"window:me": "me", "window:o": placeholder, "window:w": placeholder, "window:n": placeholder, "window:x": placeholder},
	}
	e := realAmqEnv(t, term)
	for _, p := range []struct{ block, session, label string }{
		{"block:o", "o1", core.OrchestratorRole}, {"block:w", "w1", "implementer"}, {"block:n", "n1", core.NarratorRole}, {"block:x", "x1", "implementer"},
	} {
		startPiAs(t, e, term, p.block, p.session, p.label)
	}
	put(t, e, seed{Handle: "claude-01", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me", SessionID: "session:me", WindowID: "window:me", AdoptedAt: time.Now()})
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:me"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
		t.Fatal(err)
	}
	return e, term, Location{Block: "block:me"}
}

func closeEpic(e Env, loc Location, r CloseRequest) (CloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.CloseEpic(ctx, loc, r)
}

func handleIn(t *testing.T, e Env, block string) core.Handle {
	t.Helper()
	rec, ok, err := e.agentAt(block)
	if err != nil || !ok {
		t.Fatalf("no record in %s: %v", block, err)
	}
	return rec.Handle
}

// Closing kills the Epic's session, parks each of its agents for
// epic_closed while keeping its handle reserved, tells none of them, leaves
// other sessions alone, and unlinks the Epic; the session's view stays.
func TestCloseEpicKillsParksAndUnlinks(t *testing.T) {
	e, term, loc := closeWorld(t)
	orch, worker, narr, other := handleIn(t, e, "block:o"), handleIn(t, e, "block:w"), handleIn(t, e, "block:n"), handleIn(t, e, "block:x")
	if err := e.Store.PutView("session:epic", map[string]string{"as_of": "x"}); err != nil {
		t.Fatal(err)
	}
	before := len(unread(t, e, string(orch)))
	res, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "收尾吧，关掉"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(term.killed, []string{"session:epic"}) || !res.Unlinked {
		t.Fatalf("killed %v, result %+v", term.killed, res)
	}
	slices.Sort(res.Parked)
	want := []core.Handle{orch, worker, narr}
	slices.Sort(want)
	if !slices.Equal(res.Parked, want) {
		t.Errorf("parked %v, want %v", res.Parked, want)
	}
	for _, h := range want {
		r := recordOf(t, e, h)
		if r.Live() || r.departReason() != store.DepartEpicClosed || r.unfinished() != "" {
			t.Errorf("%s: live=%v reason=%q unfinished=%q", h, r.Live(), r.departReason(), r.unfinished())
		}
	}
	if r := recordOf(t, e, other); !r.Live() {
		t.Errorf("an agent of another session was parked: %+v", r)
	}
	if n := len(unread(t, e, string(orch))); n != before {
		t.Errorf("the closed orchestrator got %d departure messages", n-before)
	}
	if links, _ := e.Store.Links(); len(links) != 0 {
		t.Errorf("links left: %+v", links)
	}
	if _, ok, _ := e.Store.ReadView("session:epic"); !ok {
		t.Error("the Epic's view was removed")
	}
	// A handle stays reserved: the worker's session resumed in a pane of
	// another session gets it back.
	term.mu.Lock()
	term.windows["block:r"], term.sessions["window:r"], term.tabs["window:r"] = "window:r", "session:new", placeholder
	term.mu.Unlock()
	if h, _ := startPi(t, e, "block:r", "w1"); h != worker {
		t.Errorf("the resumed worker is %s, want its handle %s", h, worker)
	}
}

// Closing needs a live personal caller, the user's words, and a link to
// the session; nothing is killed or parked when any is missing.
func TestCloseEpicRefusesWithoutItsGuards(t *testing.T) {
	e, term, loc := closeWorld(t)
	worker := handleIn(t, e, "block:w")
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:epic"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "--asked") {
		t.Errorf("without the user's words: %v", err)
	}
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:other", Asked: "close it"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not an Epic linked") {
		t.Errorf("an unlinked session: %v", err)
	}
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:me", Asked: "close it"}); !errors.Is(err, ErrUsage) {
		t.Errorf("the caller's own session: %v", err)
	}
	if _, err := closeEpic(e, Location{Block: "block:w"}, CloseRequest{Session: "session:epic", Asked: "close it"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a worker closing its own Epic: %v", err)
	}
	if len(term.killed) != 0 || !recordOf(t, e, worker).Live() {
		t.Errorf("a refused close killed %v or parked the worker", term.killed)
	}
	if links, _ := e.Store.Links(); len(links) != 1 {
		t.Errorf("a refused close unlinked: %+v", links)
	}
}

// An agent of the Epic that is working, waiting on a dialog, reports no
// state, or holds unread mail holds the close, named, until --force; the
// narrator's own state does not.
func TestCloseEpicWaitsForItsAgentsToRestUnlessForced(t *testing.T) {
	e, term, loc := closeWorld(t)
	orch, worker := handleIn(t, e, "block:o"), handleIn(t, e, "block:w")
	inbox := filepath.Join(e.Paths.Root(), "agents", string(worker), "inbox", "new")
	for _, c := range []struct {
		name  string
		setup func()
		want  string
	}{
		{"working", func() { term.states = map[string]rex.Status{"block:o": {State: "working"}} }, string(orch) + ": working"},
		{"blocked", func() { term.states = map[string]rex.Status{"block:o": {State: "blocked"}} }, string(orch) + ": blocked"},
		{"no state", func() { term.states = nil; term.state = "" }, "reports no state"},
		{"unread", func() {
			term.states, term.state = map[string]rex.Status{"block:n": {State: "working"}}, "idle"
			if err := os.MkdirAll(inbox, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inbox, "m1.md"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, string(worker) + ": 1 unread"},
	} {
		c.setup()
		_, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) || !forceOnlyInNextAction(err) {
			t.Errorf("%s: %v, want a refusal naming %q", c.name, err, c.want)
		}
		if len(term.killed) != 0 {
			t.Fatalf("%s: killed %v", c.name, term.killed)
		}
	}
	if ids, _ := amq.Unread(e.Paths.Root(), string(worker)); len(ids) != 1 {
		t.Fatalf("the unread mail fixture: %v", ids)
	}
	res, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it anyway", Force: true})
	if err != nil || len(res.Parked) != 3 || !slices.Equal(term.killed, []string{"session:epic"}) {
		t.Fatalf("forced: %+v %v, killed %v", res, err, term.killed)
	}
}

// A close that failed part way finishes when run again: a session already
// killed is not an error, and the agents left live are parked then.
// A waker that will not retire does not hold the close up under the lifecycle
// lock: the close stands (killed, parked, unlinked), says which departure is
// left unfinished, and collection finishes it once amq retires the waker.
func TestCloseEpicLeavesAWakerThatWillNotRetireToCollection(t *testing.T) {
	e, term, loc := closeWorld(t)
	worker := handleIn(t, e, "block:w")
	fake, ok := e.AMQ.(*amq.Fake)
	if !ok {
		t.Skip("injecting a failed retire needs amq.Fake")
	}
	e.AMQ = fake.WithFail(func(op string, args []string) error {
		if op == "wake check" && slices.Contains(args, string(worker)) {
			return errors.New("injected retire failure")
		}
		return nil
	})
	res, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it"})
	if err == nil || !strings.Contains(err.Error(), "injected") || !strings.Contains(err.Error(), string(worker)) {
		t.Fatalf("first close: %v, want the injected failure, naming %s", err, worker)
	}
	if !slices.Contains(res.Parked, worker) || !res.Unlinked {
		t.Fatalf("the close did not stand: %+v", res)
	}
	if p := recordOf(t, e, worker).departure(); p == nil || !p.Unfinished || p.DepartReason != store.DepartEpicClosed {
		t.Fatalf("the worker's departure: %+v, want epic_closed and unfinished", p)
	}
	e.AMQ = fake.WithFail(nil)
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := recordOf(t, e, worker).departure(); p.Unfinished || wakerLive(t, e, string(worker)) {
		t.Fatalf("collection did not finish the departure: %+v waker=%v", p, wakerLive(t, e, string(worker)))
	}
	if !slices.Equal(term.killed, []string{"session:epic"}) {
		t.Errorf("killed %v, want one kill", term.killed)
	}
}

// forceOnlyInNextAction reports whether err is a busy refusal that names
// --force only as its next action, never in its message: an agent reads the
// message, and only the user authorizes forcing.
func forceOnlyInNextAction(err error) bool {
	f, ok := fault.As(err)
	return ok && f.Code == fault.Busy && strings.Contains(f.NextAction, "--force") && !strings.Contains(f.Message, "--force")
}
