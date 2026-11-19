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

// The gateway's state lives in the AMQ root's extension area, which amq
// cleanup leaves alone (amq docs/adr-layer-extensions.md):
//
//   - links/<personal>__<epic>.json or links/hub_<hub>__<epic>.json: a link
//     the user made (core.Link). Every decision reads the directory, so link
//     and unlink take effect at once, for wakers and hooks already running.
//   - provenance/<msg_id>.json: who sent a message, from where, to whom, as
//     what (Provenance), written by the adapter when it sends.
//     provenance/pending/<token>.json is the same record before amq gave the
//     message its id.
//   - quarantine/<handle>/<msg_id>.md: a message the gateway took out of
//     handle's inbox undelivered, and <msg_id>.json why.
//   - notifications/: notify-personal's dedup records (notify.go).

// Extensions is the adapter's directory in the AMQ root's extension area.
func (p Paths) Extensions() string {
	return filepath.Join(p.Root(), "extensions", "rex-amq-adapter")
}

func (p Paths) links() string      { return filepath.Join(p.Extensions(), "links") }
func (p Paths) provenance() string { return filepath.Join(p.Extensions(), "provenance") }
func (p Paths) quarantine() string { return filepath.Join(p.Extensions(), "quarantine") }

func sessionKey(k core.SessionKey) string { return fileKey(k.Server + "|" + k.SessionID) }

// linkPath is where l is kept: links/hub_<sha256(hub)[:16]>__<epic>.json for
// a Hub's link, links/<personal>__<epic>.json for a session-keyed one.
func (s Store) linkPath(l core.Link) string {
	owner := sessionKey(l.Personal)
	if l.Hub != "" {
		sum := sha256.Sum256([]byte(l.Hub))
		owner = "hub_" + hex.EncodeToString(sum[:])[:16]
	}
	return filepath.Join(s.P.links(), owner+"__"+sessionKey(l.Epic)+".json")
}

// PutLink records l. Linking twice is linking once: a grant already
// recorded keeps who made it and its preferred personal agent.
func (s Store) PutLink(l core.Link) error {
	if (l.Hub == "" && l.Personal.SessionID == "") || l.Epic.SessionID == "" {
		return errors.New("a link needs its Hub or personal session, and its Epic")
	}
	if err := os.MkdirAll(s.P.links(), 0o700); err != nil {
		return err
	}
	path := s.linkPath(l)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}

// DeleteLink removes l; existed is false when there was none.
func (s Store) DeleteLink(l core.Link) (existed bool, err error) {
	err = os.Remove(s.linkPath(l))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Links lists every link. An unreadable one is an error: a decision made
// without it could refuse what the user allowed, or the reverse.
func (s Store) Links() ([]core.Link, error) {
	entries, err := os.ReadDir(s.P.links())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []core.Link
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.P.links(), e.Name()))
		if err != nil {
			return nil, err
		}
		var l core.Link
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("decode link %s: %w", e.Name(), err)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return sessionKey(out[i].Epic) < sessionKey(out[j].Epic) })
	return out, nil
}

// Provenance is a message as the adapter sent it: the sender and each
// recipient where they were at sending, and what it was sent as. It is
// written once and never changed: a later move of either side does not
// reclassify the message.
type Provenance struct {
	ID               string          `json:"id,omitempty"`
	Token            string          `json:"token,omitempty"`
	Op               core.OpKind     `json:"op"`
	ControlRequestID string          `json:"control_request_id,omitempty"`
	System           bool            `json:"system,omitempty"`
	From             core.Endpoint   `json:"from"`
	To               []core.Endpoint `json:"to"`
	At               time.Time       `json:"at"`
}

// Operation is the message as the gateway decides on it, addressed to
// recipient h as it was at sending (nil when h was not a recipient).
func (p Provenance) Operation(h core.Handle) core.Operation {
	op := core.Operation{Kind: p.Op, ControlRequestID: p.ControlRequestID, System: p.System}
	for i := range p.To {
		if p.To[i].Handle == h {
			to := p.To[i]
			op.Addressed = &to
		}
	}
	return op
}

// ProvenanceContextKey is the key of the adapter's object in a message's
// context (the header's "context"), which carries the pending token.
const ProvenanceContextKey = "rex_amq_adapter"

func (s Store) provenancePath(id string) string {
	return filepath.Join(s.P.provenance(), fileKey(id)+".json")
}

func (s Store) pendingPath(token string) string {
	return filepath.Join(s.P.provenance(), "pending", fileKey(token)+".json")
}

// PendProvenance records p under its token before the send, so an arrival
// checked before the send returns finds it.
func (s Store) PendProvenance(p Provenance) error {
	if p.Token == "" {
		return errors.New("pending provenance needs a token")
	}
	return writeJSON(s.pendingPath(p.Token), p)
}

// CommitProvenance records p under the id amq gave the message and drops
// its pending copy.
func (s Store) CommitProvenance(p Provenance) error {
	if p.ID == "" {
		return errors.New("provenance needs the message id")
	}
	if err := writeJSON(s.provenancePath(p.ID), p); err != nil {
		return err
	}
	if p.Token != "" {
		if err := os.Remove(s.pendingPath(p.Token)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ProvenanceOf finds the provenance of message id, or of the pending token
// its header carries. A pending record names no id, so it counts only for a
// message from its sender to one of its recipients. ok is false when the
// adapter did not send the message.
func (s Store) ProvenanceOf(id, token string, from core.Handle, to []string) (Provenance, bool, error) {
	if p, ok, err := readProvenance(s.provenancePath(id)); ok || err != nil {
		return p, ok, err
	}
	if token == "" {
		return Provenance{}, false, nil
	}
	p, ok, err := readProvenance(s.pendingPath(token))
	if err != nil {
		return p, false, err
	}
	if !ok {
		// Committed between the two reads (or never pending).
		return readProvenance(s.provenancePath(id))
	}
	if p.Token != token || p.From.Handle != from || !sameRecipients(p.To, to) {
		return Provenance{}, false, nil
	}
	return p, true, nil
}

func sameRecipients(eps []core.Endpoint, to []string) bool {
	if len(eps) != len(to) {
		return false
	}
	want := map[core.Handle]bool{}
	for _, e := range eps {
		want[e.Handle] = true
	}
	for _, h := range to {
		if !want[core.Handle(h)] {
			return false
		}
	}
	return true
}

func readProvenance(path string) (Provenance, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Provenance{}, false, nil
	}
	if err != nil {
		return Provenance{}, false, err
	}
	var p Provenance
	if err := json.Unmarshal(b, &p); err != nil {
		return Provenance{}, false, fmt.Errorf("decode %s: %w", path, err)
	}
	return p, true, nil
}

// Quarantined is why a message was taken out of an inbox.
type Quarantined struct {
	ID       string        `json:"id"`
	Handle   core.Handle   `json:"handle"`
	From     core.Endpoint `json:"from"`
	To       core.Endpoint `json:"to"`
	Decision core.Decision `json:"decision"`
	At       time.Time     `json:"at"`
}

func (s Store) quarantineDir(h core.Handle) string {
	return filepath.Join(s.P.quarantine(), fileKey(string(h)))
}

// Quarantine moves the message at src (an inbox file of q.Handle) into the
// quarantine with q beside it. moved is false when src was gone: another
// reader drained or quarantined it first. No receipt is written: the
// recipient never consumed the message.
func (s Store) Quarantine(src string, q Quarantined) (moved bool, err error) {
	dir := s.quarantineDir(q.Handle)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	// The reason first: a crash between the two leaves a reason for a
	// message still in the inbox, which the next check quarantines again.
	if err := writeJSON(filepath.Join(dir, fileKey(q.ID)+".json"), q); err != nil {
		return false, err
	}
	dst := filepath.Join(dir, fileKey(q.ID)+".md")
	err = os.Rename(src, dst)
	if errors.Is(err, fs.ErrNotExist) {
		// Drained by its agent: the reason is for nothing. Quarantined by a
		// concurrent check: the reason (the same decision) is that one's.
		if _, serr := os.Stat(dst); errors.Is(serr, fs.ErrNotExist) {
			_ = os.Remove(filepath.Join(dir, fileKey(q.ID)+".json"))
		}
		return false, nil
	}
	return err == nil, err
}

// QuarantinedIDs lists the messages quarantined from h's inbox, sorted.
func (s Store) QuarantinedIDs(h core.Handle) ([]string, error) {
	entries, err := os.ReadDir(s.quarantineDir(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".md"); ok && e.Type().IsRegular() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// QuarantineReason reads why message id left h's inbox.
func (s Store) QuarantineReason(h core.Handle, id string) (Quarantined, error) {
	var q Quarantined
	b, err := os.ReadFile(filepath.Join(s.quarantineDir(h), fileKey(id)+".json"))
	if err != nil {
		return q, err
	}
	return q, json.Unmarshal(b, &q)
}

// LockConsume serialises the managed commands that take mail out of h's
// inbox, so two of them never both report one message.
func (s Store) LockConsume(ctx context.Context, h core.Handle) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.ledgers(), "."+fileKey(string(h))+".consume.lock"))
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}
