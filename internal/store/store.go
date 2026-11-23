// Package store owns the adapter's state directory: each handle's Identity
// and, while its agent runs, its Lease (identity.go), per-handle
// announcement ledgers and injector logs, per-block pane state, and the locks
// that serialise them.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func (p Paths) Root() string        { return filepath.Join(p.Home, "amq-root") }
func (p Paths) Logs() string        { return filepath.Join(p.Home, "logs") }
func (p Paths) Blobs() string       { return filepath.Join(p.Home, "cache", "blobs") }
func (p Paths) ledgers() string     { return filepath.Join(p.Home, "ledger") }
func (p Paths) injects() string     { return filepath.Join(p.Home, "inject") }
func (p Paths) lifecycle() string   { return filepath.Join(p.Home, "lifecycle.lock") }
func (p Paths) epicOpening() string { return filepath.Join(p.Home, "epic-opening.lock") }
func (p Paths) pending() string     { return filepath.Join(p.Home, "pending") }
func (p Paths) window() string      { return filepath.Join(p.Home, "window") }
func (p Paths) titles() string      { return filepath.Join(p.Home, "title") }
func (p Paths) taskAsked() string   { return filepath.Join(p.Home, "task-asked") }
func (p Paths) titleStates() string { return filepath.Join(p.Home, "title-state") }
func (p Paths) tabs() string        { return filepath.Join(p.Home, "tabs") }

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

// Store reads and writes state under Paths.
type Store struct{ P Paths }

// fileKey is the file name an id is stored under: ":", "/", and a
// backslash become "_", so no id names another directory. An id that would still name no
// file of its own ("", ".", "..") is stored under a hash of itself. Every
// id the adapter stores keeps the key it always had.
func fileKey(id string) string {
	k := strings.NewReplacer(":", "_", "/", "_", "\\", "_").Replace(id)
	if k == "" || strings.Trim(k, ".") == "" {
		sum := sha256.Sum256([]byte(id))
		return "h_" + hex.EncodeToString(sum[:16])
	}
	return k
}

// LockWatch is held by the watcher of session k, so a session has at most
// one, whichever orchestrator or narrator started it.
func (s Store) LockWatch(ctx context.Context, k core.SessionKey) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.Home, "watch", sessionKey(k)+".lock"))
}

// LockLifecycle serialises the writers of identities, leases, and session
// policies. Its holders read and write local files only: a Rex or amq call
// under it makes every hook on the host wait for that call.
func (s Store) LockLifecycle(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, s.P.lifecycle())
}

// LockHandle serialises what is derived from h's lease outside the lifecycle
// lock: its amq waker. Its holder never takes the lifecycle lock.
func (s Store) LockHandle(ctx context.Context, h core.Handle) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.Home, "handles", fileKey(string(h))+".lock"))
}

// LockEpicOpening is held, exclusively, while an Epic's session is created
// and its policy, crew, and grant recorded: its agents start as Rex creates
// the session, before the adapter knows the session's id.
func (s Store) LockEpicOpening(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, s.P.epicOpening())
}

// AwaitEpicOpenings returns once no Epic is being opened: an agent adopted
// in a session of no policy may be one of an opening Epic's, whose policy is
// about to be recorded. Openings exclude only each other's waiters.
func (s Store) AwaitEpicOpenings(ctx context.Context) error {
	unlock, err := fsutil.FlockShared(ctx, s.P.epicOpening())
	if err != nil {
		return err
	}
	unlock()
	return nil
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

// RecordInject appends an injector outcome to the handle's log. Its writers
// (the handle's waker and the orchestrator watchdog) both run the injector
// under the block's inject lock (Env.Inject), which also covers this write
// and its trim.
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

// TitleState is how far h's conversation title got toward its harness,
// derived from TitlesShown and the title h should have; ok is false when it
// was never tracked. The caller holds h's ledger lock.
func (s Store) TitleState(h core.Handle) (core.TitleState, bool, error) {
	var st core.TitleState
	b, err := os.ReadFile(s.titleStatePath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, false, fmt.Errorf("decode the title state of %s: %w", h, err)
	}
	return st, true, nil
}

// SetTitleState records it.
func (s Store) SetTitleState(h core.Handle, st core.TitleState) error {
	if err := os.MkdirAll(s.P.titleStates(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.titleStatePath(h), append(b, '\n'))
}

func (s Store) titleStatePath(h core.Handle) string {
	return filepath.Join(s.P.titleStates(), fileKey(string(h)))
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

// Tab is a Rex tab (window) in the adapter's tab ledger: who names it
// (core.WindowMode) and the label the adapter last gave it. It is not
// window/<handle>, which queues an agent's notes.
type Tab struct {
	WindowID  string          `json:"window_id"`
	SessionID string          `json:"session_id,omitempty"`
	Mode      core.WindowMode `json:"mode"`
	LastLabel string          `json:"last_label,omitempty"`
}

// Modes earlier versions recorded for a tab claimed for a crew or a
// narrator. Tabs no longer name crews (a crew is its session's,
// SessionCrew), so Tab reads either as a follower: its label is the one the
// adapter last gave it, and its agent's pane label replaces it.
const (
	legacyCrewTab     core.WindowMode = "crew"
	legacyNarratorTab core.WindowMode = "narrator"
)

// State is the part of t the follow policy reads (core.FollowTab).
func (t Tab) State() core.TabState { return core.TabState{Mode: t.Mode, Last: t.LastLabel} }

func (s Store) tabPath(window string) string {
	return filepath.Join(s.P.tabs(), fileKey(window)+".json")
}

// Tab is window's ledger entry; ok is false when the ledger does not know it.
func (s Store) Tab(window string) (Tab, bool, error) {
	b, err := os.ReadFile(s.tabPath(window))
	if errors.Is(err, fs.ErrNotExist) {
		return Tab{}, false, nil
	}
	if err != nil {
		return Tab{}, false, err
	}
	var t Tab
	if err := json.Unmarshal(b, &t); err != nil {
		return Tab{}, false, fmt.Errorf("%s: %w", s.tabPath(window), err)
	}
	if t.Mode == legacyCrewTab || t.Mode == legacyNarratorTab {
		t.Mode = core.WindowAuto
	}
	return t, true, nil
}

// PutTab records t. Writers do not lock: following a tab is best effort,
// and the last write wins.
func (s Store) PutTab(t Tab) error {
	if t.WindowID == "" {
		return errors.New("a tab ledger entry needs its window")
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.tabPath(t.WindowID), append(b, '\n'))
}

// LockInject serialises the writers of one block's prompt: the injector that
// amq wake runs and the orchestrator watchdog, which re-reads the block under it.
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

// HealthMark is what the watchdog last told the session about an agent, so
// each finding is reported once. Only the watchdog writes it.
type HealthMark struct {
	// HealthBy is the agent run (turnKey) LastHungID and LastStallAt belong
	// to; another run in the block starts clean.
	HealthBy string `json:"health_by,omitempty"`
	// LastHungID is the tool call id of the last hung call told.
	LastHungID  string    `json:"last_hung_id,omitempty"`
	LastStallAt time.Time `json:"last_stall_at,omitempty"`
	// BlockedSince is when the agent run BlockedBy (turnKey) was first seen
	// on a blocked prompt or a trust screen in the current episode;
	// BlockedTold that it was told. Another run in the block starts its own.
	BlockedSince time.Time `json:"blocked_since,omitempty"`
	BlockedTold  bool      `json:"blocked_told,omitempty"`
	BlockedBy    string    `json:"blocked_by,omitempty"`
}

// LockHealth serialises the read-modify-write of block's HealthMark between
// watchdog runs.
func (s Store) LockHealth(ctx context.Context, block string) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.watchdog(), "."+fileKey(block)+".health.lock"))
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

// TurnError is a stop hook's note that the agent run By (turnKey) of a
// handle ended a turn in an error At; that agent's next turn removes it. It is one value
// in a file of its own, written whole and removed whole, so the hooks need
// no lock and never wait on the watchdog.
type TurnError struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

func (s Store) turnErrorPath(h core.Handle) string {
	return filepath.Join(s.P.Home, "turn-error", fileKey(string(h))+".json")
}

// TurnError returns h's turn error; ok is false when there is none.
func (s Store) TurnError(h core.Handle) (te TurnError, ok bool, err error) {
	b, err := os.ReadFile(s.turnErrorPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return TurnError{}, false, nil
	}
	if err != nil {
		return TurnError{}, false, err
	}
	if err := json.Unmarshal(b, &te); err != nil {
		return TurnError{}, false, fmt.Errorf("%s: %w", s.turnErrorPath(h), err)
	}
	return te, !te.At.IsZero(), nil
}

// PutTurnError replaces h's turn error.
func (s Store) PutTurnError(h core.Handle, te TurnError) error {
	b, err := json.Marshal(te)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.turnErrorPath(h), b)
}

// ClearTurnError removes h's turn error.
func (s Store) ClearTurnError(h core.Handle) error {
	err := os.Remove(s.turnErrorPath(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Summon is what a summoner wrote before launching an agent, keyed by the
// token the launch carries in its environment, for the adoption with that
// token to copy into its identity. Until then it reserves a place in the
// session's cap.
type Summon struct {
	Parent  core.Handle `json:"parent"`
	Depth   int         `json:"depth"`
	For     string      `json:"for"`
	Session string      `json:"session"`
	At      time.Time   `json:"at"`
}

// PendingSummonFor is how long an unconsumed summon reserves a place: a
// launch that was never adopted (the pane closed, trust refused) frees it.
const PendingSummonFor = 2 * time.Minute

func (p Paths) summons() string { return filepath.Join(p.Home, "summons") }

func (s Store) summonPath(token string) string {
	return filepath.Join(s.P.summons(), fileKey(token)+".json")
}

// PutSummon records a launch about to happen under token.
func (s Store) PutSummon(token string, sm Summon) error {
	b, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.summonPath(token), b)
}

// Summon reads the launch under token without consuming its reservation.
// The caller deletes it only after committing the adopting identity.
func (s Store) Summon(token string) (Summon, bool, error) {
	if token == "" {
		return Summon{}, false, nil
	}
	b, err := os.ReadFile(s.summonPath(token))
	if errors.Is(err, fs.ErrNotExist) {
		return Summon{}, false, nil
	}
	if err != nil {
		return Summon{}, false, err
	}
	var sm Summon
	if err := json.Unmarshal(b, &sm); err != nil {
		return Summon{}, false, fmt.Errorf("%s: %w", s.summonPath(token), err)
	}
	return sm, true, nil
}

// DeleteSummon frees a reservation after adoption commits or launch fails.
func (s Store) DeleteSummon(token string) error {
	err := os.Remove(s.summonPath(token))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// PendingSummons counts the launches of session not yet adopted and not
// yet expired at now.
func (s Store) PendingSummons(session string, now time.Time) (int, error) {
	entries, err := os.ReadDir(s.P.summons())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, en := range entries {
		b, err := os.ReadFile(filepath.Join(s.P.summons(), en.Name()))
		if err != nil {
			continue
		}
		var sm Summon
		// A Stealth launch (a narrator) takes no place a worker would.
		if json.Unmarshal(b, &sm) == nil && sm.Session == session && now.Sub(sm.At) < PendingSummonFor && !core.Stealth(sm.For) {
			n++
		}
	}
	return n, nil
}
