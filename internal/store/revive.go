package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// PendingRevive is a launch `epic revive` started and has not yet seen
// adopted: the block it typed the resume into or opened for it, the
// conversation it resumes there, and how (the revive's action). A retry
// that finds it, with that block still running something other than a
// shell, waits for it instead of starting the conversation a second time.
type PendingRevive struct {
	Block          string    `json:"block"`
	AgentSessionID string    `json:"agent_session_id"`
	Action         string    `json:"action"`
	At             time.Time `json:"at"`
}

func (p Paths) revivePath(id string) string { return filepath.Join(p.SessionDir(id), "revive.json") }

// PendingRevives reads session id's pending revive launches, by handle;
// none is an empty map. The caller holds LockEpicLifecycle.
func (s Store) PendingRevives(id string) (map[core.Handle]PendingRevive, error) {
	out := map[core.Handle]PendingRevive{}
	if _, err := readJSON(s.P.revivePath(id), &out); err != nil {
		return nil, fmt.Errorf("the pending revives of %s: %w", id, err)
	}
	return out, nil
}

// PutPendingRevives records session id's pending revive launches whole; an
// empty map removes the record. The caller holds LockEpicLifecycle.
func (s Store) PutPendingRevives(id string, pending map[core.Handle]PendingRevive) error {
	if len(pending) == 0 {
		if err := os.Remove(s.P.revivePath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSON(s.P.revivePath(id), pending)
}
