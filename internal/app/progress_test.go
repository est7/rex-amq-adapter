package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func TestRefreshSessionViewProgressLifecycle(t *testing.T) {
	e := viewWorld(t)
	noOpenObligations(e)
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }

	narr := Location{Block: "block:pi-04"}
	worker := Location{Block: "block:claude-02"}
	orch := Location{Block: "block:claude-01"}
	personal := Location{Block: "block:pi-05"}

	in1 := store.ProgressInput{
		GoalID:       "goal-01",
		PlanComplete: false,
		Slices: []store.SliceProgress{
			{ID: "S1", Title: "Core model", Stage: store.StageImplementing, Branch: "feat/s1-core"},
		},
	}

	// Only the session's live narrator may mutate or dry-run view progress.
	for _, loc := range []Location{worker, orch, personal} {
		for _, dry := range []bool{false, true} {
			if _, err := e.RefreshSessionViewWith(ctx, loc, RefreshRequest{Progress: &in1, DryRun: dry}); !errors.Is(err, ErrRefused) {
				t.Errorf("loc=%v dry=%v: err=%v, want ErrRefused", loc, dry, err)
			}
		}
	}
	if _, ok, _ := e.Store.ReadView("session:a"); ok {
		t.Fatal("refused non-narrator refresh wrote view.json")
	}

	// Mutually exclusive progress + clear_progress is refused before I/O.
	if _, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Progress: &in1, ClearProgress: true}); !errors.Is(err, ErrUsage) {
		t.Fatalf("both progress and clear: %v, want ErrUsage", err)
	}

	// Invalid progress is rejected as invalid_input before any write.
	bad := store.ProgressInput{
		PlanComplete: true,
		Slices:       []store.SliceProgress{{ID: "S1", Title: "bad\x07title", Stage: store.StageQueued}},
	}
	if _, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Progress: &bad}); err == nil {
		t.Fatal("invalid progress was accepted")
	} else if f, ok := fault.As(err); !ok || f.Code != fault.InvalidInput {
		t.Fatalf("invalid progress error: %v, want invalid_input", err)
	}

	// 1. Initial write with plan_complete=false -> percent is nil, revision=1.
	r1, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Summary: "S1 started", Progress: &in1})
	if err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if r1.Schema != ViewSchema || r1.DryRun || r1.Narrative == nil || r1.Narrative.Text != "S1 started" {
		t.Fatalf("r1 header: %+v", r1)
	}
	if r1.Progress == nil || r1.Progress.Revision != 1 || r1.Progress.Percent != nil || r1.Progress.TotalSlices != 1 || !r1.Progress.UpdatedAt.Equal(now) {
		t.Fatalf("r1 progress: %+v", r1.Progress)
	}

	// 2. Idempotent replay at a later time keeps revision and updated_at.
	now = now.Add(5 * time.Minute)
	r2, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Progress: &in1})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if r2.Progress == nil || r2.Progress.Revision != 1 || !r2.Progress.UpdatedAt.Equal(r1.Progress.UpdatedAt) {
		t.Fatalf("idempotent replay changed metadata: r1=%+v r2=%+v", r1.Progress, r2.Progress)
	}
	if r2.Narrative == nil || r2.Narrative.Text != "S1 started" {
		t.Fatalf("replay dropped narrative: %+v", r2.Narrative)
	}

	// 3. Summary-only refresh preserves previous progress unchanged.
	now = now.Add(5 * time.Minute)
	r3, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Summary: "still working"})
	if err != nil {
		t.Fatalf("summary-only refresh: %v", err)
	}
	if r3.Narrative == nil || r3.Narrative.Text != "still working" || r3.Progress == nil || r3.Progress.Revision != 1 || len(r3.Progress.Slices) != 1 {
		t.Fatalf("summary-only refresh did not preserve progress: %+v", r3)
	}

	// 4. Dry-run previews derived progress without writing anything to disk.
	now = now.Add(5 * time.Minute)
	in2 := store.ProgressInput{
		GoalID:       "goal-01",
		PlanComplete: true,
		Slices: []store.SliceProgress{
			{ID: "S1", Title: "Core model", Stage: store.StageDone, Branch: "feat/s1-core"},
			{ID: "S2", Title: "CLI list", Stage: store.StageReview, Branch: "feat/s2-list"},
		},
	}
	before := settledState(t, e)
	dryRes, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Summary: "S1 done, S2 in review", Progress: &in2, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run refresh: %v", err)
	}
	if !dryRes.DryRun || dryRes.Progress == nil || dryRes.Progress.Revision != 2 || dryRes.Progress.Percent == nil || *dryRes.Progress.Percent != 88 {
		t.Fatalf("dry-run preview wrong: %+v", dryRes.Progress)
	}
	sameState(t, "epic refresh --dry-run", before, stateFiles(t, e))

	// 5. Real write persists derived progress (100+75)/2 = 87.5 -> 88.
	r4, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Summary: "S1 done, S2 in review", Progress: &in2})
	if err != nil {
		t.Fatalf("real write: %v", err)
	}
	if r4.Progress == nil || r4.Progress.Revision != 2 || r4.Progress.Percent == nil || *r4.Progress.Percent != 88 || r4.Progress.DoneSlices != 1 || r4.Progress.TotalSlices != 2 {
		t.Fatalf("r4 progress: %+v", r4.Progress)
	}

	// 6. Revive's internal refreshView preserves both Narrative and Progress.
	now = now.Add(5 * time.Minute)
	narrRec := recordOf(t, e, "pi-04")
	rRevive, err := e.refreshView(ctx, narrRec, core.SessionKey{Server: fakeServer, SessionID: "session:a"}, RefreshRequest{})
	if err != nil {
		t.Fatalf("revive refresh: %v", err)
	}
	if rRevive.Progress == nil || rRevive.Progress.Revision != 2 || len(rRevive.Progress.Slices) != 2 || rRevive.Narrative == nil || rRevive.Narrative.Text != "S1 done, S2 in review" {
		t.Fatalf("revive refresh lost narrative or progress: %+v", rRevive)
	}

	// 7. Clear-progress dry-run previews nil without mutating disk.
	beforeClear := settledState(t, e)
	clearDry, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{ClearProgress: true, DryRun: true})
	if err != nil || !clearDry.DryRun || clearDry.Progress != nil {
		t.Fatalf("clear dry-run: %+v %v", clearDry, err)
	}
	sameState(t, "epic refresh --clear-progress --dry-run", beforeClear, stateFiles(t, e))

	// 8. Real clear-progress sets Progress to nil and omits "progress" from view.json (schema v1 compat).
	rCleared, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{ClearProgress: true})
	if err != nil || rCleared.Progress != nil {
		t.Fatalf("clear-progress: %+v %v", rCleared.Progress, err)
	}
	rawView, ok, err := e.Store.ReadView("session:a")
	if err != nil || !ok {
		t.Fatalf("read view.json: ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(rawView), `"progress"`) {
		t.Errorf("cleared view.json should omit progress: %s", rawView)
	}
	var decoded SessionView
	if err := json.Unmarshal(rawView, &decoded); err != nil || decoded.Schema != ViewSchema || decoded.Progress != nil {
		t.Fatalf("decoded cleared view: %+v %v", decoded, err)
	}

	// 9. Second clear-progress is idempotent.
	if rCleared2, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{ClearProgress: true}); err != nil || rCleared2.Progress != nil {
		t.Fatalf("idempotent second clear: %+v %v", rCleared2.Progress, err)
	}
}
