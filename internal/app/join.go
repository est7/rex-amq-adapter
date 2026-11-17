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
	Agent agent
	Label string
	Crew  crew.Crew // zero when no crew was named
	// Briefing is the notice text: the crew's conventions and the role's
	// prompt, then who else the crew needs.
	Briefing string
}

// ErrCrewTaken: the session works as another crew, and other agents in it
// were briefed with that crew's conventions.
var ErrCrewTaken = errors.New("this Rex session already works as another crew")

// Join makes the agent in loc take role, and with crewName claims that crew
// for its Rex session: an agent the user is already talking to becomes the
// crew's lead where it stands, instead of starting a crew elsewhere and
// handing over what it knows. Without a role it takes the crew's lead. A
// session works as one crew, which briefs every agent in it, in any tab; a
// session already working as another crew with other agents in it refuses,
// since their conventions would change under them. A narrator or personal
// agent (core.Stealth) never joins a crew.
func (e Env) Join(ctx context.Context, loc Location, crewName, role string) (Joined, error) {
	rec, ok, err := e.agentIn(loc)
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
		if core.Stealth(role) || core.Stealth(rec.Role) {
			return Joined{}, fmt.Errorf("%w: a %s or %s agent never joins a crew", ErrRefused, core.NarratorRole, core.PersonalRole)
		}
		if err := e.claimSessionCrew(ctx, rec.Handle, blk.SessionID, name); err != nil {
			return Joined{}, err
		}
	}
	if role == "" {
		return Joined{}, errors.New("name a crew, a role (--role), or both")
	}
	was := rec.Role
	if rec, err = e.takeRole(ctx, loc, rec.Handle, role); err != nil {
		return Joined{}, err
	}
	var mates []core.Peer
	if recs, err := e.agents(); err != nil {
		fmt.Fprintf(Diagnostics, "read the records: %v\n", err)
	} else {
		mates = e.joinSession(ctx, rec, blk, recs, false)
	}
	if rec.Role == core.OrchestratorRole && was != core.OrchestratorRole {
		e.tellMates(ctx, peerOf(rec), mates, "")
	}
	e.alignHolds(ctx, rec, was)
	if err := e.ensureSessionWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	out := Joined{Agent: rec, Label: core.PaneLabel(rec.Handle, rec.Role, rec.Task), Crew: c}
	out.Briefing = joinNotices(core.BriefNotice(e.briefingIn(ctx, rec, rec.Role, e.sessionCrew(blk.SessionID, rec.Role))))
	if rec.Role != core.OrchestratorRole && !core.Stealth(rec.Role) {
		if orch := core.OrchestratorNotice(mates); orch != "" {
			out.Briefing = strings.TrimSpace(out.Briefing + "\n\n" + orch)
		}
	}
	if rest := othersNeeded(c.Members, rec.Role); len(rest) > 0 {
		out.Briefing += fmt.Sprintf("\n\nCrew %s is %s; it still needs %s. Start each in a new tab of this session: `rex-amq-adapter summon <role> --subject \"<task>\" --body \"<brief>\" --kind todo --asked \"<the user's words>\"` (or `--grant <file>`), or `rex run -s \"$REX_SESSION\" --label \"<role> | <task>\" --focus=false -c \"$PWD\" -- <agent> \"<brief>\"` then `rex-amq-adapter await <block>`.",
			c.Name, strings.Join(c.Members, ", "), strings.Join(rest, ", "))
	}
	return out, nil
}

// claimSessionCrew records name as session's crew, claimed by h. A session
// that already works as name is joined as it stands; one that works as
// another crew is claimed only while no other live agent outside the Stealth
// ones is in it.
func (e Env) claimSessionCrew(ctx context.Context, h core.Handle, session, name string) error {
	if session == "" {
		return errors.New("cannot determine this Rex session")
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cur, ok, err := e.Store.SessionCrew(session)
	if err != nil {
		return err
	}
	if ok && cur.Crew == name {
		return nil
	}
	if ok {
		recs, err := e.agents()
		if err != nil {
			return err
		}
		var others []string
		for _, r := range recs {
			if r.Live() && r.Handle != h && r.SessionID == session && !core.Stealth(r.Role) {
				others = append(others, string(r.Handle))
			}
		}
		if len(others) > 0 {
			return fmt.Errorf("%w (%s, with %s in it); a goal for crew %s is another Epic, in a Rex session of its own", ErrCrewTaken, cur.Crew, strings.Join(others, ", "), name)
		}
	}
	return e.Store.PutSessionCrew(session, store.SessionCrew{Crew: name, ClaimedBy: h, ClaimedAt: e.Now().UTC()})
}

// sessionCrew is the crew session works as, for an agent of role: none for a
// Stealth agent (core.Stealth), which observes or talks to the user and
// takes no part in the crew's work. Like a briefing it is advice: a ledger
// that cannot be read is reported and names none.
func (e Env) sessionCrew(session, role string) string {
	if session == "" || core.Stealth(role) {
		return ""
	}
	c, _, err := e.Store.SessionCrew(session)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the crew of %s: %v\n", session, err)
	}
	return c.Crew
}

// takeRole records role for the agent h in loc and shows it in the pane and
// in a tab that follows it.
func (e Env) takeRole(ctx context.Context, loc Location, h core.Handle, role string) (agent, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return agent{}, err
	}
	defer unlock()
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return agent{}, err
	}
	if !ok || !rec.Live() || rec.Handle != h {
		return agent{}, fmt.Errorf("the agent in %s changed while joining", loc.Block)
	}
	rec.Role = role
	if err := e.Store.PutIdentity(rec.Identity); err != nil {
		return agent{}, err
	}
	e.refreshTitle(ctx, rec)
	if err := e.Rex.RenameBlock(ctx, loc.Block, core.PaneLabel(rec.Handle, rec.Role, rec.Task)); err != nil {
		return rec, fmt.Errorf("the role is recorded, but the pane label could not be set: %w", err)
	}
	e.followBlockTab(ctx, loc.Block)
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
