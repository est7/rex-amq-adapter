// Package store owns the adapter's state directory: one record per adopted
// block (the single source of truth for an agent's identity), per-handle
// announcement ledgers and injector logs, and the locks that serialise them.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// Paths locates the state directory.
type Paths struct{ Home string }

// DefaultPaths honours REX_AMQ_ADAPTER_HOME, else ~/.local/state/rex-amq-adapter.
func DefaultPaths() (Paths, error) {
	if h := os.Getenv("REX_AMQ_ADAPTER_HOME"); h != "" {
		return Paths{Home: h}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{Home: filepath.Join(home, ".local", "state", "rex-amq-adapter")}, nil
}

func (p Paths) Root() string      { return filepath.Join(p.Home, "amq-root") }
func (p Paths) Logs() string      { return filepath.Join(p.Home, "logs") }
func (p Paths) records() string   { return filepath.Join(p.Home, "records") }
func (p Paths) ledgers() string   { return filepath.Join(p.Home, "ledger") }
func (p Paths) injects() string   { return filepath.Join(p.Home, "inject") }
func (p Paths) lifecycle() string { return filepath.Join(p.Home, "lifecycle.lock") }
func (p Paths) pending() string   { return filepath.Join(p.Home, "pending") }
func (p Paths) failures() string  { return filepath.Join(p.Home, "adopt-failed") }

// PathsForRoot locates the state directory from its AMQ root. The injector
// uses it: amq wake passes the root explicitly, while the environment a
// repaired waker runs the injector with carries neither REX_AMQ_ADAPTER_HOME
// nor anything else the adapter set.
func PathsForRoot(root string) (Paths, error) {
	if !filepath.IsAbs(root) || filepath.Base(root) != "amq-root" {
		return Paths{}, fmt.Errorf("%q is not an adapter AMQ root (an absolute path ending in amq-root)", root)
	}
	return Paths{Home: filepath.Dir(root)}, nil
}

// Record binds an AMQ handle to one agent process in one Rex block. It holds
// only facts the adapter owns; the waker's identity and liveness belong to
// amq and are read with `amq wake check` when needed.
type Record struct {
	Handle         core.Handle `json:"handle"`
	Kind           core.Kind   `json:"kind"`
	Server         string      `json:"server"`
	SessionID      string      `json:"session_id"`
	BlockID        string      `json:"block_id"`
	AgentPID       int         `json:"agent_pid"`
	AgentSessionID string      `json:"agent_session_id,omitempty"`
	AdoptedAt      time.Time   `json:"adopted_at"`
	// DepartedAt is set when the agent ended. A departed record keeps its
	// handle reserved (and sticky for its block) until collected.
	DepartedAt *time.Time `json:"departed_at,omitempty"`
}

// Live reports whether the record describes a running agent.
func (r Record) Live() bool { return r.DepartedAt == nil }

// Store reads and writes state under Paths.
type Store struct{ P Paths }

func fileKey(id string) string { return strings.NewReplacer(":", "_", "/", "_").Replace(id) }

func (s Store) recordPath(block string) string {
	return filepath.Join(s.P.records(), fileKey(block)+".json")
}

// Record returns the record for block; ok is false when there is none.
func (s Store) Record(block string) (Record, bool, error) {
	b, err := os.ReadFile(s.recordPath(block))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, false, fmt.Errorf("decode record %s: %w", block, err)
	}
	return r, true, nil
}

func (s Store) PutRecord(r Record) error {
	if r.BlockID == "" || r.Handle == "" {
		return errors.New("record needs a block id and a handle")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.recordPath(r.BlockID), append(b, '\n'))
}

func (s Store) DeleteRecord(block string) error {
	err := os.Remove(s.recordPath(block))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Records lists every record, sorted by handle. An unreadable record is an
// error rather than skipped: it may hold the generation needed to retire a
// waker.
func (s Store) Records() ([]Record, error) {
	entries, err := os.ReadDir(s.P.records())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.P.records(), e.Name()))
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("decode record %s: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// LockLifecycle serialises adoption, departure, and collection.
func (s Store) LockLifecycle(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, s.P.lifecycle())
}

// InjectEntry is one injector outcome.
type InjectEntry struct {
	At time.Time `json:"at"`
	core.Outcome
}

// RecordInject appends an injector outcome to the handle's log.
func (s Store) RecordInject(h core.Handle, o core.Outcome, at time.Time) error {
	if err := os.MkdirAll(s.P.injects(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(InjectEntry{At: at.UTC(), Outcome: o})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.P.injects(), string(h)+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// LockLedger serialises the read-modify-write of one handle's ledger; hooks
// and the injector update it concurrently.
func (s Store) LockLedger(ctx context.Context, h core.Handle) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.ledgers(), "."+string(h)+".lock"))
}

// Ledger is what h has been told: the ids announced, still unread, and when
// the last announcement was made.
type Ledger struct {
	IDs map[string]bool
	At  time.Time // zero: never
}

// Announced returns h's ledger; a handle never told anything has an empty one.
func (s Store) Announced(h core.Handle) (Ledger, error) {
	b, err := os.ReadFile(filepath.Join(s.P.ledgers(), string(h)+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Ledger{IDs: map[string]bool{}}, nil
	}
	if err != nil {
		return Ledger{}, err
	}
	var doc struct {
		Announced []string  `json:"announced"`
		At        time.Time `json:"at"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Ledger{}, fmt.Errorf("decode ledger for %s: %w", h, err)
	}
	l := Ledger{IDs: make(map[string]bool, len(doc.Announced)), At: doc.At}
	for _, id := range doc.Announced {
		l.IDs[id] = true
	}
	return l, nil
}

// SetAnnounced replaces h's ledger after an announcement made at at. Callers
// pass only ids still unread, so drained ids fall out on every write.
func (s Store) SetAnnounced(h core.Handle, ids []string, at time.Time) error {
	sorted := append([]string{}, ids...)
	sort.Strings(sorted)
	b, err := json.Marshal(struct {
		Announced []string  `json:"announced"`
		At        time.Time `json:"at"`
	}{sorted, at.UTC()})
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(s.P.ledgers(), string(h)+".json"), append(b, '\n'))
}

// Pending is a doorbell written into a block and not yet known to be
// submitted or cleared. The next delivery attempt erases it from the box
// first, so a new doorbell never merges with it.
func (s Store) Pending(block string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(s.P.pending(), fileKey(block)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

func (s Store) SetPending(block, doorbell string) error {
	return fsutil.WriteAtomic(filepath.Join(s.P.pending(), fileKey(block)), []byte(doorbell))
}

func (s Store) ClearPending(block string) error {
	err := os.Remove(filepath.Join(s.P.pending(), fileKey(block)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// AdoptFailedAt is when adopting the agent in block last failed.
func (s Store) AdoptFailedAt(block string) (time.Time, bool, error) {
	b, err := os.ReadFile(filepath.Join(s.P.failures(), fileKey(block)))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("decode adoption failure time for %s: %w", block, err)
	}
	return t, true, nil
}

func (s Store) SetAdoptFailed(block string, at time.Time) error {
	return fsutil.WriteAtomic(filepath.Join(s.P.failures(), fileKey(block)), []byte(at.UTC().Format(time.RFC3339Nano)))
}

func (s Store) ClearAdoptFailed(block string) error {
	err := os.Remove(filepath.Join(s.P.failures(), fileKey(block)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
