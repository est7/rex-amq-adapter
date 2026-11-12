package app

import (
	"context"
	"slices"
	"strings"
	"testing"

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
