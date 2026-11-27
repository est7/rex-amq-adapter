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
//     writes and its linked personal agent reads (written whole, atomically),
//     including optional derived Slice progress (EpicProgress, progress.go).
//   - sessions/<session_id>/pulse.json: the pulse the session's watcher last
//     told the narrator about, and the notice that told it (PulseCursor).
//   - sessions/<session_id>/crew.json: the crew the session works as
//     (SessionCrew), claimed by join CREW or epic --crew.
//   - sessions/<session_id>/hand-back.json: the lead's latest hand-back
//     (HandBack), which the view carries whole.
//   - sessions/<session_id>/artifacts/<seq>/: the artifacts a hand-back
//     carried (artifact.go), and materialized.json beside them.
//   - sessions/<session_id>/policy.json: the Epic's external-write policy
//     (EpicPolicy), written when epic creates the session.
//   - sessions/<session_id>/lifecycle.json: the Epic's lifecycle
//     (EpicLifecycle, epic_lifecycle.go).

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

func (p Paths) crewPath(id string) string { return filepath.Join(p.SessionDir(id), "crew.json") }

// SessionCrew is the crew a Rex session (an Epic) works as. A session has at
// most one: its conventions brief every agent of the session that is not
// Stealth, in whichever tab it runs. ClaimedBy is the agent whose join or epic
// claimed it.
type SessionCrew struct {
	Crew      string      `json:"crew"`
	ClaimedBy core.Handle `json:"claimed_by,omitempty"`
	ClaimedAt time.Time   `json:"claimed_at"`
}

// SessionCrew reads session id's crew; ok is false when none was claimed.
func (s Store) SessionCrew(id string) (SessionCrew, bool, error) {
	var c SessionCrew
	b, err := os.ReadFile(s.P.crewPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, false, fmt.Errorf("decode the crew of %s: %w", id, err)
	}
	return c, c.Crew != "", nil
}

// PutSessionCrew records session id's crew. Callers hold the lifecycle lock,
// which makes a check of the current crew and the write one step.
func (s Store) PutSessionCrew(id string, c SessionCrew) error {
	if id == "" || c.Crew == "" {
		return errors.New("a session crew needs its session and its crew")
	}
	return writeJSON(s.P.crewPath(id), c)
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

func (p Paths) handBackPath(id string) string {
	return filepath.Join(p.SessionDir(id), "hand-back.json")
}

// HandBack is the final report an Epic's lead recorded when its run ended:
// the whole text, as written, for the session's view. Seq numbers the
// session's distinct hand-backs from 1, and Digest is what tells a new one
// from the same one recorded again: the report's (HandBackDigest), folded
// with its Artifacts' (HandBackWithArtifacts) when it carries any, so a
// revision of the artifacts alone is a new hand-back. A file written before
// Seq and Digest were kept has neither. OpHash is the SHA-256 of the
// --op-id the hand-back was recorded under, "" for none.
type HandBack struct {
	By        core.Handle     `json:"by"`
	At        time.Time       `json:"at"`
	Body      string          `json:"body"`
	Seq       int             `json:"seq,omitempty"`
	Digest    string          `json:"digest,omitempty"`
	Artifacts []ArtifactEntry `json:"artifacts,omitempty"`
	OpHash    string          `json:"op_hash,omitempty"`
}

// HandBack reads session id's latest hand-back; ok is false when none was
// recorded.
func (s Store) HandBack(id string) (HandBack, bool, error) {
	var h HandBack
	b, err := os.ReadFile(s.P.handBackPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, false, fmt.Errorf("decode the hand-back of %s: %w", id, err)
	}
	return h, true, nil
}

// PutHandBack records h as session id's latest hand-back, replacing the one
// before, whole: a reader sees the old one or the new one, never a mix.
func (s Store) PutHandBack(id string, h HandBack) error {
	if id == "" || h.By == "" || h.Body == "" {
		return errors.New("a hand-back needs its session, its author, and its body")
	}
	return writeJSON(s.P.handBackPath(id), h)
}
