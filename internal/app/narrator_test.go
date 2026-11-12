package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// startPiAs starts a Pi session in b whose pane label (and so its role) is
// label.
func startPiAs(t *testing.T, e Env, term *fakeTerm, b, session, label string) (core.Handle, string) {
	t.Helper()
	term.mu.Lock()
	term.label = label
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		term.label = ""
		term.mu.Unlock()
	}()
	return startPi(t, e, b, session)
}

func handlesOf(agents []core.Agent) []core.Handle {
	var out []core.Handle
	for _, a := range agents {
		out = append(out, a.Handle)
	}
	return out
}

// A narrator arrives and leaves unannounced: no worker is told, none lists
// it as a mate, and the ordinary who leaves it out while who --all shows
// it.
func TestNarratorStealthInTheSession(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:n": "window:2", "block:b": "window:1"}}
	e := realAmqEnv(t, term)
	worker, _ := startPi(t, e, "block:a", "s1")
	narr, n := startPiAs(t, e, term, "block:n", "sn", "narrator")
	if rec := recordOf(t, e, narr); rec.Role != core.NarratorRole {
		t.Fatalf("role %q", rec.Role)
	}
	if !strings.Contains(n, string(worker)+" (pi)") || strings.Contains(n, "An orchestrator coordinates") {
		t.Fatalf("the narrator's own start notice: %q", n)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); strings.Contains(s, string(narr)) {
		t.Fatalf("a worker was told of the narrator: %q", s)
	}
	_, n = startPi(t, e, "block:b", "s2")
	if strings.Contains(n, string(narr)) {
		t.Fatalf("a newcomer was told the narrator is beside it: %q", n)
	}
	_ = notice(t, e, EventPrompt, core.Pi, "block:a", "s1") // the newcomer's join

	s, err := e.FindSession(context.Background(), "block:a")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := e.SessionAgents(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(handlesOf(agents), narr) || len(agents) != 2 {
		t.Fatalf("who lists %v", handlesOf(agents))
	}
	if agents, _ = e.SessionAgents(s, true); !slices.Contains(handlesOf(agents), narr) {
		t.Fatalf("who --all leaves the narrator out: %v", handlesOf(agents))
	}

	depart(t, e, "block:n", "sn")
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); strings.Contains(s, string(narr)) {
		t.Fatalf("a worker was told the narrator left: %q", s)
	}
}

// A narrator neither hears of a worker's error nor takes over a stalled
// goal, and its own error reaches no worker.
func TestNarratorStealthInErrorRouting(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	orch := store.Record{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:s"}
	put(t, e, orch,
		store.Record{Handle: "codex-02", Kind: core.Codex, Role: core.NarratorRole, SessionID: "session:s"},
		store.Record{Handle: "pi-03", Kind: core.Pi, Role: "implementer", SessionID: "session:s"},
	)
	orch = recordOf(t, e, "claude-01")
	peers, _, _ := e.sessionPeersForError(recordOf(t, e, "pi-03"))
	for _, p := range peers {
		if p.Role == core.NarratorRole {
			t.Fatalf("an error is routed among %+v", peers)
		}
	}
	if peers, _, _ := e.sessionPeersForError(recordOf(t, e, "codex-02")); len(peers) != 0 {
		t.Fatalf("the narrator's own error goes to %+v", peers)
	}
	s := Session{server: fakeServer, id: "session:s", windowOf: map[string]string{"block:claude-01": "w", "block:codex-02": "w2", "block:pi-03": "w"}}
	if other, ok := e.otherFamily(s, orch, orch); ok && other.Role == core.NarratorRole {
		t.Fatalf("the narrator took over: %+v", other)
	}
}

// The narrator and a personal agent take no place a worker would: with five
// workers and both of them live, a sixth worker still starts. A narrator
// starts in a background tab of its own, claimed as the narrator tab.
func TestNarratorSummonCapacityAndTab(t *testing.T) {
	e, term, loc := summonWorld(t)
	for i, h := range []core.Handle{"codex-02", "codex-03", "codex-04", "codex-05"} {
		add(t, e, store.Record{Handle: h, Kind: core.Codex, Server: fakeServer, BlockID: "block:w" + string(rune('0'+i)), SessionID: "session:s", Role: "worker", AgentPID: 2, AdoptedAt: e.Now()})
	}
	add(t, e, store.Record{Handle: "pi-06", Kind: core.Pi, Server: fakeServer, BlockID: "block:n", SessionID: "session:s", Role: core.NarratorRole, AgentPID: 3, AdoptedAt: e.Now()})
	add(t, e, store.Record{Handle: "pi-07", Kind: core.Pi, Server: fakeServer, BlockID: "block:p", SessionID: "session:s", Role: core.PersonalRole, AgentPID: 4, AdoptedAt: e.Now()})
	asked := func(r *SummonRequest) { r.Asked = "start a qa" }
	if res := summon(t, e, loc, "qa", asked); res.Action != "start" {
		t.Fatalf("the narrator and personal agent took a worker's place: %+v", res)
	}
	add(t, e, store.Record{Handle: "codex-08", Kind: core.Codex, Server: fakeServer, BlockID: "block:q", SessionID: "session:s", Role: "qa", AgentPID: 5, AdoptedAt: e.Now()})
	if res := summon(t, e, loc, "editor", asked); res.Action != "refused" || !strings.Contains(res.Reason, "cap") {
		t.Fatalf("the cap counts workers: %+v", res)
	}

	e2, term2, loc2 := summonWorld(t)
	res := summon(t, e2, loc2, core.NarratorRole, asked)
	if res.Action != "start" {
		t.Fatalf("%+v", res)
	}
	if !slices.Contains(term2.newTabs, "window:"+core.NarratorTab) {
		t.Fatalf("the narrator did not get a tab of its own: %v", term2.newTabs)
	}
	l := term2.launches[len(term2.launches)-1]
	if term2.windows[l.Block] != "window:"+core.NarratorTab || term2.windows[l.Block] == term2.windows[block] {
		t.Fatalf("the narrator was placed in %s", term2.windows[l.Block])
	}
	tab, ok, err := e2.Store.Tab("window:" + core.NarratorTab)
	if err != nil || !ok || tab.Mode != core.WindowNarrator {
		t.Fatalf("narrator tab ledger: %+v %v %v", tab, ok, err)
	}
	_ = term
}

// watcherWorld is an orchestrator, a narrator, and an implementer in
// session:s, on a real amq.
func watcherWorld(t *testing.T) (Env, *fakeTerm, core.SessionKey) {
	t.Helper()
	term := &fakeTerm{state: "idle", fgPID: os.Getpid(), windows: map[string]string{}}
	e := realAmqEnv(t, term)
	for _, r := range []store.Record{
		{Handle: "codex-01", Role: core.OrchestratorRole, BlockID: "block:o"},
		{Handle: "codex-02", Role: core.NarratorRole, BlockID: "block:n"},
		{Handle: "codex-03", Role: "implementer", BlockID: "block:w"},
	} {
		r.Kind, r.Server, r.SessionID, r.AgentPID, r.AdoptedAt = core.Codex, fakeServer, "session:s", os.Getpid(), e.Now()
		add(t, e, r)
	}
	return e, term, core.SessionKey{Server: fakeServer, SessionID: "session:s"}
}

func leave(t *testing.T, e Env, h core.Handle) {
	t.Helper()
	if err := e.Store.PutRecord(departed(recordOf(t, e, h), e.Now())); err != nil {
		t.Fatal(err)
	}
}

// The watcher is the session's: it keeps watching after the orchestrator
// leaves while the narrator stays, ends once neither is left, and a second
// one finds the session's lock held. A narrator's hooks start it.
func TestSessionWatcherOutlivesTheOrchestrator(t *testing.T) {
	e, _, key := watcherWorld(t)
	leave(t, e, "codex-01")
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- e.WatchSession(ctx, key) }()
	time.Sleep(200 * time.Millisecond)
	// The second waits a second for the lock, finds it held, and returns.
	second, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := e.WatchSession(second, key); err != nil {
		t.Fatalf("a second watcher: %v", err)
	}
	select {
	case err := <-first:
		t.Fatalf("the watcher ended with the orchestrator gone and the narrator live: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the watcher ended on its own: %v", err)
	}
	if recordOf(t, e, "codex-02").DepartedAt != nil {
		t.Fatal("the watcher parked the narrator")
	}

	leave(t, e, "codex-02")
	done := make(chan error, 1)
	go func() { done <- e.WatchSession(context.Background(), key) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher outlived the last orchestrator and narrator")
	}

	started := filepath.Join(t.TempDir(), "started")
	self := filepath.Join(t.TempDir(), "self.sh")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho \"$*\" >>'"+started+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = self
	narr := recordOf(t, e, "codex-02")
	narr.DepartedAt = nil
	if err := e.ensureSessionWatcher(narr); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(started); strings.Contains(string(b), "__internal watch "+fakeServer+" session:s") {
			break
		}
		if time.Now().After(end) {
			t.Fatal("a narrator did not start its session's watcher")
		}
	}
}

func narratorInbox(t *testing.T, e Env) []amq.Header {
	t.Helper()
	var out []amq.Header
	for _, id := range unread(t, e, "codex-02") {
		h, err := amq.ReadHeader(e.Paths.Root(), "codex-02", amq.BoxUnread, id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// The watcher tells the narrator what changed, as system mail it may
// receive: routine activity low and coalesced while a notice waits unread, a
// departure urgent at once. The first look only records the session.
func TestSessionWatcherTellsTheNarrator(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	keepers := func() []store.Record {
		var out []store.Record
		for _, h := range []core.Handle{"codex-01", "codex-02"} {
			if r := recordOf(t, e, h); r.Live() {
				out = append(out, r)
			}
		}
		return out
	}
	look := func() {
		t.Helper()
		if err := e.watchOnce(ctx, key, keepers()); err != nil {
			t.Fatal(err)
		}
	}
	look()
	if n := narratorInbox(t, e); len(n) != 0 {
		t.Fatalf("the first look told the narrator: %+v", n)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 0 {
		t.Fatalf("nothing changed, yet: %+v", n)
	}
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "codex-03", "codex-01", "done", "implemented"); err != nil {
		t.Fatal(err)
	}
	look()
	n := narratorInbox(t, e)
	if len(n) != 1 || n[0].Priority != core.PriorityLow || !n[0].System() || n[0].From != "codex-02" {
		t.Fatalf("routine activity: %+v", n)
	}
	p, ok, err := e.Store.ProvenanceOf(n[0].ID, "", "", nil)
	if err != nil || !ok || !p.System {
		t.Fatalf("the notice carries no system provenance: %+v %v %v", p, ok, err)
	}
	if kept, err := e.admit(recordOf(t, e, "codex-02")); err != nil || len(kept) != 1 {
		t.Fatalf("the narrator's gate refused the watcher's notice: %v %v", kept, err)
	}
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), "codex-01", "codex-03", "next", "do more"); err != nil {
		t.Fatal(err)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 1 {
		t.Fatalf("routine activity was not coalesced with the waiting notice: %d notices", len(n))
	}
	leave(t, e, "codex-03")
	look()
	n = narratorInbox(t, e)
	if len(n) != 2 || n[1].Priority != core.PriorityUrgent || !strings.Contains(n[1].Subject, "attention") {
		t.Fatalf("a departure: %+v", n)
	}
	b, err := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-02"), n[1].ID+".md"))
	if err != nil || !strings.Contains(string(b), "codex-03 (implementer) left") {
		t.Fatalf("the urgent notice: %s %v", b, err)
	}
	noReceipts(t, e, "codex-01")
	noReceipts(t, e, "codex-03")
}
