package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// theUser is the Operator with no agent: a shell outside Rex (no
// REX_SERVER, so the Epic's server is the one rex reaches by default).
var theUser = Operator{}

// The user opens an Epic from outside Rex: it is linked to the Hub --hub
// names (by that Hub's key on the server the session runs on, so the Hub's
// personal agents match it, core.LinkMatchesPersonal), its lifecycle names
// that Hub and no personal agent, and its grant says the user recorded it.
// With no Hub configured, it opens unlinked and still Ready.
func TestTheUserOpensAnEpicLinkedToAHub(t *testing.T) {
	e, _, _, goal := openEpicWorld(t)
	hub := physical(t, t.TempDir())
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.OpenEpicFor(ctx, theUser, EpicRequest{Name: "login", Dir: filepath.Dir(goal), Goal: goal, Asked: "run it", Hub: hub, OpID: "me:board:1#open"})
	if err != nil || !res.Ready() || !res.Linked || res.NoHub || res.Hub != hub {
		t.Fatalf("open: %+v %v", res, err)
	}
	want := core.NewHubKey(fakeServer, hub)
	links := mustLinks(t, e)
	if len(links) != 1 || links[0].Hub != want || links[0].Personal.SessionID != "" || links[0].Epic != (core.SessionKey{Server: fakeServer, SessionID: res.SessionID}) || links[0].CreatedBy != core.UserHandle {
		t.Fatalf("links %+v", links)
	}
	personal := core.Endpoint{Handle: "claude-09", Server: fakeServer, SessionID: "session:hub", Hub: want}
	if !core.LinkMatchesPersonal(links[0], personal) {
		t.Errorf("the Hub's personal agent does not match the user's link")
	}
	lc, ok, err := e.Store.EpicLifecycle(res.SessionID)
	if err != nil || !ok || lc.Hub != want || lc.PersonalHandle != "" || lc.Server != fakeServer {
		t.Errorf("lifecycle %+v %v %v", lc, ok, err)
	}
	g, ok, err := e.Store.Grant(res.SessionID)
	if err != nil || !ok || g.RelayedBy != core.UserHandle || g.RelayedRole != core.GrantRoleUser || !strings.Contains(g.Markdown(), "- relayed_by: user (user)\n") {
		t.Errorf("grant %+v %v %v", g, ok, err)
	}
	// The same request again replays, whoever of the Hub's Operators asks.
	again, err := e.OpenEpicFor(ctx, Operator{Harness: core.Agy}, EpicRequest{Name: "login", Dir: filepath.Dir(goal), Goal: goal, Asked: "run it", Hub: hub, OpID: "me:board:1#open"})
	if err != nil || !again.Replayed || again.SessionID != res.SessionID {
		t.Errorf("replay by another Operator of the Hub: %+v %v", again, err)
	}
	if _, err := e.OpenEpicFor(ctx, theUser, EpicRequest{Name: "login", Dir: filepath.Dir(goal), Goal: goal, Asked: "run it", Hub: physical(t, t.TempDir()), OpID: "me:board:1#open"}); !isCode(err, fault.Conflict) {
		t.Errorf("the op id for another Hub: %v", err)
	}
}

func TestTheUserOpensAnEpicUnlinkedWithNoHub(t *testing.T) {
	e, _, _, goal := openEpicWorld(t)
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.OpenEpicFor(ctx, Operator{Harness: core.Claude}, EpicRequest{Name: "login", Dir: filepath.Dir(goal), Asked: "run it"})
	if err != nil || !res.Ready() || res.Linked || !res.NoHub {
		t.Fatalf("open with no Hub: %+v %v", res, err)
	}
	if links := mustLinks(t, e); len(links) != 0 {
		t.Errorf("links %+v", links)
	}
	if g, _, _ := e.Store.Grant(res.SessionID); g.RelayedBy != "claude" || g.RelayedRole != core.GrantRoleInteractive {
		t.Errorf("grant %+v", g)
	}
}

// A personal agent links its Epics to its own Hub: a --hub it does not work
// in is refused before anything starts; --hub that is no directory is
// usage for every Operator.
func TestOpenEpicHubFlag(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	ctx := context.Background()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Asked: "go", Hub: t.TempDir()}); !errors.Is(err, ErrRefused) {
		t.Errorf("a personal agent naming another Hub: %v", err)
	}
	if _, err := e.OpenEpicFor(ctx, theUser, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Asked: "go", Hub: filepath.Join(t.TempDir(), "missing"), DryRun: true}); !errors.Is(err, ErrUsage) {
		t.Errorf("a --hub that is no directory: %v", err)
	}
	hub := physical(t, t.TempDir())
	if res, err := e.OpenEpicFor(ctx, theUser, EpicRequest{Name: "x", Dir: filepath.Dir(goal), Asked: "go", Hub: hub, DryRun: true}); err != nil || res.Plan.Hub != hub {
		t.Errorf("a dry run names its Hub: %+v %v", res.Plan, err)
	}
	if len(term.sessions_) != 0 {
		t.Errorf("sessions created: %+v", term.sessions_)
	}
}

// operatorCloseWorld is closeWorld with session:epic opened for the Hub
// hub (its lifecycle and a Hub link), and session:hub a Hub's session.
func operatorCloseWorld(t *testing.T) (Env, *fakeTerm, string) {
	t.Helper()
	e, term, _ := closeWorld(t)
	hub := physical(t, t.TempDir())
	key := core.NewHubKey(fakeServer, hub)
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:epic", Server: fakeServer, Hub: key, State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutLink(core.Link{Hub: key, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: "session:me", Server: fakeServer}); err != nil {
		t.Fatal(err)
	}
	return e, term, hub
}

// The user closes any Epic the records know, every link to it removed;
// never a Hub's session; with --hub only an Epic of that Hub; and the
// --asked confirmation holds for them as for a personal agent.
func TestTheUserClosesAnEpic(t *testing.T) {
	e, term, hub := operatorCloseWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, c := range map[string]struct {
		r    CloseRequest
		want func(error) bool
	}{
		"without the user's words": {CloseRequest{Session: "session:epic"}, func(err error) bool { return isCode(err, fault.ConfirmationRequired) }},
		"a Hub's session":          {CloseRequest{Session: "session:me", Asked: "close it"}, func(err error) bool { return errors.Is(err, ErrRefused) }},
		"another Hub's filter":     {CloseRequest{Session: "session:epic", Asked: "close it", Hub: t.TempDir()}, func(err error) bool { return errors.Is(err, ErrRefused) }},
		"a session nobody knows":   {CloseRequest{Session: "session:nowhere", Asked: "close it"}, func(err error) bool { return isCode(err, fault.NotFound) }},
	} {
		if _, err := e.CloseEpicFor(ctx, theUser, c.r); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(term.killed) != 0 {
		t.Fatalf("a refused close killed %v", term.killed)
	}
	res, err := e.CloseEpicFor(ctx, theUser, CloseRequest{Session: "session:epic", Asked: "close it", Hub: hub, OpID: "me:close#1", Force: true})
	if err != nil || !res.Changed || !res.Unlinked || res.LifecycleState != store.EpicClosed {
		t.Fatalf("close: %+v %v", res, err)
	}
	if links := mustLinks(t, e); len(links) != 0 {
		t.Errorf("links left: %+v (the personal agent's session link too)", links)
	}
	// A retry answers from the tombstone and kills nothing again.
	killed := len(term.killed)
	if again, err := e.CloseEpicFor(ctx, theUser, CloseRequest{Session: "session:epic", Asked: "close it", Hub: hub, OpID: "me:close#1", Force: true}); err != nil || !again.Replayed {
		t.Errorf("retry: %+v %v", again, err)
	}
	if len(term.killed) != killed {
		t.Errorf("a retry killed again")
	}
}

// A close the user recorded under an op id that died after the unlink is
// the same request on its retry: its identity is the Hub the lifecycle
// records, which the unlink leaves alone.
func TestTheUsersCloseRecoversAfterTheUnlink(t *testing.T) {
	e, term, hub := operatorCloseWorld(t)
	hash, _, err := e.closeHashes(closer{op: theUser}, CloseRequest{Session: "session:epic", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := e.Store.PutEpicOp(store.EpicOpRecord{Kind: store.EpicOpClose, OpID: "close-9", RequestHash: hash, SessionID: "session:epic", Server: fakeServer, State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, l := range mustLinks(t, e) {
		if _, err := e.Store.DeleteLink(l); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.CloseEpicFor(ctx, theUser, CloseRequest{Session: "session:epic", Asked: "close it", Hub: hub, OpID: "close-9", Force: true})
	if err != nil || !res.Replayed || res.LifecycleState != store.EpicClosed || len(term.killed) != 1 {
		t.Fatalf("retry after the unlink: %+v %v (killed %v)", res, err, term.killed)
	}
}

// An op id recorded before v0.27, under the identity that named the
// personal agent (or the user) who asked, is the same request for the
// caller that could have made it: it replays, and its record takes the new
// hash. The legacy hashes are the ones 25e3349 computed, pinned here.
func TestLegacyOpHashesReplayAndAreRewritten(t *testing.T) {
	const (
		legacyOpen          = "sha256:565b519955c333edb3127589c21678812ee84c7d2eb0cdfce3a55dfbb93e73e4"
		legacyCloseHub      = "sha256:9828cab0726f339375fd3726190623bdd2a3e27dffa78e6b00929a6cc6c6d933"
		legacyRevivePersona = "sha256:0a56341e8e51b20268d917bf4c594b0fbad862a857cdd5955a9814dd567cf969"
		legacyReviveHuman   = "sha256:16895ca8e8e030077055f8822e5d36088c44ae86c76c3a43e6968f7648dd6ef1"
	)
	p := core.SessionKey{Server: fakeServer, SessionID: "session:me"}
	if got := requestHash(legacyOpenIdentity{Personal: p, Name: "login", Dir: "/tmp/app", Goal: "/tmp/goal.md", Asked: "run it", Crew: "pair"}); got != legacyOpen {
		t.Errorf("legacy open hash %s", got)
	}
	if got := requestHash(legacyCloseIdentityOf(core.Endpoint{Handle: "claude-01", Hub: core.NewHubKey(fakeServer, "/tmp/hub")}, CloseRequest{Session: "session:epic", Force: true})); got != legacyCloseHub {
		t.Errorf("legacy close hash %s", got)
	}
	if got := requestHash(legacyReviveIdentity{Session: "session:epic", Personal: p}); got != legacyRevivePersona {
		t.Errorf("legacy revive hash %s", got)
	}
	if got := requestHash(legacyReviveIdentity{Session: "session:epic", Human: true}); got != legacyReviveHuman {
		t.Errorf("legacy human revive hash %s", got)
	}

	// A personal agent of a Hub: its open recorded under the legacy hash
	// (which named its session, not its Hub) replays. One of no Hub has the
	// same hash either way.
	e, _, loc, goal := openEpicWorld(t)
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: "session:me", Server: fakeServer, HubKey: core.NewHubKey(fakeServer, physical(t, t.TempDir()))}); err != nil {
		t.Fatal(err)
	}
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := EpicRequest{Name: "login", Dir: filepath.Dir(goal), Goal: goal, Asked: "run it", OpID: "legacy-open"}
	first, err := e.OpenEpic(ctx, loc, req)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := e.Store.EpicOp(store.EpicOpOpen, "legacy-open")
	newHash := rec.RequestHash
	caller := recordOf(t, e, "claude-01")
	rec.RequestHash = requestHash(legacyOpenIdentity{Personal: caller.Endpoint().Session(), Name: "login", Dir: physical(t, filepath.Dir(goal)), Goal: goal, Asked: "run it"})
	if rec.RequestHash == newHash {
		t.Fatal("setup: the legacy and new open hashes are the same")
	}
	if err := e.Store.PutEpicOp(rec); err != nil {
		t.Fatal(err)
	}
	again, err := e.OpenEpic(ctx, loc, req)
	if err != nil || !again.Replayed || again.SessionID != first.SessionID {
		t.Fatalf("legacy replay: %+v %v", again, err)
	}
	if rec, _, _ := e.Store.EpicOp(store.EpicOpOpen, "legacy-open"); rec.RequestHash != newHash {
		t.Errorf("the legacy record kept %s, want %s", rec.RequestHash, newHash)
	}
	// The user could not have recorded a personal agent's open.
	rec.RequestHash = requestHash(legacyOpenIdentity{Personal: caller.Endpoint().Session(), Name: "login", Dir: physical(t, filepath.Dir(goal)), Goal: goal, Asked: "run it"})
	if err := e.Store.PutEpicOp(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := e.OpenEpicFor(ctx, theUser, req); !isCode(err, fault.Conflict) {
		t.Errorf("the user replaying a personal agent's legacy op: %v", err)
	}
}

// A retry of a close the user recorded and that died part way honours the
// request it is: --dry-run reports what the retry would do and does none of
// it, and a --hub the Epic is not of is refused, before anything is killed
// or its record touched.
func TestTheUsersCloseReplayHonoursDryRunAndHub(t *testing.T) {
	e, term, hub := operatorCloseWorld(t)
	hash, _, err := e.closeHashes(closer{op: theUser}, CloseRequest{Session: "session:epic", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := e.Store.PutEpicOp(store.EpicOpRecord{Kind: store.EpicOpClose, OpID: "close-7", RequestHash: hash, SessionID: "session:epic", Server: fakeServer, State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := CloseRequest{Session: "session:epic", Asked: "close it", OpID: "close-7", Force: true}
	dry := req
	dry.DryRun = true
	if res, err := e.CloseEpicFor(ctx, theUser, dry); err != nil || !res.DryRun || !res.Replayed || !res.Changed {
		t.Errorf("a dry run of the retry: %+v %v", res, err)
	}
	wrong := req
	wrong.Hub = t.TempDir()
	if _, err := e.CloseEpicFor(ctx, theUser, wrong); !errors.Is(err, ErrRefused) {
		t.Errorf("a retry with another Hub: %v", err)
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpClose, "close-7"); len(term.killed) != 0 || op.State != store.EpicClosing {
		t.Fatalf("a dry run or a refused retry acted: killed %v, op %s", term.killed, op.State)
	}
	right := req
	right.Hub = hub
	if res, err := e.CloseEpicFor(ctx, theUser, right); err != nil || !res.Replayed || res.LifecycleState != store.EpicClosed || len(term.killed) != 1 {
		t.Errorf("the retry: %+v %v (killed %v)", res, err, term.killed)
	}
}

// A retry of an open reconciles its Epic on the Rex server the first run
// asked, or not at all: reaching another server, it is refused and starts
// nothing there.
func TestOpenEpicRetryStaysOnItsServer(t *testing.T) {
	e, term, _, goal := openEpicWorld(t)
	hub := physical(t, t.TempDir())
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := EpicRequest{Name: "login", Dir: filepath.Dir(goal), Asked: "run it", Hub: hub, OpID: "srv-1"}
	if _, err := e.OpenEpicFor(ctx, theUser, req); err != nil {
		t.Fatal(err)
	}
	elsewhere := Operator{Loc: Location{Server: "unix:///another.sock"}}
	if _, err := e.OpenEpicFor(ctx, elsewhere, req); !isCode(err, fault.Conflict) {
		t.Errorf("a retry on another server: %v", err)
	}
	if len(term.sessions_) != 1 {
		t.Errorf("sessions created: %d", len(term.sessions_))
	}
}

// An Epic the user opened with no Hub configured is read by naming it: it
// is no Hub's to scope. One of a Hub, or a personal agent's, is not read
// this way.
func TestTheUserReadsAnUnlinkedEpic(t *testing.T) {
	e := operatorWorld(t)
	ctx := context.Background()
	for _, lc := range []store.EpicLifecycle{
		{SessionID: "session:alone", Server: fakeServer, Name: "alone", State: store.EpicOpened},
		{SessionID: "session:hubs", Server: fakeServer, Hub: core.NewHubKey(fakeServer, "/hub"), State: store.EpicOpened},
		{SessionID: "session:mine", Server: fakeServer, PersonalSessionID: "session:me", State: store.EpicOpened},
	} {
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			t.Fatal(err)
		}
	}
	user := func(string) string { return "" }
	b, err := e.ReadCockpitView(ctx, user, CockpitViewRequest{Session: "session:alone"})
	if err != nil || !strings.Contains(string(b), `"epic":"alone"`) {
		t.Fatalf("the unlinked Epic: %s %v", b, err)
	}
	for _, s := range []string{"session:hubs", "session:mine"} {
		if _, err := e.ReadCockpitView(ctx, user, CockpitViewRequest{Session: s}); err == nil {
			t.Errorf("%s read with no Hub to scope it", s)
		}
	}
}

// The Hub that admitted a close holds its retry, whatever lets the Epic be
// that Hub's: its link alone (no lifecycle Hub), or a second Hub's link
// beside its lifecycle's Hub. A first run that fails at the kill is
// finished by the same request, before the unlink and after it.
func TestTheUsersCloseRetryKeepsTheHubThatAdmittedIt(t *testing.T) {
	for _, lifecycleHub := range []bool{false, true} {
		e, term, _ := closeWorld(t)
		hub := physical(t, t.TempDir())
		lc := store.EpicLifecycle{SessionID: "session:epic", Server: fakeServer, State: store.EpicOpened}
		if lifecycleHub {
			lc.Hub = core.NewHubKey(fakeServer, physical(t, t.TempDir()))
		}
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.PutLink(core.Link{Hub: core.NewHubKey(fakeServer, hub), Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req := CloseRequest{Session: "session:epic", Asked: "close it", Hub: hub, OpID: "close-h", Force: true}
		term.mu.Lock()
		term.killErr = errors.New("rex is busy")
		term.mu.Unlock()
		if _, err := e.CloseEpicFor(ctx, theUser, req); err == nil {
			t.Fatalf("lifecycle Hub %v: the injected kill failure passed", lifecycleHub)
		}
		if op, _, _ := e.Store.EpicOp(store.EpicOpClose, "close-h"); !slices.Contains(op.HubDirs, hub) {
			t.Errorf("lifecycle Hub %v: the close op recorded Hubs %q, want %q among them", lifecycleHub, op.HubDirs, hub)
		}
		term.mu.Lock()
		term.killErr = nil
		term.mu.Unlock()
		res, err := e.CloseEpicFor(ctx, theUser, req)
		if err != nil || !res.Replayed || res.LifecycleState != store.EpicClosed || len(term.killed) != 1 {
			t.Errorf("lifecycle Hub %v: the retry: %+v %v (killed %v)", lifecycleHub, res, err, term.killed)
		}
		// After the unlink too: the record keeps the Hub.
		if again, err := e.CloseEpicFor(ctx, theUser, req); err != nil || !again.Replayed {
			t.Errorf("lifecycle Hub %v: a retry after the unlink: %+v %v", lifecycleHub, again, err)
		}
		cancel()
	}
}

// A close's record keeps only the Hubs the Epic's lifecycle and links
// prove, never a directory a personal agent's --hub named because its own
// directory is inside it: the user's retry naming that directory is
// refused, and nothing is killed.
func TestACloseRecordsOnlyTheHubsItsEpicIsOf(t *testing.T) {
	e, term, _ := closeWorld(t)
	parent := physical(t, t.TempDir())
	team := filepath.Join(parent, "team")
	if err := os.MkdirAll(team, 0o700); err != nil {
		t.Fatal(err)
	}
	key := core.NewHubKey(fakeServer, team)
	putHub(t, e, "session:me", key)
	me := seedOf(recordOf(t, e, "claude-01"))
	me.Cwd = team
	put(t, e, me)
	if err := e.Store.PutLink(core.Link{Hub: key, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	term.mu.Lock()
	term.killErr = errors.New("rex is busy")
	term.mu.Unlock()
	req := CloseRequest{Session: "session:epic", Asked: "close it", Hub: parent, OpID: "close-p", Force: true}
	if _, err := e.CloseEpic(ctx, Location{Block: "block:me"}, req); err == nil {
		t.Fatal("the injected kill failure passed")
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpClose, "close-p"); !slices.Equal(op.HubDirs, []string{team}) {
		t.Fatalf("the close op recorded Hubs %q, want only %q", op.HubDirs, team)
	}
	term.mu.Lock()
	term.killErr = nil
	term.mu.Unlock()
	if _, err := e.CloseEpicFor(ctx, theUser, req); !errors.Is(err, ErrRefused) || len(term.killed) != 0 {
		t.Errorf("the user's retry naming the parent directory: %v (killed %v)", err, term.killed)
	}
}

// A replay admitted by a Hub the Epic is linked to now, though its record
// named none, keeps that Hub in the record before it unlinks, so the same
// retry again is admitted after the unlink.
func TestACloseReplayKeepsTheHubThatAdmittedIt(t *testing.T) {
	e, term, _ := closeWorld(t)
	hub := physical(t, t.TempDir())
	hash, _, err := e.closeHashes(closer{op: theUser}, CloseRequest{Session: "session:epic", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := e.Store.PutEpicOp(store.EpicOpRecord{Kind: store.EpicOpClose, OpID: "close-k", RequestHash: hash, SessionID: "session:epic", Server: fakeServer, State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutLink(core.Link{Hub: core.NewHubKey(fakeServer, hub), Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := CloseRequest{Session: "session:epic", Asked: "close it", Hub: hub, OpID: "close-k", Force: true}
	dry := req
	dry.DryRun = true
	if _, err := e.CloseEpicFor(ctx, theUser, dry); err != nil {
		t.Fatal(err)
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpClose, "close-k"); len(op.HubDirs) != 0 {
		t.Errorf("a dry run wrote the record: %q", op.HubDirs)
	}
	if res, err := e.CloseEpicFor(ctx, theUser, req); err != nil || res.LifecycleState != store.EpicClosed || len(term.killed) != 1 {
		t.Fatalf("the replay: %+v %v", res, err)
	}
	if links := mustLinks(t, e); len(links) != 0 {
		t.Fatalf("links left %+v", links)
	}
	if again, err := e.CloseEpicFor(ctx, theUser, req); err != nil || !again.Replayed {
		t.Errorf("the same retry after the unlink: %+v %v", again, err)
	}
}
