package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Every agent projection names the agent session the handle runs as now
// (the newest of its Identity's sessions) and the directory it runs in, as
// agent_session_id and cwd: who --json, events --json (--all), and summon.

// agentJSON is the agent a in its JSON form, as who --json and events print it.
func agentJSON(t *testing.T, a core.Agent) map[string]any {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A handle that went through /clear (a new session in the same process)
// is shown as the newest session, live and, with --all, departed.
func TestWhoProjectsTheCurrentAgentSessionAndCwd(t *testing.T) {
	term := &fakeTerm{state: "working", windows: map[string]string{block: "window:1"}, cwd: "/work/app"}
	e := realAmqEnv(t, term)
	promptNotice(t, e, core.Claude, EventSessionStart, HookInput{SessionID: "conv-1"})
	promptNotice(t, e, core.Claude, EventSessionStart, HookInput{SessionID: "conv-2"}) // /clear
	a, ok, err := e.agentAt(block)
	if err != nil || !ok || len(a.Sessions) != 2 || a.Session() != "conv-2" {
		t.Fatalf("after /clear the identity is %+v (ok=%v err=%v), want sessions [conv-1 conv-2]", a.Identity, ok, err)
	}
	s, err := e.FindSession(context.Background(), block)
	if err != nil {
		t.Fatal(err)
	}
	who := func(all bool) map[string]any {
		t.Helper()
		agents, err := e.SessionAgents(s, all)
		if err != nil || len(agents) != 1 {
			t.Fatalf("who (all=%v): %+v, %v", all, agents, err)
		}
		return agentJSON(t, agents[0])
	}
	if m := who(false); m["agent_session_id"] != "conv-2" || m["cwd"] != "/work/app" {
		t.Errorf("who --json of a live agent after /clear: agent_session_id=%v cwd=%v", m["agent_session_id"], m["cwd"])
	}
	if err := e.Depart(context.Background(), Location{Block: block}, HookInput{SessionID: "conv-2"}); err != nil {
		t.Fatal(err)
	}
	if m := who(true); field(m, "lifecycle", "state") != core.LifeDeparted || m["agent_session_id"] != "conv-2" || m["cwd"] != "/work/app" {
		t.Errorf("who --all --json of the departed agent: %v", m)
	}
}

// An agent without a reported session or directory still carries both
// keys, empty, like session_id and task beside them.
func TestTheAgentProjectionAlwaysCarriesBothKeys(t *testing.T) {
	m := agentJSON(t, core.Project(core.Observations{Agents: []core.AgentObservation{{Handle: "pi-01", Kind: core.Pi}}}).Agents[0])
	for _, k := range []string{"agent_session_id", "cwd"} {
		if v, ok := m[k]; !ok || v != "" {
			t.Errorf("%s = %v (present=%v), want an empty string", k, v, ok)
		}
	}
}

// events --json streams both in its snapshot, and a /clear as an upsert that
// names agent_session_id among the changed fields.
func TestEventsStreamTheAgentSessionAndCwd(t *testing.T) {
	e, _ := eventsWorld(t)
	id, _, err := e.Store.IdentityOf("codex-reviewer-01")
	if err != nil {
		t.Fatal(err)
	}
	id.Cwd = "/work/review"
	if err := e.Store.PutIdentity(id.WithSession("thread-1")); err != nil {
		t.Fatal(err)
	}
	out, stop := startStream(t, e, Scope{Kind: ScopeSession, Server: "unix:///fake.sock", SessionID: "session:s"})
	defer stop()
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	var reviewer map[string]any
	for _, a := range snap["agents"].([]any) {
		if m := a.(map[string]any); m["handle"] == "codex-reviewer-01" {
			reviewer = m
		}
	}
	if reviewer["agent_session_id"] != "thread-1" || reviewer["cwd"] != "/work/review" {
		t.Fatalf("snapshot agent: %v", reviewer)
	}

	id, _, _ = e.Store.IdentityOf("codex-reviewer-01")
	if err := e.Store.PutIdentity(id.WithSession("thread-2")); err != nil {
		t.Fatal(err)
	}
	f := await(t, out, "the new session's upsert", func(f map[string]any) bool {
		return f["type"] == "upsert" && f["key"] == "codex-reviewer-01" && field(f, "value", "agent_session_id") == "thread-2"
	})
	changed, _ := f["changed"].([]any)
	if len(changed) != 1 || changed[0] != "agent_session_id" {
		t.Errorf("a /clear changed %v, want [agent_session_id]", changed)
	}
}

// events --json --all finds an offline handle of another Rex session, with
// the session it last ran as and its directory; who stays on its own session.
func TestEventsAllFindsAnOfflineHandleWithItsSessionAndCwd(t *testing.T) {
	e, _ := eventsWorld(t)
	gone := time.Now().UTC().Add(-time.Hour)
	if err := putSeed(e.Store, seed{Handle: "claude-02", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:old", SessionID: "session:t", WindowID: "window:3",
		AgentPID: 999999, AgentSessionID: "conv-9", Cwd: "/work/old", AdoptedAt: gone.Add(-time.Hour), DepartedAt: &gone, DepartReason: store.DepartSessionEnd}); err != nil {
		t.Fatal(err)
	}
	out, stop := startStream(t, e, Scope{Kind: ScopeAll, Server: "unix:///fake.sock"})
	defer stop()
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	var offline map[string]any
	for _, a := range snap["agents"].([]any) {
		if m := a.(map[string]any); m["handle"] == "claude-02" {
			offline = m
		}
	}
	if offline == nil || field(offline, "lifecycle", "state") != core.LifeDeparted || offline["session_id"] != "session:t" ||
		offline["agent_session_id"] != "conv-9" || offline["cwd"] != "/work/old" {
		t.Fatalf("the offline handle under --all: %v", offline)
	}
	s, err := e.loadSession(context.Background(), "unix:///fake.sock", "session:s")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := e.SessionAgents(s, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Handle == "claude-02" {
			t.Errorf("who --all in session:s listed another session's offline handle: %+v", a)
		}
	}
}

// summon names the agent it reached: a live one it mailed, a departed one it
// resumes (its last session, in the directory it resumes in), and a started
// one once adopted.
func TestSummonResultNamesTheAgentSessionAndCwd(t *testing.T) {
	resultJSON := func(t *testing.T, res SummonResult) map[string]any {
		t.Helper()
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Run("deliver", func(t *testing.T) {
		e, _, loc := summonWorld(t)
		add(t, e, seed{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:b", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", Cwd: "/work/review", AdoptedAt: e.Now()})
		res := summon(t, e, loc, "codex-02")
		if m := resultJSON(t, res); res.Action != "deliver" || m["agent_session_id"] != "thread-2" || m["cwd"] != "/work/review" {
			t.Fatalf("deliver: %v", m)
		}
	})
	t.Run("resume", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		dir := workDir(t, e, "app")
		add(t, e, departed(seed{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", AdoptedAt: e.Now().Add(-time.Hour), Cwd: dir}, e.Now().Add(-time.Minute)))
		res := summon(t, e, loc, "reviewer")
		if m := resultJSON(t, res); res.Action != "resume" || m["agent_session_id"] != "thread-2" || m["cwd"] != dir {
			t.Fatalf("resume: %v", m)
		}
		adoptLaunch(t, e, term, term.launches[0], core.Codex, "thread-2")
		res, err := e.Adopted(res, "codex-02")
		if m := resultJSON(t, res); err != nil || m["agent_session_id"] != "thread-2" || len(res.Handles) != 1 || res.Handles[0] != "codex-02" {
			t.Fatalf("resumed and adopted: %v, %v", m, err)
		}
	})
	t.Run("start", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		dir := workDir(t, e, "wt")
		res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "start a reviewer"; r.Harness = "codex"; r.Cwd = dir })
		if m := resultJSON(t, res); res.Action != "start" || m["cwd"] != dir {
			t.Fatalf("start: %v", m)
		} else if _, ok := m["agent_session_id"]; ok {
			t.Fatalf("a start names a session before the agent reported one: %v", m)
		}
		term.cwd = dir
		a := adoptLaunch(t, e, term, term.launches[0], core.Codex, "thread-new")
		res, err := e.Adopted(res, a.Handle)
		if m := resultJSON(t, res); err != nil || m["agent_session_id"] != "thread-new" || m["cwd"] != dir {
			t.Fatalf("started and adopted: %v, %v", m, err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		e, _, loc := summonWorld(t)
		m := resultJSON(t, summon(t, e, loc, "reviewer"))
		for _, k := range []string{"agent_session_id", "cwd"} {
			if _, ok := m[k]; ok {
				t.Errorf("a refusal that reached no agent carries %s: %v", k, m)
			}
		}
	})
}
