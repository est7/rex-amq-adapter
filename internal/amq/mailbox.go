package amq

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//     id, from, to, thread, subject, created, labels), then the body.
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
	// Context is the sender's context object, of which only the adapter's
	// own key is read (ContextOf).
	Context map[string]json.RawMessage `json:"context,omitempty"`
}

// ContextOf decodes the value under key in the header's context into v;
// ok is false when there is none or it does not decode.
func (h Header) ContextOf(key string, v any) bool {
	raw, ok := h.Context[key]
	return ok && json.Unmarshal(raw, v) == nil
}

// SystemLabel marks mail the adapter sends in an agent's name (a report, a
// notice): the agent did not write it, so it is never bounced to it.
const SystemLabel = "rex-amq-adapter"

// System reports whether the adapter sent the message.
func (h Header) System() bool { return slices.Contains(h.Labels, SystemLabel) }

// MaxMessage is the largest message file amq reads (its format.MaxMessageSize);
// a header is read within it, however long.
const MaxMessage = 10 << 20

// readHeader decodes a message header the way amq does: a "---json" line
// (a CR before its newline allowed), then one JSON object, then "---". It
// reads no further than the header.
func readHeader(r io.Reader) (Header, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxMessage))
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	if strings.TrimRight(line, "\r\n") != "---json" {
		return Header{}, errors.New("message has no JSON header")
	}
	dec := json.NewDecoder(br)
	var h Header
	if err := dec.Decode(&h); err != nil {
		return Header{}, fmt.Errorf("decode message header: %w", err)
	}
	rest := bufio.NewReader(io.MultiReader(dec.Buffered(), br))
	for {
		b, err := rest.ReadByte()
		if err != nil {
			return Header{}, errors.New("message header is not closed")
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		if b != '-' {
			return Header{}, errors.New("message header is not closed")
		}
		var tail [2]byte
		if _, err := io.ReadFull(rest, tail[:]); err != nil || tail != [2]byte{'-', '-'} {
			return Header{}, errors.New("message header is not closed")
		}
		return h, nil
	}
}

func parseHeader(b []byte) (Header, error) { return readHeader(bytes.NewReader(b)) }

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
// of MailboxSchema.
func ReadHeader(root, handle string, box Box, id string) (Header, error) {
	f, err := os.Open(filepath.Join(root, "agents", handle, filepath.FromSlash(string(box)), id+".md"))
	if err != nil {
		return Header{}, err
	}
	defer f.Close()
	h, err := readHeader(f)
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

// ReadDLQHeader reads the header of the message dead-lettered as dlqID in
// handle's box: amq wraps the original file in an envelope of its own.
func ReadDLQHeader(root, handle string, box Box, dlqID string) (Header, error) {
	b, err := os.ReadFile(filepath.Join(root, "agents", handle, filepath.FromSlash(string(box)), dlqID+".md"))
	if err != nil {
		return Header{}, err
	}
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return Header{}, errors.New("dead letter has no envelope")
	}
	_, original, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return Header{}, errors.New("dead letter envelope is not closed")
	}
	h, err := parseHeader(original)
	if err == nil && h.Schema != MailboxSchema {
		err = fmt.Errorf("dead letter %s of %s has header schema %d", dlqID, handle, h.Schema)
	}
	return h, err
}
