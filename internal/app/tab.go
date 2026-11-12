package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A Rex tab (window) is named by whoever the tab ledger (store.Tab) says owns
// it: a follower tab shows the pane label of the one agent in it, a crew tab
// names its crew, and a tab the user renamed keeps its name. The ledger is
// best effort: a rename racing a split, or a user renaming a tab to the very
// label the adapter would give it, is not told apart.

// ClaimTab records window of session as a crew tab claimed under label,
// "<crew>" or "<crew> | <topic>": a narrator tab when the crew it names is
// the narrator, else that crew's (core.ClaimedTabMode).
func ClaimTab(st store.Store, session, window, label string) error {
	name := crew.TabName(label)
	return st.PutTab(store.Tab{WindowID: window, SessionID: session, Mode: core.ClaimedTabMode(name), Crew: name, LastLabel: label})
}

// followBlockTab follows the tab block is in now (followTab).
func (e Env) followBlockTab(ctx context.Context, block string) {
	blk, err := e.Rex.InspectBlock(ctx, block)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the tab of %s: %v\n", block, err)
		return
	}
	e.followTab(ctx, blk.SessionID, blk.WindowID)
}

// followTab brings window, a tab of session, to the pane label of the one
// agent in it when it is a follower tab (core.FollowTab). Like a pane label
// it is advice: a failure is reported, and nothing else depends on it.
func (e Env) followTab(ctx context.Context, session, window string) {
	if session == "" || window == "" {
		return
	}
	s, err := e.loadSession(ctx, "", session)
	if err == nil {
		err = e.followTabs(ctx, s, window)
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "follow the tab %s: %v\n", window, err)
	}
}

// followTabs follows each window of s's layout (all of them when none is
// named).
func (e Env) followTabs(ctx context.Context, s Session, windows ...string) error {
	recs, err := e.Store.Records()
	if err != nil {
		return err
	}
	if len(windows) == 0 {
		for w := range s.tabs {
			windows = append(windows, w)
		}
		sort.Strings(windows)
	}
	var errs []error
	for _, w := range windows {
		if err := e.followTabIn(ctx, s, w, recs); err != nil {
			errs = append(errs, fmt.Errorf("tab %s: %w", w, err))
		}
	}
	return errors.Join(errs...)
}

// followTabIn follows window by s's layout and the records recs.
func (e Env) followTabIn(ctx context.Context, s Session, window string, recs []store.Record) error {
	label := s.tabs[window]
	if label == "" { // Rex did not describe it
		return nil
	}
	var blocks []string
	for b, w := range s.windowOf {
		if w == window {
			blocks = append(blocks, b)
		}
	}
	want, agents := "", 0
	for _, r := range recs {
		if !r.Live() || !slices.Contains(blocks, r.BlockID) {
			continue
		}
		agents++
		if len(blocks) == 1 {
			want = core.PaneLabel(r.Handle, r.Role, r.Task)
		}
	}
	t, _, err := e.Store.Tab(window)
	if err != nil {
		return err
	}
	next, rename := core.FollowTab(t.State(), label, len(blocks), agents, want)
	if rename != "" {
		if err := e.Rex.RenameWindow(ctx, window, rename); err != nil {
			return err
		}
	}
	if next == t.State() {
		return nil
	}
	t.WindowID, t.SessionID, t.Mode, t.LastLabel = window, s.id, next.Mode, next.Last
	return e.Store.PutTab(t)
}

// crewOfTab is the crew tab window, labelled label, names; "" for none. A
// tab claimed for a crew names the crew its label leads with
// (crew.TabName), else the one it was claimed for. A follower, narrator, or
// manual tab names none. A tab the ledger does not know (one named before
// it) names a crew only when its label leads with a crew found from dir,
// where the agent works: never Rex's placeholder or an agent's pane label.
func (e Env) crewOfTab(window, label string, dir func() string) string {
	t, known, err := e.Store.Tab(window)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the tab ledger of %s: %v\n", window, err)
	}
	if known {
		if t.Mode != core.WindowCrew {
			return ""
		}
		if name := crew.TabName(label); name != "" {
			return name
		}
		return t.Crew
	}
	if e.Crews == nil || core.PlaceholderTab(label) || core.AdapterLabel(label) {
		return ""
	}
	name := crew.TabName(label)
	if name == "" || name == core.NarratorTab {
		return ""
	}
	if _, found, err := e.Crews(dir()).Crew(name); err != nil {
		fmt.Fprintf(Diagnostics, "look up crew %s: %v\n", name, err)
		return ""
	} else if !found {
		return ""
	}
	return name
}
