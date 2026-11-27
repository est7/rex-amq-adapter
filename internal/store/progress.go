package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// SliceStage is the closed set of stages a Slice passes through.
type SliceStage string

const (
	StageQueued       SliceStage = "queued"
	StageImplementing SliceStage = "implementing"
	StageReview       SliceStage = "review"
	StageRework       SliceStage = "rework"
	StageApproved     SliceStage = "approved"
	StageDone         SliceStage = "done"
	StageBlocked      SliceStage = "blocked"
)

// Bounds on declarative Slice progress snapshots.
const (
	MaxProgressSlices = 32
	MaxSliceTitle     = 120
	MaxSliceBranch    = 128
	MaxSliceNote      = 160
)

var progressIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// stageWeight maps each valid stage to its deterministic progress weight.
// Blocked carries weight 0 and is excluded from done_slices so a blocked
// Slice never inflates completion toward 100%.
var stageWeight = map[SliceStage]int{
	StageQueued:       0,
	StageImplementing: 30,
	StageReview:       75,
	StageRework:       85,
	StageApproved:     95,
	StageDone:         100,
	StageBlocked:      0,
}

const validStagesText = "queued, implementing, review, rework, approved, done, blocked"

// ProgressInput is the declarative Slice progress payload a narrator passes to
// `epic refresh`. Derived fields (percent, total_slices, done_slices, counts,
// revision, updated_at) are computed solely in Go and rejected if supplied.
type ProgressInput struct {
	GoalID       string          `json:"goal_id,omitempty"`
	PlanComplete bool            `json:"plan_complete"`
	Slices       []SliceProgress `json:"slices"`
}

// SliceProgress is one Slice entry in a progress snapshot.
type SliceProgress struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Stage  SliceStage `json:"stage"`
	Branch string     `json:"branch,omitempty"`
	Note   string     `json:"note,omitempty"`
}

// StageCounts holds the per-stage counts of a progress snapshot.
type StageCounts struct {
	Queued       int `json:"queued"`
	Implementing int `json:"implementing"`
	Review       int `json:"review"`
	Rework       int `json:"rework"`
	Approved     int `json:"approved"`
	Done         int `json:"done"`
	Blocked      int `json:"blocked"`
}

// EpicProgress is the validated, derived Slice progress snapshot stored in
// sessions/<session_id>/view.json (under schema v1, optional omitempty).
type EpicProgress struct {
	GoalID       string          `json:"goal_id,omitempty"`
	PlanComplete bool            `json:"plan_complete"`
	Slices       []SliceProgress `json:"slices"`
	TotalSlices  int             `json:"total_slices"`
	DoneSlices   int             `json:"done_slices"`
	Counts       StageCounts     `json:"counts"`
	Percent      *int            `json:"percent"`
	Revision     int             `json:"revision"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// ParseProgressInput strictly decodes raw JSON into a validated, normalized
// ProgressInput with DisallowUnknownFields() before any disk I/O.
func ParseProgressInput(raw []byte) (ProgressInput, error) {
	if !utf8.Valid(raw) {
		return ProgressInput{}, errors.New("progress JSON is not valid UTF-8")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ProgressInput{}, errors.New("progress JSON is empty")
	}
	if trimmed[0] != '{' {
		return ProgressInput{}, errors.New("progress must be a JSON object")
	}
	var wire struct {
		GoalID       string          `json:"goal_id,omitempty"`
		PlanComplete *bool           `json:"plan_complete"`
		Slices       []SliceProgress `json:"slices"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return ProgressInput{}, fmt.Errorf("decode progress: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ProgressInput{}, errors.New("progress must be exactly one JSON object")
	}
	if wire.PlanComplete == nil {
		return ProgressInput{}, errors.New("plan_complete is required and must be a boolean")
	}
	return NormalizeProgressInput(ProgressInput{
		GoalID:       wire.GoalID,
		PlanComplete: *wire.PlanComplete,
		Slices:       wire.Slices,
	})
}

// NormalizeProgressInput validates every field of in and returns a normalized
// copy (trimmed single-line text, non-nil slices slice).
func NormalizeProgressInput(in ProgressInput) (ProgressInput, error) {
	if in.GoalID != "" {
		if err := core.LineClean(in.GoalID); err != nil {
			return ProgressInput{}, fmt.Errorf("goal_id %v", err)
		}
		if !progressIDRe.MatchString(in.GoalID) {
			return ProgressInput{}, fmt.Errorf("goal_id %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$", in.GoalID)
		}
	}
	if in.Slices == nil {
		return ProgressInput{}, errors.New("slices is required and must be a JSON array")
	}
	if len(in.Slices) > MaxProgressSlices {
		return ProgressInput{}, fmt.Errorf("slices has %d items, max %d", len(in.Slices), MaxProgressSlices)
	}
	norm := make([]SliceProgress, len(in.Slices))
	seen := make(map[string]bool, len(in.Slices))
	for i, s := range in.Slices {
		if err := core.LineClean(s.ID); err != nil {
			return ProgressInput{}, fmt.Errorf("slices[%d].id %v", i, err)
		}
		if !progressIDRe.MatchString(s.ID) {
			return ProgressInput{}, fmt.Errorf("slices[%d].id %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$", i, s.ID)
		}
		if seen[s.ID] {
			return ProgressInput{}, fmt.Errorf("duplicate slice id %q", s.ID)
		}
		seen[s.ID] = true

		if err := core.LineClean(s.Title); err != nil {
			return ProgressInput{}, fmt.Errorf("slices[%d].title %v", i, err)
		}
		title := strings.TrimSpace(s.Title)
		if title == "" {
			return ProgressInput{}, fmt.Errorf("slices[%d].title is required", i)
		}
		if utf8.RuneCountInString(title) > MaxSliceTitle {
			return ProgressInput{}, fmt.Errorf("slices[%d].title is longer than %d characters", i, MaxSliceTitle)
		}

		if err := core.LineClean(s.Branch); err != nil {
			return ProgressInput{}, fmt.Errorf("slices[%d].branch %v", i, err)
		}
		branch := strings.TrimSpace(s.Branch)
		if utf8.RuneCountInString(branch) > MaxSliceBranch {
			return ProgressInput{}, fmt.Errorf("slices[%d].branch is longer than %d characters", i, MaxSliceBranch)
		}

		if err := core.LineClean(s.Note); err != nil {
			return ProgressInput{}, fmt.Errorf("slices[%d].note %v", i, err)
		}
		note := strings.TrimSpace(s.Note)
		if utf8.RuneCountInString(note) > MaxSliceNote {
			return ProgressInput{}, fmt.Errorf("slices[%d].note is longer than %d characters", i, MaxSliceNote)
		}

		if _, ok := stageWeight[s.Stage]; !ok {
			return ProgressInput{}, fmt.Errorf("slices[%d].stage %q is not one of %s", i, s.Stage, validStagesText)
		}
		norm[i] = SliceProgress{
			ID:     s.ID,
			Title:  title,
			Stage:  s.Stage,
			Branch: branch,
			Note:   note,
		}
	}
	return ProgressInput{
		GoalID:       in.GoalID,
		PlanComplete: in.PlanComplete,
		Slices:       norm,
	}, nil
}

// DeriveProgress validates in and computes all derived fields deterministically
// from in and prev. An idempotent replay of identical normalized input keeps
// prev.Revision and prev.UpdatedAt and reports changed=false.
func DeriveProgress(in ProgressInput, prev *EpicProgress, now time.Time) (EpicProgress, bool, error) {
	norm, err := NormalizeProgressInput(in)
	if err != nil {
		return EpicProgress{}, false, err
	}
	var counts StageCounts
	sum := 0
	for _, s := range norm.Slices {
		sum += stageWeight[s.Stage]
		switch s.Stage {
		case StageQueued:
			counts.Queued++
		case StageImplementing:
			counts.Implementing++
		case StageReview:
			counts.Review++
		case StageRework:
			counts.Rework++
		case StageApproved:
			counts.Approved++
		case StageDone:
			counts.Done++
		case StageBlocked:
			counts.Blocked++
		}
	}
	var percent *int
	if norm.PlanComplete && len(norm.Slices) > 0 {
		n := len(norm.Slices)
		// Rounded arithmetic mean (half-up) in exact integer arithmetic.
		p := (2*sum + n) / (2 * n)
		percent = &p
	}

	same := prev != nil &&
		prev.GoalID == norm.GoalID &&
		prev.PlanComplete == norm.PlanComplete &&
		slices.Equal(prev.Slices, norm.Slices)

	rev := 1
	updatedAt := now.UTC()
	changed := true
	if same {
		changed = false
		if prev.Revision > 0 {
			rev = prev.Revision
		}
		if !prev.UpdatedAt.IsZero() {
			updatedAt = prev.UpdatedAt
		}
	} else if prev != nil && prev.Revision > 0 {
		rev = prev.Revision + 1
	}

	return EpicProgress{
		GoalID:       norm.GoalID,
		PlanComplete: norm.PlanComplete,
		Slices:       norm.Slices,
		TotalSlices:  len(norm.Slices),
		DoneSlices:   counts.Done,
		Counts:       counts,
		Percent:      percent,
		Revision:     rev,
		UpdatedAt:    updatedAt,
	}, changed, nil
}
