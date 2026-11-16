package app

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/store"
)

// Each distinct hand-back of a session takes the next Seq and its own
// digest; the same report recorded again (CRLF and surrounding blanks aside)
// keeps its Seq and leaves the stored one, time included, as it was, so the
// session's pulse does not change and the narrator is not told twice. The
// Epic's lifecycle lists each hand-back once and is marked hand_back.
func TestHandBackSeqAndDigestAcrossOverwrites(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	clock := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return clock }
	record := func(body string) store.HandBack {
		t.Helper()
		h, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, body)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	first := record("## Review queue\nPR #9\n")
	if first.Seq != 1 || first.Digest != store.HandBackDigest("## Review queue\nPR #9") || len(first.Digest) != len("sha256:")+64 {
		t.Fatalf("first hand-back %+v", first)
	}
	pulse, err := e.sessionPulse(key)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	again := record("\r\n## Review queue\r\nPR #9\r\n\r\n")
	if again.Seq != 1 || again.Digest != first.Digest || !again.At.Equal(first.At) || again.Body != first.Body {
		t.Fatalf("the same report recorded again: %+v, first %+v", again, first)
	}
	if p, _ := e.sessionPulse(key); p.Digest() != pulse.Digest() {
		t.Fatal("recording the same report again changed the pulse, which tells the narrator again")
	}
	clock = clock.Add(time.Minute)
	second := record("## Review queue\nPR #9 merged\n")
	if second.Seq != 2 || second.Digest == first.Digest || second.Body != "## Review queue\nPR #9 merged\n" {
		t.Fatalf("a new report %+v", second)
	}
	if p, _ := e.sessionPulse(key); p.Digest() == pulse.Digest() {
		t.Fatal("a new report left the pulse unchanged")
	}
	stored, ok, err := e.Store.HandBack(key.SessionID)
	if err != nil || !ok || stored != second {
		t.Fatalf("stored %+v %v %v, want %+v", stored, ok, err, second)
	}
	lc, ok, err := e.Store.EpicLifecycle(key.SessionID)
	if err != nil || !ok || lc.State != store.EpicHandBack || len(lc.HandBacks) != 2 {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
	if h := lc.HandBacks[1]; h.Seq != 2 || h.Digest != second.Digest || h.By != "codex-01" || !h.At.Equal(second.At) {
		t.Errorf("lifecycle's second hand-back %+v", h)
	}
}

// A hand-back file written before Seq and Digest were kept still reads, and
// counts as the first: the same report again is Seq 1, a new one Seq 2.
func TestHandBackUpgradesALegacyFile(t *testing.T) {
	e, _, key := watcherWorld(t)
	ctx := context.Background()
	legacy := `{"by":"codex-01","at":"2026-11-01T00:00:00Z","body":"old report"}` + "\n"
	if err := os.MkdirAll(e.Paths.SessionDir(key.SessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Paths.SessionDir(key.SessionID)+"/hand-back.json", []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	old, ok, err := e.Store.HandBack(key.SessionID)
	if err != nil || !ok || old.Seq != 0 || old.Digest != "" || old.Body != "old report" {
		t.Fatalf("legacy read %+v %v %v", old, ok, err)
	}
	h, err := e.RecordHandBack(ctx, Location{Block: "block:o"}, "old report")
	if err != nil || h.Seq != 1 || !h.At.Equal(old.At) || h.Digest != store.HandBackDigest("old report") {
		t.Fatalf("the legacy report again: %+v %v", h, err)
	}
	h, err = e.RecordHandBack(ctx, Location{Block: "block:o"}, "new report")
	if err != nil || h.Seq != 2 {
		t.Fatalf("after a legacy report: %+v %v", h, err)
	}
	b, _ := json.Marshal(store.HandBack{By: "x", Body: "y"})
	if string(b) != `{"by":"x","at":"0001-01-01T00:00:00Z","body":"y"}` {
		t.Errorf("a hand-back without Seq or Digest writes them: %s", b)
	}
}
