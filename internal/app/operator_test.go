package app

import (
	"errors"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// operatorWorld is a state directory with no agent, whose commands run as
// process 9001 under 9000, no agent's.
func operatorWorld(t *testing.T) Env {
	t.Helper()
	p := store.Paths{Home: t.TempDir()}
	return Env{Paths: p, Store: store.Store{P: p}, Now: time.Now,
		Ancestors: func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 9000}}, nil }}
}

func operatorReason(err error) string {
	f, ok := fault.As(err)
	if !ok || f.Code != fault.GuardDenied {
		return ""
	}
	r, _ := f.Details["reason"].(string)
	return r
}

// The Operator is a positive allowlist: the user with no agent above them
// and attended, a personal agent with no agent of another role above it,
// and a top-level attended agent of no role in a session nothing records.
// Every other caller is refused with guard_denied, by the rule that does not
// admit it, from the records and the process tree alone.
func TestResolveOperatorAllowlist(t *testing.T) {
	e := operatorWorld(t)
	put(t, e,
		seed{Handle: "claude-01", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:personal", SessionID: "session:hub", AgentPID: 701, AgentSessionID: "s-personal"},
		seed{Handle: "codex-02", Kind: core.Codex, BlockID: "block:alone", SessionID: "session:plain", AgentPID: 702, AgentSessionID: "s-alone"},
		seed{Handle: "codex-03", Kind: core.Codex, Role: "implementer", BlockID: "block:worker", SessionID: "session:epic", AgentPID: 703},
		seed{Handle: "codex-04", Kind: core.Codex, BlockID: "block:summoned", SessionID: "session:plain2", AgentPID: 704, Parent: "codex-02", Depth: 1},
		seed{Handle: "codex-05", Kind: core.Codex, BlockID: "block:launched", SessionID: "session:plain3", AgentPID: 705, Launched: true},
		seed{Handle: "codex-06", Kind: core.Codex, BlockID: "block:unattended", SessionID: "session:plain4", AgentPID: 706, Unattended: true},
		seed{Handle: "codex-07", Kind: core.Codex, BlockID: "block:crew", SessionID: "session:crew", AgentPID: 707},
		seed{Handle: "codex-08", Kind: core.Codex, BlockID: "block:inepic", SessionID: "session:epic2", AgentPID: 708},
		seed{Handle: "codex-09", Kind: core.Codex, BlockID: "block:inhub", SessionID: "session:hub", AgentPID: 709},
		seed{Handle: "claude-10", Kind: core.Claude, Role: core.PersonalRole, Server: "tcp://remote:7000", BlockID: "block:remote", SessionID: "session:hub2", AgentPID: 710, AgentSessionID: "s-remote"},
		seed{Handle: "claude-11", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:nopid", SessionID: "session:hub3", AgentSessionID: "s-nopid"},
	)
	if err := e.Store.PutSessionCrew("session:crew", store.SessionCrew{Crew: "pair"}); err != nil {
		t.Fatal(err)
	}
	// session:epic2's narrator crashed long ago; its lifecycle stands.
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:epic2", Server: fakeServer, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: "session:hub", Server: fakeServer}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		env       map[string]string
		ancestors []proc
		want      string // the Operator's By, or the refusal's reason
	}{
		{"the user's shell", nil, nil, "user"},
		{"the user's empty Rex pane", map[string]string{"REX_BLOCK": "block:nobody"}, nil, "user"},
		{"a Desktop app's claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "desktop"}, nil, "claude"},
		{"a Desktop app's agy", map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "desktop"}, nil, "agy"},
		{"a personal agent's pane", map[string]string{"REX_BLOCK": "block:personal"}, nil, "claude-01"},
		{"a personal agent's tool shell", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-personal"}, []proc{{PID: 9001}, {PID: 701}}, "claude-01"},
		{"a personal agent inheriting ME_UNATTENDED", map[string]string{"REX_BLOCK": "block:personal", "ME_UNATTENDED": "1"}, nil, "claude-01"},
		{"a standalone agent's pane", map[string]string{"REX_BLOCK": "block:alone"}, nil, "codex-02"},
		{"a standalone agent's tool shell", map[string]string{"CODEX_THREAD_ID": "s-alone"}, []proc{{PID: 9001}, {PID: 702}}, "codex-02"},

		{"ME_UNATTENDED=1", map[string]string{"ME_UNATTENDED": "1"}, nil, GuardUnattended},
		{"AGY_UNATTENDED", map[string]string{"AGY_UNATTENDED": "yes"}, nil, GuardUnattended},
		{"an Owl dispatch", map[string]string{"OWL_DISPATCH_RUN_ID": "r1"}, nil, GuardUnattended},
		{"a Desktop agent run unattended", map[string]string{"CODEX_THREAD_ID": "desktop", "OWL_DISPATCH_RUN": "x"}, nil, GuardUnattended},
		{"a standalone agent with an unattended marker", map[string]string{"REX_BLOCK": "block:alone", "AGY_UNATTENDED": "1"}, nil, GuardUnattended},
		{"an unattended agent of no role", map[string]string{"REX_BLOCK": "block:unattended"}, nil, GuardUnattended},
		{"a worker's pane", map[string]string{"REX_BLOCK": "block:worker"}, nil, GuardOperatorOnly},
		{"a summoned agent of no role", map[string]string{"REX_BLOCK": "block:summoned"}, nil, GuardOperatorOnly},
		{"a launched agent of no role", map[string]string{"REX_BLOCK": "block:launched"}, nil, GuardOperatorOnly},
		{"an agent of no role in a crew's session", map[string]string{"REX_BLOCK": "block:crew"}, nil, GuardOperatorOnly},
		{"an agent of no role in an Epic whose narrator crashed", map[string]string{"REX_BLOCK": "block:inepic"}, nil, GuardOperatorOnly},
		{"an agent of no role in a Hub session", map[string]string{"REX_BLOCK": "block:inhub"}, nil, GuardOperatorOnly},
		{"an unadopted claude -p in a worker's pane", map[string]string{"CLAUDE_CODE_SESSION_ID": "nested", "REX_BLOCK": "block:worker"}, nil, GuardOperatorOnly},
		{"the user's shell under a worker (env -i)", nil, []proc{{PID: 9001}, {PID: 703}}, GuardOperatorOnly},
		{"a Desktop agent under a worker", map[string]string{"PI_SESSION_ID": "desktop"}, []proc{{PID: 703}}, GuardOperatorOnly},
		{"a personal agent under a worker", map[string]string{"REX_BLOCK": "block:personal"}, []proc{{PID: 9001}, {PID: 703}}, GuardOperatorOnly},
		{"a standalone agent under another agent", map[string]string{"REX_BLOCK": "block:alone"}, []proc{{PID: 702}, {PID: 701}}, GuardOperatorOnly},
		{"a worker's session named beside the personal agent's", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-personal", "CODEX_THREAD_ID": "s-alone"}, []proc{{PID: 701}, {PID: 702}}, GuardOperatorOnly},
		{"a pane resuming the personal agent's conversation", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-personal"}, nil, GuardOperatorOnly},
		{"an unadopted codex nested under the personal agent", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-personal", "CODEX_THREAD_ID": "nested"}, []proc{{PID: 9001}, {PID: 701}}, GuardOperatorOnly},
		{"a personal agent on another machine's Rex", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-remote"}, []proc{{PID: 9001}, {PID: 710}}, GuardOperatorOnly},
		{"a personal agent whose lease records no process", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-nopid"}, nil, GuardOperatorOnly},
	} {
		e := e
		if c.ancestors != nil {
			anc := c.ancestors
			e.Ancestors = func() ([]proc, error) { return anc, nil }
		}
		op, err := e.ResolveOperator(func(k string) string { return c.env[k] })
		switch {
		case c.want == GuardUnattended || c.want == GuardOperatorOnly:
			if operatorReason(err) != c.want || !errors.Is(err, ErrRefused) {
				t.Errorf("%s: %+v %v, want guard_denied %s", c.name, op, err, c.want)
			}
		case err != nil || op.By() != core.Handle(c.want):
			t.Errorf("%s: %+v %v, want the Operator %s", c.name, op, err, c.want)
		}
	}
	// A walk up the process tree that stopped at its bound proves nothing.
	deep := make([]proc, maxAncestors)
	for i := range deep {
		deep[i] = proc{PID: 10000 + i}
	}
	e.Ancestors = func() ([]proc, error) { return deep, nil }
	if _, err := e.ResolveOperator(func(string) string { return "" }); operatorReason(err) != GuardOperatorOnly {
		t.Errorf("a walk at its bound: %v", err)
	}
}

// The guard's self admission is an Epic's own lead or narrator, and nobody
// else: a worker of the same Epic stays refused.
func TestOperatorGateAdmitsTheEpicsOwnLeadAndNarrator(t *testing.T) {
	e := operatorWorld(t)
	put(t, e,
		seed{Handle: "claude-02", Kind: core.Claude, Role: core.OrchestratorRole, BlockID: "block:lead", SessionID: "session:epic", AgentPID: 702},
		seed{Handle: "claude-03", Kind: core.Claude, Role: core.NarratorRole, BlockID: "block:narrator", SessionID: "session:epic", AgentPID: 703},
		seed{Handle: "codex-04", Kind: core.Codex, Role: "implementer", BlockID: "block:worker", SessionID: "session:epic", AgentPID: 704},
	)
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyEpic, SessionID: "session:epic", Server: fakeServer, LeadRole: core.OrchestratorRole, LeadHandle: "claude-02"}); err != nil {
		t.Fatal(err)
	}
	for block, admitted := range map[string]bool{"block:lead": true, "block:narrator": true, "block:worker": false} {
		getenv := func(k string) string { return map[string]string{"REX_BLOCK": block}[k] }
		if err := e.OperatorGate(getenv, false); operatorReason(err) != GuardOperatorOnly {
			t.Errorf("%s as the Operator: %v", block, err)
		}
		err := e.OperatorGate(getenv, true)
		if admitted && err != nil || !admitted && operatorReason(err) != GuardOperatorOnly {
			t.Errorf("%s reading its own session: %v (admitted %v)", block, err, admitted)
		}
	}
}
