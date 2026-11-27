package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func TestParseProgressInputValidAndNormalized(t *testing.T) {
	raw := []byte(`{
		"goal_id": "goal-01.v1_a",
		"plan_complete": true,
		"slices": [
			{"id": "S1", "title": "  Core model  ", "stage": "done", "branch": " feat/s1-core ", "note": " merged "},
			{"id": "S2", "title": "中文标题测试", "stage": "implementing", "note": "进行中"}
		]
	}`)
	got, err := ParseProgressInput(raw)
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if got.GoalID != "goal-01.v1_a" || !got.PlanComplete || len(got.Slices) != 2 {
		t.Fatalf("unexpected parsed input: %+v", got)
	}
	if got.Slices[0].Title != "Core model" || got.Slices[0].Branch != "feat/s1-core" || got.Slices[0].Note != "merged" {
		t.Errorf("normalization failed: %+v", got.Slices[0])
	}
	if got.Slices[1].Title != "中文标题测试" || got.Slices[1].Note != "进行中" {
		t.Errorf("utf-8 fields corrupted: %+v", got.Slices[1])
	}

	// Empty slices array is allowed (0..=32).
	empty, err := ParseProgressInput([]byte(`{"plan_complete":false,"slices":[]}`))
	if err != nil {
		t.Fatalf("empty slices rejected: %v", err)
	}
	if empty.Slices == nil || len(empty.Slices) != 0 {
		t.Fatalf("empty slices should be non-nil empty slice: %#v", empty.Slices)
	}

	// Exactly 32 slices and boundary rune lengths (120/128/160) pass.
	slices32 := make([]SliceProgress, MaxProgressSlices)
	for i := range slices32 {
		slices32[i] = SliceProgress{
			ID:     "S" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Title:  strings.Repeat("中", MaxSliceTitle),
			Stage:  StageQueued,
			Branch: strings.Repeat("b", MaxSliceBranch),
			Note:   strings.Repeat("注", MaxSliceNote),
		}
	}
	b, _ := json.Marshal(ProgressInput{GoalID: strings.Repeat("g", 32), PlanComplete: true, Slices: slices32})
	if _, err := ParseProgressInput(b); err != nil {
		t.Fatalf("32 boundary slices rejected: %v", err)
	}
}

func TestParseProgressInputRejectsInvalid(t *testing.T) {
	tooMany := make([]SliceProgress, MaxProgressSlices+1)
	for i := range tooMany {
		tooMany[i] = SliceProgress{ID: "S" + strings.Repeat("a", i%10) + string(rune('0'+i/10)) + string(rune('0'+i%10)), Title: "ok", Stage: StageQueued}
	}
	tooManyJSON, _ := json.Marshal(ProgressInput{PlanComplete: true, Slices: tooMany})

	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"array", "[]"},
		{"null", "null"},
		{"trailing_object", `{"plan_complete":false,"slices":[]} {"plan_complete":true,"slices":[]}`},
		{"missing_slices", `{"plan_complete":true}`},
		{"null_slices", `{"plan_complete":true,"slices":null}`},
		{"missing_plan_complete", `{"slices":[]}`},
		{"null_plan_complete", `{"plan_complete":null,"slices":[{"id":"S1","title":"ok","stage":"done"}]}`},
		{"unknown_top_field_percent", `{"plan_complete":true,"percent":50,"slices":[]}`},
		{"unknown_top_field_done_slices", `{"plan_complete":true,"done_slices":1,"slices":[]}`},
		{"unknown_top_field_revision", `{"plan_complete":true,"revision":2,"slices":[]}`},
		{"unknown_slice_field", `{"plan_complete":true,"slices":[{"id":"S1","title":"a","stage":"queued","extra":1}]}`},
		{"bad_goal_id_leading_dot", `{"goal_id":".bad","plan_complete":false,"slices":[]}`},
		{"bad_goal_id_slash", `{"goal_id":"a/b","plan_complete":false,"slices":[]}`},
		{"bad_goal_id_too_long", `{"goal_id":"` + strings.Repeat("a", 33) + `","plan_complete":false,"slices":[]}`},
		{"too_many_slices", string(tooManyJSON)},
		{"duplicate_slice_id", `{"plan_complete":true,"slices":[{"id":"S1","title":"a","stage":"queued"},{"id":"S1","title":"b","stage":"done"}]}`},
		{"empty_slice_id", `{"plan_complete":true,"slices":[{"id":"","title":"a","stage":"queued"}]}`},
		{"bad_slice_id_traversal", `{"plan_complete":true,"slices":[{"id":"../S1","title":"a","stage":"queued"}]}`},
		{"empty_title", `{"plan_complete":true,"slices":[{"id":"S1","title":"","stage":"queued"}]}`},
		{"blank_title", `{"plan_complete":true,"slices":[{"id":"S1","title":"   ","stage":"queued"}]}`},
		{"title_too_long", `{"plan_complete":true,"slices":[{"id":"S1","title":"` + strings.Repeat("x", MaxSliceTitle+1) + `","stage":"queued"}]}`},
		{"branch_too_long", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","branch":"` + strings.Repeat("b", MaxSliceBranch+1) + `","stage":"queued"}]}`},
		{"note_too_long", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","note":"` + strings.Repeat("n", MaxSliceNote+1) + `","stage":"queued"}]}`},
		{"unknown_stage", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","stage":"in_progress"}]}`},
		{"uppercase_stage", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","stage":"DONE"}]}`},
		{"title_newline", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\nb","stage":"queued"}]}`},
		{"title_ansi", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u001b[31mb","stage":"queued"}]}`},
		{"title_bel", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u0007b","stage":"queued"}]}`},
		{"title_c1", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u0085b","stage":"queued"}]}`},
		{"title_bidi_override", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u202eb","stage":"queued"}]}`},
		{"title_bidi_isolate", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u2066b","stage":"queued"}]}`},
		{"title_zero_width", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u200bb","stage":"queued"}]}`},
		{"title_line_sep", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u2028b","stage":"queued"}]}`},
		{"title_para_sep", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u2029b","stage":"queued"}]}`},
		{"branch_newline", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","branch":"a\nb","stage":"queued"}]}`},
		{"note_bidi", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","note":"a\u202eb","stage":"queued"}]}`},
		{"note_bom", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","note":"a\ufeffb","stage":"queued"}]}`},
	}
	for _, tc := range cases {
		if got, err := ParseProgressInput([]byte(tc.raw)); err == nil {
			t.Errorf("%s: accepted as %+v, want error", tc.name, got)
		}
	}
	// Raw invalid UTF-8 bytes are rejected before JSON decoding can replace them with U+FFFD.
	rawInvalidUTF8 := append([]byte(`{"plan_complete":true,"slices":[{"id":"S1","title":"bad`), 0xff)
	rawInvalidUTF8 = append(rawInvalidUTF8, []byte(`title","stage":"queued"}]}`)...)
	if _, err := ParseProgressInput(rawInvalidUTF8); err == nil {
		t.Error("raw invalid UTF-8 JSON was accepted")
	}
	// Invalid UTF-8 rejected directly via NormalizeProgressInput.
	if _, err := NormalizeProgressInput(ProgressInput{
		PlanComplete: true,
		Slices:       []SliceProgress{{ID: "S1", Title: "bad\xffutf8", Stage: StageQueued}},
	}); err == nil {
		t.Error("invalid UTF-8 title was accepted")
	}
}

func TestDeriveProgressWeightsRoundingAndIdempotency(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)

	// Each single stage has its exact weight.
	for _, tc := range []struct {
		stage SliceStage
		want  int
		done  int
	}{
		{StageQueued, 0, 0},
		{StageImplementing, 30, 0},
		{StageReview, 75, 0},
		{StageRework, 85, 0},
		{StageApproved, 95, 0},
		{StageDone, 100, 1},
		{StageBlocked, 0, 0},
	} {
		p, changed, err := DeriveProgress(ProgressInput{
			PlanComplete: true,
			Slices:       []SliceProgress{{ID: "S1", Title: "one", Stage: tc.stage}},
		}, nil, t0)
		if err != nil {
			t.Fatalf("%s: %v", tc.stage, err)
		}
		if !changed || p.Percent == nil || *p.Percent != tc.want || p.DoneSlices != tc.done || p.TotalSlices != 1 || p.Revision != 1 {
			t.Errorf("%s: got percent=%v done=%d total=%d rev=%d changed=%v", tc.stage, p.Percent, p.DoneSlices, p.TotalSlices, p.Revision, changed)
		}
	}

	// plan_complete=false yields percent=nil even when all slices are done.
	pIncomplete, _, err := DeriveProgress(ProgressInput{
		PlanComplete: false,
		Slices:       []SliceProgress{{ID: "S1", Title: "one", Stage: StageDone}},
	}, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if pIncomplete.Percent != nil || pIncomplete.DoneSlices != 1 || pIncomplete.TotalSlices != 1 {
		t.Fatalf("plan_complete=false: %+v", pIncomplete)
	}
	if b, _ := json.Marshal(pIncomplete); !strings.Contains(string(b), `"percent":null`) {
		t.Errorf("plan_complete=false JSON missing explicit null percent: %s", b)
	}

	// plan_complete=true with 0 slices also yields percent=nil.
	pEmpty, _, err := DeriveProgress(ProgressInput{PlanComplete: true, Slices: []SliceProgress{}}, nil, t0)
	if err != nil || pEmpty.Percent != nil || pEmpty.TotalSlices != 0 {
		t.Fatalf("empty slices: %+v %v", pEmpty, err)
	}

	// Half-up rounding and blocked exclusion from done_slices:
	// review(75) + queued(0) = 75/2 = 37.5 -> 38.
	pRound, _, err := DeriveProgress(ProgressInput{
		PlanComplete: true,
		Slices: []SliceProgress{
			{ID: "S1", Title: "one", Stage: StageReview},
			{ID: "S2", Title: "two", Stage: StageQueued},
		},
	}, nil, t0)
	if err != nil || pRound.Percent == nil || *pRound.Percent != 38 {
		t.Fatalf("rounding 37.5: got %+v %v, want 38", pRound, err)
	}

	// done(100) + blocked(0) = 50%, done_slices=1, counts.blocked=1.
	pBlocked, _, err := DeriveProgress(ProgressInput{
		PlanComplete: true,
		Slices: []SliceProgress{
			{ID: "S1", Title: "one", Stage: StageDone},
			{ID: "S2", Title: "two", Stage: StageBlocked, Note: "waiting on API"},
		},
	}, nil, t0)
	if err != nil || pBlocked.Percent == nil || *pBlocked.Percent != 50 || pBlocked.DoneSlices != 1 || pBlocked.Counts.Done != 1 || pBlocked.Counts.Blocked != 1 {
		t.Fatalf("blocked mix: %+v %v", pBlocked, err)
	}

	// Idempotent replay with different wall clock keeps revision and updated_at.
	t1 := t0.Add(5 * time.Minute)
	replay, changed, err := DeriveProgress(ProgressInput{
		PlanComplete: true,
		Slices: []SliceProgress{
			{ID: "S1", Title: " one ", Stage: StageDone},
			{ID: "S2", Title: "two", Stage: StageBlocked, Note: " waiting on API "},
		},
	}, &pBlocked, t1)
	if err != nil {
		t.Fatal(err)
	}
	if changed || replay.Revision != pBlocked.Revision || !replay.UpdatedAt.Equal(t0) {
		t.Errorf("idempotent replay mutated metadata: changed=%v rev=%d->%d at=%v->%v", changed, pBlocked.Revision, replay.Revision, t0, replay.UpdatedAt)
	}

	// Changing a stage bumps revision and updates updated_at.
	t2 := t0.Add(10 * time.Minute)
	next, changed, err := DeriveProgress(ProgressInput{
		PlanComplete: true,
		Slices: []SliceProgress{
			{ID: "S1", Title: "one", Stage: StageDone},
			{ID: "S2", Title: "two", Stage: StageApproved},
		},
	}, &replay, t2)
	if err != nil {
		t.Fatal(err)
	}
	// (100 + 95) / 2 = 97.5 -> 98.
	if !changed || next.Revision != 2 || !next.UpdatedAt.Equal(t2) || next.Percent == nil || *next.Percent != 98 {
		t.Errorf("stage update: changed=%v rev=%d at=%v pct=%v", changed, next.Revision, next.UpdatedAt, next.Percent)
	}

	// Dropping a slice declaratively updates total_slices and bumps revision.
	t3 := t0.Add(15 * time.Minute)
	dropped, changed, err := DeriveProgress(ProgressInput{
		PlanComplete: true,
		Slices:       []SliceProgress{{ID: "S1", Title: "one", Stage: StageDone}},
	}, &next, t3)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || dropped.Revision != 3 || dropped.TotalSlices != 1 || dropped.Percent == nil || *dropped.Percent != 100 {
		t.Errorf("drop slice: %+v changed=%v", dropped, changed)
	}
}
