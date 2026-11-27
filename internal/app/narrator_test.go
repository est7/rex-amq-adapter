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
	startPi(t, e, "block:a", "s1")
	narr, n := startPiAs(t, e, term, "block:n", "sn", "narrator")
	if rec := recordOf(t, e, narr); rec.Role != core.NarratorRole {
		t.Fatalf("role %q", rec.Role)
	}
	if strings.Contains(n, "beside you") || strings.Contains(n, "An orchestrator coordinates") {
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
	orch := seed{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:s"}
	put(t, e, orch,
		seed{Handle: "codex-02", Kind: core.Codex, Role: core.NarratorRole, SessionID: "session:s"},
		seed{Handle: "pi-03", Kind: core.Pi, Role: "implementer", SessionID: "session:s"},
	)
	orchRec := recordOf(t, e, "claude-01")
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
	if other, ok := e.otherFamily(s, orchRec, orchRec); ok && other.Role == core.NarratorRole {
		t.Fatalf("the narrator took over: %+v", other)
	}
}

// The narrator and a personal agent take no place a worker would: with five
// workers and both of them live, a sixth worker still starts. A narrator
// starts in a background tab of its own, which follows it like any agent's.
func TestNarratorSummonCapacityAndTab(t *testing.T) {
	e, term, loc := summonWorld(t)
	for i, h := range []core.Handle{"codex-02", "codex-03", "codex-04", "codex-05"} {
		add(t, e, seed{Handle: h, Kind: core.Codex, Server: fakeServer, BlockID: "block:w" + string(rune('0'+i)), SessionID: "session:s", Role: "worker", AgentPID: 2, AdoptedAt: e.Now()})
	}
	add(t, e, seed{Handle: "pi-06", Kind: core.Pi, Server: fakeServer, BlockID: "block:n", SessionID: "session:s", Role: core.NarratorRole, AgentPID: 3, AdoptedAt: e.Now()})
	// A personal agent lives in a Hub session of its own (C25).
	add(t, e, seed{Handle: "pi-07", Kind: core.Pi, Server: fakeServer, BlockID: "block:p", SessionID: "session:me", Role: core.PersonalRole, AgentPID: 4, AdoptedAt: e.Now()})
	asked := func(r *SummonRequest) { r.Asked = "start a qa" }
	if res := summon(t, e, loc, "qa", asked); res.Action != "start" {
		t.Fatalf("the narrator and personal agent took a worker's place: %+v", res)
	}
	add(t, e, seed{Handle: "codex-08", Kind: core.Codex, Server: fakeServer, BlockID: "block:q", SessionID: "session:s", Role: "qa", AgentPID: 5, AdoptedAt: e.Now()})
	if res := summon(t, e, loc, "editor", asked); res.Action != "refused" || !strings.Contains(res.Reason, "cap") {
		t.Fatalf("the cap counts workers: %+v", res)
	}

	e2, term2, loc2 := summonWorld(t)
	res := summon(t, e2, loc2, core.NarratorRole, asked)
	if res.Action != "start" {
		t.Fatalf("%+v", res)
	}
	l := term2.launches[len(term2.launches)-1]
	if !slices.Contains(term2.newTabs, l.Window) || term2.windows[l.Block] == term2.windows[block] {
		t.Fatalf("the narrator did not get a tab of its own: tabs %v, placed in %s", term2.newTabs, term2.windows[l.Block])
	}
	if got := term2.tabs[l.Window]; got != l.Label {
		t.Errorf("the narrator's tab is labelled %q, want its launch label %q", got, l.Label)
	}
	tab, ok, err := e2.Store.Tab(l.Window)
	if err != nil || !ok || tab.Mode != core.WindowAuto {
		t.Fatalf("narrator tab ledger: %+v %v %v, want a follower", tab, ok, err)
	}
	_ = term
}

// watcherWorld is an orchestrator, a narrator, and an implementer in
// session:s, on a real amq.
func watcherWorld(t *testing.T) (Env, *fakeTerm, core.SessionKey) {
	t.Helper()
	term := &fakeTerm{state: "idle", fgPID: os.Getpid(), windows: map[string]string{}}
	e := realAmqEnv(t, term)
	for _, r := range []seed{
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
	if err := putSeed(e.Store, departed(seedOf(recordOf(t, e, h)), e.Now())); err != nil {
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
	if recordOf(t, e, "codex-02").departedAt() != nil {
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
	narr := seedOf(recordOf(t, e, "codex-02"))
	narr.DepartedAt = nil
	if err := e.ensureSessionWatcher(narr.asAgent()); err != nil {
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
	keepers := func() []agent {
		var out []agent
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
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-03", "codex-01", "done", "implemented"); err != nil {
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
	if kept, err := e.admit(context.Background(), recordOf(t, e, "codex-02")); err != nil || len(kept) != 1 {
		t.Fatalf("the narrator's gate refused the watcher's notice: %v %v", kept, err)
	}
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-01", "codex-03", "next", "do more"); err != nil {
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

// An orchestrator's recorded hand-back reaches the narrator at once, as an
// urgent notice of its own, even while a routine notice waits unread for its
// hold; a task that only reads like one is routine, so the narrator is never
// woken before the report is stored; a second hand-back wakes it again.
func TestSessionWatcherTellsTheNarratorOfAHandBackAtOnce(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	keepers := []agent{recordOf(t, e, "codex-01"), recordOf(t, e, "codex-02")}
	look := func() {
		t.Helper()
		if err := e.watchOnce(ctx, key, keepers); err != nil {
			t.Fatal(err)
		}
	}
	look()
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-03", "codex-01", "done", "implemented"); err != nil {
		t.Fatal(err)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 1 || n[0].Priority != core.PriorityLow {
		t.Fatalf("the routine notice: %+v", n)
	}
	if _, err := e.SetTask(ctx, Location{Block: "block:o"}, "hand-back: login ships"); err != nil {
		t.Fatal(err)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 1 {
		t.Fatalf("a task alone woke the narrator before any hand-back was stored: %+v", n)
	}
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "## Review queue\nPR #9 login ships\n## Blocked on me\nmerge #9"); err != nil {
		t.Fatal(err)
	}
	look()
	n := narratorInbox(t, e)
	if len(n) != 2 || n[1].Priority != core.PriorityUrgent {
		t.Fatalf("the hand-back waited behind the routine notice: %+v", n)
	}
	b, err := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-02"), n[1].ID+".md"))
	if err != nil || !strings.Contains(string(b), "codex-01 handed back: its report is in your view (hand_back)") {
		t.Fatalf("the hand-back notice: %s %v", b, err)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 2 {
		t.Fatalf("one hand-back told twice: %d notices", len(n))
	}
	e.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "Goal 2 hand-back"); err != nil {
		t.Fatal(err)
	}
	look()
	if n := narratorInbox(t, e); len(n) != 3 || n[2].Priority != core.PriorityUrgent {
		t.Fatalf("the Epic's next hand-back: %+v", n)
	}
}

// Only a live orchestrator placed in a session records its hand-back, and
// only a non-empty one the view can hold whole; nothing is stored otherwise.
func TestHandBackOnlyTheOrchestratorRecords(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	add(t, e, seed{Handle: "pi-04", Kind: core.Pi, Role: core.PersonalRole, BlockID: "block:p", Server: fakeServer, SessionID: "session:s", AgentPID: os.Getpid(), AdoptedAt: e.Now()})
	for _, b := range []string{"block:n", "block:w", "block:p"} {
		if _, err := e.RecordHandBack(ctx, Location{Block: b}, "report"); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s recorded a hand-back: %v", b, err)
		}
	}
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:none"}, "report"); err == nil {
		t.Fatal("a pane without an agent recorded a hand-back")
	}
	for _, body := range []string{"", " \n\t", strings.Repeat("x", handBackMax+1)} {
		if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, body); err == nil {
			t.Fatalf("a hand-back of %d bytes was recorded", len(body))
		}
	}
	if _, ok, err := e.Store.HandBack(key.SessionID); ok || err != nil {
		t.Fatalf("a refused hand-back was stored: %v %v", ok, err)
	}
	h, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "report")
	if err != nil || h.By != "codex-01" || h.Body != "report" || h.At.IsZero() {
		t.Fatalf("the orchestrator's hand-back: %+v %v", h, err)
	}
	if got, ok, err := e.Store.HandBack(key.SessionID); !ok || err != nil || got.Body != "report" {
		t.Fatalf("stored %+v %v %v", got, ok, err)
	}
	leave(t, e, "codex-01")
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "late"); err == nil {
		t.Fatal("a departed orchestrator recorded a hand-back")
	}
	if got, _, _ := e.Store.HandBack(key.SessionID); got.Body != "report" {
		t.Fatalf("a refused hand-back replaced the stored one: %+v", got)
	}
}

// A hand-back stored before the watcher's first look at the session (the
// narrator may have read its view before it was) is told as new, not taken
// into the first pulse unannounced; a first look with none tells nothing.
func TestSessionWatcherTellsAHandBackStoredBeforeItsFirstLook(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "fast report"); err != nil {
		t.Fatal(err)
	}
	keepers := []agent{recordOf(t, e, "codex-01"), recordOf(t, e, "codex-02")}
	if err := e.watchOnce(ctx, key, keepers); err != nil {
		t.Fatal(err)
	}
	n := narratorInbox(t, e)
	if len(n) != 1 || n[0].Priority != core.PriorityUrgent {
		t.Fatalf("a hand-back before the first look: %+v", n)
	}
	if err := e.watchOnce(ctx, key, keepers); err != nil {
		t.Fatal(err)
	}
	if n := narratorInbox(t, e); len(n) != 1 {
		t.Fatalf("told twice: %d", len(n))
	}
}

// selfNotice puts a system notice of priority into h's inbox, as the
// session watcher does.
func selfNotice(t *testing.T, e Env, h core.Handle, priority string) string {
	t.Helper()
	sent, _, err := e.post(context.Background(), Outgoing{
		Op: core.Operation{Kind: core.OpSend, System: true}, From: h, To: []core.Handle{h},
		Args: []string{"--allow-self", "--kind", "status", "--priority", priority, "--labels", amq.SystemLabel, "--subject", "s", "--body", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sent.ID
}

// A narrator gets doorbell holds in its .wake.settings when it is adopted,
// and the adapter's hooks apply them as its waker would: Pi's mail watcher
// and a busy hook stay quiet about routine mail until its hold ends, urgent
// mail is told at once, and an agent that stops being a narrator loses them.
func TestNarratorHoldBatchesHooksAndPi(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:n": "window:2"}}
	e := realAmqEnv(t, term)
	// Real time, so a message's file is never dated after now (amq does not
	// hold future-dated mail); skip moves it on.
	var skip time.Duration
	e.Now = func() time.Time { return time.Now().Add(skip) }
	narr, _ := startPiAs(t, e, term, "block:n", "sn", "narrator")
	normal, low, err := amq.ReadHolds(e.Paths.Root(), string(narr))
	if err != nil || normal != narratorHoldNormal || low != narratorHoldLow {
		t.Fatalf("holds %v %v %v", normal, low, err)
	}
	selfNotice(t, e, narr, core.PriorityNormal)
	for _, ev := range []string{EventMail, EventPostTool} {
		if s := notice(t, e, ev, core.Pi, "block:n", "sn"); s != "" {
			t.Fatalf("%s told of held routine mail: %q", ev, s)
		}
	}
	skip = narratorHoldNormal + time.Second
	if s := notice(t, e, EventMail, core.Pi, "block:n", "sn"); !strings.Contains(s, "1 unread message(s) for "+string(narr)) {
		t.Fatalf("routine mail past its hold: %q", s)
	}

	// Newly held routine mail waits again; urgent mail takes it along at once.
	selfNotice(t, e, narr, core.PriorityLow)
	if s := notice(t, e, EventMail, core.Pi, "block:n", "sn"); s != "" {
		t.Fatalf("held low mail was told: %q", s)
	}
	selfNotice(t, e, narr, core.PriorityUrgent)
	if s := notice(t, e, EventMail, core.Pi, "block:n", "sn"); !strings.Contains(s, "3 unread message(s)") || !strings.Contains(s, "(2 new)") {
		t.Fatalf("urgent mail did not skip the hold: %q", s)
	}

	rec := recordOf(t, e, narr)
	rec.Role = "reviewer"
	e.alignHolds(context.Background(), rec, core.NarratorRole)
	if normal, low, err := amq.ReadHolds(e.Paths.Root(), string(narr)); err != nil || normal != 0 || low != 0 {
		t.Fatalf("a former narrator keeps holds %v %v %v", normal, low, err)
	}
}

// A routine wake is not a reason to mail the personal agent (F5): the notice
// itself says to refresh, summarize, and end the turn.
func TestNarratorWakeEndsInTheViewNotInMail(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	keep := []agent{recordOf(t, e, "codex-01"), recordOf(t, e, "codex-02")}
	if err := e.watchOnce(ctx, key, keep); err != nil {
		t.Fatal(err)
	}
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-03", "codex-01", "done", "implemented"); err != nil {
		t.Fatal(err)
	}
	if err := e.watchOnce(ctx, key, keep); err != nil {
		t.Fatal(err)
	}
	n := narratorInbox(t, e)
	if len(n) != 1 {
		t.Fatalf("routine activity: %+v", n)
	}
	b, err := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-02"), n[0].ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"epic refresh`, then say what changed in `--summary`",
		"pipe `summary` + `progress` via `--input - --fields as_of,progress`",
		"a hand-back or orchestrator departure",
		"otherwise end your turn without mailing anyone",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the wake notice lacks %q:\n%s", want, b)
		}
	}
}
