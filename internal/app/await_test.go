package app

import (
	"context"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A departed record is the previous agent's: the block is being reused and
// the one awaited has not been adopted yet.
func TestAwaitDoesNotTakeADepartedRecordForAdoption(t *testing.T) {
	term := &fakeTerm{state: "idle"}
	e := fixture(t, term)
	recs, err := e.Store.Records()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records: %v %v", recs, err)
	}
	rec := recs[0]
	now := e.Now()
	rec.DepartedAt = &now
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	got, err := e.AwaitStartup(ctx, rec.BlockID)
	if err != nil || got.Status != core.StartupTimeout {
		t.Fatalf("got %+v, %v", got, err)
	}
}
