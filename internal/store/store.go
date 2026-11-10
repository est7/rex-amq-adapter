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
func (p Paths) window() string    { return filepath.Join(p.Home, "window") }
func (p Paths) titles() string    { return filepath.Join(p.Home, "title") }
func (p Paths) taskAsked() string { return filepath.Join(p.Home, "task-asked") }

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

// Record binds an AMQ handle to one agent session, and while that session runs,
// to its process in one Rex block. It holds only facts the adapter owns; the
// waker's identity and liveness belong to amq and are read with `amq wake
// check` when needed. Records are stored by handle, so a handle has at most
// one record: a departed record keeps the handle for its agent session after
// the block is gone.
type Record struct {
	Handle  core.Handle `json:"handle"`
	Kind    core.Kind   `json:"kind"`
	Server  string      `json:"server"`
	BlockID string      `json:"block_id"`
	// SessionID and WindowID are the Rex session and window (tab) the block
	// was last seen in: at adoption, then whenever its session is reconciled.
	// The session is the agent's awareness boundary: departure tells the agents
	// recorded in it, because a crashed agent's block can no longer be placed.
	SessionID      string `json:"session_id,omitempty"`
	WindowID       string `json:"window_id,omitempty"`
	AgentPID       int    `json:"agent_pid"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// Role is what the agent is for ("reviewer", "goal"; "" for none): the
	// launcher's pane label at adoption, or a crew role taken with join. It
	// chooses the agent's briefing and makes a goal agent; the pane label
	// shows it before the handle.
	Role string `json:"role,omitempty"`
	// Task is what the agent said it is doing (core.Task), shown after the
	// handle in its pane label. It follows the handle, so a resume in another
	// pane shows it again and a newly named agent starts without one.
	Task      string    `json:"task,omitempty"`
	AdoptedAt time.Time `json:"adopted_at"`
	// DepartedAt is set when the agent ended. A departed record keeps its
	// handle reserved for its agent session (a resume, in any block) until
	// collected.
	DepartedAt *time.Time `json:"departed_at,omitempty"`
	// DepartReason says why: DepartSessionEnd, DepartReplaced, or the binding
	// check's code when collection found the agent gone (block_gone,
	// agent_gone, agent_left_pane). Shown to observers; nothing decides on it.
	DepartReason string `json:"depart_reason,omitempty"`
	// Unfinished names the transition this record was committed in the middle
	// of, so whoever reads it next can finish it.
	Unfinished Transition `json:"unfinished,omitempty"`
}

// Transition is a change of an agent's state with steps after the record is
// written: a process can die, or a hook run out of time, between them.
type Transition string

const (
	Adopting  Transition = "adoption"
	Departing Transition = "departure"
)

// Reasons a departure records that are not a binding check's code.
const (
	DepartSessionEnd = "session_end" // the harness ended the session
	DepartReplaced   = "replaced"    // another agent or session took the block
)

// Live reports whether the record describes a running agent.
func (r Record) Live() bool { return r.DepartedAt == nil }

// Adopted reports whether r's agent finished adoption: it has its waker and
// was told who it is. A live record that has not has a mailbox, and no more.
func (r Record) Adopted() bool { return r.Live() && r.Unfinished != Adopting }

// Store reads and writes state under Paths.
type Store struct{ P Paths }

func fileKey(id string) string { return strings.NewReplacer(":", "_", "/", "_").Replace(id) }

func (s Store) recordPath(h core.Handle) string {
	return filepath.Join(s.P.records(), fileKey(string(h))+".json")
}

// Record returns the record bound to block: its live one, else the one that
// departed last. ok is false when there is none.
func (s Store) Record(block string) (Record, bool, error) {
	recs, err := s.readRecords()
	if err != nil {
		return Record{}, false, err
	}
	var best Record
	ok := false
	for _, r := range recs {
		if r.BlockID != block {
			continue
		}
		if !ok || r.Live() || (!best.Live() && r.DepartedAt.After(*best.DepartedAt)) {
			best, ok = r, true
		}
		if r.Live() {
			break
		}
	}
	return best, ok, nil
}

// PutRecord writes r as its handle's only record.
func (s Store) PutRecord(r Record) error {
	if r.BlockID == "" || r.Handle == "" {
		return errors.New("record needs a block id and a handle")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.recordPath(r.Handle), append(b, '\n'))
}

// DeleteRecord forgets handle h: its record and its delivery history (the
// announcement ledger and the inject log), so a later agent given h starts
// clean. Its mailbox is amq's and stays.
func (s Store) DeleteRecord(h core.Handle) error {
	for _, p := range []string{s.recordPath(h), s.ledgerPath(h), s.sentCursorPath(h), s.injectLog(h), s.titlePath(h), s.taskAskedPath(h)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Records lists every record, sorted by handle. An unreadable record is an
// error rather than skipped: it may hold the generation needed to retire a
// waker.
func (s Store) Records() ([]Record, error) {
	out, err := s.readRecords()
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
}

// RecordsChanged is when a record was last written or removed: the records
// directory's modification time, which every write moves because it renames a
// file into place. Zero when there is no directory yet.
func (s Store) RecordsChanged() (time.Time, error) {
	st, err := os.Stat(s.P.records())
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// readRecords reads every record file (recordPath: one per handle).
func (s Store) readRecords() ([]Record, error) {
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
	return out, nil
}

// LockWatch is held by h's goal watcher, so there is at most one.
func (s Store) LockWatch(ctx context.Context, h core.Handle) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.Home, "watch", string(h)+".lock"))
}

// LockLifecycle serialises adoption, departure, and collection.
func (s Store) LockLifecycle(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, s.P.lifecycle())
}

// The inject log keeps every doorbell attempt for diagnosis; a retry ladder
// appends for as long as mail waits, so past maxInjectLog it is cut to its
// newest lines, at most keepInjectRuns of them and half the cap in bytes, so
// a trim leaves room and stays rare.
const (
	maxInjectLog   = 1 << 20
	keepInjectRuns = 1000
)

func (s Store) ledgerPath(h core.Handle) string {
	return filepath.Join(s.P.ledgers(), string(h)+".json")
}

func (s Store) injectLog(h core.Handle) string {
	return filepath.Join(s.P.injects(), string(h)+".jsonl")
}

// RecordInject appends an injector outcome to the handle's log. Only the
// handle's one waker runs the injector, so the log has a single writer.
func (s Store) RecordInject(h core.Handle, o core.Outcome, at time.Time) error {
	if err := os.MkdirAll(s.P.injects(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(core.Attempt{At: at.UTC(), Outcome: o})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.injectLog(h), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	st, serr := f.Stat()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil || serr != nil || st.Size() <= maxInjectLog {
		return werr
	}
	return s.trimInjectLog(h)
}

func (s Store) trimInjectLog(h core.Handle) error {
	b, err := os.ReadFile(s.injectLog(h))
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(strings.TrimRight(string(b), "\n"), "\n")
	start, size := len(lines), 0
	for start > 0 && len(lines)-start < keepInjectRuns && size+len(lines[start-1]) <= maxInjectLog/2 {
		start--
		size += len(lines[start])
	}
	kept := strings.Join(lines[start:], "")
	return fsutil.WriteAtomic(s.injectLog(h), []byte(strings.TrimRight(kept, "\n")+"\n"))
}

// InjectHistory returns h's doorbell attempts, oldest first. A line that
// cannot be read (a crash mid-append) is skipped: the log is diagnostic.
func (s Store) InjectHistory(h core.Handle) ([]core.Attempt, error) {
	b, err := os.ReadFile(s.injectLog(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []core.Attempt
	for _, line := range strings.Split(string(b), "\n") {
		var a core.Attempt
		if line != "" && json.Unmarshal([]byte(line), &a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
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
	b, err := os.ReadFile(s.ledgerPath(h))
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
	return fsutil.WriteAtomic(s.ledgerPath(h), append(b, '\n'))
}

func (s Store) sentCursorPath(h core.Handle) string {
	return filepath.Join(s.P.ledgers(), string(h)+".sent")
}

// SentCursor is the id of the last message of h's checked for a departed
// recipient, "" when h had sent none when checking began. ok is false when
// checking never began for h.
func (s Store) SentCursor(h core.Handle) (id string, ok bool, err error) {
	b, err := os.ReadFile(s.sentCursorPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// SetSentCursor records that h's messages up to id have been checked.
func (s Store) SetSentCursor(h core.Handle, id string) error {
	return fsutil.WriteAtomic(s.sentCursorPath(h), []byte(id))
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

// AddWindowNote queues note for h's next prompt: an agent started beside it.
// Callers hold h's ledger lock, which also guards TakeWindowNotes.
func (s Store) AddWindowNote(h core.Handle, note string) error {
	if err := os.MkdirAll(s.P.window(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.windowPath(h), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(note + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// TakeWindowNotes returns h's queued notes, oldest first, and clears them.
func (s Store) TakeWindowNotes(h core.Handle) ([]string, error) {
	b, err := os.ReadFile(s.windowPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.ClearWindowNotes(h); err != nil {
		return nil, err
	}
	var notes []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			notes = append(notes, line)
		}
	}
	return notes, nil
}

// ClearWindowNotes drops h's queued notes.
func (s Store) ClearWindowNotes(h core.Handle) error {
	err := os.Remove(s.windowPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s Store) windowPath(h core.Handle) string {
	return filepath.Join(s.P.window(), fileKey(string(h)))
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

// TitlesShown are the titles the adapter last gave h's harness, by the
// session that received each: a handle can pass to a new session in the
// same process (/clear, /new), which starts without a title. The caller
// holds h's ledger lock.
func (s Store) TitlesShown(h core.Handle) (map[string]string, error) {
	shown := map[string]string{}
	b, err := os.ReadFile(s.titlePath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return shown, nil
	}
	if err != nil {
		return nil, err
	}
	return shown, json.Unmarshal(b, &shown)
}

// SetTitlesShown records them.
func (s Store) SetTitlesShown(h core.Handle, shown map[string]string) error {
	if err := os.MkdirAll(s.P.titles(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(shown)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.titlePath(h), append(b, '\n'))
}

func (s Store) titlePath(h core.Handle) string {
	return filepath.Join(s.P.titles(), fileKey(string(h)))
}

// TaskAsked is the last session of h that was asked at its first prompt to
// show its task, "" when none was. The caller holds h's ledger lock.
func (s Store) TaskAsked(h core.Handle) (string, error) {
	b, err := os.ReadFile(s.taskAskedPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// SetTaskAsked records that session of h was asked.
func (s Store) SetTaskAsked(h core.Handle, session string) error {
	if err := os.MkdirAll(s.P.taskAsked(), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.taskAskedPath(h), []byte(session+"\n"))
}

func (s Store) taskAskedPath(h core.Handle) string {
	return filepath.Join(s.P.taskAsked(), fileKey(string(h)))
}

// LockInject serialises the writers of one block's prompt: the injector that
// amq wake runs and the goal watchdog, which re-reads the block under it.
func (s Store) LockInject(ctx context.Context, block string) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.injects(), "."+fileKey(block)+".lock"))
}

func (p Paths) watchdog() string { return filepath.Join(p.Home, "watchdog") }

func (s Store) episodePath(block string) string {
	return filepath.Join(s.P.watchdog(), fileKey(block)+".json")
}

// Episode returns the open watchdog episode of block, if any.
func (s Store) Episode(block string) (core.Episode, bool, error) {
	b, err := os.ReadFile(s.episodePath(block))
	if errors.Is(err, fs.ErrNotExist) {
		return core.Episode{}, false, nil
	}
	if err != nil {
		return core.Episode{}, false, err
	}
	var ep core.Episode
	if err := json.Unmarshal(b, &ep); err != nil {
		return core.Episode{}, false, fmt.Errorf("%s: %w", s.episodePath(block), err)
	}
	return ep, true, nil
}

// PutEpisode writes an episode before and after each of its steps, so a
// watchdog that restarts mid-episode continues it instead of repeating it.
func (s Store) PutEpisode(ep core.Episode) error {
	b, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.episodePath(ep.Block), b)
}

// ClearEpisode ends block's episode.
func (s Store) ClearEpisode(block string) error {
	err := os.Remove(s.episodePath(block))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// HealthMark is what the watchdog last told the goal agent about an
// agent's transcript, so each finding is reported once.
type HealthMark struct {
	LastErrorID string    `json:"last_error_id,omitempty"`
	LastHungID  string    `json:"last_hung_id,omitempty"`
	LastStallAt time.Time `json:"last_stall_at,omitempty"`
}

func (s Store) healthPath(block string) string {
	return filepath.Join(s.P.watchdog(), fileKey(block)+".health.json")
}

// HealthMark returns block's mark; a block never reported has an empty one.
func (s Store) HealthMark(block string) (HealthMark, error) {
	b, err := os.ReadFile(s.healthPath(block))
	if errors.Is(err, fs.ErrNotExist) {
		return HealthMark{}, nil
	}
	if err != nil {
		return HealthMark{}, err
	}
	var m HealthMark
	if err := json.Unmarshal(b, &m); err != nil {
		return HealthMark{}, fmt.Errorf("%s: %w", s.healthPath(block), err)
	}
	return m, nil
}

func (s Store) PutHealthMark(block string, m HealthMark) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.healthPath(block), b)
}
