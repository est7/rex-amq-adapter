package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// syncBuffer collects a stream's output while it runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) frames(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(s.b.Bytes()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("not a JSON line: %q: %v", sc.Text(), err)
		}
		out = append(out, f)
	}
	return out
}

// mail puts a message from sender into handle's box of root.
func mail(t *testing.T, root, handle, box, id, from, subject string) {
	t.Helper()
	dir := filepath.Join(root, "agents", handle, filepath.FromSlash(box))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h, _ := json.Marshal(map[string]any{"schema": 1, "id": id, "from": from, "to": []string{handle}, "thread": "p2p/x", "subject": subject, "created": "2026-11-06T08:00:00Z"})
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte("---json\n"+string(h)+"\n---\nthe body is never read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func receipt(t *testing.T, root, consumer, id, sender string, at time.Time) {
	t.Helper()
	dir := filepath.Join(root, "agents", consumer, "receipts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"schema": 1, "msg_id": id, "thread": "p2p/x", "sender": sender, "consumer": consumer, "stage": "drained", "emitted_at": at})
	if err := os.WriteFile(filepath.Join(dir, id+"__"+consumer+"__drained.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// eventsWorld has two agents in two tabs of session:s and one in session:t.
func eventsWorld(t *testing.T) (Env, *fakeTerm) {
	t.Helper()
	term := &fakeTerm{state: "idle", windows: map[string]string{
		"block:a": "window:1", "block:b": "window:2", "block:c": "window:3",
	}, sessions: map[string]string{"window:3": "session:t"}, tabs: map[string]string{"window:1": "Work"}}
	paths := store.Paths{Home: t.TempDir()}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Rex: term, RexOn: func(string) Terminal { return term }, SelfBin: "/x/rex-amq-adapter", Now: time.Now}
	for _, r := range []store.Record{
		{Handle: "codex-reviewer-01", Kind: core.Codex, Role: "reviewer", BlockID: "block:a", SessionID: "session:s", WindowID: "window:1"},
		{Handle: "pi-01", Kind: core.Pi, BlockID: "block:b", SessionID: "session:s", WindowID: "window:2"},
		{Handle: "claude-01", Kind: core.Claude, BlockID: "block:c", SessionID: "session:t", WindowID: "window:3"},
	} {
		r.Server, r.AgentPID, r.AdoptedAt = "unix:///fake.sock", os.Getpid(), time.Now().UTC()
		if err := e.Store.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	return e, term
}

func startStream(t *testing.T, e Env, sc Scope) (*syncBuffer, func()) {
	t.Helper()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.Stream(ctx, out, sc, StreamTicks{Local: 15 * time.Millisecond, Rex: 30 * time.Millisecond, Heartbeat: 100 * time.Millisecond})
	}()
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stream: %v", err)
		}
	}
	t.Cleanup(func() { cancel() })
	return out, stop
}

// await waits for a frame that match accepts, and returns it.
func await(t *testing.T, out *syncBuffer, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range out.frames(t) {
			if match(f) {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no frame for %s; got:\n%s", what, dumpFrames(t, out))
	return nil
}

func dumpFrames(t *testing.T, out *syncBuffer) string {
	var b strings.Builder
	for _, f := range out.frames(t) {
		j, _ := json.Marshal(f)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func field(f map[string]any, path ...string) any {
	var v any = f
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func TestStreamTellsTheSessionsChanges(t *testing.T) {
	e, _ := eventsWorld(t)
	root := e.Paths.Root()
	sc := Scope{Kind: ScopeSession, Server: "unix:///fake.sock", SessionID: "session:s"}
	out, stop := startStream(t, e, sc)

	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	agents, _ := snap["agents"].([]any)
	if len(agents) != 2 || field(snap, "scope", "session_id") != "session:s" || snap["schema"] != float64(1) {
		t.Fatalf("snapshot: %v", snap)
	}
	byHandle := map[string]map[string]any{}
	for _, a := range agents {
		m := a.(map[string]any)
		byHandle[m["handle"].(string)] = m
	}
	if r := byHandle["codex-reviewer-01"]; field(r, "tab") != "Work" || field(r, "role") != "reviewer" || field(r, "activity", "state") != "idle" || field(r, "pane") != "present" {
		t.Errorf("reviewer: %v", r)
	}
	if p := byHandle["pi-01"]; field(p, "tab") != "window:2" || field(p, "delivery", "in_process") != true {
		t.Errorf("pi in the other tab: %v", p)
	}
	if _, ok := byHandle["claude-01"]; ok {
		t.Error("an agent of another session was streamed")
	}

	mail(t, root, "codex-reviewer-01", "inbox/new", "2026-11-06T08-00-00.000Z_m1", "pi-01", "Review\nthis ‮please")
	m := await(t, out, "the queued message", func(f map[string]any) bool {
		return f["type"] == "upsert" && f["entity"] == "message" && field(f, "value", "state") == "queued"
	})
	if field(m, "value", "subject") != "Review this please" || field(m, "value", "body") != nil {
		t.Errorf("message: %v", m)
	}
	await(t, out, "the recipient's unread count", func(f map[string]any) bool {
		return f["type"] == "upsert" && f["key"] == "codex-reviewer-01" && field(f, "value", "delivery", "unread") == float64(1)
	})

	if err := e.Store.RecordInject("codex-reviewer-01", core.Outcome{Progress: core.Deferred, Code: "agent_working"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	await(t, out, "the doorbell event", func(f map[string]any) bool {
		return f["type"] == "event" && f["kind"] == "doorbell" && f["handle"] == "codex-reviewer-01" && f["code"] == "agent_working"
	})

	src := filepath.Join(root, "agents", "codex-reviewer-01", "inbox", "new", "2026-11-06T08-00-00.000Z_m1.md")
	if err := os.MkdirAll(filepath.Join(root, "agents", "codex-reviewer-01", "inbox", "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, strings.Replace(src, "/new/", "/cur/", 1)); err != nil {
		t.Fatal(err)
	}
	receipt(t, root, "codex-reviewer-01", "2026-11-06T08-00-00.000Z_m1", "pi-01", time.Now().UTC())
	await(t, out, "the drained message", func(f map[string]any) bool {
		return f["type"] == "upsert" && f["entity"] == "message" && field(f, "value", "state") == "drained"
	})
	await(t, out, "the drained event", func(f map[string]any) bool {
		return f["type"] == "event" && f["kind"] == "drained" && f["msg_id"] == "2026-11-06T08-00-00.000Z_m1" && f["sender"] == "pi-01"
	})

	rec := recordNamed(t, e, "pi-01")
	gone := time.Now().UTC()
	rec.DepartedAt, rec.DepartReason = &gone, "block_gone"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	await(t, out, "the departure", func(f map[string]any) bool {
		return f["type"] == "upsert" && f["key"] == "pi-01" && field(f, "value", "lifecycle", "state") == "departed" && field(f, "value", "lifecycle", "depart_reason") == "block_gone"
	})
	if err := e.Store.DeleteRecord("pi-01"); err != nil {
		t.Fatal(err)
	}
	await(t, out, "the removal", func(f map[string]any) bool { return f["type"] == "remove" && f["key"] == "pi-01" })
	await(t, out, "a heartbeat", func(f map[string]any) bool { return f["type"] == "heartbeat" })

	// Quiet now: Rex is polled again and again, and nothing is told.
	before := len(out.frames(t))
	time.Sleep(150 * time.Millisecond)
	for _, f := range out.frames(t)[before:] {
		if f["type"] != "heartbeat" {
			t.Errorf("a quiet scope emitted %v", f)
		}
	}
	stop()

	frames := out.frames(t)
	for i, f := range frames {
		if f["seq"] != float64(i+1) || f["stream"] != frames[0]["stream"] {
			t.Fatalf("frame %d: seq %v stream %v", i, f["seq"], f["stream"])
		}
	}
}

func TestStreamOfATab(t *testing.T) {
	e, _ := eventsWorld(t)
	out, stop := startStream(t, e, Scope{Kind: ScopeTab, Server: "unix:///fake.sock", SessionID: "session:s", WindowID: "window:2"})
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	stop()
	agents, _ := snap["agents"].([]any)
	if len(agents) != 1 || field(agents[0].(map[string]any), "handle") != "pi-01" {
		t.Fatalf("tab snapshot: %v", snap["agents"])
	}
}

func TestStreamOfTheServer(t *testing.T) {
	e, _ := eventsWorld(t)
	out, stop := startStream(t, e, Scope{Kind: ScopeAll, Server: "unix:///fake.sock"})
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	stop()
	if agents, _ := snap["agents"].([]any); len(agents) != 3 {
		t.Fatalf("server snapshot: %v", snap["agents"])
	}
}

// An observer must not change what it watches: no record, ledger, log,
// injector wrapper, or lock file is created or touched.
func TestObservingWritesNothing(t *testing.T) {
	e, _ := eventsWorld(t)
	root := e.Paths.Root()
	mail(t, root, "codex-reviewer-01", "inbox/new", "2026-11-06T08-00-00.000Z_m1", "pi-01", "hi")
	mail(t, root, "pi-01", "dlq/new", "2026-11-06T08-00-01.000Z_m2", "codex-reviewer-01", "lost")
	receipt(t, root, "pi-01", "2026-11-06T07-00-00.000Z_m0", "codex-reviewer-01", time.Now().Add(-time.Hour))
	if err := e.Store.RecordInject("codex-reviewer-01", core.Outcome{Progress: core.Accepted}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.SetAnnounced("codex-reviewer-01", []string{"2026-11-06T08-00-00.000Z_m1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tree := func() string {
		var b strings.Builder
		_ = filepath.WalkDir(e.Paths.Home, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, _ := d.Info()
			fmt.Fprintf(&b, "%s %d %s %v\n", p, info.Size(), info.ModTime().Format(time.RFC3339Nano), info.Mode())
			return nil
		})
		return b.String()
	}
	before := tree()
	out, stop := startStream(t, e, Scope{Kind: ScopeSession, Server: "unix:///fake.sock", SessionID: "session:s"})
	snap := await(t, out, "the snapshot", func(f map[string]any) bool { return f["type"] == "snapshot" })
	time.Sleep(120 * time.Millisecond)
	stop()
	if after := tree(); after != before {
		t.Errorf("observing changed the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	msgs, _ := snap["messages"].([]any)
	states := map[string]any{}
	for _, m := range msgs {
		states[field(m.(map[string]any), "id").(string)] = field(m.(map[string]any), "state")
	}
	if states["2026-11-06T08-00-00.000Z_m1"] != "queued" || states["2026-11-06T08-00-01.000Z_m2"] != "dead_lettered" {
		t.Errorf("message states %v", states)
	}
	for _, f := range out.frames(t) {
		if f["type"] == "event" {
			t.Errorf("history was told as news: %v", f)
		}
	}
}

// A doorbell recorded after the stream started but before its first look is
// in the snapshot already: the first tick must not tell it again.
func TestADoorbellInTheSnapshotIsNotToldAgain(t *testing.T) {
	e, _ := eventsWorld(t)
	start := time.Now().UTC()
	s := &streamer{e: e, start: start, doorbellAt: map[core.Handle]time.Time{}, receipts: map[core.Handle]map[string]bool{}, dead: map[string]bool{}}
	obs := core.Observations{Agents: []core.AgentObservation{{
		Handle:    "codex-reviewer-01",
		Doorbells: []core.Attempt{{At: start.Add(time.Millisecond), Outcome: core.Outcome{Progress: core.Accepted}}},
	}}}
	s.seed(nil, obs)
	if evs := s.events(nil, obs); len(evs) != 0 {
		t.Fatalf("the snapshot's doorbell was told again: %+v", evs)
	}
	obs.Agents[0].Doorbells = append(obs.Agents[0].Doorbells, core.Attempt{At: start.Add(time.Second), Outcome: core.Outcome{Progress: core.Deferred, Code: "agent_working"}})
	if evs := s.events(nil, obs); len(evs) != 1 || evs[0].Code != "agent_working" {
		t.Fatalf("a later doorbell: %+v", evs)
	}
}
