package app

import (
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An Epic's lead is the one agent that runs its goal: it hears its grant,
// takes the narrator's --control mail, reads its session's view and grant,
// records the hand-back, runs the watchdog, and hears departures. Which
// agent that is, is one fact of the session's policy (store.SessionPolicy):
// lead_role, written when the Epic opens (epicLead), and lead_handle,
// written when the first agent of that role nobody summoned is adopted
// there (claimLead). sessionLead is the one predicate every one of those
// asks.

// leadRule is who leads a session: an agent nobody summoned, placed in it,
// of role, and, once the lead's pane was adopted, that agent's handle. A
// zero rule (a Hub session's) leads nothing.
type leadRule struct {
	role   string
	handle core.Handle
}

// holds reports whether rec leads its session under l.
func (l leadRule) holds(rec agent) bool {
	return l.role != "" && rec.Parent == "" && placed(rec) && rec.Role == l.role && (l.handle == "" || l.handle == rec.Handle)
}

// leadRuleOf is the lead rule of session key, from its policy. A session
// whose policy names no lead role (one opened before policies recorded it,
// or one no Epic opened) falls back to the rule the adapter used then: its
// orchestrator, live or departed, else its crew's lead (a pair crew's
// implementer); recs is read for that only, and loaded when nil.
func (e Env) leadRuleOf(key core.SessionKey, recs []agent) (leadRule, error) {
	if key.SessionID == "" {
		return leadRule{}, nil
	}
	p, ok, err := e.Store.SessionPolicy(key.SessionID)
	if err != nil {
		return leadRule{}, err
	}
	if ok && p.Hub() {
		return leadRule{}, nil
	}
	if ok && p.LeadRole != "" {
		return leadRule{role: p.LeadRole, handle: p.LeadHandle}, nil
	}
	if recs == nil {
		if recs, err = e.agents(); err != nil {
			return leadRule{}, err
		}
	}
	for _, r := range recs {
		if r.Role == core.OrchestratorRole && r.Server == key.Server && r.SessionID == key.SessionID {
			return leadRule{role: core.OrchestratorRole}, nil
		}
	}
	c, _, err := e.Store.SessionCrew(key.SessionID)
	if err != nil {
		return leadRule{}, err
	}
	return leadRule{role: epicLead(c.Crew)}, nil
}

// sessionLead reports whether rec leads its session (leadRule).
func (e Env) sessionLead(rec agent) (bool, error) {
	if rec.Parent != "" || !placed(rec) {
		return false, nil
	}
	rule, err := e.leadRuleOf(rec.Endpoint().Session(), nil)
	if err != nil {
		return false, fmt.Errorf("tell whether %s leads its session: %w", rec.Handle, err)
	}
	return rule.holds(rec), nil
}

// claimLead records rec as its Epic's lead_handle when rec is the first
// agent of the policy's lead role nobody summoned to be adopted in it, so a
// later agent of that role (a council architect, a replacement pane) never
// leads. Adoption calls it under the lifecycle lock on every adoption, so a
// claim a failed write left out is made on the next hook.
func (e Env) claimLead(rec agent) error {
	if rec.Parent != "" || !placed(rec) {
		return nil
	}
	p, ok, err := e.Store.SessionPolicy(rec.SessionID)
	if err != nil || !ok || !p.Epic() || p.LeadRole == "" || p.LeadHandle != "" || rec.Role != p.LeadRole {
		return err
	}
	if p.Kind == "" {
		p.Kind = store.PolicyEpic
	}
	p.LeadHandle = rec.Handle
	if err := e.Store.PutSessionPolicy(p); err != nil {
		return fmt.Errorf("record %s as the lead of %s: %w", rec.Handle, rec.SessionID, err)
	}
	return nil
}

// whoIs names rec's role, and that it was summoned, for a refusal.
func whoIs(rec agent) string {
	role := rec.Role
	if role == "" {
		role = "of no role"
	}
	if rec.Parent != "" {
		return fmt.Sprintf("%q, summoned by %s", role, rec.Parent)
	}
	return fmt.Sprintf("%q", role)
}
