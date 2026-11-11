package amq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// These read amq's on-disk mailbox format, which the adapter does not own:
//
//   - agents/<h>/inbox/{new,cur}/<id>.md: undrained and drained mail;
//     dlq/{new,cur}/<id>.md: dead-lettered; outbox/sent/<id>.md: sent.
//   - A message is a JSON header between "---json" and "---" lines (schema 1:
//     id, from, to, thread, subject, created), then the body.
//   - agents/<h>/receipts/<id>__<consumer>__<stage>.json: schema 1 receipts
//     (msg_id, thread, sender, consumer, stage, emitted_at); amq 0.x writes
//     stage "drained".
//
// Nothing here takes a lock or writes: amq renames files into place, so a
// reader sees a whole file or none.

// MailboxSchema is the version of amq's message header and receipt format
// that ReadHeader and ReadReceipt understand. Another is an error, so a change
// in amq shows as decode errors rather than as wrong states.
const MailboxSchema = 1

// Header is a message's header. The body is never read.
type Header struct {
	Schema  int       `json:"schema"`
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      []string  `json:"to"`
	Thread  string    `json:"thread"`
	Subject string    `json:"subject"`
	Created time.Time `json:"created"`
	Labels  []string  `json:"labels"`
}

// SystemLabel marks mail the adapter sends in an agent's name (a report, a
// notice): the agent did not write it, so it is never bounced to it.
const SystemLabel = "rex-amq-adapter"

// System reports whether the adapter sent the message.
func (h Header) System() bool { return slices.Contains(h.Labels, SystemLabel) }

// maxHeader bounds how much of a message is read for its header.
const maxHeader = 64 << 10

func parseHeader(b []byte) (Header, error) {
	rest, ok := bytes.CutPrefix(b, []byte("---json\n"))
	if !ok {
		return Header{}, errors.New("message has no JSON header")
	}
	header, _, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return Header{}, errors.New("message header is not closed")
	}
	var h Header
	if err := json.Unmarshal(header, &h); err != nil {
		return Header{}, fmt.Errorf("decode message header: %w", err)
	}
	return h, nil
}

// Box is a directory of handle's mailbox that holds messages.
type Box string

const (
	BoxUnread  Box = "inbox/new"
	BoxDrained Box = "inbox/cur"
	BoxDLQNew  Box = "dlq/new"
	BoxDLQCur  Box = "dlq/cur"
	BoxSent    Box = "outbox/sent"
)

// IDs lists the ids of the messages in handle's box, sorted. A missing box has
// none.
func IDs(root, handle string, box Box) ([]string, error) {
	return messageIDs(filepath.Join(root, "agents", handle, filepath.FromSlash(string(box))))
}

// ReadHeader reads the header of message id in handle's box, which must be
// of MailboxSchema. (Recipients reads only "to", whatever the schema: the
// check of undelivered mail should not stop on a field it never reads.)
func ReadHeader(root, handle string, box Box, id string) (Header, error) {
	f, err := os.Open(filepath.Join(root, "agents", handle, filepath.FromSlash(string(box)), id+".md"))
	if err != nil {
		return Header{}, err
	}
	defer f.Close()
	b := make([]byte, maxHeader)
	n, err := f.Read(b)
	if err != nil && n == 0 {
		return Header{}, err
	}
	h, err := parseHeader(b[:n])
	if err == nil && h.Schema != MailboxSchema {
		err = fmt.Errorf("message %s in %s has header schema %d; this adapter reads schema %d", id, handle, h.Schema, MailboxSchema)
	}
	return h, err
}

// Receipt is amq's record of what happened to a message in a consumer's
// mailbox.
type Receipt struct {
	Schema    int       `json:"schema"`
	MsgID     string    `json:"msg_id"`
	Thread    string    `json:"thread"`
	Sender    string    `json:"sender"`
	Consumer  string    `json:"consumer"`
	Stage     string    `json:"stage"`
	EmittedAt time.Time `json:"emitted_at"`
}

// Receipts lists the receipt files of handle's mailbox, sorted.
func Receipts(root, handle string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "agents", handle, "receipts"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && strings.Contains(e.Name(), "__") && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// ReadReceipt reads one receipt Receipts listed.
func ReadReceipt(root, handle, name string) (Receipt, error) {
	b, err := os.ReadFile(filepath.Join(root, "agents", handle, "receipts", name))
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return Receipt{}, fmt.Errorf("decode receipt %s: %w", name, err)
	}
	if r.Schema != MailboxSchema {
		return Receipt{}, fmt.Errorf("receipt %s of %s has schema %d; this adapter reads schema %d", name, handle, r.Schema, MailboxSchema)
	}
	return r, nil
}
