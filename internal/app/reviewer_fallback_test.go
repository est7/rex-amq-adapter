package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// unapprovedCodexHook gives the summon world's Codex a hooks.json whose one
// hook the user never approved in /hooks, as the host had in F8.
func unapprovedCodexHook(t *testing.T, e Env) {
	t.Helper()
	dir := filepath.Join(e.TranscriptHome, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooks := `{"hooks":{"PreToolUse":[{"hooks":[{"command":"me hook pretooluse --agent codex","type":"command"}],"matcher":"Bash"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installedOnly(kinds ...core.Kind) func(string) (string, error) {
	return func(bin string) (string, error) {
		for _, k := range kinds {
			if string(k) == bin {
				return "/bin/" + bin, nil
			}
		}
		return "", errors.New("not found")
	}
}

// F8: with --harness omitted, a reviewer for a Claude caller goes to Codex
// when Codex is ready, and to Antigravity when a Codex hook waits for the
// user's approval, instead of a pane stuck on "Hooks need review". The
// result names the harness started and the one passed over, and why.
func TestSummonReviewerFallsBackPastUnapprovedCodexHooks(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" })
	if res.Action != "start" || res.Harness != core.Codex || len(res.Passed) != 0 || launchProgram(term.launches[0])[0] != "codex" {
		t.Fatalf("a ready Codex: %+v %q", res, launchProgram(term.launches[0]))
	}

	e, term, loc = summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	unapprovedCodexHook(t, e)
	res = summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" })
	if res.Action != "start" || res.Harness != core.Agy || len(term.launches) != 1 || launchProgram(term.launches[0])[0] != "agy" {
		t.Fatalf("Codex hooks unapproved: %+v %+v", res, term.launches)
	}
	if len(res.Passed) != 1 || !strings.HasPrefix(res.Passed[0], "codex: unapproved_hooks") {
		t.Fatalf("passed over %q", res.Passed)
	}
}

// An explicit --harness codex is the summoner's choice: never swapped, and
// refused before any pane opens while a Codex hook is unapproved.
func TestSummonExplicitCodexWithUnapprovedHooksRefusesBeforeAnyPane(t *testing.T) {
	e, term, loc := summonWorld(t)
	unapprovedCodexHook(t, e)
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it"; r.Harness = "codex" })
	if res.Action != "refused" || !strings.Contains(res.Reason, "unapproved_hooks") || !strings.Contains(res.Reason, "/hooks") {
		t.Fatalf("%+v", res)
	}
	if len(term.launches) != 0 {
		t.Fatalf("a pane opened: %+v", term.launches)
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
}

// A default reviewer never lands in the caller's own family: a Codex
// caller's goes to Antigravity even with Codex ready, and a harness that is
// not installed is passed over.
func TestSummonReviewerDefaultSkipsTheCallersFamily(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	caller, _, err := e.Store.Record(block)
	if err != nil {
		t.Fatal(err)
	}
	caller.Kind = core.Codex
	if err := e.Store.PutRecord(caller); err != nil {
		t.Fatal(err)
	}
	res := summon(t, e, loc, "qa", func(r *SummonRequest) { r.Asked = "test it" })
	if res.Action != "start" || res.Harness != core.Agy || launchProgram(term.launches[0])[0] != "agy" {
		t.Fatalf("a Codex caller's qa: %+v", res)
	}

	e, term, loc = summonWorld(t)
	e.LookPath = installedOnly(core.Agy, core.Claude)
	res = summon(t, e, loc, "editor", func(r *SummonRequest) { r.Asked = "edit it" })
	if res.Action != "start" || res.Harness != core.Agy || len(res.Passed) != 1 || res.Passed[0] != "codex: not installed" {
		t.Fatalf("no codex installed: %+v", res)
	}
}

// When no other family is ready the summon refuses, naming each reason,
// and starts nothing: it never falls back to the caller's own family.
func TestSummonReviewerFallbackRefusesWhenNoneIsReady(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Claude)
	unapprovedCodexHook(t, e)
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" })
	if res.Action != "refused" || len(term.launches) != 0 {
		t.Fatalf("%+v %+v", res, term.launches)
	}
	for _, want := range []string{"codex: unapproved_hooks", "agy: not installed", "--harness"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, res.Reason)
		}
	}
	if strings.Contains(res.Reason, "claude:") || len(res.Passed) != 2 {
		t.Errorf("the caller's family was considered: %+v", res)
	}
}

// Roles other than review roles keep their one default harness, which is
// launched even when this process cannot find it on its PATH.
func TestSummonReviewerFallbackLeavesOtherRolesAlone(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly()
	unapprovedCodexHook(t, e)
	res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it" })
	if res.Action != "start" || res.Harness != core.Claude || launchProgram(term.launches[0])[0] != "claude" {
		t.Fatalf("%+v", res)
	}
}

// logs/summon.log keeps which harness a fallback started and what it passed
// over, so the summoner's hand-back can list it.
func TestSummonFallbackIsLogged(t *testing.T) {
	e, _, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy)
	unapprovedCodexHook(t, e)
	if res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "review it" }); res.Action != "start" {
		t.Fatalf("%+v", res)
	}
	b, err := os.ReadFile(filepath.Join(e.Paths.Logs(), "summon.log"))
	if err != nil || !strings.Contains(string(b), `"harness":"agy"`) || !strings.Contains(string(b), `"passed_over":["codex: unapproved_hooks`) {
		t.Fatalf("summon.log %s %v", b, err)
	}
}

// A council seats three reviewers in one Slice worktree, one per harness:
// with --harness omitted the second seat passes over the harness already
// seated there, a third names its own, and a second seat on a harness that
// already has one, or a default start with every other family seated, is
// refused. A writer role still never shares the directory.
func TestSummonCouncilSeatsReviewersOnDistinctHarnessesInOneWorktree(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	wt := workDir(t, e, "app-s1")
	trustAgy(t, e, workDir(t, e, "app"), wt)
	seat := func(harness string, want core.Kind, session string) {
		t.Helper()
		n := len(term.launches)
		res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "council review"; r.Cwd = wt; r.Harness = harness })
		if res.Action != "start" || res.Harness != want || len(term.launches) != n+1 || term.launches[n].Dir != wt {
			t.Fatalf("seat --harness=%q: %+v launches %+v", harness, res, term.launches)
		}
		rec := adoptLaunch(t, e, term, term.launches[n], want, session)
		rec.Cwd = wt
		add(t, e, rec)
	}
	seat("", core.Codex, "sess-codex")
	seat("", core.Agy, "sess-agy")
	seat("claude", core.Claude, "sess-claude")
	n := len(term.launches)
	for _, c := range []struct{ harness, reason string }{
		{"codex", "already works in"},
		{"", "already has a live reviewer"},
	} {
		res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "council review"; r.Cwd = wt; r.Harness = c.harness })
		if res.Action != "refused" || !strings.Contains(res.Reason, c.reason) || len(term.launches) != n {
			t.Errorf("a fourth seat --harness=%q: %+v", c.harness, res)
		}
	}
	if res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it"; r.Cwd = wt }); res.Action != "start" {
		t.Fatalf("setup the writer: %+v", res)
	}
	w := adoptLaunch(t, e, term, term.launches[n], core.Claude, "sess-writer")
	w.Cwd = wt
	add(t, e, w)
	if res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "build it"; r.Cwd = wt; r.Harness = "codex" }); res.Action != "refused" || !strings.Contains(res.Reason, "would edit the same files") {
		t.Errorf("a second writer on another harness in the council's worktree: %+v", res)
	}
}
