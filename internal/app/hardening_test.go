package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func TestSummonDashLeadingBriefIsPrompt(t *testing.T) {
	for _, kind := range []core.Kind{core.Claude, core.Codex, core.Pi, core.Agy} {
		t.Run(string(kind), func(t *testing.T) {
			e, term, loc := summonWorld(t)
			res := summon(t, e, loc, "writer", func(r *SummonRequest) {
				r.Asked = "start one"
				r.Harness = string(kind)
				r.Body = "--help=review this diff"
			})
			if res.Action != "start" || len(term.launches) != 1 {
				t.Fatalf("launch: %+v", res)
			}
			cmd := launchProgram(term.launches[0])
			preceding := cmd[len(cmd)-2]
			want := "--"
			if kind == core.Agy {
				want = "-i"
			}
			if preceding != want || !strings.HasPrefix(cmd[len(cmd)-1], "--help=review this diff") {
				t.Fatalf("brief parsed as option: %q", cmd)
			}
		})
	}
}

func TestSummonGrantMustBeRegularFile(t *testing.T) {
	for _, asked := range []string{"", "start one"} {
		t.Run(fmt.Sprintf("asked=%t", asked != ""), func(t *testing.T) {
			e, term, loc := summonWorld(t)
			res := summon(t, e, loc, "writer", func(r *SummonRequest) { r.Grant = t.TempDir(); r.Asked = asked })
			if res.Action != "refused" || len(term.launches) != 0 {
				t.Fatalf("directory authorized launch: %+v", res)
			}
		})
	}
}

func TestStartCommandRejectsUnknownKind(t *testing.T) {
	if got := startCommand(core.Kind("unlisted"), "brief"); len(got) != 0 {
		t.Fatalf("unknown kind launched %q", got)
	}
}

func TestAdoptRetainsSummonOnRecordFailure(t *testing.T) {
	e, term, _ := summonWorld(t)
	const child = "block:child"
	term.windows[child] = "window:1"
	const token = "retry-lineage"
	sm := store.Summon{Parent: "claude-01", Depth: 2, For: "implementer", Session: "session:s", At: e.Now()}
	if err := e.Store.PutSummon(token, sm); err != nil {
		t.Fatal(err)
	}
	// A directory at the target makes the atomic record rename fail, even as root.
	bad := filepath.Join(e.Paths.Home, "records", "pi-01.json")
	e.Rex = &failRecordTerm{fakeTerm: term, beforeRecord: func() {
		if err := os.MkdirAll(bad, 0700); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := e.Adopt(context.Background(), Location{Block: child}, core.Pi, HookInput{AgentPID: 12345, SessionID: "retry-child", SummonToken: token}); err == nil {
		t.Fatal("record write unexpectedly succeeded")
	}
	pending, err := e.Store.PendingSummons("session:s", e.Now())
	if err != nil || pending != 1 {
		t.Fatalf("failed record consumed reservation: pending=%d err=%v", pending, err)
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	a, err := e.Adopt(context.Background(), Location{Block: child}, core.Pi, HookInput{AgentPID: 12345, SessionID: "retry-child", SummonToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if a.Record.Parent != sm.Parent || a.Record.Depth != sm.Depth || a.Record.SummonedFor != sm.For {
		t.Fatalf("retry lost lineage: %+v", a.Record)
	}
	if n, err := e.Store.PendingSummons("session:s", e.Now()); err != nil || n != 0 {
		t.Fatalf("successful commit retained reservation: %d %v", n, err)
	}
}

type failRecordTerm struct {
	*fakeTerm
	beforeRecord func()
}

func (f *failRecordTerm) Foreground(ctx context.Context, block string) (rex.Process, bool, error) {
	out, ok, err := f.fakeTerm.Foreground(ctx, block)
	if f.beforeRecord != nil {
		fn := f.beforeRecord
		f.beforeRecord = nil
		fn()
	}
	return out, ok, err
}

type adoptionBetweenLayouts struct {
	*fakeTerm
	afterSnapshot func()
}

func (f *adoptionBetweenLayouts) InspectSession(ctx context.Context, id string) (rex.Session, error) {
	out, err := f.fakeTerm.InspectSession(ctx, id)
	if f.afterSnapshot != nil {
		fn := f.afterSnapshot
		f.afterSnapshot = nil
		fn()
	}
	return out, err
}

func TestSummonReloadsLayoutBeforeCountingCap(t *testing.T) {
	e, term, loc := summonWorld(t)
	for i := 2; i <= 5; i++ {
		add(t, e, store.Record{Handle: core.Handle(fmt.Sprintf("pi-%02d", i)), Kind: core.Pi, Server: "unix:///fake.sock", BlockID: fmt.Sprintf("block:%d", i), SessionID: "session:s", WindowID: "window:1", AdoptedAt: e.Now()})
	}
	const token = "last-place"
	if err := e.Store.PutSummon(token, store.Summon{Parent: "claude-01", Depth: 1, For: "editor", Session: "session:s", At: e.Now()}); err != nil {
		t.Fatal(err)
	}
	e.Rex = &adoptionBetweenLayouts{fakeTerm: term, afterSnapshot: func() {
		unlock, err := e.Store.LockLifecycle(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		term.mu.Lock()
		term.windows["block:six"] = "window:1"
		term.mu.Unlock()
		if err := e.Store.PutRecord(store.Record{Handle: "pi-06", Kind: core.Pi, Server: "unix:///fake.sock", BlockID: "block:six", SessionID: "session:s", WindowID: "window:1", AdoptedAt: e.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.DeleteSummon(token); err != nil {
			t.Fatal(err)
		}
	}}
	res := summon(t, e, loc, "writer", func(r *SummonRequest) { r.Asked = "start one" })
	if res.Action != "refused" || len(term.launches) != 0 {
		t.Fatalf("six live agents exceeded cap: %+v", res)
	}
}
