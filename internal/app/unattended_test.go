package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// unattendedPrefix reports whether cmd's env prefix, before its program,
// sets UnattendedEnv=1; anywhere else (a brief quoting it) does not count.
func unattendedPrefix(cmd []string) bool {
	if len(cmd) == 0 || cmd[0] != "env" {
		return false
	}
	for _, a := range cmd[1:] {
		if !strings.Contains(a, "=") {
			return false
		}
		if a == UnattendedEnv+"=1" {
			return true
		}
	}
	return false
}

func mentionsUnattended(cmd []string) bool {
	return strings.Contains(strings.Join(cmd, " "), UnattendedEnv)
}

// Every Epic is unattended, whoever asked for it: both its agents' commands
// carry the marker in their own env prefix (the program still follows it),
// and the session's policy is recorded with who opened it. The personal
// caller's record and the adapter's own environment are left as they were,
// an inherited marker included.
func TestOpenEpicRunsItsAgentsUnattended(t *testing.T) {
	for name, req := range map[string]func(goal string) EpicRequest{
		"goal contract": func(goal string) EpicRequest { return EpicRequest{Name: "login", Dir: filepath.Dir(goal), Goal: goal} },
		"user's words": func(goal string) EpicRequest {
			return EpicRequest{Name: "login", Dir: filepath.Dir(goal), Asked: "开个 Epic"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(UnattendedEnv, "1") // the personal agent inherited it
			e, term, loc, goal := openEpicWorld(t)
			now := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
			e.Now = func() time.Time { return now }
			adoptPane(t, e, "block:epic-1-1", "agy-02", core.NarratorRole)
			adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := e.OpenEpic(ctx, loc, req(goal)); err != nil {
				t.Fatal(err)
			}
			panes := term.sessions_[0].panes
			for i, agent := range []struct{ role, harness string }{{"narrator", "agy"}, {"orchestrator", "claude"}} {
				if cmd := panes[i].Command; !unattendedPrefix(cmd) || program(cmd)[0] != agent.harness {
					t.Errorf("%s command %q: want %s=1 in its env prefix before %s", agent.role, cmd, UnattendedEnv, agent.harness)
				}
			}
			p, ok, err := e.Store.SessionPolicy("session:epic-1")
			want := store.SessionPolicy{Kind: store.PolicyEpic, SessionID: "session:epic-1", Server: fakeServer, LeadRole: core.OrchestratorRole, Unattended: true, CreatedBy: "claude-01", CreatedAt: now}
			if err != nil || !ok || p != want {
				t.Errorf("policy %+v ok=%v err=%v, want %+v", p, ok, err, want)
			}
			if me, _, _ := e.agentAt(loc.Block); me.Unattended {
				t.Errorf("the personal caller was marked unattended: %+v", me)
			}
			if got := os.Getenv(UnattendedEnv); got != "1" {
				t.Errorf("the adapter's environment changed: %s=%q", UnattendedEnv, got)
			}
		})
	}
}

// putPolicy records session as an Epic's, as epic would.
func putPolicy(t *testing.T, e Env, session string) {
	t.Helper()
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyEpic, SessionID: session, Unattended: true, CreatedBy: "claude-09", CreatedAt: e.Now()}); err != nil {
		t.Fatal(err)
	}
}

// A worker started in an Epic's session runs unattended; so does one an
// unattended caller starts, and an orchestrator or narrator anywhere. A
// worker an attended caller starts outside any Epic does not. The summon
// token stays the first assignment, which binds the launch to its adoption.
func TestSummonStartsUnattendedInAnEpic(t *testing.T) {
	asked := func(r *SummonRequest) { r.Asked = "开个 implementer"; r.Harness = "claude" }
	cases := []struct {
		name   string
		setup  func(t *testing.T, e Env, loc Location)
		target string
		want   bool
	}{
		{"epic session", func(t *testing.T, e Env, _ Location) { putPolicy(t, e, "session:s") }, "implementer", true},
		{"unattended caller", func(t *testing.T, e Env, loc Location) {
			me, _, _ := e.agentAt(loc.Block)
			me.Unattended = true
			add(t, e, seedOf(me))
		}, "implementer", true},
		{"orchestrator", func(*testing.T, Env, Location) {}, core.OrchestratorRole, true},
		{"attended, no epic", func(*testing.T, Env, Location) {}, "implementer", false},
		{"another session's epic", func(t *testing.T, e Env, _ Location) { putPolicy(t, e, "session:other") }, "implementer", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, term, loc := summonWorld(t)
			c.setup(t, e, loc)
			res := summon(t, e, loc, c.target, asked)
			if res.Action != "start" {
				t.Fatalf("%+v", res)
			}
			l := term.launches[0]
			if got := unattendedPrefix(l.Command); got != c.want || got != mentionsUnattended(l.Command) {
				t.Fatalf("launch %q: unattended %v, want %v", l.Command, got, c.want)
			}
			if !strings.HasPrefix(l.Command[1], SummonEnv+"=") || tokenOf(l) != res.Token || launchProgram(l)[0] != "claude" {
				t.Fatalf("launch %q: the token is not the first assignment, or the program moved", l.Command)
			}
			rec := adoptUnattendedLaunch(t, e, term, l, "s-"+c.name)
			if rec.Unattended != c.want {
				t.Errorf("adopted %+v: unattended %v, want %v", rec, rec.Unattended, c.want)
			}
		})
	}
}

// A resumed worker runs unattended when it ran so before, or its session is
// an Epic's; a resumed personal agent never does, whoever resumes it.
func TestSummonResumesUnattendedWorkersButNeverAPersonalAgent(t *testing.T) {
	gone := func(e Env, h core.Handle, role string, unattended bool) seed {
		return departed(seed{Handle: h, Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: role, AgentPID: 7, AgentSessionID: "conv-" + string(h), AdoptedAt: e.Now().Add(-time.Hour), Cwd: workDir(t, e, "app"), Unattended: unattended}, e.Now().Add(-time.Minute))
	}
	t.Run("worker that ran unattended", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		add(t, e, gone(e, "claude-07", "implementer", true))
		if res := summon(t, e, loc, "claude-07"); res.Action != "resume" || !unattendedPrefix(term.launches[0].Command) {
			t.Fatalf("%+v launch %q", res, term.launches)
		}
		if l := term.launches[0]; tokenOf(l) == "" || launchProgram(l)[0] != "claude" || !strings.Contains(strings.Join(launchProgram(l), " "), "--resume conv-claude-07") {
			t.Fatalf("launch %q", l.Command)
		}
	})
	t.Run("worker in an epic's session", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		putPolicy(t, e, "session:s")
		add(t, e, gone(e, "claude-07", "implementer", false))
		if res := summon(t, e, loc, "claude-07"); res.Action != "resume" || !unattendedPrefix(term.launches[0].Command) {
			t.Fatalf("%+v launch %q", res, term.launches)
		}
		// Its record learns it at adoption, from the session's policy.
		rec := adoptLaunch(t, e, term, term.launches[0], core.Claude, "conv-claude-07") // its hook without the marker
		if rec.Handle != "claude-07" || !rec.Unattended {
			t.Fatalf("resumed as %+v", rec)
		}
	})
	// The gateway keeps a worker's mail, and so its summon, from a personal
	// agent; the decision refuses it on its own all the same.
	t.Run("personal agent", func(t *testing.T) {
		e, _, _ := summonWorld(t)
		putPolicy(t, e, "session:s")
		p := gone(e, "claude-05", core.PersonalRole, true).asAgent()
		for _, caller := range []seed{{Role: core.OrchestratorRole, Unattended: true}, {Role: core.NarratorRole}} {
			if got, err := e.launchUnattended("session:s", core.PersonalRole, caller.asAgent(), &p); err != nil || got {
				t.Errorf("a personal agent resumed by %+v: unattended %v err %v", caller, got, err)
			}
		}
	})
}

// adoptUnattendedLaunch plays the launched pane's first hook, its
// environment carrying the marker exactly when the launch command set it.
func adoptUnattendedLaunch(t *testing.T, e Env, term *fakeTerm, l launch, session string) agent {
	t.Helper()
	term.mu.Lock()
	term.label = l.Label
	term.mu.Unlock()
	a, err := e.Adopt(context.Background(), Location{Block: l.Block}, core.Claude, HookInput{SessionID: session, SummonToken: tokenOf(l), Unattended: unattendedPrefix(l.Command)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.AMQ.RetireHandle(context.Background(), e.Paths.Root(), string(a.Agent.Handle)) })
	return a.Agent
}

// An unreadable Epic policy starts nothing: the launch is not made attended
// by default, and its reservation is released.
func TestSummonStartsNothingOnAnUnreadablePolicy(t *testing.T) {
	e, term, loc := summonWorld(t)
	putPolicy(t, e, "session:s")
	if err := os.WriteFile(filepath.Join(e.Paths.SessionDir("session:s"), "policy.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.Summon(context.Background(), loc, SummonRequest{Target: "implementer", Kind: "question", Subject: "s", Body: "b", Asked: "开个 implementer", Harness: "claude"})
	if err == nil || !strings.Contains(err.Error(), "Epic policy") {
		t.Fatalf("err %v", err)
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched: %+v", term.launches)
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
}

// Adoption records an agent unattended from its hook's environment or its
// session's policy, and keeps it across a repeated hook; a personal agent is
// never recorded so, marker or policy. `env` exports the marker to an
// unattended agent's shells and says nothing of it to anyone else.
func TestAdoptRecordsUnattendedAndEnvExportsIt(t *testing.T) {
	adopt := func(t *testing.T, e Env, term *fakeTerm, block, label, session string, in HookInput) agent {
		t.Helper()
		term.mu.Lock()
		term.label = label
		term.windows[block] = "window:1"
		term.mu.Unlock()
		in.SessionID = session
		a, err := e.Adopt(context.Background(), Location{Block: block}, core.Claude, in)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = e.AMQ.RetireHandle(context.Background(), e.Paths.Root(), string(a.Agent.Handle)) })
		return a.Agent
	}
	exports := func(t *testing.T, e Env, block string) string {
		t.Helper()
		out, err := e.Exports(Location{Block: block})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	e, term, _ := summonWorld(t)
	rec := adopt(t, e, term, "block:w", "implementer | x", "conv-w", HookInput{Unattended: true})
	if !rec.Unattended {
		t.Fatalf("launched with the marker, adopted as %+v", rec)
	}
	if out := exports(t, e, "block:w"); !strings.Contains(out, " "+UnattendedEnv+"=1") || !strings.HasPrefix(out, "export ") {
		t.Errorf("env for an unattended worker: %q", out)
	}
	if again := adopt(t, e, term, "block:w", "implementer(claude-02) | x", "conv-w", HookInput{}); !again.Unattended || again.Handle != rec.Handle {
		t.Errorf("a repeated hook without the marker dropped it: %+v", again)
	}

	plain := adopt(t, e, term, "block:p", "implementer | y", "conv-p", HookInput{})
	if plain.Unattended || mentionsUnattended([]string{exports(t, e, "block:p")}) {
		t.Errorf("an attended worker outside any Epic: %+v", plain)
	}

	e2, term2, _ := summonWorld(t)
	putPolicy(t, e2, "session:s")
	if rec := adopt(t, e2, term2, "block:w", "implementer | x", "conv-w", HookInput{}); !rec.Unattended {
		t.Errorf("a worker adopted in an Epic's session: %+v", rec)
	}
	// A personal agent lives in a session of its own (C25), never the Epic's.
	term2.mu.Lock()
	term2.sessions = map[string]string{"window:1": "session:me"}
	term2.mu.Unlock()
	for _, in := range []HookInput{{}, {Unattended: true}} {
		me := adopt(t, e2, term2, "block:me", "personal", "conv-me", in)
		if me.Role != core.PersonalRole || me.Unattended {
			t.Errorf("a personal agent (hook marker %v) adopted as %+v", in.Unattended, me)
		}
		if out := exports(t, e2, "block:me"); mentionsUnattended([]string{out}) {
			t.Errorf("env for a personal agent mentions the marker: %q", out)
		}
	}
}
