package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A departed record is the previous agent's: the block is being reused and
// the one awaited has not been adopted yet.
func TestAwaitDoesNotTakeADepartedRecordForAdoption(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := fixture(t, term)
	recs, err := e.agents()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records: %v %v", recs, err)
	}
	rec := seedOf(recs[0])
	now := e.Now()
	rec.DepartedAt = &now
	if err := putSeed(e.Store, rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	got, err := e.AwaitStartup(ctx, rec.BlockID)
	if err != nil || got.Status != core.StartupTimeout {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// F8 by screen: a Codex pane stopped on "Hooks need review" ends the wait
// at once as trust_screen, and the note names the fix (/hooks, the user's)
// instead of a generic dialog.
func TestAwaitReportsCodexHooksNeedReview(t *testing.T) {
	term := &fakeTerm{state: "idle", box: "Hooks need review  1 hook is new or changed."}
	e := fixture(t, term)
	recs, err := e.agents()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records: %v %v", recs, err)
	}
	rec := seedOf(recs[0])
	now := e.Now()
	rec.DepartedAt = &now
	if err := putSeed(e.Store, rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := e.AwaitStartup(ctx, rec.BlockID)
	if err != nil || got.Status != core.StartupTrust || !strings.Contains(got.Note, "/hooks") || !strings.Contains(got.Note, "Hooks need review") {
		t.Fatalf("got %+v, %v", got, err)
	}
}
