package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// gitRepo makes a git repository with no origin under parent.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	parent, _ = filepath.EvalSymlinks(parent)
	dir := filepath.Join(parent, name)
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func claudeTrusts(t *testing.T, e Env, dir string) bool {
	t.Helper()
	trusted, err := install.ClaudeTrustedProjects(filepath.Join(e.TranscriptHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	return trusted[dir]
}

func trustLog(t *testing.T, e Env) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(e.Paths.Logs(), "trust.log"))
	return string(b)
}

// A git repository is a trust boundary of its own for Claude: an Epic there
// that nothing trusts is refused with untrusted_folder and the fix command,
// before any Rex session exists, and no store is written.
func TestOpenEpicTrustPreflightRefusesBeforeAnySession(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	repo := gitRepo(t, t.TempDir(), "app")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: repo, Goal: goal})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "untrusted_folder") || !strings.Contains(err.Error(), "rex-amq-adapter trust grant "+repo) {
		t.Fatalf("err %v", err)
	}
	if len(term.sessions_) != 0 || claudeTrusts(t, e, repo) || trustLog(t, e) != "" {
		t.Fatalf("sessions %v, trusted %v, log %q", term.sessions_, claudeTrusts(t, e, repo), trustLog(t, e))
	}
}

// Under a declared trust root, a repository with no origin is granted on
// first need, logged with its root as the source, and the Epic starts.
func TestOpenEpicTrustRootAutoGrantsAnOriginlessRepo(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	side, _ := filepath.EvalSymlinks(t.TempDir())
	repo := gitRepo(t, side, "me-cli")
	if _, err := e.trustGate().AddRoots(context.Background(), side); err != nil {
		t.Fatal(err)
	}
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: repo, Goal: goal}); err != nil {
		t.Fatal(err)
	}
	if len(term.sessions_) != 1 || !claudeTrusts(t, e, repo) {
		t.Fatalf("sessions %v, trusted %v", term.sessions_, claudeTrusts(t, e, repo))
	}
	if log := trustLog(t, e); strings.Count(log, "\n") != 1 || !strings.Contains(log, `"source":"root:`+side+`"`) || !strings.Contains(log, `"root":"`+repo+`"`) {
		t.Fatalf("trust.log %q", log)
	}
}

// A summon start into an untrusted repository launches nothing and holds no
// reservation; under a declared root it is granted and starts, for the
// harness it starts (Codex's store too).
func TestSummonTrustPreflightBeforeLaunch(t *testing.T) {
	e, term, loc := summonWorld(t)
	side, _ := filepath.EvalSymlinks(t.TempDir())
	repo := gitRepo(t, side, "app")
	for _, harness := range []string{"claude", "codex"} {
		res := summon(t, e, loc, "implementer", func(r *SummonRequest) { r.Asked = "start one"; r.Cwd = repo; r.Harness = harness })
		if res.Action != "refused" || !strings.Contains(res.Reason, "untrusted_folder") || !strings.Contains(res.Reason, "trust grant "+repo) {
			t.Fatalf("%s: %+v", harness, res)
		}
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched %+v", term.launches)
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
	if _, err := e.trustGate().AddRoots(context.Background(), side); err != nil {
		t.Fatal(err)
	}
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "start one"; r.Cwd = repo })
	if res.Action != "start" || len(term.launches) != 1 || term.launches[0].Dir != repo {
		t.Fatalf("%+v %+v", res, term.launches)
	}
	if ok, err := install.CodexProjectTrusted(filepath.Join(e.TranscriptHome, ".codex", "config.toml"), repo); err != nil || !ok || !claudeTrusts(t, e, repo) {
		t.Fatalf("codex %v %v, claude %v", ok, err, claudeTrusts(t, e, repo))
	}
}

// A resume preflights the departed agent's own directory: an untrusted one
// launches nothing, and the message waits in its mailbox.
func TestSummonTrustPreflightCoversAResume(t *testing.T) {
	e, term, loc := summonWorld(t)
	repo := gitRepo(t, t.TempDir(), "app")
	add(t, e, departed(store.Record{Handle: "claude-07", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "implementer", AgentPID: 7, AgentSessionID: "sess-7", AdoptedAt: e.Now().Add(-time.Hour), Cwd: repo}, e.Now().Add(-time.Minute)))
	res := summon(t, e, loc, "claude-07")
	if res.Action != "refused" || !strings.Contains(res.Reason, "untrusted_folder") || !strings.Contains(res.Reason, "waits in claude-07's mailbox") || !res.Mailed {
		t.Fatalf("%+v", res)
	}
	if len(term.launches) != 0 || len(unread(t, e, "claude-07")) != 1 {
		t.Fatalf("launches %+v, unread %v", term.launches, unread(t, e, "claude-07"))
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
}
