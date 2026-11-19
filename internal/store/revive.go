package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// PendingRevive is a launch `epic revive` committed to and has not seen
// adopted, written before the launch so a retry can tell what it did
// whatever the launch's reply said or whether the revive lived to read it:
//
//   - Script is the one-shot launcher the launch runs, which removes itself
//     as it starts: while it exists the launch has not run.
//   - Block is the block it runs in: the agent's own (in place), or the tab
//     Rex opened for it; "" while that tab's id is not known, and then the
//     tab is the one new block (not in PriorBlocks) labelled Label.
//   - ShellPID is the login shell an in-place launch was typed into: that
//     shell in the foreground again means the agent ran and exited.
type PendingRevive struct {
	Block          string    `json:"block,omitempty"`
	AgentSessionID string    `json:"agent_session_id"`
	Action         string    `json:"action"`
	Script         string    `json:"script"`
	Label          string    `json:"label,omitempty"`
	PriorBlocks    []string  `json:"prior_blocks,omitempty"`
	ShellPID       int       `json:"shell_pid,omitempty"`
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
