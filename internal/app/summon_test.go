package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// summonWorld is a session with the caller claude-01 (the user's agent,
// depth 0) in block:a, and records the test adds. Mailboxes are real amq.
func summonWorld(t *testing.T) (Env, *fakeTerm, Location) {
	t.Helper()
	term := &fakeTerm{state: "idle", windows: map[string]string{block: "window:1"}, tabs: map[string]string{"window:1": "Work"}, rows: map[string]int{block: 40}}
	e := realAmqEnv(t, term)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	e.TranscriptHome = t.TempDir()
	add(t, e, store.Record{Handle: "claude-01", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: block, SessionID: "session:s", WindowID: "window:1", AgentPID: term.fgPID, AdoptedAt: now, Cwd: "/work/app"})
	return e, term, Location{Block: block}
}

func add(t *testing.T, e Env, r store.Record) {
	t.Helper()
	if err := e.Store.PutRecord(r); err != nil {
		t.Fatal(err)
	}
	if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), string(r.Handle)); err != nil {
		t.Fatal(err)
	}
	if r.Live() {
		e.Rex.(*fakeTerm).windows[r.BlockID] = "window:1"
	}
}

func departed(r store.Record, at time.Time) store.Record {
	r.DepartedAt = &at
	r.DepartReason = store.DepartSessionEnd
	return r
}

func unread(t *testing.T, e Env, h string) []string {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), h)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func summon(t *testing.T, e Env, loc Location, target string, extra ...func(*SummonRequest)) SummonResult {
	t.Helper()
	r := SummonRequest{Target: target, Kind: "question", Subject: "look at #42", Body: "please review #42"}
	for _, f := range extra {
		f(&r)
	}
	res, err := e.Summon(context.Background(), loc, r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A live peer gets the mail, by handle or by role; several of a role all do.
func TestSummonDeliversToLivePeers(t *testing.T) {
	e, term, loc := summonWorld(t)
	add(t, e, store.Record{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:b", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AdoptedAt: e.Now()})
	add(t, e, store.Record{Handle: "agy-03", Kind: core.Agy, Server: "unix:///fake.sock", BlockID: "block:c", SessionID: "session:s", Role: "reviewer", AgentPID: 3, AdoptedAt: e.Now()})
	if res := summon(t, e, loc, "codex-02"); res.Action != "deliver" || len(res.Handles) != 1 || len(unread(t, e, "codex-02")) != 1 {
		t.Fatalf("%+v", res)
	}
	if res := summon(t, e, loc, "reviewer"); res.Action != "deliver" || len(res.Handles) != 2 || len(unread(t, e, "agy-03")) != 1 || len(unread(t, e, "codex-02")) != 2 {
		t.Fatalf("%+v", res)
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched: %+v", term.launches)
	}
}

// A departed peer of this session is resumed in a pane beside the caller,
// in the directory it ran in, and its mail waits for it; after adoption it
// keeps its handle. Without a recorded directory or a transcript naming
// one, the resume is refused and the mail still waits.
func TestSummonResumesADepartedPeer(t *testing.T) {
	e, term, loc := summonWorld(t)
	add(t, e, departed(store.Record{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", AdoptedAt: e.Now().Add(-time.Hour), Cwd: "/work/app"}, e.Now().Add(-time.Minute)))
	res := summon(t, e, loc, "reviewer")
	if res.Action != "resume" || res.Block == "" || !res.Mailed || len(unread(t, e, "codex-02")) != 1 {
		t.Fatalf("%+v", res)
	}
	l := term.launches[0]
	if l.Dir != "/work/app" || strings.Join(l.Command, " ") != "codex --no-daemon resume thread-2" || l.Label != "reviewer(codex-02)" {
		t.Fatalf("launch: %+v", l)
	}
	// No cwd anywhere: refused, mail waits.
	e2, term2, loc2 := summonWorld(t)
	add(t, e2, departed(store.Record{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AgentSessionID: "thread-2", AdoptedAt: e2.Now().Add(-time.Hour)}, e2.Now().Add(-time.Minute)))
	if res := summon(t, e2, loc2, "codex-02"); res.Action != "refused" || !strings.Contains(res.Reason, "working directory") || len(term2.launches) != 0 || len(unread(t, e2, "codex-02")) != 1 {
		t.Fatalf("%+v launches %v", res, term2.launches)
	}
	// A transcript naming the directory makes it resumable.
	p := filepath.Join(e2.TranscriptHome, ".codex/sessions/2026/11/10/rollout-2026-11-10T02-00-00-thread-2.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"type":"session_meta","payload":{"cwd":"/work/from-transcript"}}`+"\n"), 0o600)
	if res := summon(t, e2, loc2, "codex-02"); res.Action != "resume" || term2.launches[0].Dir != "/work/from-transcript" {
		t.Fatalf("%+v launches %v", res, term2.launches)
	}
}

// With no agent of the role, one is started for it with the message as its
// brief, only with the user's words or grant file; its record carries the
// summoner as parent at depth 1 once adopted.
func TestSummonStartsARoleOnceAuthorized(t *testing.T) {
	e, term, loc := summonWorld(t)
	if res := summon(t, e, loc, "reviewer"); res.Action != "refused" || !strings.Contains(res.Reason, "user's own words") || len(term.launches) != 0 {
		t.Fatalf("%+v", res)
	}
	if res := summon(t, e, loc, "codex-09", func(r *SummonRequest) { r.Asked = "开个 codex 审一下" }); res.Action != "refused" || !strings.Contains(res.Reason, "by role, not by handle") {
		t.Fatalf("%+v", res)
	}
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "开个 codex 审一下" })
	if res.Action != "start" || res.Block == "" || res.Mailed || res.Depth != 1 || res.Parent != "claude-01" {
		t.Fatalf("%+v", res)
	}
	l := term.launches[0]
	if l.Dir != "/work/app" || l.Label != "reviewer | look at #42" || l.Command[0] != "codex" || !strings.Contains(l.Command[len(l.Command)-1], "please review #42") || !strings.Contains(l.Command[len(l.Command)-1], "amq send --to claude-01") {
		t.Fatalf("launch: %+v", l)
	}
	// The harness adopts the launched block: the record carries the lineage.
	term.mu.Lock()
	term.label = l.Label
	term.mu.Unlock()
	a, err := e.Adopt(context.Background(), Location{Block: l.Block}, core.Codex, HookInput{SessionID: "thread-9"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = amq.RetireFor(context.Background(), e.AmqBin, e.Paths.Root(), string(a.Record.Handle), l.Block)
	})
	if a.Record.Parent != "claude-01" || a.Record.Depth != 1 || a.Record.SummonedFor != "look at #42" || a.Record.Role != "reviewer" {
		t.Fatalf("adopted: %+v", a.Record)
	}
	if _, ok, _ := e.Store.TakeSummon("session:s", l.Label); ok {
		t.Fatal("the summon marker was not consumed")
	}
	if lines, _ := e.Summons("claude-01"); len(lines) != 3 || !strings.Contains(lines[2], "start reviewer") {
		t.Fatalf("summon log: %v", lines)
	}
	// A grant file authorizes a start too, and --harness picks the CLI.
	e3, term3, loc3 := summonWorld(t)
	if res := summon(t, e3, loc3, "writer", func(r *SummonRequest) { r.Grant = "/g.md"; r.Harness = "pi" }); res.Action != "start" || term3.launches[0].Command[0] != "pi" {
		t.Fatalf("%+v %v", res, term3.launches)
	}
}

// The guards: a reply or FYI never summons; a cycle, the depth limit and
// the session cap refuse a resume or start; a live peer still gets mail.
func TestSummonGuards(t *testing.T) {
	e, term, loc := summonWorld(t)
	authorized := func(r *SummonRequest) { r.Asked = "yes, start one" }
	if res := summon(t, e, loc, "reviewer", authorized, func(r *SummonRequest) { r.Kind = "answer" }); res.Action != "refused" || !strings.Contains(res.Reason, "never summons") {
		t.Fatalf("reply: %+v", res)
	}
	// Cycle: claude-01 was summoned by a reviewer (codex-00, departed); asking for a reviewer again.
	rec, _, _ := e.Store.Record(block)
	rec.Parent, rec.Depth = "codex-00", 1
	add(t, e, rec)
	add(t, e, departed(store.Record{Handle: "codex-00", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:z", SessionID: "session:s", Role: "reviewer", AgentPID: 9, AgentSessionID: "t0", AdoptedAt: e.Now().Add(-2 * time.Hour), Cwd: "/w"}, e.Now().Add(-time.Hour)))
	if res := summon(t, e, loc, "reviewer", authorized); res.Action != "refused" || !strings.Contains(res.Reason, "cycle") || !res.Mailed {
		t.Fatalf("cycle: %+v", res)
	}
	if res := summon(t, e, loc, "codex-00", authorized); res.Action != "refused" || !strings.Contains(res.Reason, "cycle") {
		t.Fatalf("self-cycle by handle: %+v", res)
	}
	// Depth: at depth 2 nothing more may be summoned.
	rec.Parent, rec.Depth = "", 2
	add(t, e, rec)
	if res := summon(t, e, loc, "writer", authorized); res.Action != "refused" || !strings.Contains(res.Reason, "too deep") {
		t.Fatalf("depth: %+v", res)
	}
	// Cap: six live agents in the session.
	rec.Depth = 0
	add(t, e, rec)
	for i := 2; i <= 6; i++ {
		add(t, e, store.Record{Handle: core.Handle("pi-0" + string(rune('0'+i))), Kind: core.Pi, Server: "unix:///fake.sock", BlockID: "block:p" + string(rune('0'+i)), SessionID: "session:s", Role: "qa", AgentPID: i, AdoptedAt: e.Now()})
	}
	if res := summon(t, e, loc, "writer", authorized); res.Action != "refused" || !strings.Contains(res.Reason, "cap") {
		t.Fatalf("cap: %+v", res)
	}
	if res := summon(t, e, loc, "qa"); res.Action != "deliver" || len(res.Handles) != 5 {
		t.Fatalf("live peers under the cap still get mail: %+v", res)
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched despite a refusal: %+v", term.launches)
	}
}

// A caller whose pane is too small to split gets the peer in a new tab.
func TestSummonOpensATabWhenThePaneIsSmall(t *testing.T) {
	e, term, loc := summonWorld(t)
	term.rows[block] = 9
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "go" })
	if res.Action != "start" || len(term.newTabs) != 1 || term.windows[res.Block] != term.newTabs[0] {
		t.Fatalf("%+v tabs %v window %q", res, term.newTabs, term.windows[res.Block])
	}
}
