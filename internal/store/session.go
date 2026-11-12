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

// A session's narrator state lives beside the gateway's, in the AMQ root's
// extension area, which amq cleanup leaves alone:
//
//   - sessions/<session_id>/view.json: the session's view, which its narrator
//     writes and its linked personal agent reads (written whole, atomically).
//   - sessions/<session_id>/pulse.json: the pulse the session's watcher last
//     told the narrator about, and the notice that told it (PulseCursor).

// SessionDir is the extension directory of Rex session id.
func (p Paths) SessionDir(id string) string {
	return filepath.Join(p.Extensions(), "sessions", fileKey(id))
}

// ViewPath is where the view of Rex session id is kept.
func (p Paths) ViewPath(id string) string { return filepath.Join(p.SessionDir(id), "view.json") }

func (p Paths) pulsePath(id string) string { return filepath.Join(p.SessionDir(id), "pulse.json") }

// PulseCursor is what a session's watcher last told its narrator: the pulse,
// and the id of the notice that carried it, so a routine change waits while
// that notice is still unread.
type PulseCursor struct {
	Pulse  core.SessionPulse `json:"pulse"`
	Notice string            `json:"notice,omitempty"`
	At     time.Time         `json:"at"`
}

// Pulse reads session id's pulse cursor; ok is false before the first.
func (s Store) Pulse(id string) (PulseCursor, bool, error) {
	var c PulseCursor
	b, err := os.ReadFile(s.P.pulsePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, false, fmt.Errorf("decode the pulse of %s: %w", id, err)
	}
	return c, true, nil
}

// PutPulse records session id's pulse cursor.
func (s Store) PutPulse(id string, c PulseCursor) error {
	return writeJSON(s.P.pulsePath(id), c)
}

// PutView writes session id's view whole: a reader sees the old view or the
// new one, never a mix.
func (s Store) PutView(id string, v any) error {
	return writeJSON(s.P.ViewPath(id), v)
}

// ReadView returns session id's view as written; ok is false when none was.
func (s Store) ReadView(id string) (json.RawMessage, bool, error) {
	b, err := os.ReadFile(s.P.ViewPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}
