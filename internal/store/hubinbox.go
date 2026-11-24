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
	// Delivery names the record of the amq delivery of an entry kept
	// before it was delivered; "" for none.
	Delivery string `json:"delivery,omitempty"`
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
	return e, true, f.Sync()
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
		if x := &entries[i]; x.Delivery != "" && x.MsgID == "" {
			var d hubInboxDelivery
			if ok, err := readJSON(s.hubInboxDeliveryPath(dir, x.Delivery), &d); err != nil {
				return nil, err
			} else if ok {
				x.MsgID = d.MsgID
				if x.Thread == "" {
					x.Thread = d.Thread
				}
			}
		}
	}
	return entries, nil
}

// hubInboxDelivery is the amq delivery of an entry kept before it.
type hubInboxDelivery struct {
	MsgID  string `json:"msg_id"`
	Thread string `json:"thread,omitempty"`
}

func (s Store) hubInboxDeliveryPath(dir, delivery string) string {
	return filepath.Join(s.P.hubInboxDir(dir), "delivered", fileKey(delivery)+".json")
}

// PutHubInboxDelivery records that the entry of the Hub directory dir
// named delivery was delivered as amq message msgID on thread: readers
// see them as the entry's (HubInbox). It takes no lock: each delivery is
// its own file, written once whole.
func (s Store) PutHubInboxDelivery(dir, delivery, msgID, thread string) error {
	if delivery == "" || msgID == "" {
		return errors.New("a Hub inbox delivery needs its entry and the message id")
	}
	path := s.hubInboxDeliveryPath(dir, delivery)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(hubInboxDelivery{MsgID: msgID, Thread: thread})
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
