package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// A Hub's inbox is what its Epics told its Operator, kept whatever personal
// agent was live: an append-only JSON Lines file per Hub, keyed by the Hub's
// canonical directory alone (a Hub's key also names its Rex server, which a
// reader outside Rex does not know and which a restarted server changes),
// so reading it needs no Rex at all. Entries are written whole, one line
// each, under the inbox's own lock, which is a leaf: its holder takes no
// other lock and does no Rex or amq I/O. Readers take no lock and stop at a
// trailing line with no newline, which a writer is still writing or died
// writing; the next writer cuts such a line off before it appends.

// HubInboxSchema is the version of an inbox entry.
const HubInboxSchema = 1

// HubInboxMaxLine bounds an entry's line, newline included: an entry
// longer is refused before it is written, and every line written is one a
// reader can read. A body of cli.BodyMax JSON-escaped at its worst (six
// bytes a character) fits.
const HubInboxMaxLine = 2 << 20

// Kinds of inbox entry.
const (
	InboxHandBack = "handback" // a lead handed back (its seq and digest)
	InboxNarrator = "narrator" // a narrator's mail to its Hub's personal agent
	InboxOperator = "operator" // the Operator's message to a narrator
	InboxReply    = "reply"    // a personal agent's reply to a narrator
)

// DeliveryState is what became of an inbox entry kept before its amq
// delivery (Delivery != ""). Derived from its delivery record (or recovered
// from the sender's sent mail by the app); never stored in inbox.jsonl.
type DeliveryState string

const (
	DeliveryNone      DeliveryState = ""          // no deferred delivery
	DeliveryPending   DeliveryState = "pending"   // kept before delivery; not yet settled
	DeliveryHubOnly   DeliveryState = "hub_only"  // settled without amq delivery (no live personal agent)
	DeliveryDelivered DeliveryState = "delivered" // settled with amq delivery (MsgID set)
)

// HubInboxEntry is one entry of a Hub's inbox. Seq numbers the Hub's
// entries from 1; DedupeKey names the event, so writing it again (a retry
// after a failure part way) appends nothing.
type HubInboxEntry struct {
	Schema int         `json:"schema"`
	Seq    int64       `json:"seq"`
	At     time.Time   `json:"at"`
	Epic   string      `json:"epic"`
	HubKey core.HubKey `json:"hub_key,omitempty"`
	Kind   string      `json:"kind"`
	// MsgID is the amq message the entry is, when it is one; Thread its
	// thread. For an entry kept before its delivery, they are read from
	// its delivery record (Delivery, PutHubInboxDelivery).
	MsgID   string `json:"msg_id,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Subject string `json:"subject,omitempty"`
	// HandBackSeq is the Epic's latest hand-back when the entry was
	// written (for a handback entry, the one it records), 0 for none.
	HandBackSeq int `json:"hand_back_seq,omitempty"`
	// Digest is a handback entry's report digest.
	Digest string `json:"digest,omitempty"`
	Body   string `json:"body,omitempty"`
	// InReplyTo is the Seq of the entry this one answers, 0 for none.
	InReplyTo int64       `json:"in_reply_to,omitempty"`
	By        core.Handle `json:"by"`
	DedupeKey string      `json:"dedupe_key"`
	// RequestHash is the sha256 digest of the immutable normalized request
	// that created an --op-id narrator entry, separate from amq-assigned
	// Thread or dynamically selected live recipients; "" when unset.
	RequestHash string `json:"request_hash,omitempty"`
	// Delivery is the immutable durable correlation token of an entry kept
	// before its amq delivery; "" for none. Separate from DeliveryState and
	// never cleared on read.
	Delivery string `json:"delivery,omitempty"`
	// DeliveryState, DeliveryRoutedTo, and DeliveryReroutedFrom hold the
	// resolved delivery outcome in memory (never written to inbox.jsonl).
	DeliveryState        DeliveryState `json:"-"`
	DeliveryRoutedTo     core.Handle   `json:"-"`
	DeliveryReroutedFrom core.Handle   `json:"-"`
}

// hubInboxDir is where the inbox of the Hub directory dir is kept.
func (p Paths) hubInboxDir(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(p.Extensions(), "hub-inbox", hex.EncodeToString(sum[:12]))
}

// HubInboxPath is the inbox file of the Hub directory dir (canonical).
func (p Paths) HubInboxPath(dir string) string {
	return filepath.Join(p.hubInboxDir(dir), "inbox.jsonl")
}

// hubInboxMeta names the directory an inbox is the Hub's, for a person
// reading the state directory.
type hubInboxMeta struct {
	Dir string `json:"dir"`
}

// AppendHubInbox appends e to the inbox of the Hub directory dir unless an
// entry with its DedupeKey is there already, which it returns with added
// false. A new entry gets the next Seq, the schema, and its time (e.At,
// else now). The inbox lock is held for the file I/O alone.
func (s Store) AppendHubInbox(ctx context.Context, dir string, e HubInboxEntry, now time.Time) (HubInboxEntry, bool, error) {
	if dir == "" || e.DedupeKey == "" || e.Epic == "" || e.Kind == "" {
		return e, false, errors.New("a Hub inbox entry needs its Hub, Epic, kind, and dedupe key")
	}
	base := s.P.hubInboxDir(dir)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return e, false, err
	}
	unlock, err := fsutil.Flock(ctx, filepath.Join(base, "inbox.lock"))
	if err != nil {
		return e, false, err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(base, "hub.json")); errors.Is(err, fs.ErrNotExist) {
		b, _ := json.Marshal(hubInboxMeta{Dir: dir})
		if err := fsutil.WriteAtomic(filepath.Join(base, "hub.json"), append(b, '\n')); err != nil {
			return e, false, err
		}
	}
	path := filepath.Join(base, "inbox.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return e, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return e, false, err
	}
	whole := len(b)
	if i := bytes.LastIndexByte(b, '\n'); i+1 != len(b) {
		// A line a writer died writing: cut off, never appended to.
		whole = i + 1
		if err := f.Truncate(int64(whole)); err != nil {
			return e, false, fmt.Errorf("cut the torn last line of %s: %w", path, err)
		}
	}
	entries, err := parseHubInbox(b[:whole])
	if err != nil {
		return e, false, fmt.Errorf("%s: %w", path, err)
	}
	var last int64
	for _, old := range entries {
		if old.DedupeKey == e.DedupeKey {
			return old, false, nil
		}
		last = old.Seq
	}
	e.Schema, e.Seq = HubInboxSchema, last+1
	if e.At.IsZero() {
		e.At = now.UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return e, false, err
	}
	if len(line)+1 > HubInboxMaxLine {
		return e, false, fmt.Errorf("a Hub inbox entry is at most %d bytes, and this one is %d", HubInboxMaxLine, len(line)+1)
	}
	if _, err := f.WriteAt(append(line, '\n'), int64(whole)); err != nil {
		return e, false, err
	}
	if e.Delivery != "" {
		if e.MsgID != "" {
			e.DeliveryState = DeliveryDelivered
		} else {
			e.DeliveryState = DeliveryPending
		}
	}
	return e, true, f.Sync()
}

// LockHubInboxOp serialises lookup, inbox append, amq delivery, and delivery
// recording for one narrator op-id request (scoped by Hub dir, Epic session,
// and opID). It is taken outside inbox.lock (which stays a leaf lock for
// inbox.jsonl file I/O alone) and outside any lifecycle lock.
func (s Store) LockHubInboxOp(ctx context.Context, dir, epic, opID string) (func(), error) {
	if dir == "" || epic == "" || opID == "" {
		return nil, errors.New("a Hub inbox op lock needs Hub, Epic, and op id")
	}
	base := filepath.Join(s.P.hubInboxDir(dir), "ops")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(len(epic)) + ":" + epic + ":" + opID))
	return fsutil.Flock(ctx, filepath.Join(base, hex.EncodeToString(sum[:16])+".lock"))
}

// HubInbox reads the inbox of the Hub directory dir, taking no lock: its
// entries in Seq order, none for a Hub that has none. A trailing line with
// no newline is not read.
func (s Store) HubInbox(dir string) ([]HubInboxEntry, error) {
	path := s.P.HubInboxPath(dir)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(b, '\n'); i+1 != len(b) {
		b = b[:i+1]
	}
	entries, err := parseHubInbox(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i := range entries {
		x := &entries[i]
		if x.Delivery == "" {
			continue
		}
		if x.MsgID != "" {
			x.DeliveryState = DeliveryDelivered
			continue
		}
		// A record that cannot be read leaves the entry pending with its
		// durable Delivery token intact so the app can recover it from
		// sent mail or retry with the same correlation token.
		var d HubInboxDelivery
		if ok, err := readJSON(s.hubInboxDeliveryPath(dir, x.Delivery), &d); err == nil && ok {
			state := d.State
			if state == "" {
				// Legacy sidecar written before explicit state.
				if d.MsgID != "" {
					state = DeliveryDelivered
				} else {
					state = DeliveryHubOnly
				}
			}
			switch {
			case state == DeliveryDelivered && d.MsgID != "":
				x.DeliveryState = DeliveryDelivered
				x.MsgID = d.MsgID
				if x.Thread == "" {
					x.Thread = d.Thread
				}
				x.DeliveryRoutedTo = d.RoutedTo
				x.DeliveryReroutedFrom = d.ReroutedFrom
			case state == DeliveryHubOnly && d.MsgID == "":
				x.DeliveryState = DeliveryHubOnly
				x.DeliveryRoutedTo = d.RoutedTo
				x.DeliveryReroutedFrom = d.ReroutedFrom
			default:
				// Corrupt or inconsistent sidecar: leave pending so the app
				// can recover from sent mail via the durable Delivery token.
				x.DeliveryState = DeliveryPending
			}
		} else {
			x.DeliveryState = DeliveryPending
		}
	}
	return entries, nil
}

// HubInboxDelivery is the settled delivery outcome of an entry kept before
// delivery: either DeliveryDelivered (with MsgID) or DeliveryHubOnly (no
// live personal agent took it).
type HubInboxDelivery struct {
	State        DeliveryState `json:"state"`
	MsgID        string        `json:"msg_id,omitempty"`
	Thread       string        `json:"thread,omitempty"`
	RoutedTo     core.Handle   `json:"routed_to,omitempty"`
	ReroutedFrom core.Handle   `json:"rerouted_from,omitempty"`
}

func (s Store) hubInboxDeliveryPath(dir, delivery string) string {
	return filepath.Join(s.P.hubInboxDir(dir), "delivered", fileKey(delivery)+".json")
}

// PutHubInboxDelivery records the settled delivery outcome d of the entry of
// the Hub directory dir named delivery: DeliveryDelivered (with MsgID) or
// DeliveryHubOnly (with empty MsgID). It takes no lock: each delivery is its
// own file, written once whole.
func (s Store) PutHubInboxDelivery(dir, delivery string, d HubInboxDelivery) error {
	if delivery == "" {
		return errors.New("a Hub inbox delivery needs its entry")
	}
	switch d.State {
	case DeliveryDelivered:
		if d.MsgID == "" {
			return errors.New("a delivered Hub inbox record needs its amq message id")
		}
	case DeliveryHubOnly:
		if d.MsgID != "" {
			return errors.New("a Hub-only inbox record cannot carry an amq message id")
		}
	default:
		return fmt.Errorf("a Hub inbox delivery record needs a settled state, not %q", d.State)
	}
	path := s.hubInboxDeliveryPath(dir, delivery)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}

// parseHubInbox parses whole lines of an inbox.
func parseHubInbox(b []byte) ([]HubInboxEntry, error) {
	var out []HubInboxEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), HubInboxMaxLine)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e HubInboxEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
