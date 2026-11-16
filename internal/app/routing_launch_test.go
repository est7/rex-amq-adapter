package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// envOf reads the assignments of cmd's env prefix, as a launched agent's
// hook finds them in its environment.
func launchEnvOf(cmd []string) func(string) (string, bool) {
	vars := map[string]string{}
	if len(cmd) > 0 && cmd[0] == "env" {
		for _, a := range cmd[1:] {
			k, v, ok := strings.Cut(a, "=")
			if !ok || strings.HasPrefix(a, "-") {
				break
			}
			vars[k] = v
		}
	}
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

// adoptLaunched plays the first hook of a launched pane with the environment
// its command set.
func adoptLaunched(t *testing.T, e Env, term *fakeTerm, l launch, kind core.Kind, session string) store.Record {
	t.Helper()
	term.mu.Lock()
	term.label = l.Label
	term.mu.Unlock()
	a, err := e.Adopt(context.Background(), Location{Block: l.Block}, kind, HookInput{SessionID: session, SummonToken: tokenOf(l), Launch: LaunchFromEnv(launchEnvOf(l.Command))})
	if err != nil {
		t.Fatal(err)
	}
	h, blk := a.Record.Handle, l.Block
	t.Cleanup(func() { _, _ = e.AMQ.RetireFor(context.Background(), e.Paths.Root(), string(h), blk) })
	return a.Record
}

// A started agent runs on its role's routing: the flags are on its command
// line and the model and effort in its environment, which adoption records,
// a repeated hook keeps, who --json shows, and a resume reuses as recorded.
func TestSummonLaunchesOnTheRolesRouting(t *testing.T) {
	e, term, loc := summonWorld(t)
	res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it" })
	if res.Action != "start" || res.Harness != core.Claude {
		t.Fatalf("%+v", res)
	}
	l := term.launches[0]
	if got := launchProgram(l)[:5]; !slices.Equal(got, []string{"claude", "--model", "opus", "--effort", "high"}) {
		t.Fatalf("argv %q", l.Command)
	}
	if v, _ := launchEnvOf(l.Command)(LaunchModelEnv); v != "opus" {
		t.Fatalf("env %q", l.Command)
	}
	rec := adoptLaunched(t, e, term, l, core.Claude, "sess-1")
	if rec.Model != "opus" || rec.Effort != "high" {
		t.Fatalf("adopted %+v", rec)
	}
	// A repeated start hook (a compaction), whose input carries no launch, keeps them.
	a, err := e.Adopt(context.Background(), Location{Block: l.Block}, core.Claude, HookInput{SessionID: "sess-1"})
	if err != nil || a.Record.Model != "opus" || a.Record.Effort != "high" {
		t.Fatalf("re-adopted %+v %v", a.Record, err)
	}
	s, err := e.FindSession(context.Background(), loc.Block)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := e.SessionAgents(s, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ag := range agents {
		if ag.Handle == rec.Handle {
			found = ag.Model == "opus" && ag.Effort == "high"
		}
	}
	if !found {
		t.Fatalf("who --json: %+v", agents)
	}
	// Departed with another model on record: the resume reuses the record, not the table.
	stored, _, _ := e.Store.Record(l.Block)
	stored.Model, stored.Effort, stored.Cwd = "sonnet", "max", workDir(t, e, "app")
	add(t, e, departed(stored, e.Now()))
	if res := summon(t, e, loc, "implementer"); res.Action != "resume" {
		t.Fatalf("%+v", res)
	}
	r := term.launches[1]
	if got := launchProgram(r)[:6]; !slices.Equal(got, []string{"claude", "--model", "sonnet", "--effort", "max", "--resume"}) {
		t.Fatalf("resume argv %q", r.Command)
	}
	if again := adoptLaunched(t, e, term, r, core.Claude, "sess-1"); again.Handle != rec.Handle || again.Model != "sonnet" || again.Effort != "max" {
		t.Fatalf("resumed %+v", again)
	}
}

// A record is a file on disk: a resume checks its model and effort again and
// refuses one that could be read as a flag, before any pane opens. A record
// from before records kept them takes its role's routing (a narrator stays
// on its cheap tier); one with no role keeps its CLI's defaults.
func TestSummonResumeLaunchChecksTheRecord(t *testing.T) {
	old := func(e Env, h core.Handle, kind core.Kind, role, model, effort string) store.Record {
		return departed(store.Record{Handle: h, Kind: kind, Server: "unix:///fake.sock", BlockID: "block:gone-" + string(h), SessionID: "session:s", Role: role, AgentPID: 9, AgentSessionID: "s-" + string(h), AdoptedAt: e.Now().Add(-time.Hour), Cwd: workDir(t, e, "app"), Model: model, Effort: effort}, e.Now().Add(-time.Minute))
	}
	e, term, loc := summonWorld(t)
	add(t, e, old(e, "claude-05", core.Claude, "implementer", "--dangerously-skip-permissions", ""))
	if res := summon(t, e, loc, "claude-05"); res.Action != "refused" || !strings.Contains(res.Reason, "not a model name") || len(term.launches) != 0 || len(unread(t, e, "claude-05")) != 1 {
		t.Fatalf("flag model: %+v %v", res, term.launches)
	}
	add(t, e, old(e, "claude-06", core.Claude, "implementer", "opus", "minimal"))
	if res := summon(t, e, loc, "claude-06"); res.Action != "refused" || !strings.Contains(res.Reason, "effort") || len(term.launches) != 0 {
		t.Fatalf("effort claude does not take: %+v", res)
	}
	add(t, e, old(e, "agy-07", core.Agy, "reviewer", "", ""))
	if res := summon(t, e, loc, "agy-07"); res.Action != "resume" || !slices.Equal(launchProgram(term.launches[0])[:10], []string{"agy", "--model", "gemini-4-carbon-high", "--effort", "high", "--dangerously-skip-permissions", "--mode", "auto", "--conversation", "s-agy-07"}) {
		t.Fatalf("old reviewer: %+v %q", res, term.launches)
	}
	// A narrator takes no member's mail, so its resume is read directly.
	if l, err := e.resumeLaunch(workDir(t, e, "app"), old(e, "claude-09", core.Claude, core.NarratorRole, "", "")); err != nil || l != (core.Launch{Model: "haiku", Effort: "low"}) {
		t.Fatalf("old narrator: %+v %v", l, err)
	}
	add(t, e, old(e, "claude-08", core.Claude, "", "", ""))
	if res := summon(t, e, loc, "claude-08"); res.Action != "resume" || launchProgram(term.launches[1])[1] != "--resume" {
		t.Fatalf("role-less: %+v %q", res, term.launches)
	}
}

// A review role goes to another model family than its caller's, judged by
// the caller's recorded model and each harness's routed model, not by
// harness: an Antigravity caller on a Claude model gets no Claude reviewer,
// and one on Gemini no Antigravity reviewer. When no routed harness is of a
// known other family, the summon refuses, saying why.
func TestSummonReviewerFamilyFollowsTheModels(t *testing.T) {
	caller := func(e Env, kind core.Kind, model string) {
		rec, _, _ := e.Store.Record(block)
		rec.Kind, rec.Model = kind, model
		add(t, e, rec)
	}
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Agy, core.Claude)
	caller(e, core.Agy, "claude-opus-4")
	if res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" }); res.Action != "start" || res.Harness != core.Agy || len(res.Passed) != 1 || !strings.HasPrefix(res.Passed[0], "codex: not installed") {
		t.Fatalf("agy on claude: %+v %v", res, term.launches)
	}
	// agy starts in auto mode (F17): a plan-mode or approval prompt would
	// stall an agent no one watches.
	if got := launchProgram(term.launches[0])[:9]; !slices.Equal(got, []string{"agy", "--model", "gemini-4-carbon-high", "--effort", "high", "--dangerously-skip-permissions", "--mode", "auto", "-i"}) {
		t.Fatalf("argv %q", got)
	}
	e2, term2, loc2 := summonWorld(t)
	e2.LookPath = installedOnly(core.Agy, core.Claude)
	caller(e2, core.Agy, "gemini-4-carbon-high")
	if res := summon(t, e2, loc2, "reviewer", func(r *SummonRequest) { r.Asked = "review it" }); res.Action != "start" || res.Harness != core.Claude || launchProgram(term2.launches[0])[0] != "claude" {
		t.Fatalf("agy on gemini: %+v", res)
	}
	// A user table that routes reviewers only to Antigravity on a Claude model.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeFile(t, filepath.Join(xdg, "rex-amq-adapter", "crews", crew.RoutingFile), "[role.reviewer]\nharness = [\"agy\"]\nagy = { model = \"claude-opus-4\" }\n")
	e3, term3, loc3 := summonWorld(t)
	e3.Crews = crew.Load
	if res := summon(t, e3, loc3, "reviewer", func(r *SummonRequest) { r.Asked = "review it" }); res.Action != "refused" || !strings.Contains(res.Reason, "family known to differ") || len(term3.launches) != 0 {
		t.Fatalf("same family only: %+v", res)
	}
	// An explicit --harness is the caller's call and is never filtered.
	if res := summon(t, e3, loc3, "reviewer", func(r *SummonRequest) { r.Asked = "review it"; r.Harness = "agy" }); res.Action != "start" || !slices.Equal(launchProgram(term3.launches[0])[:3], []string{"agy", "--model", "claude-opus-4"}) {
		t.Fatalf("explicit agy: %+v %v", res, term3.launches)
	}
}

// A project's routing.toml, found from the started agent's directory,
// routes it; a broken one refuses the start, naming the file, before any pane.
func TestSummonRoutingFromTheProject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	wt := t.TempDir()
	p := filepath.Join(wt, ".agents", "crews", crew.RoutingFile)
	writeFile(t, p, "[role.implementer]\nharness = [\"codex\"]\ncodex = { model = \"gpt-6.1-sol-mini\", effort = \"off\" }\n")
	e, term, loc := summonWorld(t)
	e.Crews = crew.Load
	if res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it"; r.Cwd = wt }); res.Action != "start" || res.Harness != core.Codex {
		t.Fatalf("%+v", res)
	}
	if got := launchProgram(term.launches[0])[:6]; !slices.Equal(got, []string{"codex", "-m", "gpt-6.1-sol-mini", "-c", "model_reasoning_effort=none", "--no-daemon"}) {
		t.Fatalf("argv %q", got)
	}
	writeFile(t, p, "[role.implementer]\nharness = [\"codex\"]\ncodex = { model = \"-c\" }\n")
	wt2 := t.TempDir()
	writeFile(t, filepath.Join(wt2, ".agents", "crews", crew.RoutingFile), "[role.implementer]\nharness = [\"codex\"]\ncodex = { model = \"-c\" }\n")
	if res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it"; r.Cwd = wt2 }); res.Action != "refused" || !strings.Contains(res.Reason, filepath.Join(wt2, ".agents", "crews", crew.RoutingFile)) || len(term.launches) != 1 {
		t.Fatalf("broken project routing: %+v", res)
	}
}

// An Epic's narrator and orchestrator start on their roles' first harness
// with its model and effort in their flags and their environment: the
// narrator on agy, the orchestrator on claude; --harness moves the
// orchestrator to another harness with that harness's routing, and leaves
// the narrator on its own.
func TestOpenEpicLaunchesOnTheRouting(t *testing.T) {
	for _, tc := range []struct {
		harness  string
		orchArgv []string
		effort   string
	}{
		{"", []string{"claude", "--model", "opus", "--effort", "high"}, "high"},
		{"claude", []string{"claude", "--model", "opus", "--effort", "high"}, "high"},
		{"agy", []string{"agy", "--model", "gemini-4-carbon-high", "--effort", "high", "--dangerously-skip-permissions", "--mode", "auto", "-i"}, "high"},
		{"codex", []string{"codex", "-m", "gpt-6.1-sol", "-c", "model_reasoning_effort=high"}, "high"},
	} {
		t.Run("harness="+tc.harness, func(t *testing.T) {
			e, term, loc, goal := openEpicWorld(t)
			adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
			adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "routed", Dir: filepath.Dir(goal), Goal: goal, Harness: tc.harness}); err != nil {
				t.Fatal(err)
			}
			narr, orch := term.sessions_[0].panes[0].Command, term.sessions_[0].panes[1].Command
			if got := program(narr)[:6]; !slices.Equal(got, []string{"agy", "--model", "gemini-4-hydrogen-low", "--dangerously-skip-permissions", "--mode", "auto"}) {
				t.Errorf("narrator %q", narr)
			}
			if m, _ := launchEnvOf(narr)(LaunchModelEnv); m != "gemini-4-hydrogen-low" {
				t.Errorf("narrator env %q", narr)
			}
			if got := program(orch)[:len(tc.orchArgv)]; !slices.Equal(got, tc.orchArgv) {
				t.Errorf("orchestrator %q", orch)
			}
			if ef, ok := launchEnvOf(orch)(LaunchEffortEnv); !ok || ef != tc.effort {
				t.Errorf("orchestrator env %q", orch)
			}
			// Only Claude Code reads the brief as its /goal command.
			if brief := orch[len(orch)-1]; strings.HasPrefix(brief, "/goal ") != (program(orch)[0] == "claude") {
				t.Errorf("orchestrator brief %q", brief)
			}
		})
	}
}

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An agent launched on its CLI's defaults resumes on them: a routing change
// made after its start, or a broken routing file, does not reach it. Only a
// record the adapter never wrote a launch to takes the current routing.
func TestSummonResumeLaunchKeepsALaunchOnTheDefaults(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	user := filepath.Join(xdg, "rex-amq-adapter", "crews", crew.RoutingFile)
	writeFile(t, user, "[role.implementer]\nharness = [\"claude\"]\n")
	e, term, loc := summonWorld(t)
	e.Crews = crew.Load
	if res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it" }); res.Action != "start" || launchProgram(term.launches[0])[1] != "--" {
		t.Fatalf("%+v %q", res, term.launches)
	}
	rec := adoptLaunched(t, e, term, term.launches[0], core.Claude, "sess-d")
	if !rec.Launched || rec.Model != "" || rec.Effort != "" {
		t.Fatalf("adopted %+v", rec)
	}
	writeFile(t, user, "[role.implementer]\nharness = [\"claude\"]\nclaude = { model = \"sonnet\", effort = \"max\" }\n")
	rec.Cwd = workDir(t, e, "app")
	add(t, e, departed(rec, e.Now()))
	if res := summon(t, e, loc, string(rec.Handle)); res.Action != "resume" || launchProgram(term.launches[1])[1] != "--resume" {
		t.Fatalf("resumed on the new routing: %+v %q", res, term.launches)
	}
	writeFile(t, user, "[role.implementer]\nharness = [\"claude\"]\nclaude = { model = \"-x\" }\n")
	if res := summon(t, e, loc, string(rec.Handle)); res.Action != "resume" {
		t.Fatalf("a broken routing file blocked a resume that does not read it: %+v", res)
	}
}

// A default review role checks that its harness is installed even when the
// routing, or the family filter, leaves only one.
func TestSummonReviewerChecksASingleRoutedHarnessIsInstalled(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeFile(t, filepath.Join(xdg, "rex-amq-adapter", "crews", crew.RoutingFile), "[role.reviewer]\nharness = [\"codex\"]\ncodex = { model = \"gpt-6-astra\" }\n")
	e, term, loc := summonWorld(t)
	e.Crews = crew.Load
	e.LookPath = installedOnly(core.Claude)
	if res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" }); res.Action != "refused" || !strings.Contains(res.Reason, "codex: not installed") || len(term.launches) != 0 {
		t.Fatalf("%+v %v", res, term.launches)
	}
	// An explicit --harness is still launched as named.
	if res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it"; r.Harness = "codex" }); res.Action != "start" {
		t.Fatalf("explicit: %+v", res)
	}
}
