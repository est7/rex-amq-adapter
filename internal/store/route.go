package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// A Hub's narrators reach whichever of its personal agents is live (C26):
//
//   - activity/<handle>: touched at each prompt a personal agent submits;
//     its mtime is when the user last spoke to it. Derived state, written
//     without the lease's CAS: a lost touch only makes the recency older.
//   - extensions/rex-amq-adapter/redelivered/<msg_id>.json: a narrator's
//     message left unread by a personal agent that departed, delivered again
//     to a live personal agent of the Hub (Redelivery), so delivering it
//     again is skipped: at least once, deduplicated by the original id.

func (p Paths) activity() string    { return filepath.Join(p.Home, "activity") }
func (p Paths) redelivered() string { return filepath.Join(p.Extensions(), "redelivered") }

// TouchActivity records that h's user spoke to it now.
func (s Store) TouchActivity(h core.Handle, at time.Time) error {
	path := filepath.Join(s.P.activity(), fileKey(string(h)))
	if err := os.Chtimes(path, at, at); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.P.activity(), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(path, nil); err != nil {
		return err
	}
	return os.Chtimes(path, at, at)
}

// ActivityAt is when h's user last spoke to it; ok is false when never.
func (s Store) ActivityAt(h core.Handle) (time.Time, bool, error) {
	st, err := os.Stat(filepath.Join(s.P.activity(), fileKey(string(h))))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return st.ModTime(), true, nil
}

// Redelivery is a message delivered again: the original's id and mailbox,
// and the new message's id and recipient.
type Redelivery struct {
	OrigID string      `json:"orig_id"`
	From   core.Handle `json:"from"`
	Left   core.Handle `json:"left"`
	NewID  string      `json:"new_id"`
	To     core.Handle `json:"to"`
	At     time.Time   `json:"at"`
}

func (s Store) redeliveryPath(origID string) string {
	return filepath.Join(s.P.redelivered(), fileKey(origID)+".json")
}

// Redelivered reads the redelivery of message origID; ok is false when it
// was not delivered again.
func (s Store) Redelivered(origID string) (Redelivery, bool, error) {
	var r Redelivery
	ok, err := readJSON(s.redeliveryPath(origID), &r)
	return r, ok, err
}

// PutRedelivery records r.
func (s Store) PutRedelivery(r Redelivery) error {
	if r.OrigID == "" || r.NewID == "" {
		return errors.New("a redelivery needs the original id and the new one")
	}
	return writeJSON(s.redeliveryPath(r.OrigID), r)
}
