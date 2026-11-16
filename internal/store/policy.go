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

// EpicPolicy is the external-write policy of an Epic's Rex session. With
// Unattended, every agent the session launches or resumes, other than a
// personal agent, runs with ME_UNATTENDED=1: a request for engineering work
// authorizes no later outward message, so those stage for the human instead.
// CreatedBy is the personal agent whose epic opened the session.
type EpicPolicy struct {
	SessionID  string      `json:"session_id"`
	Server     string      `json:"server,omitempty"`
	Unattended bool        `json:"unattended"`
	CreatedBy  core.Handle `json:"created_by,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// EpicPolicy reads the policy of Rex session sessionID; ok is false when the
// session has none (no epic opened it).
func (s Store) EpicPolicy(sessionID string) (EpicPolicy, bool, error) {
	var p EpicPolicy
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
		return p, false, fmt.Errorf("decode the Epic policy of %s: %w", sessionID, err)
	}
	return p, true, nil
}

// PutEpicPolicy records p as its session's policy, whole: a reader sees the
// old policy or the new one, never a mix.
func (s Store) PutEpicPolicy(p EpicPolicy) error {
	if p.SessionID == "" {
		return errors.New("an Epic policy needs its session")
	}
	return writeJSON(s.P.policyPath(p.SessionID), p)
}
