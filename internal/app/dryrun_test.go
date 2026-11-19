package app

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// stateFiles is every file under the state directory with its contents.
// Lock files are left out: a preview may take a lock (§3.7), it leaves no
// state beside it.
func stateFiles(t *testing.T, e Env) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(e.Paths.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".lock") {
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// settledState is stateFiles once every session watcher a fixture's
// adoption spawned has written its log: the watcher appends after the spawn
// returns, which must not count as the command under test writing.
func settledState(t *testing.T, e Env) map[string]string {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		logs, _ := filepath.Glob(filepath.Join(e.Paths.Logs(), "watch-*.log"))
		pending := false
		for _, l := range logs {
			if st, err := os.Stat(l); err == nil && st.Size() == 0 {
				pending = true
			}
		}
		if !pending {
			break
		}
	}
	return stateFiles(t, e)
}

func sameState(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	if maps.Equal(before, after) {
		return
	}
	for k, v := range after {
		if before[k] != v {
			t.Errorf("%s changed %s", what, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Errorf("%s removed %s", what, k)
		}
	}
}

// --dry-run runs a mutator's whole resolution and every guard, then stops
// before its effect: each preview below leaves the state directory as it
// was, and asks Rex to kill, close, launch, or open nothing, and amq to
// deliver nothing.
func TestDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	t.Run("epic close", func(t *testing.T) {
		e, term, loc := closeWorld(t)
		before := settledState(t, e)
		res, err := e.CloseEpic(ctx, loc, CloseRequest{Session: "session:epic", Asked: "close it", DryRun: true})
		if err != nil || !res.DryRun {
			t.Fatalf("preview: %+v %v", res, err)
		}
		sameState(t, "epic close --dry-run", before, stateFiles(t, e))
		if len(term.killed) != 0 || len(term.closed) != 0 {
			t.Errorf("killed %v closed %v", term.killed, term.closed)
		}
	})
	t.Run("epic link, unlink, handback", func(t *testing.T) {
		e, _, loc := closeWorld(t)
		before := settledState(t, e)
		if _, changed, err := e.PreviewLink(ctx, loc, "session:epic", false); err != nil || !changed {
			t.Fatalf("unlink preview: changed=%v %v", changed, err)
		}
		if _, changed, err := e.PreviewLink(ctx, loc, "session:epic", true); err != nil || changed {
			t.Fatalf("link preview of a linked Epic: changed=%v %v", changed, err)
		}
		if h, err := e.PreviewHandBack(Location{Block: "block:o"}, "## Review queue\n"); err != nil || h.By == "" || h.Digest == "" {
			t.Fatalf("handback preview: %+v %v", h, err)
		}
		if _, err := e.PreviewHandBack(Location{Block: "block:w"}, "report"); err == nil {
			t.Error("a worker's handback preview passed the lead check")
		}
		sameState(t, "link, unlink, handback --dry-run", before, stateFiles(t, e))
	})
	t.Run("agent retire", func(t *testing.T) {
		e, term, _, fake := retireWorld(t)
		before := stateFiles(t, e)
		res, err := e.Retire(ctx, Location{Block: "block:o"}, RetireRequest{Handle: "codex-03", DryRun: true})
		if err != nil || !res.DryRun || res.Action != RetireRetired {
			t.Fatalf("preview: %+v %v", res, err)
		}
		sameState(t, "agent retire --dry-run", before, stateFiles(t, e))
		if len(term.closed) != 0 || fake.Waker(e.Paths.Root(), "codex-03").Status == "missing" {
			t.Errorf("closed %v, waker %+v", term.closed, fake.Waker(e.Paths.Root(), "codex-03"))
		}
	})
	t.Run("agent summon", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		add(t, e, seed{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:b", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AdoptedAt: e.Now()})
		before := stateFiles(t, e)
		dry := func(r *SummonRequest) { r.DryRun = true }
		if res := summon(t, e, loc, "reviewer", dry); res.Action != "deliver" || res.Mailed || !res.DryRun {
			t.Fatalf("deliver preview: %+v", res)
		}
		if res := summon(t, e, loc, "implementer", dry, func(r *SummonRequest) { r.Asked = "start one" }); res.Action != "start" || res.Harness == "" {
			t.Fatalf("start preview: %+v", res)
		}
		sameState(t, "agent summon --dry-run", before, stateFiles(t, e))
		if ids, _ := amq.Unread(e.Paths.Root(), "codex-02"); len(ids) != 0 || len(term.launches) != 0 || len(term.newTabs) != 0 {
			t.Errorf("unread %v launches %v tabs %v", ids, term.launches, term.newTabs)
		}
	})
	t.Run("mail send", func(t *testing.T) {
		e, _ := managedWorld(t)
		before := stateFiles(t, e)
		p, err := e.PreviewPost(Location{Block: "block:claude-02"}, []core.Handle{"codex-01"}, "", "")
		if err != nil || len(p.Recipients) != 1 || !p.Recipients[0].Allow {
			t.Fatalf("preview: %+v %v", p, err)
		}
		if _, err := e.PreviewPost(Location{Block: "block:claude-02"}, []core.Handle{"claude-09"}, "", ""); err == nil {
			t.Error("a cross-Epic preview was allowed")
		}
		sameState(t, "mail send --dry-run", before, stateFiles(t, e))
		if wakerLive(t, e, "codex-01") {
			t.Error("a preview started the recipient's waker")
		}
	})
	t.Run("hub notify", func(t *testing.T) {
		hub := t.TempDir()
		e, _ := notifyWorld(t, hub)
		before := stateFiles(t, e)
		n, err := ParseNotify(NotifyRequest{Ref: "inbox:1", EventID: "ev-1"})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		res, err := e.PreviewNotify(cctx, n)
		if err != nil || res.Status != NotifyNotified || res.To != "claude-01" || !res.DryRun || res.MessageID != "" {
			t.Fatalf("preview: %+v %v", res, err)
		}
		sameState(t, "hub notify --dry-run", before, stateFiles(t, e))
		if ids, _ := amq.Unread(e.Paths.Root(), "claude-01"); len(ids) != 0 {
			t.Errorf("a preview delivered %v", ids)
		}
	})
	t.Run("epic open", func(t *testing.T) {
		e, term, loc := closeWorld(t)
		before := settledState(t, e)
		dir := t.TempDir()
		res, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "preview", Dir: dir, Asked: "open it", OpID: "op-preview", DryRun: true})
		if err != nil || !res.DryRun || res.Plan == nil || res.SessionID != "" || res.Lead != core.OrchestratorRole {
			t.Fatalf("preview: %+v %v", res, err)
		}
		sameState(t, "epic open --dry-run", before, stateFiles(t, e))
		if len(term.sessions_) != 0 || len(term.launches) != 0 {
			t.Errorf("sessions %v launches %v", term.sessions_, term.launches)
		}
		if _, ok, _ := e.Store.EpicOp(store.EpicOpOpen, "op-preview"); ok {
			t.Error("a preview recorded its op id")
		}
	})
}

// agent join --dry-run resolves the agent, the crew, and the role it would
// take, refuses what Join refuses, and claims nothing.
func TestPreviewJoin(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b")
	ctx := context.Background()
	before := stateFiles(t, e)
	p, err := e.PreviewJoin(ctx, Location{Block: "block:a"}, "pair", "")
	if err != nil || p.Crew != "pair" || p.Role != "implementer" || !strings.HasPrefix(p.Label, "implementer(") {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if _, err := e.PreviewJoin(ctx, Location{Block: "block:a"}, "nosuch", ""); err == nil {
		t.Error("an unknown crew previewed")
	}
	if _, err := e.PreviewJoin(ctx, Location{Block: "block:a"}, "pair", core.NarratorRole); err == nil {
		t.Error("a narrator joining a crew previewed")
	}
	sameState(t, "agent join --dry-run", before, stateFiles(t, e))
	if c := e.sessionCrew("session:s", "implementer"); c != "" {
		t.Errorf("the preview claimed crew %q", c)
	}
}

// A summon's --dry-run takes the real summon's decisions (harness
// readiness, hooks and trust, the review family, council seats, the
// user's authority, the cap) and reports them: the harness it would start
// and each it passed over, or each council seat, or the refusal; nothing
// is launched, reserved, mailed, or written.
func TestSummonDryRunMatchesTheRealDecision(t *testing.T) {
	asked := func(r *SummonRequest) { r.Asked = "review it" }
	dry := func(r *SummonRequest) { r.DryRun = true }
	nothing := func(t *testing.T, e Env, term *fakeTerm, before map[string]string) {
		t.Helper()
		sameState(t, "summon --dry-run", before, stateFiles(t, e))
		if len(term.launches) != 0 || len(term.newTabs) != 0 {
			t.Errorf("launched %+v tabs %v", term.launches, term.newTabs)
		}
		if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
			t.Errorf("pending reservations: %d", n)
		}
	}
	t.Run("falls back past unapproved Codex hooks", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
		unapprovedCodexHook(t, e)
		before := stateFiles(t, e)
		res := summon(t, e, loc, "reviewer", asked, dry)
		if res.Action != "start" || res.Harness != core.Agy || len(res.Passed) != 1 || !strings.HasPrefix(res.Passed[0], "codex: unapproved_hooks") || !res.DryRun {
			t.Fatalf("%+v", res)
		}
		nothing(t, e, term, before)
	})
	t.Run("an explicit Codex with unapproved hooks is refused", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		unapprovedCodexHook(t, e)
		before := stateFiles(t, e)
		res := summon(t, e, loc, "reviewer", asked, dry, func(r *SummonRequest) { r.Harness = "codex" })
		if res.Action != "refused" || !strings.Contains(res.Reason, "unapproved_hooks") {
			t.Fatalf("%+v", res)
		}
		nothing(t, e, term, before)
	})
	t.Run("no installed harness is refused", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		e.LookPath = installedOnly(core.Claude)
		before := stateFiles(t, e)
		res := summon(t, e, loc, "reviewer", asked, dry)
		if res.Action != "refused" || !strings.Contains(res.Reason, "not installed") {
			t.Fatalf("%+v", res)
		}
		nothing(t, e, term, before)
	})
	t.Run("a council plans every seat", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
		live := seed{Handle: "codex-12", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:codex-12", SessionID: "session:s", Role: "reviewer", AgentPID: 42, AdoptedAt: e.Now()}
		add(t, e, live)
		before := stateFiles(t, e)
		res := summon(t, e, loc, "reviewer", councilOf(asked), dry)
		if res.Action != "council" || !res.DryRun || len(res.Seats) < 2 || seatActions(res) == "" {
			t.Fatalf("%+v", res)
		}
		for _, s := range res.Seats {
			if s.Mailed || s.Block != "" {
				t.Errorf("seat %+v was mailed or launched", s)
			}
		}
		if n := len(unread(t, e, "codex-12")); n != 0 {
			t.Errorf("the live seat got %d mails", n)
		}
		nothing(t, e, term, before)
	})
	t.Run("a council with a seat that cannot start is refused", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
		unapprovedCodexHook(t, e)
		before := stateFiles(t, e)
		res := summon(t, e, loc, "reviewer", councilOf(asked), dry)
		if res.Action != "refused" || !strings.Contains(res.Reason, "codex: unapproved_hooks") || len(res.Seats) != 0 {
			t.Fatalf("%+v", res)
		}
		nothing(t, e, term, before)
	})
}

// agent join --dry-run runs the session's crew claim too: a switch to
// another crew while others work under the first is refused as Join would
// refuse it, and the preview records nothing.
func TestPreviewJoinRefusesATakenCrew(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b3c4d", "block:b")
	ctx := context.Background()
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "pair", ""); err != nil {
		t.Fatal(err)
	}
	before := stateFiles(t, e)
	if _, err := e.PreviewJoin(ctx, Location{Block: "block:b"}, "ship", ""); !errors.Is(err, ErrCrewTaken) || !strings.Contains(err.Error(), "pair") {
		t.Fatalf("a taken crew previewed: %v", err)
	}
	if p, err := e.PreviewJoin(ctx, Location{Block: "block:b"}, "pair", "reviewer"); err != nil || p.Crew != "pair" {
		t.Fatalf("joining the session's own crew: %+v %v", p, err)
	}
	sameState(t, "agent join --dry-run", before, stateFiles(t, e))
	if c, _, _ := e.Store.SessionCrew("session:s"); c.Crew != "pair" {
		t.Errorf("the session crew is %q", c.Crew)
	}
}
