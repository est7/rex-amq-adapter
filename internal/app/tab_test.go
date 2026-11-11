package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

const placeholder = "Window 4c6fe88d"

// tabWorld has the given blocks in window:1, a tab labelled tab, and
// another tab, window:2, holding block:z; nobody is adopted yet.
func tabWorld(t *testing.T, tab string, blocks ...string) (Env, *fakeTerm) {
	t.Helper()
	windows := map[string]string{"block:z": "window:2"}
	for _, b := range blocks {
		windows[b] = "window:1"
	}
	term := &fakeTerm{state: "idle", windows: windows, tabs: map[string]string{"window:1": tab, "window:2": "elsewhere"}}
	e := realAmqEnv(t, term)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return e, term
}

func ledgerOf(t *testing.T, e Env, window string) store.Tab {
	t.Helper()
	tab, ok, err := e.Store.Tab(window)
	if err != nil || !ok {
		t.Fatalf("no ledger entry for %s: ok=%v err=%v", window, ok, err)
	}
	return tab
}

func tabOf(term *fakeTerm, window string) string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.tabs[window]
}

// A tab Rex left as "Window <8-hex>" with one agent in it follows that
// agent's pane label through adoption, a task, and a role, and the ledger
// records each label it gave.
func TestFollowerTabFollowsAdoptSetTaskAndTakeRole(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a")
	ctx := context.Background()
	h, _ := startPi(t, e, "block:a", "s1")
	steps := []struct {
		name string
		do   func() error
		want string
	}{
		{"adoption", func() error { return nil }, string(h)},
		{"a task", func() error { _, err := e.SetTask(ctx, Location{Block: "block:a"}, "fix login"); return err }, string(h) + " | fix login"},
		{"a role", func() error {
			_, err := e.Join(ctx, Location{Block: "block:a"}, "", "reviewer")
			return err
		}, "reviewer(" + string(h) + ") | fix login"},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := tabOf(term, "window:1"); got != s.want {
			t.Errorf("after %s the tab shows %q, want %q", s.name, got, s.want)
		}
		if l := ledgerOf(t, e, "window:1"); l.Mode != core.WindowAuto || l.LastLabel != s.want || l.SessionID != "session:s" {
			t.Errorf("after %s the ledger holds %+v", s.name, l)
		}
	}
	if got := tabOf(term, "window:2"); got != "elsewhere" {
		t.Errorf("another tab was renamed to %q", got)
	}
}

// A peer summoned into a new tab (summon.go: the caller's pane is too small)
// gets a follower tab labelled for its launch, which its adoption brings to
// its pane label.
func TestFollowerTabOfASummonedPeerTakesItsPaneLabel(t *testing.T) {
	e, term, loc := summonWorld(t)
	term.rows[block] = 9
	res := summon(t, e, loc, "reviewer", func(r *SummonRequest) { r.Asked = "go" })
	if res.Action != "start" || len(term.newTabs) != 1 {
		t.Fatalf("%+v tabs %v", res, term.newTabs)
	}
	window := term.newTabs[0]
	if l := ledgerOf(t, e, window); l.Mode != core.WindowAuto || l.LastLabel != res.Label || l.SessionID != "session:s" {
		t.Fatalf("seeded %+v, want auto with the launch label %q", l, res.Label)
	}
	h, _ := startAs(t, e, term, res.Block, "s2", res.Label)
	rec := recordOf(t, e, h)
	want := core.PaneLabel(rec.Handle, rec.Role, rec.Task)
	if want == res.Label || !strings.HasPrefix(want, "reviewer(") {
		t.Fatalf("pane label %q (launch label %q)", want, res.Label)
	}
	if got := tabOf(term, window); got != want {
		t.Errorf("the tab shows %q, want the pane label %q", got, want)
	}
	if l := ledgerOf(t, e, window); l.Mode != core.WindowAuto || l.LastLabel != want {
		t.Errorf("ledger %+v", l)
	}
	if got := tabOf(term, "window:1"); got != "Work" {
		t.Errorf("the caller's named tab became %q", got)
	}
}

// A follower tab someone renamed keeps that name: the ledger turns it
// manual, and neither a task nor a new adoption renames it again.
func TestFollowerTabRenamedByTheUserBecomesManual(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a")
	ctx := context.Background()
	startPi(t, e, "block:a", "s1")
	term.mu.Lock()
	term.tabs["window:1"] = "login bug"
	renames := len(term.renamedTabs)
	term.mu.Unlock()
	if _, err := e.SetTask(ctx, Location{Block: "block:a"}, "fix login"); err != nil {
		t.Fatal(err)
	}
	notice(t, e, EventSessionStart, core.Pi, "block:a", "s1")
	if got := tabOf(term, "window:1"); got != "login bug" || len(term.renamedTabs) != renames {
		t.Errorf("the renamed tab shows %q after %v", got, term.renamedTabs)
	}
	if l := ledgerOf(t, e, "window:1"); l.Mode != core.WindowManual {
		t.Errorf("ledger %+v, want manual", l)
	}
}

// A tab of several agents keeps its label whatever each of them does; when
// one leaves and closes its pane, park brings the tab to the agent left.
func TestFollowerTabOfSeveralBlocksIsNotOverwrittenUntilParkLeavesOne(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a", "block:b")
	ctx := context.Background()
	a, _ := startPi(t, e, "block:a", "s1")
	startPi(t, e, "block:b", "s2")
	if _, err := e.SetTask(ctx, Location{Block: "block:a"}, "fix login"); err != nil {
		t.Fatal(err)
	}
	if got := tabOf(term, "window:1"); got != placeholder || len(term.renamedTabs) != 0 {
		t.Fatalf("a shared tab was renamed: %q %v", got, term.renamedTabs)
	}
	if err := term.CloseBlock(ctx, "block:b"); err != nil {
		t.Fatal(err)
	}
	if err := e.Depart(ctx, Location{Block: "block:b"}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if got, want := tabOf(term, "window:1"), string(a)+" | fix login"; got != want {
		t.Errorf("after park the tab shows %q, want %q", got, want)
	}
}

// A departed agent's own tab keeps the label it had: its pane label is
// unchanged, and nobody else is in the tab to follow.
func TestFollowerTabParkOfItsOnlyAgentKeepsTheLabel(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a")
	h, _ := startPi(t, e, "block:a", "s1")
	if err := e.Depart(context.Background(), Location{Block: "block:a"}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if got := tabOf(term, "window:1"); got != string(h) {
		t.Errorf("tab %q", got)
	}
}

// Reconciling a session brings a follower tab adoption did not reach (an
// agent adopted before the ledger, or a tab Rex reset) to its agent, and
// leaves a crew tab and a named tab alone.
func TestFollowerTabCollectSessionSyncsAPlaceholderTab(t *testing.T) {
	e, term := tabWorld(t, "Pair", "block:a")
	ctx := context.Background()
	term.windows["block:c"] = "window:3"
	term.tabs["window:3"] = "Work"
	a, _ := startPi(t, e, "block:a", "s1")
	z, _ := startPi(t, e, "block:z", "z1")
	startPi(t, e, "block:c", "c1")
	if err := os.RemoveAll(filepath.Join(e.Paths.Home, "tabs")); err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	term.tabs["window:2"] = "Window 0123abcd"
	term.renamedTabs = nil
	term.mu.Unlock()
	s, err := e.FindSession(ctx, "block:a")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CollectSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := tabOf(term, "window:2"); got != string(z) {
		t.Errorf("the placeholder tab shows %q, want %q", got, z)
	}
	if !slices.Equal(term.renamedTabs, []string{"window:2=" + string(z)}) {
		t.Errorf("tabs renamed: %v", term.renamedTabs)
	}
	if r := recordOf(t, e, a); r.Crew != "pair" {
		t.Errorf("the agent in the crew's tab has crew %q", r.Crew)
	}
}

// Joining a crew claims the tab in the ledger, so the role it takes next
// and every later task leave the crew's name on it.
func TestCrewTabJoinRegistersTheCrew(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a")
	ctx := context.Background()
	startPi(t, e, "block:a", "s1")
	j, err := e.Join(ctx, Location{Block: "block:a"}, "Pair", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetTask(ctx, Location{Block: "block:a"}, "fix login"); err != nil {
		t.Fatal(err)
	}
	if got := tabOf(term, "window:1"); got != "pair" {
		t.Errorf("the crew's tab shows %q", got)
	}
	if l := ledgerOf(t, e, "window:1"); l.Mode != core.WindowCrew || l.Crew != "pair" {
		t.Errorf("ledger %+v", l)
	}
	if !strings.Contains(j.Briefing, `crew "pair"`) {
		t.Errorf("briefing:\n%s", j.Briefing)
	}
}

// A tab already named after the crew is claimed as it stands.
func TestCrewTabJoinClaimsATabAlreadyNamed(t *testing.T) {
	e, term := tabWorld(t, "Ship | release", "block:a", "block:b")
	ctx := context.Background()
	startPi(t, e, "block:a", "s1")
	startPi(t, e, "block:b", "s2")
	if _, err := e.Join(ctx, Location{Block: "block:b"}, "ship", "qa"); err != nil {
		t.Fatal(err)
	}
	if len(term.renamedTabs) != 0 {
		t.Errorf("tabs renamed: %v", term.renamedTabs)
	}
	if l := ledgerOf(t, e, "window:1"); l.Mode != core.WindowCrew || l.Crew != "ship" {
		t.Errorf("ledger %+v", l)
	}
}

// A crew tab labelled "<crew> | <topic>" briefs its agents as that crew.
func TestCrewTabTopicLabelBriefsTheCrew(t *testing.T) {
	e, _ := briefed(t, "Pair | login", "reviewer")
	_, n := startPi(t, e, "block:a", "s1")
	if !strings.Contains(n, `crew "pair"`) || !strings.Contains(n, "Project pair reviewer prompt.") {
		t.Fatalf("start notice:\n%s", n)
	}
}

// Crew identity comes from a tab claimed for a crew, or an unrecorded tab
// whose label leads with a crew that exists; never from a follower,
// narrator, or manual tab, Rex's placeholder, or an agent's pane label.
func TestCrewTabResolution(t *testing.T) {
	paths := store.Paths{Home: t.TempDir()}
	project := fstest.MapFS{
		"ship-topic/team-conventions.md": {Data: []byte("hyphenated crew")},
		// A crew that reads like a handle: an agent's pane label still
		// names no crew.
		"pi-01/team-conventions.md": {Data: []byte("crew named like a handle")},
	}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Crews: func(string) crew.Library {
		builtin := crew.Load("")
		return append(crew.Library{{Name: "project", FS: project, Dir: "/work/.agents/crews"}}, builtin[len(builtin)-1])
	}}
	put := func(w string, m core.WindowMode, c string) {
		if err := e.Store.PutTab(store.Tab{WindowID: w, Mode: m, Crew: c}); err != nil {
			t.Fatal(err)
		}
	}
	put("w:crew", core.WindowCrew, "ship")
	put("w:crew-hyphen", core.WindowCrew, "ship-topic")
	put("w:auto", core.WindowAuto, "")
	put("w:narrator", core.WindowNarrator, "narrator")
	put("w:manual", core.WindowManual, "")
	dir := func() string { return "/work/app" }
	for _, c := range []struct{ window, label, want string }{
		{"w:crew", "ship | login", "ship"},
		{"w:crew", "", "ship"},
		{"w:crew-hyphen", "ship-topic | fix it", "ship-topic"},
		{"w:auto", "pair", ""},
		{"w:narrator", "narrator", ""},
		{"w:manual", "pair", ""},
		{"w:new", placeholder, ""},
		{"w:new", "reviewer(claude-85) | fix", ""},
		{"w:new", "pi-01", ""},
		{"w:new", "Pair", "pair"},
		{"w:new", "Ship | login", "ship"},
		{"w:new", "ship-topic", "ship-topic"},
		{"w:new", "Work", ""},
	} {
		if got := e.crewOfTab(c.window, c.label, dir); got != c.want {
			t.Errorf("crewOfTab(%s, %q) = %q, want %q", c.window, c.label, got, c.want)
		}
	}
	e.Crews = nil
	if got := e.crewOfTab("w:new", "Pair", dir); got != "" {
		t.Errorf("without crews an unrecorded tab names %q", got)
	}
}

// An agent that moves into a follower tab whose label happens to read as a
// crew is in no crew; one that moves into a crew's tab takes that crew.
func TestCrewTabFollowMovesReadsTheLedger(t *testing.T) {
	e, term := tabWorld(t, "pair", "block:a")
	ctx := context.Background()
	term.windows["block:c"] = "window:3"
	term.tabs["window:3"] = "ship | release"
	a, _ := startPi(t, e, "block:a", "s1")
	if err := e.Store.PutTab(store.Tab{WindowID: "window:1", SessionID: "session:s", Mode: core.WindowAuto, LastLabel: "pair"}); err != nil {
		t.Fatal(err)
	}
	if err := ClaimTab(e.Store, "session:s", "window:3", "ship | release"); err != nil {
		t.Fatal(err)
	}
	rec := recordOf(t, e, a)
	rec.WindowID, rec.Crew = "window:9", "old"
	if err := e.Store.PutRecord(rec); err != nil {
		t.Fatal(err)
	}
	s, err := e.FindSession(ctx, "block:a")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.followMoves(ctx, s); err != nil {
		t.Fatal(err)
	}
	if r := recordOf(t, e, a); r.Crew != "" {
		t.Errorf("moved into a follower tab, crew %q", r.Crew)
	}
	term.mu.Lock()
	term.windows["block:a"] = "window:3"
	term.mu.Unlock()
	if s, err = e.FindSession(ctx, "block:a"); err != nil {
		t.Fatal(err)
	}
	if err := e.followMoves(ctx, s); err != nil {
		t.Fatal(err)
	}
	if r := recordOf(t, e, a); r.Crew != "ship" {
		t.Errorf("moved into the crew's tab, crew %q", r.Crew)
	}
}

// ClaimTab records a crew tab under its crew, and a narrator tab as the
// narrator's, which names no crew.
func TestWindowLedgerClaimTab(t *testing.T) {
	st := store.Store{P: store.Paths{Home: t.TempDir()}}
	e := Env{Store: st, Crews: func(string) crew.Library { return crew.Load("") }}
	for _, c := range []struct {
		label string
		want  store.Tab
		crew  string
	}{
		{"ship-topic | fix", store.Tab{WindowID: "w", SessionID: "s", Mode: core.WindowCrew, Crew: "ship-topic", LastLabel: "ship-topic | fix"}, "ship-topic"},
		{"narrator", store.Tab{WindowID: "w", SessionID: "s", Mode: core.WindowNarrator, Crew: "narrator", LastLabel: "narrator"}, ""},
	} {
		if err := ClaimTab(st, "s", "w", c.label); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := st.Tab("w"); got != c.want {
			t.Errorf("ClaimTab(%q) recorded %+v, want %+v", c.label, got, c.want)
		}
		if got := e.crewOfTab("w", c.label, func() string { return "" }); got != c.crew {
			t.Errorf("tab claimed as %q names crew %q, want %q", c.label, got, c.crew)
		}
	}
}

// Park follows the tab on the departed record's own server: collection may
// run from another.
func TestFollowerTabParkUsesTheRecordsServer(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:a", "block:b")
	ctx := context.Background()
	a, _ := startPi(t, e, "block:a", "s1")
	b, _ := startPi(t, e, "block:b", "s2")
	var asked []string
	e.Rex = &fakeTerm{}
	e.RexOn = func(server string) Terminal { asked = append(asked, server); return term }
	if err := term.CloseBlock(ctx, "block:b"); err != nil {
		t.Fatal(err)
	}
	if err := e.park(ctx, recordOf(t, e, b), "block_gone"); err != nil {
		t.Fatal(err)
	}
	if got := tabOf(term, "window:1"); got != string(a) || !slices.Contains(asked, "unix:///fake.sock") {
		t.Errorf("tab %q, servers asked %v", got, asked)
	}
}

type closedBlockTerm struct {
	Terminal
	closed string
}

func (c closedBlockTerm) InspectBlock(ctx context.Context, id string) (rex.Block, error) {
	if id == c.closed {
		return rex.Block{}, rex.ErrNotFound
	}
	return c.Terminal.InspectBlock(ctx, id)
}

func (c closedBlockTerm) Foreground(ctx context.Context, id string) (rex.Process, bool, error) {
	if id == c.closed {
		return rex.Process{}, false, rex.ErrNotFound
	}
	return c.Terminal.Foreground(ctx, id)
}

// When one of two agents in a follower tab closes its pane without SessionEnd,
// CollectSession's park renames the tab to the remaining agent; followTabs in
// the same pass must not read the pre-park snapshot and misclassify the tab as
// manual.
func TestFollowerTabStaysAutoWhenCollectParksASharedMate(t *testing.T) {
	e, term := tabWorld(t, placeholder, "block:b")
	ctx := context.Background()
	b, _ := startPi(t, e, "block:b", "s1")
	if got := tabOf(term, "window:1"); got != string(b) {
		t.Fatalf("initial tab %q, want %q", got, b)
	}
	term.mu.Lock()
	term.windows["block:a"] = "window:1"
	term.mu.Unlock()
	a, _ := startPi(t, e, "block:a", "s2")
	if err := term.CloseBlock(ctx, "block:b"); err != nil {
		t.Fatal(err)
	}
	wrapped := closedBlockTerm{Terminal: term, closed: "block:b"}
	e.Rex = wrapped
	e.RexOn = func(string) Terminal { return wrapped }
	s, err := e.FindSession(ctx, "block:a")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CollectSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if l := ledgerOf(t, e, "window:1"); l.Mode != core.WindowAuto || l.LastLabel != string(a) {
		t.Fatalf("after CollectSession the ledger holds %+v, want auto/%s", l, a)
	}
	if _, err := e.SetTask(ctx, Location{Block: "block:a"}, "after collect"); err != nil {
		t.Fatal(err)
	}
	if got, want := tabOf(term, "window:1"), string(a)+" | after collect"; got != want {
		t.Errorf("after SetTask the tab shows %q, want %q", got, want)
	}
}
