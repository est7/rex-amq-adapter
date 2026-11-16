package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
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

func openWithin(e Env, loc Location, r EpicRequest, d time.Duration) (EpicResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return e.OpenEpic(ctx, loc, r)
}

// An `epic --op-id` whose first run timed out with the narrator not yet
// adopted left one session, recorded partial_start under the op id before
// any wait. Its retry starts nothing: it reconciles that session (relinking
// it when the first run died before the link), sees both agents adopted, and
// moves the lifecycle to opened. The same op id for another request is
// refused, and a session Rex no longer has is reported, not replaced.
func TestOpenEpicOpIDReconcilesPartialStartWithoutADuplicate(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	req := EpicRequest{Name: "login revamp", Dir: dir, Goal: goal, OpID: "me:board:epic-7#open"}
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	first, err := openWithin(e, loc, req, 600*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Ready() || first.SessionID != "session:epic-1" || first.LifecycleState != store.EpicPartialStart || first.OpID != req.OpID {
		t.Fatalf("first run %+v", first)
	}
	op, ok, err := e.Store.EpicOp(store.EpicOpOpen, req.OpID)
	if err != nil || !ok || op.SessionID != "session:epic-1" || op.State != store.EpicPartialStart || op.NarratorBlock != "block:epic-1-1" || op.OrchestratorBlock != "block:epic-1-2" || op.PersonalHandle != "claude-01" {
		t.Fatalf("op record %+v %v %v", op, ok, err)
	}
	lc, ok, err := e.Store.EpicLifecycle("session:epic-1")
	if err != nil || !ok || lc.State != store.EpicPartialStart || lc.Name != "login revamp" || lc.OpenOpID != req.OpID || lc.PersonalHandle != "claude-01" || lc.OpenedAt.IsZero() {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
	// The first run as if it had died before linking.
	for _, l := range mustLinks(t, e) {
		if _, err := e.Store.DeleteLink(l); err != nil {
			t.Fatal(err)
		}
	}
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	again, err := openWithin(e, loc, req, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || !again.Ready() || again.SessionID != first.SessionID || again.LifecycleState != store.EpicOpened || again.Narrator.Handle != "claude-02" {
		t.Fatalf("retry %+v", again)
	}
	if len(term.sessions_) != 1 {
		t.Fatalf("a retry created another session: %+v", term.sessions_)
	}
	if links := mustLinks(t, e); len(links) != 1 || links[0].Epic.SessionID != first.SessionID {
		t.Fatalf("links after the retry %+v", links)
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpOpen, req.OpID); op.State != store.EpicOpened {
		t.Errorf("op record after the retry %+v", op)
	}
	for name, other := range map[string]EpicRequest{
		"other words": {Name: req.Name, Dir: dir, Asked: "something else", OpID: req.OpID},
		"other name":  {Name: "other", Dir: dir, Goal: goal, OpID: req.OpID},
	} {
		if _, err := openWithin(e, loc, other, time.Second); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another epic open request") {
			t.Errorf("%s under the same op id: %v", name, err)
		}
	}
	term.mu.Lock()
	term.killed = append(term.killed, first.SessionID)
	term.mu.Unlock()
	if _, err := openWithin(e, loc, req, time.Second); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Errorf("a vanished session: %v", err)
	}
	if len(term.sessions_) != 1 {
		t.Fatalf("a refused or failed retry created a session: %+v", term.sessions_)
	}
}

func mustLinks(t *testing.T, e Env) []core.Link {
	t.Helper()
	links, err := e.Store.Links()
	if err != nil {
		t.Fatal(err)
	}
	return links
}

// Two runs of one op id at once create one session between them: the second
// waits for the first to record it, then reconciles it.
func TestOpenEpicOpIDRunsAtOnceCreateOneSession(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	term.sessionStarted = func(string, []string) { time.Sleep(200 * time.Millisecond) }
	req := EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal, OpID: "op-1"}
	var wg sync.WaitGroup
	results := make([]EpicResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = openWithin(e, loc, req, 5*time.Second)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].SessionID != results[1].SessionID || results[0].Replayed == results[1].Replayed {
		t.Fatalf("results %+v errors %v", results, errs)
	}
	if len(term.sessions_) != 1 {
		t.Fatalf("sessions %+v", term.sessions_)
	}
}

// Without an op id an Epic's lifecycle is still recorded: opened once both
// agents are adopted, with no op record; an invalid op id is usage and
// starts nothing.
func TestOpenEpicRecordsItsLifecycleWithoutAnOpID(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	res, err := openWithin(e, loc, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Goal: goal, Crew: "pair"}, 5*time.Second)
	if err != nil || res.LifecycleState != store.EpicOpened || res.Replayed || res.OpID != "" {
		t.Fatalf("result %+v %v", res, err)
	}
	lc, ok, err := e.Store.EpicLifecycle(res.SessionID)
	if err != nil || !ok || lc.State != store.EpicOpened || lc.OpenOpID != "" || lc.Crew != "pair" || lc.Server != fakeServer {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.Paths.Extensions(), "epic-ops")); len(entries) != 0 {
		t.Errorf("op records without an op id: %v", entries)
	}
	for _, bad := range []string{"a\nb", "tab\there", strings.Repeat("x", maxOpID+1), "   ", "\xff"} {
		if _, err := openWithin(e, loc, EpicRequest{Name: "y", Dir: filepath.Dir(goal), Goal: goal, OpID: bad}, time.Second); !errors.Is(err, ErrUsage) {
			t.Errorf("op id %q: %v, want usage", bad, err)
		}
	}
	if len(term.sessions_) != 1 {
		t.Errorf("an invalid op id created a session: %+v", term.sessions_)
	}
	if err := ValidOpID(strings.Repeat("é", maxOpID/2)); err != nil {
		t.Errorf("a %d-byte op id: %v", maxOpID, err)
	}
}

// `epic close --op-id` is recorded once its guards pass and before the kill:
// a run whose kill failed is finished by its retry, which kills, parks,
// unlinks, and records the Epic closed. The same op id for another session
// is refused and touches nothing.
func TestCloseEpicOpIDFinishesAPartialKill(t *testing.T) {
	e, term, loc := closeWorld(t)
	req := CloseRequest{Session: "session:epic", Asked: "close it", OpID: "close-1"}
	term.killErr = errors.New("injected kill failure")
	if _, err := closeEpic(e, loc, req); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("first close: %v", err)
	}
	op, ok, err := e.Store.EpicOp(store.EpicOpClose, req.OpID)
	if err != nil || !ok || op.State != store.EpicClosing || op.SessionID != "session:epic" || op.Server != fakeServer {
		t.Fatalf("op record after the failed kill %+v %v %v", op, ok, err)
	}
	if len(mustLinks(t, e)) != 1 {
		t.Fatal("a failed kill unlinked")
	}
	term.killErr = nil
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:other", Asked: "close it", OpID: req.OpID}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "another epic close request") {
		t.Fatalf("another session under the same op id: %v", err)
	}
	if len(term.killed) != 0 {
		t.Fatalf("a conflicting op id killed %v", term.killed)
	}
	res, err := closeEpic(e, loc, req)
	if err != nil || !res.Replayed || !res.Unlinked || len(res.Parked) != 3 || res.LifecycleState != store.EpicClosed || !slices.Equal(term.killed, []string{"session:epic"}) {
		t.Fatalf("retry %+v %v, killed %v", res, err, term.killed)
	}
	if len(mustLinks(t, e)) != 0 {
		t.Error("the retry left the link")
	}
	lc, ok, err := e.Store.EpicLifecycle("session:epic")
	if err != nil || !ok || lc.State != store.EpicClosed || lc.CloseOpID != req.OpID || lc.ClosedAt == nil {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpClose, req.OpID); op.State != store.EpicClosed {
		t.Errorf("op record %+v", op)
	}
	// Once more: everything is done, and saying so is not an error.
	if res, err := closeEpic(e, loc, req); err != nil || !res.Replayed || len(res.Parked) != 0 {
		t.Errorf("a third run %+v %v", res, err)
	}
}

// A close whose first run died after the unlink is finished by its retry
// under the same op id, though the link is gone; without an op id, or under
// one never recorded, a close of a session not linked stays refused, and the
// op id recorded by one personal agent is not another's.
func TestCloseEpicOpIDRecoversAfterTheUnlink(t *testing.T) {
	e, term, loc := closeWorld(t)
	me := recordOf(t, e, "claude-01")
	hash := requestHash(closeIdentity{Personal: me.Endpoint().Session(), Session: "session:epic"})
	now := time.Now().UTC()
	op := store.EpicOpRecord{Kind: store.EpicOpClose, OpID: "close-2", RequestHash: hash, SessionID: "session:epic", Server: fakeServer, PersonalHandle: me.Handle, PersonalSessionID: me.SessionID, State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}
	if err := e.Store.PutEpicOp(op); err != nil {
		t.Fatal(err)
	}
	for _, l := range mustLinks(t, e) {
		if _, err := e.Store.DeleteLink(l); err != nil {
			t.Fatal(err)
		}
	}
	for name, r := range map[string]CloseRequest{
		"no op id":            {Session: "session:epic", Asked: "close it"},
		"an unrecorded op id": {Session: "session:epic", Asked: "close it", OpID: "close-3"},
	} {
		if _, err := closeEpic(e, loc, r); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not an Epic linked") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, ok, _ := e.Store.EpicOp(store.EpicOpClose, "close-3"); ok {
		t.Error("a refused close recorded its op id")
	}
	put(t, e, store.Record{Handle: "claude-09", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me2", SessionID: "session:me2", AdoptedAt: time.Now()})
	if _, err := closeEpic(e, Location{Block: "block:me2"}, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "close-2"}); !errors.Is(err, ErrRefused) {
		t.Errorf("another personal agent replayed the close: %v", err)
	}
	if len(term.killed) != 0 {
		t.Fatalf("killed %v before the retry", term.killed)
	}
	res, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "close-2"})
	if err != nil || !res.Replayed || !res.Unlinked || len(res.Parked) != 3 || !slices.Equal(term.killed, []string{"session:epic"}) {
		t.Fatalf("retry %+v %v, killed %v", res, err, term.killed)
	}
	if lc, _, _ := e.Store.EpicLifecycle("session:epic"); lc.State != store.EpicClosed {
		t.Errorf("lifecycle %+v", lc)
	}
}

// A close without an op id records the Epic's lifecycle closed too, though no
// lifecycle was recorded before (an Epic opened before lifecycles were).
func TestCloseEpicRecordsItsLifecycleWithoutAnOpID(t *testing.T) {
	e, _, loc := closeWorld(t)
	res, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it"})
	if err != nil || res.LifecycleState != store.EpicClosed || res.Replayed {
		t.Fatalf("close %+v %v", res, err)
	}
	lc, ok, err := e.Store.EpicLifecycle("session:epic")
	if err != nil || !ok || lc.State != store.EpicClosed || lc.ClosedAt == nil || lc.CloseOpID != "" || lc.PersonalHandle != "claude-01" || !lc.OpenedAt.IsZero() {
		t.Fatalf("lifecycle %+v %v %v", lc, ok, err)
	}
}
