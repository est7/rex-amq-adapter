package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A pid no process holds: the agent that held it has exited.
const exitedPID = 0x3ffffff0

// transcriptOf writes a Claude transcript with one entry per session, in the
// shape a conversation that changed session id leaves: the old entries are
// copied in front of the new ones.
func transcriptOf(t *testing.T, sessions ...string) json.RawMessage {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"file-history-snapshot","messageId":"m"}` + "\n")
	for _, s := range sessions {
		fmt.Fprintf(&b, `{"type":"user","sessionId":%q,"timestamp":"2026-11-02T04:30:42.934Z"}`+"\n", s)
	}
	path := filepath.Join(t.TempDir(), sessions[len(sessions)-1]+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return rawPath(t, path)
}

func rawPath(t *testing.T, path string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// startClaude runs the SessionStart hook of a Claude session in block b.
func startClaude(t *testing.T, e Env, b string, in HookInput) store.Record {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Hook(ctx, EventSessionStart, core.Claude, Location{Block: b}, in); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: b}, HookInput{}) })
	rec, ok, err := e.Store.Record(b)
	if err != nil || !ok {
		t.Fatalf("no record for %s: %v", b, err)
	}
	return rec
}

// The conversation moved to a new session id while its process ran, so the
// record still names the old one and SessionEnd does not match it. Restarting
// the conversation is a resume of the same agent: peers keep reaching it under
// the handle they already hold.
func TestConversationThatChangedSessionIDKeepsItsHandle(t *testing.T) {
	term := &fakeTerm{fgPID: exitedPID}
	e := realAmqEnv(t, term)
	first := startClaude(t, e, "block:a", HookInput{SessionID: "old", Source: "startup"})
	if _, err := e.Hook(context.Background(), EventSessionEnd, core.Claude, Location{Block: "block:a"}, HookInput{SessionID: "new"}); err != nil {
		t.Fatal(err)
	}

	term.fgPID = os.Getpid()
	resumed := startClaude(t, e, "block:a", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "old", "old", "new")})

	if resumed.Handle != first.Handle {
		t.Fatalf("handle changed: %s -> %s", first.Handle, resumed.Handle)
	}
	if !resumed.Live() || resumed.AgentSessionID != "new" {
		t.Fatalf("record does not follow the conversation: %+v", resumed)
	}

	// From here the record holds the new id, so a plain resume finds it in
	// any pane, without reading the transcript.
	depart(t, e, "block:a", "new")
	again := startClaude(t, e, "block:b", HookInput{SessionID: "new", Source: "resume"})
	if again.Handle != first.Handle {
		t.Fatalf("handle lost on a later resume: %s -> %s", first.Handle, again.Handle)
	}
}

func TestConversationThatChangedSessionIDKeepsItsHandleInANewPane(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	first := startClaude(t, e, "block:a", HookInput{SessionID: "old", Source: "startup"})
	depart(t, e, "block:a", "old")

	resumed := startClaude(t, e, "block:b", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "old", "new")})
	if resumed.Handle != first.Handle {
		t.Fatalf("handle changed: %s -> %s", first.Handle, resumed.Handle)
	}
}

// With several ancestors recorded, the conversation is the nearest one's.
func TestNearestAncestorWinsTheHandle(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	far := startClaude(t, e, "block:a", HookInput{SessionID: "far", Source: "startup"})
	depart(t, e, "block:a", "far")
	near := startClaude(t, e, "block:b", HookInput{SessionID: "near", Source: "startup"})
	depart(t, e, "block:b", "near")

	resumed := startClaude(t, e, "block:c", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "far", "near", "new")})
	if resumed.Handle != near.Handle || resumed.Handle == far.Handle {
		t.Fatalf("took %s; far=%s near=%s", resumed.Handle, far.Handle, near.Handle)
	}
}

// A handle is never shared: while the ancestor runs in another pane, this
// conversation is a second agent.
func TestAncestorRunningElsewhereKeepsItsHandle(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	running := startClaude(t, e, "block:a", HookInput{SessionID: "old", Source: "startup"})

	other := startClaude(t, e, "block:b", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "old", "new")})
	if other.Handle == running.Handle {
		t.Fatalf("%s is held twice", other.Handle)
	}
}

// A nearer ancestor still running means this conversation forked off it. An
// older ancestor's handle is not this agent's either.
func TestRunningAncestorStopsTheWalk(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	far := startClaude(t, e, "block:a", HookInput{SessionID: "far", Source: "startup"})
	depart(t, e, "block:a", "far")
	near := startClaude(t, e, "block:b", HookInput{SessionID: "near", Source: "startup"})

	other := startClaude(t, e, "block:c", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "far", "near", "new")})
	if other.Handle == far.Handle || other.Handle == near.Handle {
		t.Fatalf("took %s; far=%s near=%s", other.Handle, far.Handle, near.Handle)
	}
}

// Entries after the session's own are not its past.
func TestEntriesAfterItsOwnAreNotAncestors(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	later := startClaude(t, e, "block:a", HookInput{SessionID: "later", Source: "startup"})
	depart(t, e, "block:a", "later")

	other := startClaude(t, e, "block:b", HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "new", "later")})
	if other.Handle == later.Handle {
		t.Fatalf("took %s from an entry after its own", later.Handle)
	}
}

// The nearest ancestor is the last one before the session's own, even if an
// older one shows up on both sides of it.
func TestNearestAncestorIsTheLastBeforeItsOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	body := `{"sessionId":"far"}` + "\n" + `{"sessionId":"near"}` + "\n" + `{"sessionId":"far"}` + "\n" + `{"sessionId":"near"}` + "\n" + `{"sessionId":"new"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := transcriptAncestors(path, "new")
	if err != nil || strings.Join(got, ",") != "near,far" {
		t.Fatalf("%v %v", got, err)
	}
}

// Only Claude Code copies entries this way; another kind's transcript proves
// nothing about its handle.
func TestOtherKindsDoNotReadALineage(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	first := adoptIn(t, e, "block:a", "old", 0)
	depart(t, e, "block:a", "old")

	if _, err := e.Hook(context.Background(), EventSessionStart, core.Codex, Location{Block: "block:b"}, HookInput{SessionID: "new", Source: "resume", TranscriptPath: transcriptOf(t, "old", "new")}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: "block:b"}, HookInput{}) })
	next, _, _ := e.Store.Record("block:b")
	if next.Handle == first.Handle {
		t.Fatalf("took %s from a transcript", first.Handle)
	}
}

// Only what the transcript proves is claimed: without an ancestor on record
// the session is new, whatever ran in the pane before.
func TestNoRecordedAncestorIsANewAgent(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	cases := map[string]json.RawMessage{
		"only its own id": transcriptOf(t, "new"),
		"other ancestors": transcriptOf(t, "unrelated", "new"),
		"no transcript":   nil,
		"null transcript": json.RawMessage("null"),
		"missing file":    rawPath(t, filepath.Join(t.TempDir(), "absent.jsonl")),
		"not a path":      json.RawMessage("7"),
	}
	n := 0
	for name, transcript := range cases {
		t.Run(name, func(t *testing.T) {
			n++
			block := fmt.Sprintf("block:%d", n)
			old := fmt.Sprintf("old-%d", n)
			first := startClaude(t, e, block, HookInput{SessionID: old, Source: "startup"})
			depart(t, e, block, old)
			next := startClaude(t, e, block, HookInput{SessionID: fmt.Sprintf("new-%d", n), Source: "resume", TranscriptPath: transcript})
			if next.Handle == first.Handle {
				t.Fatalf("inherited %s without proof", first.Handle)
			}
		})
	}
}

// An agent that crashed mid-append leaves a torn line; the entries around it
// still prove the lineage.
func TestTornTranscriptLineDoesNotHideTheAncestor(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{})
	first := startClaude(t, e, "block:a", HookInput{SessionID: "old", Source: "startup"})
	depart(t, e, "block:a", "old")

	path := filepath.Join(t.TempDir(), "torn.jsonl")
	body := `{"type":"user","sessionId":"ol` + "\n" + `{"type":"user","sessionId":"old"}` + "\n" + `{"type":"user","sessionId":"new"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := startClaude(t, e, "block:a", HookInput{SessionID: "new", Source: "resume", TranscriptPath: rawPath(t, path)})
	if resumed.Handle != first.Handle {
		t.Fatalf("handle changed: %s -> %s", first.Handle, resumed.Handle)
	}
}
