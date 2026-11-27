package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// epic refresh validates --progress, --clear-progress, and --input strictly
// before any caller lookup or disk I/O: bad JSON, unknown fields, derived
// fields, control characters, empty stdin, and conflicting flags all exit 2
// with neither Rex/amq invoked nor the state directory created.
func TestEpicRefreshPreIOValidationAndInputContracts(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	state := filepath.Join(t.TempDir(), "state-never-made")
	t.Setenv("REX_AMQ_ADAPTER_HOME", state)

	validJSON := `{"plan_complete":true,"slices":[{"id":"S1","title":"Core","stage":"implementing"}]}`
	validFile := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(validFile, []byte(validJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	badFile := filepath.Join(t.TempDir(), "bad-progress.json")
	if err := os.WriteFile(badFile, []byte(`{"plan_complete":true,"percent":50,"slices":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-I/O failures: exit 2 (usage or invalid_input).
	for _, tc := range []struct {
		name  string
		stdin string
		args  []string
		code  string
	}{
		{"both_flags", "", []string{"epic", "refresh", "--progress", validJSON, "--clear-progress"}, "usage"},
		{"both_in_input", `{"progress":` + validJSON + `,"clear_progress":true}`, []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"both_mixed", `{"clear_progress":true}`, []string{"epic", "refresh", "--progress", validJSON, "--input", "-"}, "usage"},
		{"empty_input_stdin", "", []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"empty_progress_stdin", "", []string{"epic", "refresh", "--progress", "-"}, "usage"},
		{"double_stdin_summary_progress", "{}", []string{"epic", "refresh", "--summary", "-", "--progress", "-"}, "usage"},
		{"double_stdin_input_progress", `{"progress":"-"}`, []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"input_unknown_key", `{"unknown":1}`, []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"input_duplicate_alias", `{"clear_progress":true,"clear-progress":false}`, []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"input_progress_null", `{"progress":null}`, []string{"epic", "refresh", "--input", "-"}, "invalid_input"},
		{"input_clear_null", `{"clear_progress":null}`, []string{"epic", "refresh", "--input", "-"}, "usage"},
		{"progress_missing_plan_complete", `{"progress":{"slices":[]}}`, []string{"epic", "refresh", "--input", "-"}, "invalid_input"},
		{"progress_null_plan_complete", `{"progress":{"plan_complete":null,"slices":[{"id":"S1","title":"Model","stage":"done"}]}}`, []string{"epic", "refresh", "--input", "-"}, "invalid_input"},
		{"progress_line_separator", "", []string{"epic", "refresh", "--progress", `{"plan_complete":true,"slices":[{"id":"S1","title":"a\u2028b","stage":"queued"}]}`}, "invalid_input"},
		{"progress_raw_invalid_utf8", "{\"plan_complete\":true,\"slices\":[{\"id\":\"S1\",\"title\":\"bad\xfftitle\",\"stage\":\"queued\"}]}", []string{"epic", "refresh", "--progress", "-"}, "invalid_input"},
		{"input_string_progress_raw_invalid_utf8", "{\"progress\":\"{\\\"plan_complete\\\":true,\\\"slices\\\":[{\\\"id\\\":\\\"S1\\\",\\\"title\\\":\\\"bad\xfftitle\\\",\\\"stage\\\":\\\"queued\\\"}]}\"}", []string{"epic", "refresh", "--input", "-"}, "invalid_input"},
		{"progress_derived_percent", "", []string{"epic", "refresh", "--progress", `{"plan_complete":true,"percent":80,"slices":[]}`}, "invalid_input"},
		{"progress_unknown_field_input", `{"progress":{"plan_complete":true,"foo":1,"slices":[]}}`, []string{"epic", "refresh", "--input", "-"}, "invalid_input"},
		{"progress_bad_file", "", []string{"epic", "refresh", "--progress", "@" + badFile}, "invalid_input"},
		{"progress_control_char", "", []string{"epic", "refresh", "--progress", `{"plan_complete":true,"slices":[{"id":"S1","title":"bad\u001b[31m","stage":"queued"}]}`}, "invalid_input"},
		{"progress_bidi_char", "", []string{"epic", "refresh", "--progress", `{"plan_complete":true,"slices":[{"id":"S1","title":"bad\u202e","stage":"queued"}]}`}, "invalid_input"},
		{"progress_bad_stage", "", []string{"epic", "refresh", "--progress", `{"plan_complete":true,"slices":[{"id":"S1","title":"ok","stage":"wip"}]}`}, "invalid_input"},
	} {
		r := invoke(t, tc.stdin, tc.args...)
		env := decode(t, tc.args, r)
		if r.code != 2 || env.code() != tc.code {
			t.Errorf("%s: exit %d %q (%+v), want 2 %q", tc.name, r.code, env.code(), env.Error, tc.code)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a rejected epic refresh ran rex or amq")
	}
	if _, err := os.Stat(state); err == nil {
		t.Errorf("a rejected epic refresh created state dir %s", state)
	}

	// Valid invocations pass pre-I/O checks and reach caller lookup (outside Rex -> exit 5 failed_precondition).
	for _, tc := range []struct {
		name  string
		stdin string
		args  []string
	}{
		{"inline_progress", "", []string{"epic", "refresh", "--progress", validJSON, "--dry-run"}},
		{"at_file_progress", "", []string{"epic", "refresh", "--progress", "@" + validFile}},
		{"stdin_progress", validJSON, []string{"epic", "refresh", "--progress", "-"}},
		{"input_atomic", `{"summary":"S1 implementing","progress":` + validJSON + `}`, []string{"epic", "refresh", "--input", "-", "--fields", "as_of,progress"}},
		{"input_clear_snake", `{"summary":"reset","clear_progress":true}`, []string{"epic", "refresh", "--input", "-", "--fields", "as_of,progress"}},
		{"input_clear_kebab", `{"clear-progress":true}`, []string{"epic", "refresh", "--input", "-"}},
		{"clear_flag", "", []string{"epic", "refresh", "--clear-progress", "--dry-run"}},
	} {
		r := invoke(t, tc.stdin, tc.args...)
		env := decode(t, tc.args, r)
		if r.code != 5 || env.code() != "failed_precondition" {
			t.Errorf("%s: exit %d %q (%+v), want 5 failed_precondition", tc.name, r.code, env.code(), env.Error)
		}
	}
}

// schema and --help for epic refresh expose --progress, --clear-progress,
// --input, --dry-run, and --fields from the single command registry.
func TestEpicRefreshSchemaAndFieldsProjection(t *testing.T) {
	isolate(t)
	code, env := machine(t, "schema", "epic", "refresh")
	if code != 0 {
		t.Fatalf("schema epic refresh: exit %d %+v", code, env.Error)
	}
	var info cli.CommandInfo
	if err := json.Unmarshal(env.Data, &info); err != nil {
		t.Fatal(err)
	}
	if !info.DryRun || !info.Input || !info.Mutating || !info.Idempotent || info.Tier != cli.TierUndoable {
		t.Fatalf("unexpected command traits: %+v", info)
	}
	flags := map[string]bool{}
	for _, f := range info.Flags {
		flags[f.Name] = true
	}
	for _, want := range []string{"summary", "progress", "clear-progress", "dry-run", "input", "fields"} {
		if !flags[want] {
			t.Errorf("schema epic refresh missing flag --%s", want)
		}
	}

	// --fields as_of,progress works even when Progress is nil (after clear)
	// because RefreshResult declares progress as a JSON field.
	cleared := app.RefreshResult{SessionView: app.SessionView{Schema: app.ViewSchema}}
	proj, err := cli.Project(cleared, []string{"as_of", "progress"}, true)
	if err != nil {
		t.Fatalf("Project(cleared RefreshResult, as_of,progress) failed: %v", err)
	}
	b, _ := json.Marshal(proj)
	if !strings.Contains(string(b), `"as_of"`) {
		t.Errorf("projected cleared RefreshResult: %s", b)
	}
	if _, err := cli.Project(cleared, []string{"as_of", "no_such_field"}, true); err == nil {
		t.Error("Project accepted undeclared field on RefreshResult")
	}

	// With progress populated, --fields as_of,progress keeps only those two keys.
	pct := 30
	withProg := app.RefreshResult{
		SessionView: app.SessionView{
			Schema: app.ViewSchema,
			Progress: &store.EpicProgress{
				PlanComplete: true,
				Slices:       []store.SliceProgress{{ID: "S1", Title: "Core", Stage: store.StageImplementing}},
				TotalSlices:  1,
				Percent:      &pct,
				Revision:     1,
			},
		},
	}
	proj2, err := cli.Project(withProg, []string{"as_of", "progress"}, true)
	if err != nil {
		t.Fatalf("Project(populated RefreshResult, as_of,progress) failed: %v", err)
	}
	m, _ := proj2.(map[string]any)
	if len(m) != 2 || m["as_of"] == nil || m["progress"] == nil {
		t.Errorf("projected keys: %+v", m)
	}
}

// A non-narrator (implementer, orchestrator, personal agent) calling epic
// refresh with valid progress or --clear-progress (live or --dry-run) exits 3
// permission_denied and writes no view.json.
func TestEpicRefreshCLINarratorOnlyGuard(t *testing.T) {
	isolate(t)
	stubTools(t)
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	s := store.Store{P: store.Paths{Home: home}}
	for _, a := range []testAgent{
		{Handle: "agy-02", Kind: core.Agy, Role: "implementer", Server: "unix:///t.sock", BlockID: "block:impl", SessionID: "session:e1", WindowID: "window:1", AgentPID: 999999},
		{Handle: "agy-01", Kind: core.Agy, Role: core.OrchestratorRole, Server: "unix:///t.sock", BlockID: "block:orch", SessionID: "session:e1", WindowID: "window:1", AgentPID: 999998},
	} {
		if err := putAgent(s, a); err != nil {
			t.Fatal(err)
		}
	}
	validJSON := `{"plan_complete":true,"slices":[{"id":"S1","title":"Core","stage":"implementing"}]}`
	for _, block := range []string{"block:impl", "block:orch"} {
		t.Setenv("REX_BLOCK", block)
		check(t, []want{
			{[]string{"epic", "refresh", "--progress", validJSON}, 3, "permission_denied"},
			{[]string{"epic", "refresh", "--progress", validJSON, "--dry-run"}, 3, "permission_denied"},
			{[]string{"epic", "refresh", "--clear-progress"}, 3, "permission_denied"},
		})
	}
	if _, ok, _ := s.ReadView("session:e1"); ok {
		t.Error("non-narrator epic refresh wrote view.json")
	}
}

// An unknown --fields name on mutating `epic refresh` is rejected BEFORE
// the handler runs, so an existing view.json (and its progress snapshot)
// survives byte-for-byte untouched.
func TestEpicRefreshRejectsUnknownFieldsBeforeMutation(t *testing.T) {
	isolate(t)
	stubTools(t)
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	s := store.Store{P: store.Paths{Home: home}}
	if err := putAgent(s, testAgent{
		Handle: "agy-04", Kind: core.Agy, Role: core.NarratorRole,
		Server: "unix:///t.sock", BlockID: "block:narr", SessionID: "session:e1",
		WindowID: "window:2", AgentPID: 999997,
	}); err != nil {
		t.Fatal(err)
	}
	pct := 100
	saved := app.SessionView{
		Schema:   app.ViewSchema,
		Session:  app.ViewSession{Server: "unix:///t.sock", SessionID: "session:e1", Epic: "demo"},
		Narrator: "agy-04",
		Progress: &store.EpicProgress{
			PlanComplete: true,
			Slices:       []store.SliceProgress{{ID: "S1", Title: "Core", Stage: store.StageDone}},
			TotalSlices:  1,
			DoneSlices:   1,
			Counts:       store.StageCounts{Done: 1},
			Percent:      &pct,
			Revision:     2,
		},
	}
	if err := s.PutView("session:e1", saved); err != nil {
		t.Fatal(err)
	}
	before, ok, err := s.ReadView("session:e1")
	if err != nil || !ok {
		t.Fatalf("seed view: ok=%v err=%v", ok, err)
	}
	t.Setenv("REX_BLOCK", "block:narr")

	code, env := machine(t, "epic", "refresh", "--clear-progress", "--fields", "as_of,progres")
	if code != 2 || env.code() != "invalid_input" {
		t.Fatalf("bad --fields: exit %d %q (%+v), want 2 invalid_input", code, env.code(), env.Error)
	}
	after, ok, err := s.ReadView("session:e1")
	if err != nil || !ok {
		t.Fatalf("read view after error: ok=%v err=%v", ok, err)
	}
	if string(after) != string(before) {
		t.Fatalf("epic refresh mutated view.json before rejecting --fields:\nbefore: %s\nafter:  %s", before, after)
	}
}

// epic list projects goal_path, narrative, and progress (null when absent)
// in one call with zero Rex/amq fan-out, supports --fields including optional
// omitted fields, and filters closed Epics with --include-closed / --closed-within.
func TestEpicListCLIProgressProjectionAndClosedFiltering(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)

	hub, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := store.Store{P: store.Paths{Home: os.Getenv("REX_AMQ_ADAPTER_HOME")}}
	srv := "unix:///t.sock"
	hubKey := core.NewHubKey(srv, hub)
	otherHub, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherHubKey := core.NewHubKey(srv, otherHub)

	sLive1 := "session:01a1eac4-f222-7d79-9291-9a8a86d39e01"
	sLive2 := "session:01a1eac4-f222-7d79-9291-9a8a86d39e02"
	sClosedRecent := "session:01a1eac4-f222-7d79-9291-9a8a86d39e03"
	sClosedOld := "session:01a1eac4-f222-7d79-9291-9a8a86d39e04"
	sClosedOther := "session:01a1eac4-f222-7d79-9291-9a8a86d39e05"
	sClosedLegacy := "session:01a1eac4-f222-7d79-9291-9a8a86d39e06"

	now := time.Now().UTC()
	recentAt := now.Add(-2 * time.Hour)
	oldAt := now.Add(-48 * time.Hour)

	for _, l := range []core.Link{
		{Hub: hubKey, Epic: core.SessionKey{Server: srv, SessionID: sLive1}},
		{Hub: hubKey, Epic: core.SessionKey{Server: srv, SessionID: sLive2}},
	} {
		if err := s.PutLink(l); err != nil {
			t.Fatal(err)
		}
	}
	for _, lc := range []store.EpicLifecycle{
		{SessionID: sLive1, Server: srv, Hub: hubKey, Name: "live-with-progress", Dir: "/repo/a", State: store.EpicOpened, UpdatedAt: now},
		{SessionID: sLive2, Server: srv, Hub: hubKey, Name: "live-no-progress", Dir: "/repo/b", State: store.EpicOpened, UpdatedAt: now},
		{SessionID: sClosedRecent, Server: srv, Hub: hubKey, Name: "closed-recent", Dir: "/repo/c", State: store.EpicClosed, ClosedAt: &recentAt, UpdatedAt: recentAt, ClosedLinks: []core.Link{{Hub: hubKey, Epic: core.SessionKey{Server: srv, SessionID: sClosedRecent}}}},
		{SessionID: sClosedOld, Server: srv, Hub: hubKey, Name: "closed-old", Dir: "/repo/d", State: store.EpicClosed, ClosedAt: &oldAt, UpdatedAt: oldAt, ClosedLinks: []core.Link{{Hub: hubKey, Epic: core.SessionKey{Server: srv, SessionID: sClosedOld}}}},
		{SessionID: sClosedOther, Server: srv, Hub: otherHubKey, Name: "closed-other-hub", Dir: "/repo/e", State: store.EpicClosed, ClosedAt: &recentAt, UpdatedAt: recentAt, ClosedLinks: []core.Link{{Hub: otherHubKey, Epic: core.SessionKey{Server: srv, SessionID: sClosedOther}}}},
		{SessionID: sClosedLegacy, Server: srv, Hub: hubKey, Name: "closed-legacy-no-grant", Dir: "/repo/f", State: store.EpicClosed, ClosedAt: &recentAt, UpdatedAt: recentAt},
	} {
		if err := s.PutEpicLifecycle(lc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutGrant(store.Grant{SessionID: sLive1, RelayedBy: "agy-01", RelayedAt: now, Contract: "/me/epics/01.md", ContractSHA256: "abc"}); err != nil {
		t.Fatal(err)
	}
	pct30, pct100 := 30, 100
	if err := s.PutView(sLive1, app.SessionView{
		Schema:    app.ViewSchema,
		Session:   app.ViewSession{Server: srv, SessionID: sLive1, Epic: "live-with-progress"},
		Narrative: &app.ViewNarrative{Text: "S1 implementing", At: now},
		Progress: &store.EpicProgress{
			PlanComplete: true,
			Slices:       []store.SliceProgress{{ID: "S1", Title: "Core", Stage: store.StageImplementing}},
			TotalSlices:  1,
			Counts:       store.StageCounts{Implementing: 1},
			Percent:      &pct30,
			Revision:     1,
			UpdatedAt:    now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutView(sClosedRecent, app.SessionView{
		Schema:  app.ViewSchema,
		Session: app.ViewSession{Server: srv, SessionID: sClosedRecent, Epic: "closed-recent"},
		Progress: &store.EpicProgress{
			PlanComplete: true,
			Slices:       []store.SliceProgress{{ID: "S1", Title: "Done", Stage: store.StageDone}},
			TotalSlices:  1,
			DoneSlices:   1,
			Counts:       store.StageCounts{Done: 1},
			Percent:      &pct100,
			Revision:     2,
			UpdatedAt:    recentAt,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Flag validation errors.
	check(t, []want{
		{[]string{"epic", "list", "--hub", hub, "--closed-within", "12h"}, 2, "usage"},
		{[]string{"epic", "list", "--hub", hub, "--include-closed", "--closed-within", "721h"}, 2, "invalid_input"},
		{[]string{"epic", "list", "--hub", hub, "--include-closed", "--closed-within", "0"}, 2, "invalid_input"},
		{[]string{"epic", "list", "--hub", hub, "--fields", "session_id,nonesuch"}, 2, "invalid_input"},
	})

	// Single-call projected read of live Epics: --fields works with omitted optional fields and explicit null progress.
	code, env := machine(t, "epic", "list", "--hub", hub, "--fields", "session_id,name,dir,lifecycle,goal_path,narrative,progress")
	if code != 0 {
		t.Fatalf("epic list --fields: exit %d %+v", code, env.Error)
	}
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("live page: %s %v", env.Data, err)
	}
	for _, it := range page.Items {
		switch it["session_id"] {
		case sLive1:
			p, _ := it["progress"].(map[string]any)
			if it["goal_path"] != "/me/epics/01.md" || p == nil || p["percent"] != float64(30) {
				t.Errorf("live1 item: %+v", it)
			}
		case sLive2:
			val, present := it["progress"]
			if !present || val != nil {
				t.Errorf("live2 progress present=%v val=%v, want explicit null", present, val)
			}
		default:
			t.Errorf("unexpected live item: %v", it["session_id"])
		}
	}

	// --include-closed defaults to 24h: includes sClosedRecent, excludes sClosedOld and sClosedOther.
	code, env = machine(t, "epic", "list", "--hub", hub, "--include-closed", "--fields", "session_id,lifecycle,progress")
	if code != 0 {
		t.Fatalf("epic list --include-closed: exit %d %+v", code, env.Error)
	}
	page.Items, page.Total = nil, 0
	if err := json.Unmarshal(env.Data, &page); err != nil || page.Total != 3 {
		t.Fatalf("include-closed 24h: %s %v", env.Data, err)
	}
	if page.Items[2]["session_id"] != sClosedRecent {
		t.Errorf("expected sClosedRecent third, got %+v", page.Items)
	}

	// --include-closed --closed-within 72h includes sClosedOld too (still excludes sClosedOther).
	code, env = machine(t, "epic", "list", "--hub", hub, "--include-closed", "--closed-within", "72h", "--fields", "session_id")
	if code != 0 {
		t.Fatalf("epic list --include-closed 72h: exit %d %+v", code, env.Error)
	}
	page.Items, page.Total = nil, 0
	if err := json.Unmarshal(env.Data, &page); err != nil || page.Total != 4 {
		t.Fatalf("include-closed 72h: %s %v", env.Data, err)
	}

	// Cursor from live-only listing is rejected on --include-closed listing.
	code, env = machine(t, "epic", "list", "--hub", hub, "--limit", "1")
	if code != 0 {
		t.Fatalf("live page 1: exit %d %+v", code, env.Error)
	}
	var paged struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(env.Data, &paged); err != nil || paged.NextCursor == "" {
		t.Fatalf("missing next_cursor: %s", env.Data)
	}
	check(t, []want{{[]string{"epic", "list", "--hub", hub, "--include-closed", "--limit", "1", "--cursor", paged.NextCursor}, 2, "invalid_input"}})

	if _, err := os.Stat(marker); err == nil {
		t.Error("epic list invoked rex or amq")
	}
}
