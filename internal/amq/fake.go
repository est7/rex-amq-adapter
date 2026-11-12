package amq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is a test double for the adapter's Queue. It writes the mailbox files
// amq writes (meta/config.json, agents/<h>/{inbox,outbox,dlq,receipts},
// .wake.settings), so the package's readers see what amq would leave, and it
// keeps wakers in memory.
//
// It does not do what only amq can: it starts no waker process, so no
// doorbell is injected; it computes no open obligations (ThreadOpen reports
// none); it derives no defaults amq reports for a reply (kind answer, a
// priority from the kind); and it runs none of amq's own verbs (Run fails
// with exit status 2).
// A test that needs those runs against amq itself (CLI). The contract tests
// in this package run the same scenarios against both, so the two cannot
// drift apart unnoticed.
type Fake struct {
	// Fail, when set, is asked before every operation; a non-nil error fails
	// the operation before it has any effect.
	Fail func(op string, args []string) error

	mu     sync.Mutex
	wakers map[string]*WakeState
	seq    int
}

// ErrNotEmulated is what Fake returns for an operation only amq performs.
var ErrNotEmulated = errors.New("amq.Fake does not emulate this; the test needs real amq")

func (f *Fake) fail(op string, args ...string) error {
	if f.Fail == nil {
		return nil
	}
	return f.Fail(op, args)
}

// EnsureMailbox registers handle in root's meta/config.json and creates its
// mailbox directories, as `amq init --force` does.
func (f *Fake) EnsureMailbox(ctx context.Context, root, handle string) error {
	if err := f.fail("init", root, handle); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := struct {
		Agents     []string `json:"agents"`
		CreatedUTC string   `json:"created_utc"`
		Version    int      `json:"version"`
	}{Version: 1, CreatedUTC: time.Now().UTC().Format(time.RFC3339)}
	path := filepath.Join(root, "meta", "config.json")
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !slices.Contains(cfg.Agents, handle) {
		cfg.Agents = append(cfg.Agents, handle)
	}
	sort.Strings(cfg.Agents) // amq writes the merged list sorted
	for _, h := range cfg.Agents {
		for _, d := range []string{"inbox/new", "inbox/cur", "inbox/tmp", "outbox/sent", "dlq/new", "dlq/cur", "receipts"} {
			if err := os.MkdirAll(filepath.Join(root, "agents", h, d), 0o700); err != nil {
				return err
			}
		}
	}
	return writeJSON(path, cfg)
}

// Post sends or replies (verb) with amq's flags and returns what amq --json
// reports. An unknown flag is an error, so a caller using a flag the fake
// does not know fails loudly instead of being half-emulated.
func (f *Fake) Post(ctx context.Context, root, verb string, args ...string) (Sent, []byte, error) {
	if err := f.fail(verb, args...); err != nil {
		return Sent{}, nil, err
	}
	fl, err := postFlags(verb, args)
	if err != nil {
		return Sent{}, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := Header{Schema: MailboxSchema, From: fl["me"], Subject: fl["subject"], Priority: fl["priority"], Kind: fl["kind"]}
	var refs []string
	switch verb {
	case "send":
		for _, to := range strings.Split(fl["to"], ",") {
			if to = strings.TrimSpace(to); to != "" {
				if to == h.From {
					return Sent{}, nil, fmt.Errorf("refusing send: recipient %q matches the sender", to)
				}
				h.To = append(h.To, to)
			}
		}
		if len(h.To) == 0 {
			return Sent{}, nil, errors.New("send needs --to")
		}
		h.Thread = fl["thread"]
		if h.Thread == "" {
			h.Thread = p2pThread(h.From, h.To)
		}
	case "reply":
		orig, err := findMessage(root, h.From, fl["id"])
		if err != nil {
			return Sent{}, nil, err
		}
		h.To, h.Thread = []string{orig.From}, orig.Thread
		if h.Subject == "" {
			h.Subject = "Re: " + orig.Subject
		}
		refs = []string{orig.ID}
	default:
		return Sent{}, nil, fmt.Errorf("amq.Fake.Post: unknown verb %q", verb)
	}
	if l := fl["labels"]; l != "" {
		h.Labels = strings.Split(l, ",")
	}
	if c := fl["context"]; c != "" {
		if err := json.Unmarshal([]byte(c), &h.Context); err != nil {
			return Sent{}, nil, fmt.Errorf("--context must be a JSON object: %w", err)
		}
	}
	body := fl["body"]
	if strings.HasPrefix(body, "@") {
		b, err := os.ReadFile(body[1:])
		if err != nil {
			return Sent{}, nil, err
		}
		body = string(b)
	}
	now := time.Now().UTC()
	f.seq++
	h.ID = fmt.Sprintf("%s.%03dZ_pid%d_%08x", now.Format("2006-01-02T15-04-05"), now.Nanosecond()/1e6, os.Getpid(), f.seq)
	h.Created = now
	file := messageFile(h, refs, body)
	for _, to := range h.To {
		if err := writeFile(filepath.Join(root, "agents", to, "inbox", "new", h.ID+".md"), file); err != nil {
			return Sent{}, nil, err
		}
	}
	if err := writeFile(filepath.Join(root, "agents", h.From, "outbox", "sent", h.ID+".md"), file); err != nil {
		return Sent{}, nil, err
	}
	s := Sent{ID: h.ID, To: h.To, Thread: h.Thread}
	out, _ := json.Marshal(map[string]any{"id": s.ID, "to": s.To, "thread": s.Thread, "subject": h.Subject, "root": root})
	return s, out, nil
}

// postFlags parses the flags of amq send and reply that the adapter passes.
func postFlags(verb string, args []string) (map[string]string, error) {
	known := map[string]bool{"me": true, "subject": true, "body": true, "kind": true, "priority": true, "labels": true, "context": true}
	switch verb {
	case "send":
		known["to"], known["thread"] = true, true
	case "reply":
		known["id"] = true
	}
	fl := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" {
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || !known[name] {
			return nil, fmt.Errorf("amq.Fake %s: unknown argument %q", verb, a)
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("amq.Fake %s: --%s needs a value", verb, name)
			}
			i++
			value = args[i]
		}
		fl[name] = value
	}
	if fl["me"] == "" {
		return nil, fmt.Errorf("amq.Fake %s: --me is required", verb)
	}
	return fl, nil
}

// p2pThread is amq's default thread for mail between two agents.
func p2pThread(from string, to []string) string {
	parties := append([]string{from}, to...)
	sort.Strings(parties)
	return "p2p/" + strings.Join(parties, "__")
}

func messageFile(h Header, refs []string, body string) string {
	head := map[string]any{"schema": h.Schema, "id": h.ID, "from": h.From, "to": h.To, "thread": h.Thread, "subject": h.Subject, "created": h.Created}
	if h.Priority != "" {
		head["priority"] = h.Priority
	}
	if h.Kind != "" {
		head["kind"] = h.Kind
	}
	if len(h.Labels) > 0 {
		head["labels"] = h.Labels
	}
	if len(h.Context) > 0 {
		head["context"] = h.Context
	}
	if len(refs) > 0 {
		head["refs"] = refs
	}
	b, _ := json.MarshalIndent(head, "", "  ")
	return "---json\n" + string(b) + "\n---\n" + body + "\n"
}

// findMessage finds id among the mail handle received.
func findMessage(root, handle, id string) (Header, error) {
	for _, box := range []Box{BoxUnread, BoxDrained} {
		h, err := ReadHeader(root, handle, box, id)
		if err == nil {
			return h, nil
		}
	}
	return Header{}, fmt.Errorf("message %s not found for %s", id, handle)
}

// ConfigureHolds writes handle's .wake.settings as `amq wake config` does.
func (f *Fake) ConfigureHolds(ctx context.Context, root, handle string, normal, low time.Duration) error {
	if err := f.fail("wake config", root, handle); err != nil {
		return err
	}
	settings := map[string]string{}
	if normal > 0 || low > 0 {
		settings["hold_normal"], settings["hold_low"] = normal.String(), low.String()
	}
	return writeJSON(filepath.Join(root, "agents", handle, ".wake.settings"), map[string]any{"schema": 1, "settings": settings})
}

func wakerKey(root, handle string) string { return canonPath(root) + "\x00" + handle }

// Ensure leaves one live waker for w with w's target, deciding as Ensure does
// with amq: keep a matching live one, repair a stale one with the same target,
// replace anything else. No process is started.
func (f *Fake) Ensure(ctx context.Context, w Waker, logDir string) (WakeState, WakeDecision, error) {
	if err := f.fail("wake", w.Root, w.Handle); err != nil {
		return WakeState{}, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := w.Target()
	st := f.stateLocked(w.Root, w.Handle)
	decision := DecideWake(st, want)
	switch decision {
	case DecisionKeep:
		return st, decision, nil
	case DecisionRepair:
		f.wakers[wakerKey(w.Root, w.Handle)].Live, f.wakers[wakerKey(w.Root, w.Handle)].Status = true, "valid"
	default:
		f.seq++
		if f.wakers == nil {
			f.wakers = map[string]*WakeState{}
		}
		f.wakers[wakerKey(w.Root, w.Handle)] = &WakeState{Status: "valid", Live: true, PID: 100000 + f.seq, Generation: fmt.Sprintf("fake-%d", f.seq), Target: want, HasTarget: true}
	}
	return f.stateLocked(w.Root, w.Handle), decision, nil
}

func (f *Fake) stateLocked(root, handle string) WakeState {
	if s, ok := f.wakers[wakerKey(root, handle)]; ok {
		return *s
	}
	return WakeState{Status: "missing"}
}

// Waker reports handle's waker as amq wake check would.
func (f *Fake) Waker(root, handle string) WakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateLocked(root, handle)
}

// Kill makes handle's waker die, leaving its lock stale, as a killed amq wake
// does.
func (f *Fake) Kill(root, handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.wakers[wakerKey(root, handle)]; ok {
		s.Live, s.Status, s.PID = false, "stale", 0
	}
}

func (f *Fake) RetireFor(ctx context.Context, root, handle, block string) (bool, error) {
	return f.retire(root, handle, func(t WakeTarget) bool { return wakes(t, block, handle, root) })
}

func (f *Fake) RetireHandle(ctx context.Context, root, handle string) (bool, error) {
	return f.retire(root, handle, func(t WakeTarget) bool { return wakesAnyBlock(t, handle, root) })
}

func (f *Fake) retire(root, handle string, ours func(WakeTarget) bool) (bool, error) {
	if err := f.fail("wake retire", root, handle); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stateLocked(root, handle)
	if st.Status == "missing" || !ours(st.Target) {
		return false, nil
	}
	delete(f.wakers, wakerKey(root, handle))
	return true, nil
}

// ThreadOpen reports no open obligations: computing them is amq's.
func (f *Fake) ThreadOpen(ctx context.Context, root, me string, handles []string) (OpenObligations, error) {
	if err := f.fail("thread open", root, me); err != nil {
		return OpenObligations{}, err
	}
	return OpenObligations{}, nil
}

// ThreadRead reads exactly ids of thread from the copies agents hold, failing
// the whole read when one is missing, as amq does.
func (f *Fake) ThreadRead(ctx context.Context, root, me, thread string, agents, ids []string) ([]ThreadMessage, error) {
	if err := f.fail("thread", root, me, thread); err != nil {
		return nil, err
	}
	var out []ThreadMessage
	for _, id := range ids {
		m, ok := readCopy(root, agents, id)
		if !ok || m.Thread != thread {
			return nil, fmt.Errorf("amq thread %s: message %s not found", thread, id)
		}
		out = append(out, m)
	}
	return out, nil
}

func readCopy(root string, agents []string, id string) (ThreadMessage, bool) {
	for _, a := range agents {
		for _, dir := range []string{"inbox/new", "inbox/cur", "outbox/sent"} {
			b, err := os.ReadFile(filepath.Join(root, "agents", a, dir, id+".md"))
			if err != nil {
				continue
			}
			h, err := readHeader(strings.NewReader(string(b)))
			if err != nil {
				continue
			}
			_, body, _ := strings.Cut(string(b), "\n---\n")
			return ThreadMessage{ID: h.ID, From: h.From, To: h.To, Thread: h.Thread, Subject: h.Subject, Created: h.Created, Body: body, Priority: h.Priority, Kind: h.Kind}, true
		}
	}
	return ThreadMessage{}, false
}

// Run fails: amq's own verbs (read, drain, thread, trace, dlq, list) are not
// emulated.
func (f *Fake) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	return 2, fmt.Errorf("amq %s: %w", verb, ErrNotEmulated)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(b)+"\n")
}

// writeFile renames the file into place, as amq does, so a reader sees all of
// it or none.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
