package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
)

// stopBudget is install.Budget("stop"): a 3 s hook timeout less its margin.
const stopBudget = 2500 * time.Millisecond

// Codex rollout rows without an "ordinal": the row id is empty, so only the
// call id tells one tool call from the next.
const (
	codexCallA = `{"timestamp":"2026-11-10T01:30:00.000Z","type":"response_item","payload":{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}}`
	codexOutA  = `{"timestamp":"2026-11-10T01:31:00.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_a","output":"ok"}}`
	codexCallB = `{"timestamp":"2026-11-10T01:35:00.000Z","type":"response_item","payload":{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"}}`
	codexOutB  = `{"timestamp":"2026-11-10T01:36:00.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_b","output":"ok"}}`
	codexCallC = `{"timestamp":"2026-11-10T01:40:00.000Z","type":"response_item","payload":{"type":"function_call","call_id":"call_c","name":"exec","arguments":"{}"}}`
	// claudeHung is a Claude tool call at 01:30 with no result; now is 02:00.
	claudeHung = `{"type":"assistant","uuid":"h1","timestamp":"2026-11-10T01:30:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}`
)

// noticesWith counts h's unread mail whose text contains what.
func noticesWith(t *testing.T, e Env, h, what string) int {
	t.Helper()
	n := 0
	for _, id := range inboxOf(t, e, h) {
		if strings.Contains(mailBody(t, e, h, id), what) {
			n++
		}
	}
	return n
}

// A hung tool call is told once per call, by its call id: a harness that
// writes no row ordinal still gets each new hung call reported, and a
// notice that could not be sent is tried again on the next poll.
func TestHungToolCallsAreToldOncePerCallID(t *testing.T) {
	e, _, orch, s, _, _ := healthWorld(t)
	hung := func() int { return noticesWith(t, e, "orch-01", "tool_hung") }

	writeRollout(t, e.TranscriptHome, codexCallA)
	e.watchdog(context.Background(), orch, s)
	if n := hung(); n != 1 {
		dumpLog(t, e)
		t.Fatalf("first poll with call_a hung: %d tool_hung notices, want 1", n)
	}
	e.watchdog(context.Background(), orch, s)
	if n := hung(); n != 1 {
		t.Fatalf("repeat poll of the same hung call: %d notices, want 1", n)
	}

	writeRollout(t, e.TranscriptHome, codexCallA, codexOutA, codexCallB)
	e.watchdog(context.Background(), orch, s)
	if n := hung(); n != 2 {
		t.Fatalf("call_b hung after call_a finished: %d notices, want 2", n)
	}

	writeRollout(t, e.TranscriptHome, codexCallA, codexOutA, codexCallB, codexOutB, codexCallC)
	failing := e
	failing.AmqBin = filepath.Join(t.TempDir(), "amq-fails")
	if err := os.WriteFile(failing.AmqBin, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failing.watchdog(context.Background(), orch, s)
	if n := hung(); n != 2 {
		t.Fatalf("a poll whose mail failed: %d notices, want 2", n)
	}
	e.watchdog(context.Background(), orch, s)
	if n := hung(); n != 3 {
		dumpLog(t, e)
		t.Fatalf("the poll after a failed send: %d notices, want 3 (call_c retried)", n)
	}
	e.watchdog(context.Background(), orch, s)
	if n := hung(); n != 3 {
		t.Fatalf("call_c told again: %d notices, want 3", n)
	}
}

// gatedTerm holds every pane capture of one block until released, as a
// Rex call that hangs would.
type gatedTerm struct {
	*fakeTerm
	block   string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedTerm) CaptureVT(ctx context.Context, block string) (string, error) {
	if block == g.block {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-time.After(3 * time.Second):
		}
	}
	return g.fakeTerm.CaptureVT(ctx, block)
}

// A watchdog poll slowed by Rex or by AMQ never holds up the stop hook of
// the agent it is looking at: the hook marks the agent errored and mails its
// turn error within its own budget, and routing then passes over it.
func TestASlowWatchdogPollDoesNotHoldUpTheStopHook(t *testing.T) {
	for _, slow := range []string{"rex capture", "amq send"} {
		t.Run(slow, func(t *testing.T) {
			e, term, orch, _, _, _ := healthWorld(t)
			rec := claude04(t, e)
			s, err := e.FindSession(context.Background(), "block:g") // now holding claude-04's pane
			if err != nil {
				t.Fatal(err)
			}
			if term.fgs == nil {
				term.fgs = map[string]rex.Process{}
			}
			term.fgs["block:c"] = rex.Process{PID: 42, Name: "claude"}
			p := filepath.Join(e.TranscriptHome, ".claude/projects/-Users-x--app/sid-c4.jsonl")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(claudeHung+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			wd := e
			entered := make(chan struct{})
			release := make(chan struct{})
			switch slow {
			case "rex capture":
				wd.Rex = &gatedTerm{fakeTerm: term, block: "block:c", entered: entered, release: release}
			case "amq send":
				dir := t.TempDir()
				wd.AmqBin = filepath.Join(dir, "amq-slow")
				script := "#!/bin/sh\ntouch '" + filepath.Join(dir, "entered") + "'\n" +
					"i=0; while [ ! -f '" + filepath.Join(dir, "release") + "' ] && [ $i -lt 60 ]; do sleep 0.05; i=$((i+1)); done\n" +
					"exec '" + e.AmqBin + "' \"$@\"\n"
				if err := os.WriteFile(wd.AmqBin, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				go func() {
					for {
						if _, err := os.Stat(filepath.Join(dir, "entered")); err == nil {
							close(entered)
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
				go func() {
					<-release
					_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
				}()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				wd.watchdog(context.Background(), orch, s)
			}()
			var releaseOnce sync.Once
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				<-done
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the watchdog never reached the slow call")
			}

			ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
			defer cancel()
			start := time.Now()
			_, err = e.Hook(ctx, EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"},
				HookInput{SessionID: "sid-c4", AgentPID: 42, Error: "rate_limit"})
			took := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				t.Fatal("setup: the watchdog poll finished before the hook did; nothing was slow")
			default:
			}
			if n := noticesWith(t, e, "orch-01", "claude-04 turn_error"); n != 1 || took >= stopBudget {
				t.Fatalf("stop hook during a slow watchdog poll (%s): %d turn_error mails to orch-01 in %v, want 1 within %v", slow, n, took, stopBudget)
			}
			if !e.flagged(rec) {
				t.Fatal("the stop hook did not mark claude-04 errored")
			}
			summoned := core.Peer{Handle: "codex", Kind: core.Codex, Parent: "claude-04"}
			peers := []core.Peer{summoned, peerOf(rec), {Handle: "orch-01", Kind: core.Claude, Role: core.OrchestratorRole}}
			to, _, ok := core.ErrorRecipient(summoned, peers, "", core.NoticeTurnError, func(h core.Handle) bool {
				return h == rec.Handle && e.flagged(rec)
			})
			if !ok || to.Handle != "orch-01" {
				t.Fatalf("routing after claude-04 errored: got %s ok=%v, want orch-01 (claude-04 skipped as flagged)", to.Handle, ok)
			}
		})
	}
}

// When the stop hook cannot record the error mark, the user's desktop hears
// that, and no mail claims a delivery that routing could not check.
func TestAStopHookThatCannotMarkTheErrorTellsTheDesktop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	e, term, _, _, _, desktop := healthWorld(t)
	rec := claude04(t, e)
	if term.fgs == nil {
		term.fgs = map[string]rex.Process{}
	}
	term.fgs["block:c"] = rex.Process{PID: 42, Name: "claude"}
	dir := filepath.Join(e.Paths.Home, "turn-error")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := e.Hook(context.Background(), EventStopFailure, core.Claude, Location{Server: "unix:///fake.sock", Block: "block:c"},
		HookInput{SessionID: "sid-c4", AgentPID: 42, Error: "rate_limit"}); err != nil {
		t.Fatal(err)
	}
	if len(*desktop) != 1 || !strings.Contains((*desktop)[0], "claude-04: turn_error") || !strings.Contains((*desktop)[0], "turn-error flag failed") {
		t.Fatalf("desktop %q, want one notice that claude-04's turn-error flag failed", *desktop)
	}
	if n := noticesWith(t, e, "orch-01", "turn_error"); n != 0 {
		t.Fatalf("%d turn_error mails sent though the flag was not recorded", n)
	}
	if e.flagged(rec) {
		t.Fatal("claude-04 reads as flagged though the mark was never written")
	}
}
