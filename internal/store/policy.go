package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func (p Paths) policyPath(id string) string { return filepath.Join(p.SessionDir(id), "policy.json") }

// Kinds of SessionPolicy.
const (
	// PolicyHub: the session is a Hub's, which holds personal agents only.
	PolicyHub = "hub"
	// PolicyEpic: the session is an Epic's, which holds no personal agent.
	PolicyEpic = "epic"
)

// SessionPolicy is what a Rex session is for (Claim C25): a Hub's, which
// holds only personal agents, or an Epic's, which holds the crew that works
// its goal and never a personal agent. A record written before policies had
// a kind (Kind "") is an Epic's: only epic wrote one then.
//
// With Unattended, every agent an Epic's session launches or resumes, other
// than a personal agent, runs with ME_UNATTENDED=1: a request for
// engineering work authorizes no later outward message, so those stage for
// the human instead. CreatedBy is the personal agent whose epic opened the
// session, or the first personal agent a Hub session adopted. HubKey names
// the Hub of a Hub session; LeadRole, LeadHandle, and Crew describe an Epic.
// A Hub session's policy stays when its last personal agent leaves.
type SessionPolicy struct {
	Kind       string      `json:"kind,omitempty"`
	SessionID  string      `json:"session_id"`
	Server     string      `json:"server,omitempty"`
	HubKey     core.HubKey `json:"hub_key,omitempty"`
	LeadRole   string      `json:"lead_role,omitempty"`
	LeadHandle core.Handle `json:"lead_handle,omitempty"`
	Crew       string      `json:"crew,omitempty"`
	Unattended bool        `json:"unattended"`
	CreatedBy  core.Handle `json:"created_by,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Hub reports whether p is a Hub session's policy.
func (p SessionPolicy) Hub() bool { return p.Kind == PolicyHub }

// Epic reports whether p is an Epic session's policy, a record without a
// kind included.
func (p SessionPolicy) Epic() bool { return p.Kind == PolicyEpic || p.Kind == "" && p.SessionID != "" }

// SessionPolicy reads the policy of Rex session sessionID; ok is false when
// the session has none (no epic opened it, and no Hub adopted a personal
// agent in it).
func (s Store) SessionPolicy(sessionID string) (SessionPolicy, bool, error) {
	var p SessionPolicy
	if sessionID == "" {
		return p, false, nil
	}
	b, err := os.ReadFile(s.P.policyPath(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, false, fmt.Errorf("decode the session policy (an Epic policy or a Hub policy) of %s: %w", sessionID, err)
	}
	if p.Kind != "" && p.Kind != PolicyHub && p.Kind != PolicyEpic {
		return p, false, fmt.Errorf("the policy of session %s has kind %q, neither %s nor %s", sessionID, p.Kind, PolicyHub, PolicyEpic)
	}
	return p, true, nil
}

// PutSessionPolicy records p as its session's policy, whole: a reader sees
// the old policy or the new one, never a mix.
func (s Store) PutSessionPolicy(p SessionPolicy) error {
	if p.SessionID == "" {
		return errors.New("a session policy needs its session")
	}
	if p.Kind != PolicyHub && p.Kind != PolicyEpic {
		return fmt.Errorf("a session policy is a %s's or an %s's, not %q", PolicyHub, PolicyEpic, p.Kind)
	}
	return writeJSON(s.P.policyPath(p.SessionID), p)
}
