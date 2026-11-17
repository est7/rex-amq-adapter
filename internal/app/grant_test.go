package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// grantWorld is an Epic opening whose lead's first hook (SessionStart, on
// Pi) races the open: started as the session is created, it waits on the
// lock the open holds, and adopted reports the notice it got.
func grantWorld(t *testing.T) (Env, *fakeTerm, chan adoptedNotice) {
	t.Helper()
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:me": "window:me"}, tabs: map[string]string{"window:me": "me"}}
	e := realAmqEnv(t, term)
	e.TranscriptHome = t.TempDir()
	trustTemp(t, e)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	put(t, e, seed{Handle: "claude-01", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me", SessionID: "session:me", WindowID: "window:me", AdoptedAt: time.Now()})
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	done := make(chan adoptedNotice, 1)
	term.sessionStarted = func(_ string, blocks []string) {
		// The lead's pane label, which adoption reads its role from.
		term.mu.Lock()
		term.label = term.blockLabels[blocks[1]]
		term.mu.Unlock()
		go func() {
			r, err := e.Hook(context.Background(), EventSessionStart, core.Pi, Location{Block: blocks[1]}, HookInput{SessionID: "lead-1", AgentPID: os.Getpid()})
			done <- adoptedNotice{r.Notice, err}
		}()
		// Give the hook every chance to win the race it would win unguarded.
		select {
		case a := <-done:
			done <- a
		case <-time.After(300 * time.Millisecond):
		}
	}
	return e, term, done
}

type adoptedNotice struct {
	notice string
	err    error
}

func openGrant(t *testing.T, e Env, r EpicRequest) EpicResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.OpenEpic(ctx, Location{Block: "block:me"}, r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readOnly(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Errorf("%s is %v, want read-only 0444", path, info.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	return string(b)
}

// epic --goal FILE --asked WORDS notarizes both before the lead is briefed:
// the read-only grant.json and grant.md record the user's words verbatim,
// who relayed them, and the contract with its digest; the lead's argv runs
// the contract, and its start notice, though its first hook ran while the
// Epic opened, ends with the Goal and Grant lines. The result and the view
// expose both paths.
func TestOpenEpicGrantNotarizesGoalAndAskedTogether(t *testing.T) {
	e, term, done := grantWorld(t)
	dir := t.TempDir()
	trustAgy(t, e, physical(t, dir))
	goal := filepath.Join(dir, "goal.md")
	contract := "GOAL: fix x, y, z\nDONE WHEN:\n- `go test ./...`\n"
	if err := os.WriteFile(goal, []byte(contract), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := "fix x, y, z. Full stop with council 9.5"
	res := openGrant(t, e, EpicRequest{Name: "xyz", Dir: dir, Goal: goal, Asked: asked})
	id := res.SessionID
	if res.GoalPath != goal || res.GrantPath != e.Paths.GrantPath(id) {
		t.Fatalf("result paths %q %q", res.GoalPath, res.GrantPath)
	}
	var g store.Grant
	if err := json.Unmarshal([]byte(readOnly(t, filepath.Join(e.Paths.SessionDir(id), "grant.json"))), &g); err != nil {
		t.Fatal(err)
	}
	if g.Asked != asked || g.RelayedBy != "claude-01" || g.SessionID != id || g.Contract != goal || g.ContractSHA256 != store.ContractDigest([]byte(contract)) || g.RelayedAt.IsZero() {
		t.Fatalf("grant.json %+v", g)
	}
	md := readOnly(t, res.GrantPath)
	for _, want := range []string{"> " + asked + "\n", "relayed_by: claude-01", "contract: " + goal, "contract_sha256: " + g.ContractSHA256} {
		if !strings.Contains(md, want) {
			t.Errorf("grant.md lacks %q:\n%s", want, md)
		}
	}
	if _, err := os.Stat(e.Paths.GoalPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a contract with a file of its own was copied to goal.md: %v", err)
	}
	lead := term.sessions_[0].panes[1].Command
	if brief := lead[len(lead)-1]; !strings.Contains(brief, "Execute the goal contract at "+goal) {
		t.Errorf("lead brief %q", brief)
	}
	a := <-done
	grantLine := "Grant: " + res.GrantPath + " (user's words relayed verbatim by personal claude-01, recorded by rex-amq-adapter)"
	if a.err != nil || !strings.HasSuffix(a.notice, "Goal: "+goal+"\n"+grantLine) {
		t.Fatalf("the lead's start notice (%v) does not end with its Goal and Grant lines:\n%s", a.err, a.notice)
	}
	if strings.Contains(e.identityNotice(context.Background(), Adoption{Agent: recordOf(t, e, "claude-02")}, 0), "Grant:") {
		t.Error("the narrator was told the grant meant for the lead")
	}
	out, err := e.withStored(id, json.RawMessage(`{"schema":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var v SessionView
	if err := json.Unmarshal(out, &v); err != nil || v.GoalPath != goal || v.GrantPath != res.GrantPath {
		t.Fatalf("view %s (%v)", out, err)
	}
}

// epic --goal - pipes the contract in: the Epic keeps it read-only as its
// goal.md, the grant cites that copy and its digest, and the pair lead, whose
// argv cannot name a path the session did not have yet, is pointed at the
// Goal line its start notice carries.
func TestOpenEpicGrantKeepsAPipedGoalForAPairLead(t *testing.T) {
	e, term, done := grantWorld(t)
	dir := t.TempDir()
	trustAgy(t, e, physical(t, dir))
	contract := "GOAL: one Slice\nDONE WHEN:\n- `true`\n"
	res := openGrant(t, e, EpicRequest{Name: "piped", Dir: dir, GoalText: contract, Asked: "fix it. Full send", Crew: pairCrew})
	id := res.SessionID
	if res.Lead != implementerRole || res.GoalPath != e.Paths.GoalPath(id) || res.GrantPath != e.Paths.GrantPath(id) {
		t.Fatalf("result %+v", res)
	}
	if got := readOnly(t, res.GoalPath); got != contract {
		t.Fatalf("goal.md %q, want %q", got, contract)
	}
	g, ok, err := e.Store.Grant(id)
	if err != nil || !ok || g.Contract != res.GoalPath || g.ContractSHA256 != store.ContractDigest([]byte(contract)) || g.Asked != "fix it. Full send" {
		t.Fatalf("grant %+v %v %v", g, ok, err)
	}
	lead := term.sessions_[0].panes[1].Command
	if brief := lead[len(lead)-1]; !strings.Contains(brief, "Execute the goal contract at the path on the `Goal:` line of your AMQ start notice") {
		t.Errorf("lead brief %q", brief)
	}
	a := <-done
	if a.err != nil || !strings.Contains(a.notice, "Goal: "+res.GoalPath+"\nGrant: "+res.GrantPath) || !strings.Contains(a.notice, "Your role here is implementer") {
		t.Fatalf("the pair lead's start notice (%v):\n%s", a.err, a.notice)
	}
}

// An open whose contract or words are malformed is refused before any
// session exists.
func TestOpenEpicGrantRefusesAMalformedGoal(t *testing.T) {
	e, term, _ := grantWorld(t)
	dir := t.TempDir()
	goal := filepath.Join(dir, "goal.md")
	if err := os.WriteFile(goal, []byte("GOAL: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		r    EpicRequest
		want error
	}{
		"file and piped": {EpicRequest{Goal: goal, GoalText: "GOAL: y"}, ErrUsage},
		"blank piped":    {EpicRequest{GoalText: " \n\t"}, ErrUsage},
		"not UTF-8":      {EpicRequest{GoalText: "GOAL: \xff"}, ErrUsage},
		"too large":      {EpicRequest{GoalText: strings.Repeat("x", EpicGoalMax+1)}, ErrUsage},
		"missing file":   {EpicRequest{Goal: filepath.Join(dir, "missing.md")}, ErrRefused},
		"nothing":        {EpicRequest{}, ErrRefused},
	} {
		c.r.Name, c.r.Dir = "bad", dir
		if _, err := e.OpenEpic(context.Background(), Location{Block: "block:me"}, c.r); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if len(term.sessions_) != 0 {
		t.Fatalf("a refused open created a session: %+v", term.sessions_)
	}
}

// A retry of an --op-id open keeps the grant its first run notarized, time
// included, and a piped contract with other text under the same op id is
// another request.
func TestOpenEpicGrantSurvivesAnOpIDReplay(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	clock := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return clock }
	req := EpicRequest{Name: "replay", Dir: dir, GoalText: "GOAL: piped\n", Asked: "go", OpID: "me:epic-9#open"}
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	first, err := openWithin(e, loc, req, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, _ := e.Store.Grant(first.SessionID)
	clock = clock.Add(time.Hour)
	again, err := openWithin(e, loc, req, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.GoalPath != first.GoalPath || again.GrantPath != first.GrantPath || len(term.sessions_) != 1 {
		t.Fatalf("replay %+v, first %+v", again, first)
	}
	if g, _, _ := e.Store.Grant(first.SessionID); g != kept || !g.RelayedAt.Equal(time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("the replay rewrote the grant: %+v, kept %+v", g, kept)
	}
	other := req
	other.GoalText = "GOAL: other\n"
	if _, err := openWithin(e, loc, other, time.Second); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another epic open request") {
		t.Errorf("another piped contract under the same op id: %v", err)
	}
}
