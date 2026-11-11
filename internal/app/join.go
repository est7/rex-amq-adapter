package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Joined is what join made of the agent: its record, its pane label, and the
// briefing it now works under.
type Joined struct {
	Record store.Record
	Label  string
	Crew   crew.Crew // zero when no crew was named
	// Briefing is the notice text: the crew's conventions and the role's
	// prompt, then who else the crew needs.
	Briefing string
}

// ErrTabTaken: the tab holds another agent, whose crew would change under it.
var ErrTabTaken = errors.New("another agent is already in this tab")

// Join makes the agent in loc take role, and with crewName names its tab
// after that crew. An agent the user is already talking to becomes the
// crew's lead where it stands, instead of starting a crew elsewhere and
// handing over what it knows. Without a role it takes the crew's lead. The
// tab is renamed only while no other live agent is in it: their crew would
// change under them; a crew of its own then goes in a new tab
// (`rex-amq-adapter tab`).
func (e Env) Join(ctx context.Context, loc Location, crewName, role string) (Joined, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return Joined{}, err
	}
	if !ok || !rec.Live() {
		return Joined{}, fmt.Errorf("no AMQ identity for this Rex block (%s), so there is no agent to join a crew", loc.Block)
	}
	if e.Crews == nil {
		return Joined{}, errors.New("no crews are available")
	}
	blk, err := e.Rex.InspectBlock(ctx, loc.Block)
	if err != nil {
		return Joined{}, err
	}
	role = crew.Name(role)
	var c crew.Crew
	if name := crew.Name(crewName); name != "" {
		var found bool
		if c, found, err = e.Crews(e.agentDir(ctx, rec)).Crew(name); err != nil {
			return Joined{}, err
		}
		if !found {
			return Joined{}, fmt.Errorf("no crew %q: `rex-amq-adapter crews` lists them", name)
		}
		if role == "" {
			role = c.Lead
		}
		if role == "" {
			return Joined{}, fmt.Errorf("crew %s names no lead: pass --role", name)
		}
		if err := e.nameTab(ctx, rec, blk.SessionID, blk.WindowID, name); err != nil {
			return Joined{}, err
		}
	}
	if role == "" {
		return Joined{}, errors.New("name a crew, a role (--role), or both")
	}
	was := rec.Role
	if rec, err = e.takeRole(ctx, loc, rec.Handle, role, c.Name); err != nil {
		return Joined{}, err
	}
	var mates []core.Peer
	if recs, err := e.Store.Records(); err != nil {
		fmt.Fprintf(Diagnostics, "read the records: %v\n", err)
	} else {
		mates = e.joinSession(ctx, rec, blk, recs, false)
	}
	if rec.Role == core.OrchestratorRole && was != core.OrchestratorRole {
		e.tellMates(ctx, peerOf(rec), mates, "")
	}
	if err := e.ensureOrchestratorWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	out := Joined{Record: rec, Label: core.PaneLabel(rec.Handle, rec.Role, rec.Task), Crew: c}
	name := c.Name
	if name == "" {
		name = e.tabCrew(ctx, blk.SessionID, blk.WindowID)
	}
	out.Briefing = joinNotices(core.BriefNotice(e.briefingIn(ctx, rec, rec.Role, name)))
	if rec.Role != core.OrchestratorRole {
		if orch := core.OrchestratorNotice(mates); orch != "" {
			out.Briefing = strings.TrimSpace(out.Briefing + "\n\n" + orch)
		}
	}
	if rest := othersNeeded(c.Members, rec.Role); len(rest) > 0 {
		out.Briefing += fmt.Sprintf("\n\nCrew %s is %s; it still needs %s. Start each in a pane of this tab: `rex split -s \"$REX_SESSION\" -b \"$REX_BLOCK\" --label \"<role> | <task>\" … -- <agent> \"<brief>\"`, then `rex-amq-adapter await <block>`.",
			c.Name, strings.Join(c.Members, ", "), strings.Join(rest, ", "))
	}
	return out, nil
}

// nameTab names the window after the crew, unless it already is. Another live
// agent in the window refuses it.
func (e Env) nameTab(ctx context.Context, rec store.Record, session, window, name string) error {
	if session == "" || window == "" {
		return errors.New("cannot determine this Rex tab")
	}
	if e.tabCrew(ctx, session, window) == name {
		return nil
	}
	blocks, err := e.Rex.ListBlocks(ctx, session)
	if err != nil {
		return err
	}
	recs, err := e.Store.Records()
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.WindowID != window || b.ID == rec.BlockID {
			continue
		}
		for _, r := range recs {
			if r.Live() && r.BlockID == b.ID && r.Handle != rec.Handle {
				return fmt.Errorf("%w (%s); start crew %s in a new tab with `rex-amq-adapter tab %s --role \"<role> | <task>\" -- <agent> \"<brief>\"`", ErrTabTaken, r.Handle, name, name)
			}
		}
	}
	return e.Rex.RenameWindow(ctx, window, name)
}

// tabCrew is the crew window's label names, "" when Rex cannot say.
func (e Env) tabCrew(ctx context.Context, session, window string) string {
	s, err := e.Rex.InspectSession(ctx, session)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the tab %s: %v\n", window, err)
		return ""
	}
	for _, w := range s.Windows {
		if w.ID == window {
			return crew.Name(w.Label)
		}
	}
	return ""
}

// takeRole records role (and crewName, when non-empty) for the agent h in loc
// and shows it in the pane.
func (e Env) takeRole(ctx context.Context, loc Location, h core.Handle, role, crewName string) (store.Record, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return store.Record{}, err
	}
	defer unlock()
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return store.Record{}, err
	}
	if !ok || !rec.Live() || rec.Handle != h {
		return store.Record{}, fmt.Errorf("the agent in %s changed while joining", loc.Block)
	}
	rec.Role = role
	if crewName != "" {
		rec.Crew = crewName
	}
	if err := e.Store.PutRecord(rec); err != nil {
		return store.Record{}, err
	}
	if err := e.Rex.RenameBlock(ctx, loc.Block, core.PaneLabel(rec.Handle, rec.Role, rec.Task)); err != nil {
		return rec, fmt.Errorf("the role is recorded, but the pane label could not be set: %w", err)
	}
	return rec, nil
}

// othersNeeded are the members of a crew besides one taking role.
func othersNeeded(members []string, role string) []string {
	rest := slices.Clone(members)
	if i := slices.Index(rest, role); i >= 0 {
		rest = slices.Delete(rest, i, i+1)
	}
	return rest
}
