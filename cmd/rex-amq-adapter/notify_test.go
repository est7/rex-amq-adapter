package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Outside Rex with no personal agent recorded, notify-personal reports
// no_live_personal as JSON and exits 0: a launchd job's tick is not a
// failure, and nothing is started or recorded.
func TestNotifyPersonalWithNoLivePersonalExitsZero(t *testing.T) {
	isolate(t)
	// No real Rex or amq is needed, or touched: nothing is recorded to ask
	// about.
	never := filepath.Join(t.TempDir(), "never-run")
	if err := os.WriteFile(never, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", never)
	t.Setenv("REX_AMQ_AMQ", never)
	code, out := capture(t, runNotifyPersonal, "--ref", "timer:7", "--event-id", "tick-7", "--summary", "stand-up")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "no_live_personal" || res["ref"] != "timer:7" || res["event_id"] != "tick-7" || len(res) != 3 {
		t.Fatalf("result %v", res)
	}
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	if recs, _ := filepath.Glob(filepath.Join(home, "records", "*")); len(recs) != 0 {
		t.Errorf("records %v", recs)
	}
	if n, _ := filepath.Glob(filepath.Join(home, "amq-root", "extensions", "rex-amq-adapter", "notifications", "*.json")); len(n) != 0 {
		t.Errorf("an unsent event was recorded: %v", n)
	}
}
