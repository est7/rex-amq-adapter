package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// inRoster reports whether h still has its mailbox directory in the amq
// root: what a roster retirement takes away (its meta/config.json entry
// stays, amq's to keep).
func inRoster(t *testing.T, e Env, h core.Handle) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(e.Paths.Root(), "agents", string(h)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func hasIdentity(e Env, h core.Handle) bool {
	_, ok, _ := e.agentOf(h)
	return ok
}

// A retired peer whose pane closed leaves the amq roster and its mailbox
// goes; its record stays, departed as retired, for its session's narrator.
func TestRetiringAPeerTakesItOutOfTheRosterAndKeepsItsRecord(t *testing.T) {
	e, _, _, _ := retireWorld(t)
	if !inRoster(t, e, "codex-04") {
		t.Fatal("setup: codex-04 is not in the roster")
	}
	res, err := retire(e, "block:o", "codex-04", true)
	if err != nil || res.Pane != PaneClosed {
		t.Fatalf("retire: %+v %v", res, err)
	}
	if inRoster(t, e, "codex-04") || mailboxMade(e, "codex-04") {
		t.Fatal("the retired peer stayed in the roster")
	}
	if r := recordOf(t, e, "codex-04"); r.Live() || r.departReason() != store.DepartRetired {
		t.Fatalf("the retired peer's record: %+v", r)
	}
	if !inRoster(t, e, "codex-03") {
		t.Fatal("another agent left the roster")
	}
}

// The sweep takes what no event took: closed Epics' handles (identities
// forgotten), peers retired from Epics since closed (forgotten) or still
// open (record kept), and roster entries the adapter named with no identity.
// It keeps the running, the reserved for a resume, the departing, a launch's
// reservation, a mailbox holding mail, and names the adapter never gives.
func TestSweepRosterTakesOnlyHandlesNeverToBeResumed(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{}})
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	departedFor := func(h core.Handle, session, reason string, unfinished bool) seed {
		return seed{Handle: h, Kind: core.Codex, Server: fakeServer, BlockID: "block:" + string(h), SessionID: session, AdoptedAt: past, DepartedAt: &past, DepartReason: reason, Departing: unfinished}
	}
	for _, s := range []seed{
		{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: "block:codex-01", SessionID: "session:live", AdoptedAt: past},
		departedFor("codex-02", "session:closed", store.DepartEpicClosed, false),
		departedFor("codex-03", "session:closed", store.DepartRetired, false),
		departedFor("codex-04", "session:open", store.DepartRetired, false),
		departedFor("codex-05", "session:free", store.DepartSessionEnd, false),
		departedFor("codex-06", "session:closed", store.DepartEpicClosed, true),
		departedFor("codex-07", "session:closed", store.DepartEpicClosed, false),
	} {
		add(t, e, s)
	}
	for _, h := range []string{"codex-08", "codex-09", "helper"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutReservation("tok", store.Reservation{Handle: "codex-09", Kind: core.Codex, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-07"), "m1.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"session:closed", "session:open"} {
		state := store.EpicOpened
		if s == "session:closed" {
			state = store.EpicClosed
		}
		if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: s, Server: fakeServer, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := e.SweepRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for h, want := range map[core.Handle]struct{ roster, identity bool }{
		"codex-01": {true, true},   // running
		"codex-02": {false, false}, // its Epic closed
		"codex-03": {false, false}, // retired from an Epic since closed
		"codex-04": {false, true},  // retired from an open Epic: its narrator reads it
		"codex-05": {true, true},   // reserved for a resume
		"codex-06": {true, true},   // its departure is unfinished
		"codex-07": {true, true},   // its mailbox holds mail
		"codex-08": {false, false}, // the adapter's name, no identity
		"codex-09": {true, false},  // a launch reserved it
		"helper":   {true, false},  // a name the adapter never gives
	} {
		if got := (struct{ roster, identity bool }{inRoster(t, e, h), hasIdentity(e, h)}); got != want {
			t.Errorf("%s: roster=%v identity=%v, want %+v", h, got.roster, got.identity, want)
		}
	}
	if rep.Kept["codex-07"] == "" || !slices.Contains(rep.Pruned, "codex-02") || !slices.Contains(rep.Forgotten, "codex-03") {
		t.Errorf("report %+v", rep)
	}
	// The handles it took keep their roster entries (amq's), but a second
	// sweep looks at none of them again: it asks amq to remove only the
	// mailboxes that are still there.
	if got, _ := amq.Agents(e.Paths.Root()); !slices.Contains(got, "codex-08") {
		t.Errorf("the roster lost codex-08's entry: %v", got)
	}
	var removals []string
	plain := e.AMQ
	e.AMQ = hookQueue{Queue: plain, beforeRemoveMailbox: func(_, h string) error {
		removals = append(removals, h)
		return nil
	}}
	if again, err := e.SweepRoster(ctx); err != nil || len(again.Pruned) != 0 {
		t.Errorf("a second sweep pruned %v: %v", again.Pruned, err)
	}
	slices.Sort(removals)
	if want := []string{"codex-07"}; !slices.Equal(removals, want) {
		t.Errorf("the second sweep asked amq to remove %v, want only %v (the one still holding mail)", removals, want)
	}
	e.AMQ = plain
	// An event names handles itself (a retire, an Epic close): one that
	// runs, or is reserved for a resume, stays all the same.
	rep, err = e.retireRoster(ctx, []core.Handle{"codex-01", "codex-05"}, func(agent) bool { return true })
	if err != nil || len(rep.Pruned) != 0 || rep.Kept["codex-01"] != "running" || rep.Kept["codex-05"] != "reserved for a resume" {
		t.Errorf("retiring a running and a resumable handle: %+v %v", rep, err)
	}
}

// Every Rex and amq call of a sweep runs outside the lifecycle lock.
func TestSweepRosterCallsNoAmqUnderTheLifecycleLock(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{}})
	past := time.Now().Add(-time.Hour)
	add(t, e, seed{Handle: "codex-02", Kind: core.Codex, Server: fakeServer, BlockID: "block:x", SessionID: "session:closed", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartEpicClosed})
	add(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: "block:y", SessionID: "session:live", AdoptedAt: past})
	plain := e
	w := watchLocks(&e)
	if _, err := e.SweepRoster(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inRoster(t, plain, "codex-02") {
		t.Fatal("the sweep left the closed Epic's handle")
	}
	assertNoCallsUnderLock(t, w, "the roster sweep")
}

// A conversation of a retired peer resumed after its handle left the roster
// (its pane had stayed open) gets its mailbox back with its adoption.
func TestAHandleTakenBackAfterItLeftTheRosterGetsItsMailboxAgain(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	ctx := context.Background()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	rec := recordOf(t, e, "codex-01")
	unlock, _ := e.Store.LockLifecycle(ctx)
	_, err := e.departQuietly(rec, store.DepartRetired)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.finishQuietly(ctx, []agent{recordOf(t, e, "codex-01")}); err != nil {
		t.Fatal(err)
	}
	if rep, err := e.retireRoster(ctx, []core.Handle{"codex-01"}, nil); err != nil || !slices.Contains(rep.Pruned, "codex-01") {
		t.Fatalf("setup: %+v %v", rep, err)
	}
	a, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"})
	if err != nil || a.Agent.Handle != "codex-01" {
		t.Fatalf("the resume: %+v %v", a.Agent, err)
	}
	if !inRoster(t, e, "codex-01") || !mailboxMade(e, "codex-01") || !wakerLive(t, e, "codex-01") {
		t.Fatal("the resumed handle has no mailbox or waker")
	}
}

// The session watchers sweep the roster at most once per rosterSweepEvery,
// host-wide.
func TestTheWatchersSweepTheRosterAtMostOncePerInterval(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{}})
	past := time.Now().Add(-time.Hour)
	closed := func(h core.Handle) seed {
		return seed{Handle: h, Kind: core.Codex, Server: fakeServer, BlockID: "block:" + string(h), SessionID: "session:closed", AdoptedAt: past, DepartedAt: &past, DepartReason: store.DepartEpicClosed}
	}
	add(t, e, seed{Handle: "codex-01", Kind: core.Codex, Server: fakeServer, BlockID: "block:a", SessionID: "session:live", AdoptedAt: past})
	add(t, e, closed("codex-02"))
	e.sweepIfDue(context.Background())
	if inRoster(t, e, "codex-02") {
		t.Fatal("the first look did not sweep")
	}
	add(t, e, closed("codex-03"))
	e.sweepIfDue(context.Background())
	if !inRoster(t, e, "codex-03") {
		t.Fatal("a look within the interval swept again")
	}
	later := e
	later.Now = func() time.Time { return time.Now().Add(rosterSweepEvery + time.Minute) }
	later.sweepIfDue(context.Background())
	if inRoster(t, e, "codex-03") {
		t.Fatal("a look after the interval did not sweep")
	}
}

// A sweep that removes a mailbox while the handle is being taken back does
// not leave the resumed agent without it: its adoption makes the mailbox
// under the handle's lock, after the sweep is done with it.
func TestASweepRacingAResumeLeavesTheResumedAgentItsMailbox(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{}})
	ctx := context.Background()
	if _, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	unlock, _ := e.Store.LockLifecycle(ctx)
	_, err := e.departQuietly(recordOf(t, e, "codex-01"), store.DepartRetired)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.finishQuietly(ctx, []agent{recordOf(t, e, "codex-01")}); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	var once sync.Once
	e.AMQ = hookQueue{
		Queue: e.AMQ,
		beforeRemoveMailbox: func(_, _ string) error {
			once.Do(func() {
				// The conversation resumes while the sweep removes its mailbox.
				go func() {
					_, err := e.Adopt(ctx, Location{Block: block}, core.Codex, HookInput{SessionID: "s1"})
					resumed <- err
				}()
				time.Sleep(300 * time.Millisecond)
			})
			return nil
		},
	}
	if _, err := e.retireRoster(ctx, []core.Handle{"codex-01"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	if !recordOf(t, e, "codex-01").Live() || !inRoster(t, e, "codex-01") || !mailboxMade(e, "codex-01") {
		t.Fatalf("the resumed agent: live=%v roster=%v mailbox=%v", recordOf(t, e, "codex-01").Live(), inRoster(t, e, "codex-01"), mailboxMade(e, "codex-01"))
	}
}

// A mailbox directory the roster does not list (amq makes one for mail sent
// to a handle outside meta/config.json) is a mailbox all the same: a new
// name avoids one holding unread mail, and the sweep finds it and keeps it.
func TestMailboxesOutsideTheRosterAreSeenAndKept(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle", windows: map[string]string{}})
	ctx := context.Background()
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "helper"); err != nil {
		t.Fatal(err)
	}
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "helper", "codex-01", "s", "x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := amq.Agents(e.Paths.Root()); slices.Contains(got, "codex-01") {
		t.Fatalf("setup: roster %v lists codex-01", got)
	}
	if ids, err := amq.Unread(e.Paths.Root(), "codex-01"); err != nil || len(ids) != 1 {
		t.Fatalf("setup: codex-01's unread %v %v", ids, err)
	}
	taken, err := e.takenHandles(nil, "")
	if err != nil || !taken["codex-01"] {
		t.Fatalf("taken %v (%v): want codex-01, whose unlisted mailbox holds unread mail", taken, err)
	}
	if h := core.NameHandle(core.Codex, taken); h != "codex-02" {
		t.Errorf("a new codex agent is named %s, want codex-02", h)
	}
	rep, err := e.SweepRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kept["codex-01"] == "" || !inRoster(t, e, "codex-01") {
		t.Errorf("the sweep did not find and keep the unlisted mailbox holding mail: %+v", rep)
	}
}
