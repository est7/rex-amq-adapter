package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
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
	plan, err := e.joinPlan(ctx, loc, crewName, role)
	if err != nil {
		return Joined{}, err
	}
	rec, blk, c, role := plan.rec, plan.blk, plan.crew, plan.role
	was := rec.Role
	if rec, err = e.takeRole(ctx, loc, rec.Handle, blk.SessionID, role, c.Name, false); err != nil {
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
		out.Briefing += fmt.Sprintf("\n\nCrew %s is %s; it still needs %s. Start each in a new tab of this session: `rex-amq-adapter agent summon <role> --subject \"<task>\" --body \"<brief>\" --kind todo --asked \"<the user's words>\"` (or `--grant <file>`), or `rex run -s \"$REX_SESSION\" --label \"<role> | <task>\" --focus=false -c \"$PWD\" -- <agent> \"<brief>\"` then `rex-amq-adapter await <block>`.",
			c.Name, strings.Join(c.Members, ", "), strings.Join(rest, ", "))
	}
	return out, nil
}

// sessionCrewClaim decides whether h may claim crew name for session, the
// caller holding the lifecycle lock, and records the claim unless preview. A
// session that already works as name is joined as it stands; one that works
// as another crew is claimed only while no other live agent outside the
// Stealth ones is in it.
func (e Env) sessionCrewClaim(h core.Handle, session, name string, recs []agent, preview bool) error {
	if session == "" {
		return errors.New("cannot determine this Rex session")
	}
	cur, ok, err := e.Store.SessionCrew(session)
	if err != nil {
		return err
	}
	if ok && cur.Crew == name {
		return nil
	}
	if ok {
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
	if preview {
		return nil
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

// takeRole decides, under the lifecycle lock, whether the agent h in loc
// may take role (and with crewName claim that crew for session): admission
// (core.AdmitRole) first, with or without a crew, so a refusal leaves its
// role, the session's crew, and its lease as they were. Unless preview, it
// records the claim and the role (a personal agent's makes the session a
// Hub's) and shows the role in the pane and in a tab that follows it.
func (e Env) takeRole(ctx context.Context, loc Location, h core.Handle, session, role, crewName string, preview bool) (agent, error) {
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
	recs, err := e.agents()
	if err != nil {
		return agent{}, err
	}
	f, p, err := e.sessionFacts(rec.SessionID, recs, rec.Handle)
	if err != nil {
		return agent{}, err
	}
	reason := core.AdmitRole(f, rec.Role, role)
	if reason == "" && crewName != "" && f.Hub() {
		reason = core.ReasonSessionPure
	}
	warnConflict(rec.SessionID, withSelf(f, rec.Role))
	if reason != "" {
		return agent{}, admissionRefused(withSelf(f, rec.Role), rec.Handle, rec.Role, role, reason)
	}
	if crewName != "" {
		if err := e.sessionCrewClaim(rec.Handle, session, crewName, recs, preview); err != nil {
			return agent{}, err
		}
	}
	if preview {
		return rec, nil
	}
	if role == core.PersonalRole && rec.Role != core.PersonalRole {
		key := p.HubKey
		if key == "" {
			if key, _, err = e.hubOf(rec.Server, rec.Cwd); err != nil {
				return agent{}, err
			}
		}
		if err := e.ensureHubPolicy(rec.SessionID, rec.Server, key, rec.Handle); err != nil {
			return agent{}, err
		}
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

// joinPlan is what a join of the agent in loc resolves before it changes
// anything: the agent, its block, the crew, and the role it would take.
type joinPlan struct {
	rec  agent
	blk  rex.Block
	crew crew.Crew
	role string
}

func (e Env) joinPlan(ctx context.Context, loc Location, crewName, role string) (joinPlan, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return joinPlan{}, err
	}
	if !ok || !rec.Live() {
		return joinPlan{}, fmt.Errorf("no AMQ identity for this Rex block (%s), so there is no agent to join a crew", loc.Block)
	}
	if e.Crews == nil {
		return joinPlan{}, errors.New("no crews are available")
	}
	blk, err := e.Rex.InspectBlock(ctx, loc.Block)
	if err != nil {
		return joinPlan{}, err
	}
	role = crew.Name(role)
	var c crew.Crew
	if name := crew.Name(crewName); name != "" {
		var found bool
		if c, found, err = e.Crews(e.agentDir(ctx, rec)).Crew(name); err != nil {
			return joinPlan{}, err
		}
		if !found {
			return joinPlan{}, fmt.Errorf("no crew %q: `rex-amq-adapter crew list` lists them", name)
		}
		c.Name = name
		if role == "" {
			role = c.Lead
		}
		if role == "" {
			return joinPlan{}, fmt.Errorf("crew %s names no lead: pass --role", name)
		}
		if core.Stealth(role) {
			return joinPlan{}, fmt.Errorf("%w: a %s or %s agent never joins a crew", ErrRefused, core.NarratorRole, core.PersonalRole)
		}
	}
	if role == "" {
		return joinPlan{}, errors.New("name a crew, a role (--role), or both")
	}
	return joinPlan{rec: rec, blk: blk, crew: c, role: role}, nil
}

// JoinPreview is what `agent join --dry-run` reports.
type JoinPreview struct {
	Handle core.Handle `json:"handle"`
	Crew   string      `json:"crew,omitempty"`
	Role   string      `json:"role"`
	Label  string      `json:"label"`
	DryRun bool        `json:"dry_run"`
}

// PreviewJoin runs Join's checks on the agent, the crew, the role, and the
// session's crew claim, and changes nothing.
func (e Env) PreviewJoin(ctx context.Context, loc Location, crewName, role string) (JoinPreview, error) {
	p, err := e.joinPlan(ctx, loc, crewName, role)
	if err != nil {
		return JoinPreview{}, err
	}
	if _, err := e.takeRole(ctx, loc, p.rec.Handle, p.blk.SessionID, p.role, p.crew.Name, true); err != nil {
		return JoinPreview{}, err
	}
	return JoinPreview{Handle: p.rec.Handle, Crew: p.crew.Name, Role: p.role, Label: core.PaneLabel(p.rec.Handle, p.role, p.rec.Task), DryRun: true}, nil
}
