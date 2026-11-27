package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Locking view writes must leave zero new files or lock directories on disk.
	if ents, err := os.ReadDir(e.Paths.SessionDir("session:a")); err != nil || len(ents) != 1 || ents[0].Name() != "view.json" {
		t.Fatalf("unexpected files in session dir after refresh: %v %v", ents, err)
	}
	if _, err := os.Stat(filepath.Join(e.Paths.Home, "views")); err == nil {
		t.Fatal("refresh created an extra views lock directory on disk")
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

// Concurrent refreshes of the same session serialize their view.json
// read-derive-write section so every distinct update gets a unique,
// monotonically increasing revision and the final stored revision never
// moves backward.
func TestRefreshSessionViewProgressConcurrentRevisionsMonotonic(t *testing.T) {
	e := viewWorld(t)
	noOpenObligations(e)
	ctx := context.Background()
	narr := Location{Block: "block:pi-04"}

	seed := store.ProgressInput{
		PlanComplete: true,
		Slices:       []store.SliceProgress{{ID: "S1", Title: "seed", Stage: store.StageQueued}},
	}
	if r, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Progress: &seed}); err != nil || r.Progress == nil || r.Progress.Revision != 1 {
		t.Fatalf("seed refresh: %+v %v", r.Progress, err)
	}

	const n = 20
	revs := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			in := store.ProgressInput{
				PlanComplete: true,
				Slices:       []store.SliceProgress{{ID: "S1", Title: fmt.Sprintf("update-%02d", idx), Stage: store.StageImplementing}},
			}
			r, err := e.RefreshSessionViewWith(ctx, narr, RefreshRequest{Progress: &in})
			errs[idx] = err
			if err == nil && r.Progress != nil {
				revs[idx] = r.Progress.Revision
			}
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int]bool, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v", i, errs[i])
		}
		if revs[i] < 2 || revs[i] > n+1 {
			t.Fatalf("worker %d got out-of-range revision %d", i, revs[i])
		}
		if seen[revs[i]] {
			t.Fatalf("duplicate revision %d across concurrent refreshes: %v", revs[i], revs)
		}
		seen[revs[i]] = true
	}

	raw, ok, err := e.Store.ReadView("session:a")
	if err != nil || !ok {
		t.Fatalf("read final view: ok=%v err=%v", ok, err)
	}
	var final SessionView
	if err := json.Unmarshal(raw, &final); err != nil {
		t.Fatal(err)
	}
	if final.Progress == nil || final.Progress.Revision != n+1 {
		t.Fatalf("final stored revision = %+v, want %d", final.Progress, n+1)
	}
}

// epic list projects goal_path, narrative, and progress (null when unrecorded)
// onto CockpitEpic without N+1 fan-out; --include-closed scans existing closed
// lifecycle.json + archived view.json within --closed-within while respecting
// Hub and --personal boundaries and writing zero files to disk.
func TestCockpitEpicListProgressAndIncludeClosed(t *testing.T) {
	e, hubA := cockpitWorld(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }

	// Give session:a a goal grant and recorded progress (65%).
	if _, err := e.Store.PutGrant(store.Grant{
		SessionID:      "session:a",
		RelayedBy:      "pi-05",
		RelayedAt:      now.Add(-2 * time.Hour),
		Contract:       "/tmp/goals/01-auth.md",
		ContractSHA256: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	inA := store.ProgressInput{
		GoalID:       "goal-01",
		PlanComplete: true,
		Slices: []store.SliceProgress{
			{ID: "S1", Title: "Token model", Stage: store.StageDone},
			{ID: "S2", Title: "Refresh hook", Stage: store.StageImplementing},
		},
	}
	if _, err := e.RefreshSessionViewWith(ctx, Location{Block: "block:pi-04"}, RefreshRequest{Summary: "S1 done, S2 implementing", Progress: &inA}); err != nil {
		t.Fatal(err)
	}

	// Seed closed Epics in Hub A (2h ago, 48h ago), Hub B (1h ago), and unlinked (1h ago).
	hubAResolved, err := resolvedDir(hubA)
	if err != nil {
		t.Fatal(err)
	}
	hubB := t.TempDir()
	hubBResolved, err := resolvedDir(hubB)
	if err != nil {
		t.Fatal(err)
	}
	hubAKey := core.NewHubKey(fakeServer, hubAResolved)
	hubBKey := core.NewHubKey(fakeServer, hubBResolved)
	// Record session:p's Hub policy so pi-05 matches Hub A links strictly too.
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: "session:p", Server: fakeServer, HubKey: hubAKey}); err != nil {
		t.Fatal(err)
	}

	closed2h := now.Add(-2 * time.Hour)
	closed48h := now.Add(-48 * time.Hour)
	closed1h := now.Add(-1 * time.Hour)
	for _, lc := range []store.EpicLifecycle{
		{SessionID: "session:closed-recent", Server: fakeServer, Hub: hubAKey, Name: "shipped-today", Dir: "/repo/x", PersonalHandle: "pi-05", PersonalSessionID: "session:p", State: store.EpicClosed, ClosedAt: &closed2h, UpdatedAt: closed2h},
		{SessionID: "session:closed-old", Server: fakeServer, Hub: hubAKey, Name: "shipped-2d-ago", Dir: "/repo/x", PersonalHandle: "pi-05", PersonalSessionID: "session:p", State: store.EpicClosed, ClosedAt: &closed48h, UpdatedAt: closed48h},
		{SessionID: "session:closed-other-hub", Server: fakeServer, Hub: hubBKey, Name: "other-hub", Dir: "/repo/y", PersonalHandle: "pi-06", PersonalSessionID: "session:q", State: store.EpicClosed, ClosedAt: &closed1h, UpdatedAt: closed1h},
		{SessionID: "session:closed-unlinked", Server: fakeServer, Name: "orphan", State: store.EpicClosed, ClosedAt: &closed1h, UpdatedAt: closed1h},
	} {
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			t.Fatal(err)
		}
	}
	pct100 := 100
	if err := e.Store.PutView("session:closed-recent", SessionView{
		Schema:    ViewSchema,
		Session:   ViewSession{Server: fakeServer, SessionID: "session:closed-recent", Epic: "shipped-today"},
		Narrator:  "pi-04",
		Narrative: &ViewNarrative{Text: "All 2 slices landed", At: closed2h},
		GoalPath:  "/tmp/goals/00-shipped.md",
		Progress: &store.EpicProgress{
			PlanComplete: true,
			Slices:       []store.SliceProgress{{ID: "S1", Title: "Done", Stage: store.StageDone}},
			TotalSlices:  1,
			DoneSlices:   1,
			Counts:       store.StageCounts{Done: 1},
			Percent:      &pct100,
			Revision:     3,
			UpdatedAt:    closed2h,
		},
	}); err != nil {
		t.Fatal(err)
	}

	before := settledState(t, e)

	// 1. Default epic list (IncludeClosed=false): live Epics only (session:a, session:b),
	// with session:a carrying progress=65, narrative, goal_path and session:b carrying explicit null progress.
	rawDefault, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: hubA})
	if err != nil {
		t.Fatalf("default epic list: %v", err)
	}
	var vDefault CockpitView
	if err := json.Unmarshal(rawDefault, &vDefault); err != nil {
		t.Fatal(err)
	}
	if len(vDefault.Epics) != 2 {
		t.Fatalf("default list returned %d epics, want 2: %+v", len(vDefault.Epics), vDefault.Epics)
	}
	var rawMap struct {
		Epics []map[string]any `json:"epics"`
	}
	if err := json.Unmarshal(rawDefault, &rawMap); err != nil {
		t.Fatal(err)
	}
	for _, ep := range vDefault.Epics {
		switch ep.SessionID {
		case "session:a":
			if ep.GoalPath != "/tmp/goals/01-auth.md" || ep.Narrative == nil || ep.Narrative.Text != "S1 done, S2 implementing" ||
				ep.Progress == nil || ep.Progress.Percent == nil || *ep.Progress.Percent != 65 || ep.Progress.DoneSlices != 1 || ep.Progress.TotalSlices != 2 {
				t.Errorf("session:a projection: %+v", ep)
			}
		case "session:b":
			if ep.Progress != nil {
				t.Errorf("session:b without progress got non-nil %+v", ep.Progress)
			}
		default:
			t.Errorf("unexpected epic in default list: %s", ep.SessionID)
		}
	}
	// Verify explicit `"progress": null` key on session:b in raw JSON (never fabricated 0%).
	foundNull := false
	for _, m := range rawMap.Epics {
		if m["session_id"] == "session:b" {
			val, present := m["progress"]
			if !present || val != nil {
				t.Errorf("session:b raw progress field present=%v val=%v, want explicit null", present, val)
			}
			foundNull = true
		}
	}
	if !foundNull {
		t.Fatal("session:b not found in raw JSON")
	}

	// 2. --closed-within without --include-closed is ErrUsage.
	if _, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: hubA, ClosedWithin: time.Hour}); !errors.Is(err, ErrUsage) {
		t.Errorf("ClosedWithin without IncludeClosed: %v, want ErrUsage", err)
	}

	// 3. --include-closed (default 24h) returns live + closed-recent (100%), excludes 48h-old, other-hub, and unlinked.
	for name, envMap := range map[string]map[string]string{
		"outside_hub":    nil,
		"personal_agent": {"REX_BLOCK": "block:pi-05"},
	} {
		req := CockpitViewRequest{IncludeClosed: true}
		if envMap == nil {
			req.Hub = hubA
		}
		rawClosed, err := e.ReadCockpitView(ctx, cockpitEnv(envMap), req)
		if err != nil {
			t.Fatalf("%s include-closed: %v", name, err)
		}
		var vClosed CockpitView
		if err := json.Unmarshal(rawClosed, &vClosed); err != nil {
			t.Fatal(err)
		}
		var ids []string
		var recent CockpitEpic
		for _, ep := range vClosed.Epics {
			ids = append(ids, ep.SessionID)
			if ep.SessionID == "session:closed-recent" {
				recent = ep
			}
		}
		if len(ids) != 3 || recent.SessionID == "" {
			t.Fatalf("%s include-closed ids=%v, want [session:a session:b session:closed-recent]", name, ids)
		}
		if recent.Lifecycle == nil || recent.Lifecycle.State != store.EpicClosed ||
			recent.Progress == nil || recent.Progress.Percent == nil || *recent.Progress.Percent != 100 ||
			recent.Narrative == nil || recent.Narrative.Text != "All 2 slices landed" || recent.GoalPath != "/tmp/goals/00-shipped.md" {
			t.Errorf("%s closed-recent projection: %+v", name, recent)
		}
	}

	// 4. --include-closed --closed-within 72h also includes session:closed-old (48h ago), still excludes other-hub/unlinked.
	raw72, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: hubA, IncludeClosed: true, ClosedWithin: 72 * time.Hour})
	if err != nil {
		t.Fatalf("include-closed 72h: %v", err)
	}
	var v72 CockpitView
	if err := json.Unmarshal(raw72, &v72); err != nil {
		t.Fatal(err)
	}
	if len(v72.Epics) != 4 || v72.Epics[2].SessionID != "session:closed-recent" || v72.Epics[3].SessionID != "session:closed-old" {
		t.Fatalf("include-closed 72h order/items: %+v", v72.Epics)
	}

	// 5. Pure read: zero new files or mutations anywhere in state dir.
	sameState(t, "epic list --include-closed", before, stateFiles(t, e))
}
