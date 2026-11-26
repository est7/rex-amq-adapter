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
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// TurnRecord is the per-handle hook-driven turn ledger stored under
// <home>/turn/<handle>.json. It is written atomically without a read-path
// flock so hooks and activity readers never block each other. Callers compare
// Incarnation against the live lease's Incarnation and discard stale records.
type TurnRecord struct {
	Seq         uint64         `json:"seq"`
	Phase       core.TurnPhase `json:"phase"`
	At          time.Time      `json:"at"`
	Incarnation Incarnation    `json:"incarnation"`
	SessionID   string         `json:"session_id,omitempty"`
}

func (p Paths) turns() string { return filepath.Join(p.Home, "turn") }

func (s Store) turnPath(h core.Handle) string {
	return filepath.Join(s.P.turns(), fileKey(string(h))+".json")
}

// Turn reads h's turn ledger record; ok is false when none has been written.
func (s Store) Turn(h core.Handle) (rec TurnRecord, ok bool, err error) {
	b, err := os.ReadFile(s.turnPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return TurnRecord{}, false, nil
	}
	if err != nil {
		return TurnRecord{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return TurnRecord{}, false, fmt.Errorf("%s: %w", s.turnPath(h), err)
	}
	if rec.Phase != core.TurnWorking && rec.Phase != core.TurnIdle {
		return TurnRecord{}, false, fmt.Errorf("%s: invalid turn phase %q", s.turnPath(h), rec.Phase)
	}
	return rec, true, nil
}

// PutTurn atomically writes h's turn ledger record.
func (s Store) PutTurn(h core.Handle, rec TurnRecord) error {
	if rec.Phase != core.TurnWorking && rec.Phase != core.TurnIdle {
		return fmt.Errorf("invalid turn phase %q", rec.Phase)
	}
	rec.At = rec.At.UTC()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.turnPath(h), append(b, '\n'))
}

// ClearTurn removes h's turn ledger record if present.
func (s Store) ClearTurn(h core.Handle) error {
	err := os.Remove(s.turnPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
