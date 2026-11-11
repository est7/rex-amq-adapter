package core

import (
	"strings"
	"testing"
	"time"
)

func TestStuckNeedsAHeldDeferralThatIsNotWork(t *testing.T) {
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	run := func(code string, ago ...time.Duration) []Attempt {
		var h []Attempt
		for _, d := range ago {
			h = append(h, Attempt{At: now.Add(-d), Outcome: Outcome{Progress: Deferred, Code: code}})
		}
		return h
	}
	for name, tc := range map[string]struct {
		history []Attempt
		unread  int
		want    bool
	}{
		"box missing for 40s":         {run("box_not_found", 40*time.Second, 10*time.Second), 1, true},
		"box missing for 10s":         {run("box_not_found", 10*time.Second), 1, false},
		"working for 40s":             {run(CodeAgentWorking, 40*time.Second, 10*time.Second), 1, false},
		"working for an hour":         {run(CodeAgentWorking, time.Hour), 3, false},
		"no mail":                     {run("box_not_found", time.Hour), 0, false},
		"accepted last":               {append(run("box_not_found", time.Hour), Attempt{At: now, Outcome: Outcome{Progress: Accepted}}), 1, false},
		"work ended, box missing 40s": {append(run(CodeAgentWorking, time.Hour), run("box_not_found", 40*time.Second)...), 1, true},
	} {
		if _, _, got := Stuck(tc.history, tc.unread, now); got != tc.want {
			t.Errorf("%s: stuck %v, want %v", name, got, tc.want)
		}
	}
}

func TestClassifyStopReadsOnlyTheHarnessReason(t *testing.T) {
	for _, tc := range []struct {
		reason, outcome string
		want            StopClass
	}{
		{"", "", StopNormal},
		{"NO_TOOL_CALL", "", StopNormal},
		{"USER_CANCELED", "", StopCancelled},
		{"MAX_INVOCATIONS", "", StopLimit},
		{"MAX_TOKEN_BUDGET_EXCEEDED", "", StopLimit},
		{"MAX_FORCED_INVOCATIONS", "", StopLimit},
		{"MODEL_ERROR", "", StopError},
		{"", "completed", StopNormal},
		{"", "error", StopError},
		{"", "aborted", StopCancelled},
		{"", "some_future_outcome", StopNormal},
	} {
		if got := ClassifyStop(tc.reason, tc.outcome); got != tc.want {
			t.Errorf("ClassifyStop(%q, %q) = %d, want %d", tc.reason, tc.outcome, got, tc.want)
		}
	}
}

// The report says what was done to the pane, from the episode alone: never
// a resize that did not take.
func TestWatchdogReportSaysOnlyWhatTook(t *testing.T) {
	peer := Peer{Handle: "codex-02", Kind: Codex}
	for _, tc := range []struct {
		ep           Episode
		want, absent string
	}{
		{Episode{}, "moved nothing", "resized"},
		{Episode{Placement: Detached}, "not placed in a tab yet", "resized"},
		{Episode{Placement: Detached, Resized: true}, "resized it to 120x30", "placed it"},
		{Episode{Placement: InOverflow}, "placed it in the overflow tab", "resized"},
		{Episode{Placement: InOverflow, Resized: true}, "resized it to 120x30 and placed it", ""},
		{Episode{Placement: Theirs, Resized: true}, "the user has placed it", "overflow tab"},
	} {
		_, body := WatchdogReport(peer, tc.ep, ReportRecoveryFailed, "", nil)
		if !strings.Contains(body, tc.want) || tc.absent != "" && strings.Contains(body, tc.absent) {
			t.Errorf("%+v: want %q and no %q in:\n%s", tc.ep, tc.want, tc.absent, body)
		}
	}
}
