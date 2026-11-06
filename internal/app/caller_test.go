package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// envOf is a command environment holding the given name, value pairs.
func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestCallerIsTheSessionsRecord(t *testing.T) {
	const server, stale = "unix:///rec.sock", "block:inherited"
	departed := time.Now().Add(-time.Hour)
	recs := []store.Record{
		{Handle: "claude-01", Kind: core.Claude, Server: server, BlockID: "block:claude", AgentPID: 101, AgentSessionID: "cl"},
		{Handle: "codex-01", Kind: core.Codex, Server: server, BlockID: "block:codex", AgentPID: 102, AgentSessionID: "cx"},
		{Handle: "pi-01", Kind: core.Pi, Server: server, BlockID: "block:pi", AgentPID: 103, AgentSessionID: "pi"},
		{Handle: "agy-01", Kind: core.Agy, Server: server, BlockID: "block:agy", AgentPID: 104, AgentSessionID: "ag"},
		{Handle: "codex-02", Kind: core.Codex, Server: server, BlockID: "block:gone", AgentPID: 105, AgentSessionID: "cx-gone", DepartedAt: &departed},
	}
	// The command runs in a shell of the Claude session that the Codex agent
	// started (claude-01's process is the nearer ancestor), unless noted.
	nested := []proc{{1, "rex-amq-adapter"}, {2, "zsh"}, {101, "claude"}, {3, "zsh"}, {102, "codex"}}
	for _, c := range []struct {
		name      string
		env       func(string) string
		ancestors []proc
		want      string // block; "" wants an error
		wantErr   string // in the error
	}{
		{"claude without REX_BLOCK", envOf("CLAUDE_CODE_SESSION_ID", "cl"), nil, "block:claude", ""},
		{"codex without REX_BLOCK", envOf("CODEX_THREAD_ID", "cx"), nil, "block:codex", ""},
		{"pi without REX_BLOCK", envOf("PI_SESSION_ID", "pi"), nil, "block:pi", ""},
		{"agy without REX_BLOCK", envOf("ANTIGRAVITY_CONVERSATION_ID", "ag"), nil, "block:agy", ""},
		{"a session over an inherited REX_BLOCK", envOf("CODEX_THREAD_ID", "cx", "REX_BLOCK", stale), nil, "block:codex", ""},
		{"no session: REX_BLOCK", envOf("REX_BLOCK", stale, "REX_SERVER", server), nil, stale, ""},
		{"no session, no REX_BLOCK", envOf(), nil, "", "REX_BLOCK is unset"},
		{"an unadopted claude -p in an adopted codex's shell", envOf("CODEX_THREAD_ID", "cx", "CLAUDE_CODE_SESSION_ID", "nested", "REX_BLOCK", "block:codex"), nested, "", "claude session (nested) has no AMQ identity"},
		{"an agy subagent", envOf("ANTIGRAVITY_CONVERSATION_ID", "subagent", "REX_BLOCK", "block:agy"), nil, "", "agy session (subagent) has no AMQ identity"},
		{"a departed session", envOf("CODEX_THREAD_ID", "cx-gone", "REX_BLOCK", "block:gone"), nil, "", "has no AMQ identity"},
		{"an adopted agent another adopted agent started", envOf("CODEX_THREAD_ID", "cx", "CLAUDE_CODE_SESSION_ID", "cl"), nested, "block:claude", ""},
		{"the nearer agent is the outer harness's kind", envOf("CODEX_THREAD_ID", "cx", "CLAUDE_CODE_SESSION_ID", "cl"),
			[]proc{{1, "rex-amq-adapter"}, {2, "zsh"}, {102, "codex"}, {101, "claude"}}, "block:codex", ""},
		{"several sessions, none an ancestor", envOf("CODEX_THREAD_ID", "cx", "PI_SESSION_ID", "pi"), nested[:2], "", "descends from none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			paths := store.Paths{Home: t.TempDir()}
			e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: &fakeTerm{}}
			for _, r := range recs {
				if err := e.Store.PutRecord(r); err != nil {
					t.Fatal(err)
				}
			}
			e.Ancestors = func() ([]proc, error) { return c.ancestors, nil }
			loc, err := e.Caller(context.Background(), c.env)
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Caller = %+v, %v; want an error containing %q", loc, err, c.wantErr)
				}
				return
			}
			if err != nil || loc.Block != c.want {
				t.Fatalf("Caller = %+v, %v; want block %s", loc, err, c.want)
			}
			if c.want != stale && loc.Server != server {
				t.Errorf("server %q: want the record's %q", loc.Server, server)
			}
		})
	}
}

func TestCallerWithNoRexLocationIsNotInRex(t *testing.T) {
	paths := store.Paths{Home: t.TempDir()}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: &fakeTerm{}}
	if _, err := e.Caller(context.Background(), envOf()); !errors.Is(err, ErrNotInRex) {
		t.Errorf("err = %v, want ErrNotInRex", err)
	}
}
