package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// startPi starts a Pi session in b and returns the handle it was given and
// its SessionStart notice.
func startPi(t *testing.T, e Env, b, session string) (core.Handle, string) {
	t.Helper()
	ctx := context.Background()
	r, err := e.Hook(ctx, EventSessionStart, core.Pi, Location{Block: b}, HookInput{SessionID: session, AgentPID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(ctx, Location{Block: b}, HookInput{}) })
	rec, ok, err := e.Store.Record(b)
	if err != nil || !ok {
		t.Fatalf("no record for %s: %v", b, err)
	}
	return rec.Handle, r.Notice
}

func notice(t *testing.T, e Env, event string, kind core.Kind, b, session string) string {
	t.Helper()
	r, err := e.Hook(context.Background(), event, kind, Location{Block: b}, HookInput{SessionID: session, AgentPID: os.Getpid(), Idle: true})
	if err != nil {
		t.Fatalf("%s in %s: %v", event, b, err)
	}
	return r.Notice
}

// Agents the user put side by side in one Rex window learn of each other: the
// newcomer from its start notice, the others at their next prompt, once. An
// agent in another window hears nothing, and nothing that would start a turn
// or interrupt one (Pi's mail watcher, a tool call) carries the news.
func TestAgentsInOneWindowLearnOfEachOther(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{
		"block:a": "window:1", "block:b": "window:1", "block:c": "window:2",
	}})
	first, n := startPi(t, e, "block:a", "s1")
	if strings.Contains(n, "beside you") {
		t.Fatalf("the first agent in a window has no mates: %q", n)
	}
	other, _ := startPi(t, e, "block:c", "s3")
	joiner, n := startPi(t, e, "block:b", "s2")
	if !strings.Contains(n, "beside you in this Rex tab: "+string(first)+" (pi).") {
		t.Fatalf("the newcomer must be told %s is beside it, and only it: %q", first, n)
	}

	if s := notice(t, e, EventMail, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("Pi's mail watcher would start a turn for the news: %q", s)
	}
	if s := notice(t, e, EventPostTool, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("a tool call carried the news mid-turn: %q", s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); !strings.Contains(s, string(joiner)+" (pi) joined your Rex tab") {
		t.Fatalf("the next prompt must say %s joined: %q", joiner, s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("the news is told once: %q", s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:c", "s3"); s != "" {
		t.Fatalf("%s in another window was told: %q", other, s)
	}
}

// A live agent adopted again in its own block (a repeated start hook, a
// compaction) is no news. One that departed and takes its handle back is: its
// mates heard it leave. Notes queued for an agent that departs are dropped,
// so a later resume hears no stale news.
func TestOnlyAnArrivalIsNews(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{
		"block:a": "window:1", "block:b": "window:1", "block:c": "window:1",
	}})
	startPi(t, e, "block:a", "s1")
	startPi(t, e, "block:b", "s2")
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); !strings.Contains(s, "joined") {
		t.Fatalf("setup: the join was not told: %q", s)
	}

	if s := notice(t, e, EventSessionStart, core.Pi, "block:b", "s2"); strings.Contains(s, "joined") {
		t.Fatalf("setup: %q", s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("a live agent adopted again in its block was told as news: %q", s)
	}

	depart(t, e, "block:b", "s2")
	if s := notice(t, e, EventMail, core.Pi, "block:b", "s2"); s != "" {
		t.Fatalf("setup: re-adoption of the same session said %q", s)
	}
	s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1")
	if left, back := strings.Index(s, "left your Rex tab"), strings.Index(s, "joined your Rex tab"); left < 0 || back < left {
		t.Fatalf("a departure and the session's return must both be told, in order: %q", s)
	}

	startPi(t, e, "block:c", "s4") // queues news for block:a's agent
	depart(t, e, "block:a", "s1")
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); strings.Contains(s, "joined") {
		t.Fatalf("a resumed agent heard news from before it left: %q", s)
	}
}

// Window-mates are advice: when Rex cannot list the window, the agent is
// still adopted, without mates.
func TestAdoptionStandsWhenTheWindowCannotBeListed(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", listErr: errors.New("rex timed out"), windows: map[string]string{
		"block:a": "window:1", "block:b": "window:1",
	}})
	startPi(t, e, "block:a", "s1")
	h, n := startPi(t, e, "block:b", "s2")
	if !strings.Contains(n, "you are "+string(h)) || strings.Contains(n, "beside you") {
		t.Fatalf("adoption without a window listing: %q", n)
	}
}

// An agent that departs, by SessionEnd or found gone by collection, is told
// to the agents still in its window at their next prompt, once; an agent in
// another window hears nothing, and a departure that ends the last agent in
// a window tells no one.
func TestWindowMatesLearnOfADeparture(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{
		"block:a": "window:1", "block:b": "window:1", "block:c": "window:2",
	}}
	e := realAmqEnv(t, term)
	startPi(t, e, "block:a", "s1")
	leaver, _ := startPi(t, e, "block:b", "s2")
	startPi(t, e, "block:c", "s3")
	_ = notice(t, e, EventPrompt, core.Pi, "block:a", "s1") // drain the join

	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Pi, Location{Block: "block:b"}, HookInput{SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	if s := notice(t, e, EventMail, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("Pi's mail watcher would start a turn for the departure: %q", s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); !strings.Contains(s, string(leaver)+" (pi) left your Rex tab") {
		t.Fatalf("the next prompt must say %s left: %q", leaver, s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); s != "" {
		t.Fatalf("the departure is told once: %q", s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:c", "s3"); s != "" {
		t.Fatalf("an agent in another window was told: %q", s)
	}
}

func TestCollectionTellsTheWindowOfACrash(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:b": "window:1"}}
	e := realAmqEnv(t, term)
	startPi(t, e, "block:a", "s1")
	crashed, _ := startPi(t, e, "block:b", "s2")
	_ = notice(t, e, EventPrompt, core.Pi, "block:a", "s1")

	rec, _, _ := e.Store.Record("block:b")
	rec.AgentPID = 999999 // the process in block:b is gone; another runs in its place
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	term.fgPID = 999998
	term.mu.Unlock()
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := recordOf(t, e, crashed); r.Live() {
		t.Fatalf("setup: collection kept %s live", crashed)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); !strings.Contains(s, string(crashed)+" (pi) left") {
		t.Fatalf("a crash found by collection must be told: %q", s)
	}
}
