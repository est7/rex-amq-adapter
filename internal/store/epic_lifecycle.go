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
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// An Epic's lifecycle is recorded at its explicit edges, so a retry, a
// cockpit, or a later reader can tell how far it got without asking Rex:
//
//   - sessions/<session_id>/lifecycle.json: the Epic's lifecycle
//     (EpicLifecycle), one per Rex session.
//   - epic-ops/<kind>-<sha256(op_id)>.json: one `epic --op-id` or
//     `epic close --op-id` request (EpicOpRecord), so a retry of the same
//     operation finds what the first run did instead of doing it again.
//
// Writers hold LockEpicLifecycle across read, decide, and write. It is taken
// before LockLifecycle wherever both are held.

// Lifecycle states, in the order an Epic moves through them. A state never
// moves back (EpicLifecycle.Advance).
const (
	// EpicPartialStart: the Epic's Rex session exists; not both of its
	// agents have been seen adopted.
	EpicPartialStart = "partial_start"
	// EpicOpened: both agents were adopted.
	EpicOpened = "opened"
	// EpicHandBack: its orchestrator recorded a hand-back.
	EpicHandBack = "hand_back"
	// EpicClosed: the personal agent closed it, on the user's words.
	EpicClosed = "closed"
)

var epicRank = map[string]int{EpicPartialStart: 1, EpicOpened: 2, EpicHandBack: 3, EpicClosed: 4}

// HandBackRef names one recorded hand-back without its body: its sequence
// number in the session, its digest (HandBack.Digest), who recorded it, and
// when.
type HandBackRef struct {
	Seq    int         `json:"seq"`
	Digest string      `json:"digest"`
	By     core.Handle `json:"by"`
	At     time.Time   `json:"at"`
	// OpHash is the hand-back's HandBack.OpHash: a retry under the same
	// --op-id finds the hand-back it recorded. OpHashes are the other op
	// ids accepted for this Seq since (the same report again under a new
	// --op-id), so each stays bound to this hand-back (Accepts).
	OpHash   string   `json:"op_hash,omitempty"`
	OpHashes []string `json:"op_hashes,omitempty"`
}

// Accepts reports whether the op id whose hash is opHash recorded, or was
// accepted for, this hand-back.
func (r HandBackRef) Accepts(opHash string) bool {
	return opHash != "" && (r.OpHash == opHash || slices.Contains(r.OpHashes, opHash))
}

// withOp is r with opHash among the op ids it accepts.
func (r HandBackRef) withOp(opHash string) HandBackRef {
	switch {
	case opHash == "" || r.Accepts(opHash):
	case r.OpHash == "":
		r.OpHash = opHash
	default:
		r.OpHashes = append(slices.Clip(r.OpHashes), opHash)
	}
	return r
}

// EpicLifecycle is lifecycle.json. Hub is the Hub that opened (and may
// close) the Epic; PersonalHandle is the personal agent that opened it, a
// preference for where its narrator's mail goes first (preferred_personal),
// never who may act on it.
type EpicLifecycle struct {
	SessionID         string        `json:"session_id"`
	Server            string        `json:"server,omitempty"`
	Hub               core.HubKey   `json:"hub,omitempty"`
	Name              string        `json:"name,omitempty"`
	Dir               string        `json:"dir,omitempty"`
	PersonalHandle    core.Handle   `json:"personal_handle,omitempty"`
	PersonalSessionID string        `json:"personal_session_id,omitempty"`
	Crew              string        `json:"crew,omitempty"`
	OpenOpID          string        `json:"open_op_id,omitempty"`
	CloseOpID         string        `json:"close_op_id,omitempty"`
	State             string        `json:"state"`
	OpenedAt          time.Time     `json:"opened_at,omitzero"`
	ClosedAt          *time.Time    `json:"closed_at,omitempty"`
	UpdatedAt         time.Time     `json:"updated_at"`
	HandBacks         []HandBackRef `json:"hand_backs,omitempty"`
}

// Advance moves the lifecycle to state at, unless it is already as far or
// further: a reconciliation that sees both agents adopted after a hand-back,
// or a hand-back after the close, changes nothing. It reports whether the
// state moved.
func (l *EpicLifecycle) Advance(state string, at time.Time) bool {
	if epicRank[state] == 0 {
		panic("store: unknown Epic state " + state)
	}
	if epicRank[l.State] >= epicRank[state] {
		return false
	}
	l.State, l.UpdatedAt = state, at
	if state == EpicClosed {
		l.ClosedAt = &at
	}
	return true
}

// NoteHandBack records h among the lifecycle's hand-backs: the entry of the
// same sequence number is replaced, wherever it is, keeping every op id it
// accepted (h's added); a new one is appended. A Seq once given a digest
// keeps it: noting another digest under it panics, so the caller numbers a
// different report after the latest (LatestHandBack).
func (l *EpicLifecycle) NoteHandBack(h HandBack) {
	ref := HandBackRef{Seq: h.Seq, Digest: h.Digest, By: h.By, At: h.At, OpHash: h.OpHash}
	if i := slices.IndexFunc(l.HandBacks, func(r HandBackRef) bool { return r.Seq == h.Seq }); i >= 0 {
		prev := l.HandBacks[i]
		if prev.Digest != h.Digest {
			panic(fmt.Sprintf("store: hand-back seq %d already has digest %s, not %s", h.Seq, prev.Digest, h.Digest))
		}
		ref.OpHash, ref.OpHashes = prev.OpHash, prev.OpHashes
		l.HandBacks[i] = ref.withOp(h.OpHash)
	} else {
		l.HandBacks = append(l.HandBacks, ref)
	}
	l.Advance(EpicHandBack, h.At)
	if h.At.After(l.UpdatedAt) {
		l.UpdatedAt = h.At
	}
}

// NoteHandBackOp binds the op id whose hash is opHash to the hand-back of
// seq the lifecycle lists, which the request it named matched (the same
// report again): a later request under that op id with another report is
// then a conflict. It reports whether seq is listed.
func (l *EpicLifecycle) NoteHandBackOp(seq int, opHash string) bool {
	i := slices.IndexFunc(l.HandBacks, func(r HandBackRef) bool { return r.Seq == seq })
	if i < 0 {
		return false
	}
	l.HandBacks[i] = l.HandBacks[i].withOp(opHash)
	return true
}

// LatestHandBack is the lifecycle's hand-back of the highest Seq; ok is
// false when it lists none.
func (l EpicLifecycle) LatestHandBack() (latest HandBackRef, ok bool) {
	for _, r := range l.HandBacks {
		if !ok || r.Seq > latest.Seq {
			latest, ok = r, true
		}
	}
	return latest, ok
}

func (p Paths) lifecyclePath(id string) string {
	return filepath.Join(p.SessionDir(id), "lifecycle.json")
}

func (p Paths) epicLifecycleLock() string { return filepath.Join(p.Home, "epic-lifecycle.lock") }

func (p Paths) epicOps() string { return filepath.Join(p.Extensions(), "epic-ops") }

// LockEpicLifecycle serialises the Epic lifecycle's writers: opening,
// closing, and recording hand-backs.
func (s Store) LockEpicLifecycle(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, s.P.epicLifecycleLock())
}

// EpicLifecycle reads session id's lifecycle; ok is false when none was
// recorded (an Epic opened before lifecycles were, or a session no Epic
// opened).
func (s Store) EpicLifecycle(id string) (EpicLifecycle, bool, error) {
	var l EpicLifecycle
	ok, err := readJSON(s.P.lifecyclePath(id), &l)
	if err != nil {
		return l, false, fmt.Errorf("the lifecycle of %s: %w", id, err)
	}
	return l, ok, nil
}

// PutEpicLifecycle records l whole. The caller holds LockEpicLifecycle.
func (s Store) PutEpicLifecycle(l EpicLifecycle) error {
	if l.SessionID == "" || epicRank[l.State] == 0 {
		return fmt.Errorf("an Epic lifecycle needs its session and a known state (%q)", l.State)
	}
	return writeJSON(s.P.lifecyclePath(l.SessionID), l)
}

// EpicLifecycles scans every recorded lifecycle.json under SessionsDir,
// sorted by SessionID. Sessions without a lifecycle.json are skipped; an
// unreadable or malformed lifecycle.json returns an error.
func (s Store) EpicLifecycles() ([]EpicLifecycle, error) {
	entries, err := os.ReadDir(s.P.SessionsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Epic lifecycles: %w", err)
	}
	var out []EpicLifecycle
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		var lc EpicLifecycle
		ok, err := readJSON(filepath.Join(s.P.SessionsDir(), ent.Name(), "lifecycle.json"), &lc)
		if err != nil {
			return nil, fmt.Errorf("the lifecycle in %s: %w", ent.Name(), err)
		}
		if !ok {
			continue
		}
		out = append(out, lc)
	}
	slices.SortFunc(out, func(a, b EpicLifecycle) int { return strings.Compare(a.SessionID, b.SessionID) })
	return out, nil
}

// Epic operation kinds.
const (
	EpicOpOpen   = "open"
	EpicOpClose  = "close"
	EpicOpRevive = "revive"
)

// Epic operation states, besides the lifecycle's own (EpicPartialStart,
// EpicOpened, EpicClosed): EpicCreating is an open about to ask Rex for its
// session, which may or may not exist (its SessionID is not known yet);
// EpicClosing is a close that passed its guards and may have killed the
// session, not yet seen through.
const (
	EpicCreating = "creating"
	EpicClosing  = "closing"
	// EpicRevived is an `epic revive --op-id` that resumed what it found:
	// a retry replays its record and resumes again only what is not live.
	EpicRevived = "revived"
)

// EpicOpRecord is one `epic open`, `epic close`, or `epic revive --op-id` request and how
// far it got. RequestHash identifies the request (who asked for what), so
// the same op id reused for another request is told apart from a retry.
type EpicOpRecord struct {
	Kind        string `json:"kind"`
	OpID        string `json:"op_id"`
	RequestHash string `json:"request_hash"`
	SessionID   string `json:"session_id"`
	// Name is the Epic an open asked for, its session's label.
	Name   string `json:"name,omitempty"`
	Server string `json:"server,omitempty"`
	// HubDirs are the Hubs a close's Epic was of when it was recorded, by
	// its lifecycle and its links: what a retry's --hub is held to after
	// the close's unlink removed the links.
	HubDirs           []string    `json:"hub_dirs,omitempty"`
	PersonalHandle    core.Handle `json:"personal_handle"`
	PersonalSessionID string      `json:"personal_session_id"`
	OrchestratorBlock string      `json:"orchestrator_block,omitempty"`
	NarratorBlock     string      `json:"narrator_block,omitempty"`
	State             string      `json:"state"`
	// PriorSessions is every Rex session there was just before an open
	// asked for its own (EpicCreating): a retry that finds the op without
	// its session tells the one this op created from those.
	PriorSessions []string  `json:"prior_sessions,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// opRank orders the states of each kind of operation.
var opRank = map[string]map[string]int{
	EpicOpOpen:   {EpicCreating: 1, EpicPartialStart: 2, EpicOpened: 3},
	EpicOpClose:  {EpicClosing: 1, EpicClosed: 2},
	EpicOpRevive: {EpicRevived: 1},
}

// Advance moves the operation to state at, unless it is already as far or
// further. It reports whether the state moved.
func (r *EpicOpRecord) Advance(state string, at time.Time) bool {
	rank := opRank[r.Kind]
	if rank[state] == 0 {
		panic("store: no state " + state + " for an Epic " + r.Kind + " operation")
	}
	if rank[r.State] >= rank[state] {
		return false
	}
	r.State, r.UpdatedAt = state, at
	return true
}

func (s Store) epicOpPath(kind, opID string) string {
	sum := sha256.Sum256([]byte(opID))
	return filepath.Join(s.P.epicOps(), kind+"-"+hex.EncodeToString(sum[:])+".json")
}

// EpicOp reads the record of operation opID of kind; ok is false when there
// is none.
func (s Store) EpicOp(kind, opID string) (EpicOpRecord, bool, error) {
	var r EpicOpRecord
	ok, err := readJSON(s.epicOpPath(kind, opID), &r)
	if err != nil {
		return r, false, fmt.Errorf("the %s operation %q: %w", kind, opID, err)
	}
	if ok && (r.Kind != kind || r.OpID != opID) {
		return r, false, fmt.Errorf("the %s operation %q: its record names %s %q", kind, opID, r.Kind, r.OpID)
	}
	return r, ok, nil
}

// PutEpicOp records r whole. The caller holds LockEpicLifecycle.
func (s Store) PutEpicOp(r EpicOpRecord) error {
	if opRank[r.Kind][r.State] == 0 || r.OpID == "" || r.RequestHash == "" || (r.SessionID == "") != (r.State == EpicCreating) {
		return errors.New("an Epic operation needs its kind and a state of it, its op id, its request hash, and its session once it is past creating")
	}
	return writeJSON(s.epicOpPath(r.Kind, r.OpID), r)
}

// HandBackDigest is the digest a hand-back's body is known by:
// "sha256:<hex>" of the body with CRLF line ends made LF and the surrounding
// whitespace trimmed, so the same report saved twice by another editor is
// the same report.
func HandBackDigest(body string) string {
	norm := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	sum := sha256.Sum256([]byte(norm))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// readJSON decodes path into v; ok is false when there is no file.
// EpicOps reads every recorded operation of kind. A record that cannot be
// read is an error, never skipped: a caller deciding whose a session is must
// see every op that could claim it.
func (s Store) EpicOps(kind string) ([]EpicOpRecord, error) {
	entries, err := os.ReadDir(s.P.epicOps())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the %s operations: %w", kind, err)
	}
	var out []EpicOpRecord
	for _, ent := range entries {
		if !strings.HasPrefix(ent.Name(), kind+"-") || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		var r EpicOpRecord
		if _, err := readJSON(filepath.Join(s.P.epicOps(), ent.Name()), &r); err != nil {
			return nil, fmt.Errorf("the %s operations: %w", kind, err)
		}
		if r.Kind != kind {
			return nil, fmt.Errorf("the %s operations: %s names a %s operation", kind, ent.Name(), r.Kind)
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteEpicOp removes the record of operation opID of kind; none is not an
// error.
func (s Store) DeleteEpicOp(kind, opID string) error {
	if err := os.Remove(s.epicOpPath(kind, opID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}
