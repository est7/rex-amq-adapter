package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
)

// `rex-amq-adapter events --json` streams the projection of a scope as NDJSON
// (docs/design.md, "Observing"): a snapshot, then upsert and remove frames for
// what changed, event frames for what happened (a doorbell's outcome, a
// message drained or dead-lettered), and a heartbeat. Every frame carries the
// stream's id and a sequence number, so a reader can tell a restart from a
// gap.

// EventsSchema is the version of the frame format.
const EventsSchema = 1

// StreamTicks are how often a stream looks. Zero values take the defaults:
// local state every second, Rex every two, a heartbeat every five.
type StreamTicks struct {
	Local, Rex, Heartbeat time.Duration
}

type envelope struct {
	Schema int       `json:"schema"`
	Stream string    `json:"stream"`
	Seq    uint64    `json:"seq"`
	Type   string    `json:"type"`
	At     time.Time `json:"at"`
}

type snapshotFrame struct {
	envelope
	Scope    Scope          `json:"scope"`
	Agents   []core.Agent   `json:"agents"`
	Messages []core.Message `json:"messages"`
	// Panel is the session's panel (`panel --json`); events leaves it out.
	Panel *PanelProjection `json:"panel,omitempty"`
}

type entityFrame struct {
	envelope
	Entity  string   `json:"entity"`
	Key     string   `json:"key"`
	Value   any      `json:"value,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// Event kinds.
const (
	EventDoorbell     = "doorbell"
	EventDrained      = "drained"
	EventDeadLettered = "dead_lettered"
)

type eventFrame struct {
	envelope
	Kind       string        `json:"kind"`
	Handle     core.Handle   `json:"handle,omitempty"`
	MsgID      string        `json:"msg_id,omitempty"`
	Sender     string        `json:"sender,omitempty"`
	Thread     string        `json:"thread,omitempty"`
	Progress   core.Progress `json:"progress,omitempty"`
	Code       string        `json:"code,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
}

// streamer is one stream's state. It owns nothing on disk.
type streamer struct {
	e     Env
	enc   *json.Encoder
	id    string
	seq   uint64
	scope Scope
	start time.Time

	v        view
	activity map[core.Handle]*core.ActivityObservation
	headers  headerCache
	prev     core.Snapshot

	recs      []agent
	recsStamp time.Time

	// panel is the panel's state when the stream is `panel --json`'s; nil
	// for `events --json`.
	panel *panelState

	// What has been told already, so a reread file or a box listed again
	// tells nothing twice.
	doorbellAt map[core.Handle]time.Time
	receipts   map[core.Handle]map[string]bool
	dead       map[string]bool // handle/id
}

// Stream writes the stream of sc to w until ctx ends or a write fails.
func (e Env) Stream(ctx context.Context, w io.Writer, sc Scope, ticks StreamTicks) error {
	return e.stream(ctx, w, sc, ticks, false)
}

// stream is Stream, with the session's panel (PanelProjection) in its
// snapshot and its changes as panel upserts when withPanel is set.
func (e Env) stream(ctx context.Context, w io.Writer, sc Scope, ticks StreamTicks, withPanel bool) error {
	if ticks.Local <= 0 {
		ticks.Local = time.Second
	}
	if ticks.Rex <= 0 {
		ticks.Rex = 2 * time.Second
	}
	if ticks.Heartbeat <= 0 {
		ticks.Heartbeat = 5 * time.Second
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	s := &streamer{
		e: e, enc: json.NewEncoder(w), id: hex.EncodeToString(id), scope: sc, start: e.Now().UTC(),
		headers: headerCache{}, doorbellAt: map[core.Handle]time.Time{}, receipts: map[core.Handle]map[string]bool{}, dead: map[string]bool{},
	}
	var err error
	if s.v, err = e.loadView(ctx, sc); err != nil {
		return err
	}
	held, err := s.held()
	if err != nil {
		return err
	}
	s.activity = e.observeActivity(ctx, held)
	snap, obs, err := s.observe(held)
	if err != nil {
		return err
	}
	s.seed(held, obs)
	frame := snapshotFrame{Scope: sc, Agents: snap.Agents, Messages: snap.Messages}
	if withPanel {
		s.panel = &panelState{}
		p, err := s.panelNow(held)
		if err != nil {
			return err
		}
		frame.Panel = &p
	}
	frame.envelope = s.next("snapshot")
	if err := s.emit(frame); err != nil {
		return err
	}
	s.prev = snap

	local, rex, beat := time.NewTicker(ticks.Local), time.NewTicker(ticks.Rex), time.NewTicker(ticks.Heartbeat)
	defer local.Stop()
	defer rex.Stop()
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-beat.C:
			if err := s.emit(s.next("heartbeat")); err != nil {
				return err
			}
			continue
		case <-rex.C:
			if v, err := e.loadView(ctx, sc); err != nil {
				fmt.Fprintf(Diagnostics, "events: %v\n", err)
			} else {
				s.v = v
			}
			if held, err := s.held(); err == nil {
				s.activity = e.observeActivity(ctx, held)
			}
		case <-local.C:
		}
		if err := s.tick(); err != nil {
			return err
		}
	}
}

func (s *streamer) next(typ string) envelope {
	s.seq++
	return envelope{Schema: EventsSchema, Stream: s.id, Seq: s.seq, Type: typ, At: s.e.Now().UTC()}
}

func (s *streamer) emit(frame any) error { return s.enc.Encode(frame) }

// held are the agents in scope. They are reread only when their identities
// or leases changed: every write renames a file into their directory.
func (s *streamer) held() ([]agent, error) {
	stamp, err := s.e.Store.AgentsChanged()
	if err != nil {
		return nil, err
	}
	if s.recs == nil || stamp.IsZero() || !stamp.Equal(s.recsStamp) {
		recs, err := s.e.agents()
		if err != nil {
			return nil, err
		}
		s.recs, s.recsStamp = recs, stamp
	}
	var out []agent
	for _, r := range s.recs {
		if s.v.holds(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *streamer) observe(held []agent) (core.Snapshot, core.Observations, error) {
	agents, err := s.e.observeAgents(s.v, held, s.activity)
	if err != nil {
		return core.Snapshot{}, core.Observations{}, err
	}
	handles := make([]core.Handle, len(held))
	for i, r := range held {
		handles[i] = r.Handle
	}
	msgs, err := s.e.observeMessages(handles, held, s.headers)
	if err != nil {
		return core.Snapshot{}, core.Observations{}, err
	}
	obs := core.Observations{Agents: agents, Messages: msgs}
	return core.Project(obs), obs, nil
}

// tick emits what changed since the last look. A look that fails is reported
// and tried again at the next tick; only a failed write ends the stream.
func (s *streamer) tick() error {
	held, err := s.held()
	if err != nil {
		fmt.Fprintf(Diagnostics, "events: %v\n", err)
		return nil
	}
	snap, obs, err := s.observe(held)
	if err != nil {
		fmt.Fprintf(Diagnostics, "events: %v\n", err)
		return nil
	}
	for _, f := range core.Diff(s.prev, snap) {
		if err := s.emit(entityFrame{envelope: s.next(f.Type), Entity: f.Entity, Key: f.Key, Value: f.Value, Changed: f.Changed}); err != nil {
			return err
		}
	}
	s.prev = snap
	if s.panel != nil {
		if err := s.panelTick(held); err != nil {
			return err
		}
	}
	for _, ev := range s.events(held, obs) {
		ev.envelope = s.next("event")
		if err := s.emit(ev); err != nil {
			return err
		}
	}
	return nil
}

// seed marks what already happened when the stream starts, so its first
// events are news: doorbell attempts, receipts, and dead letters present now
// are history, and the snapshot already shows them.
func (s *streamer) seed(held []agent, obs core.Observations) {
	for _, a := range obs.Agents {
		at := s.start
		if n := len(a.Doorbells); n > 0 && a.Doorbells[n-1].At.After(at) {
			at = a.Doorbells[n-1].At
		}
		s.doorbellAt[a.Handle] = at
	}
	for _, r := range held {
		s.receiptsOf(r.Handle, true)
	}
	for _, m := range obs.Messages {
		for _, c := range m.Recipients {
			if c.Box == core.BoxDead {
				s.dead[c.Handle+"/"+m.ID] = true
			}
		}
	}
}

// events are what happened since the last look: doorbell attempts appended
// to an agent's injector log after the stream started (the log is cut by
// rewriting it, so attempts are told by time, not by offset), receipts that
// are new, and messages that reached a dead-letter box.
func (s *streamer) events(held []agent, obs core.Observations) []eventFrame {
	var out []eventFrame
	for _, a := range obs.Agents {
		after, ok := s.doorbellAt[a.Handle]
		if !ok {
			after = s.start
		}
		for _, at := range a.Doorbells {
			if at.At.After(after) {
				out = append(out, eventFrame{Kind: EventDoorbell, Handle: a.Handle, Progress: at.Progress, Code: at.Code, OccurredAt: at.At})
				after = at.At
			}
		}
		s.doorbellAt[a.Handle] = after
	}
	for _, r := range held {
		for _, rc := range s.receiptsOf(r.Handle, false) {
			out = append(out, eventFrame{Kind: EventDrained, Handle: core.Handle(rc.Consumer), MsgID: rc.MsgID, Sender: rc.Sender, Thread: rc.Thread, OccurredAt: rc.EmittedAt})
		}
	}
	for _, m := range obs.Messages {
		for _, c := range m.Recipients {
			if k := c.Handle + "/" + m.ID; c.Box == core.BoxDead && !s.dead[k] {
				s.dead[k] = true
				out = append(out, eventFrame{Kind: EventDeadLettered, Handle: core.Handle(c.Handle), MsgID: m.ID, Sender: m.From, Thread: m.Thread, OccurredAt: s.e.Now().UTC()})
			}
		}
	}
	return out
}

// receiptsOf returns h's drained receipts not told yet. A handle first seen
// after the stream started tells only receipts emitted after that; seeding
// tells none.
func (s *streamer) receiptsOf(h core.Handle, seeding bool) []amq.Receipt {
	root := s.e.Paths.Root()
	names, err := amq.Receipts(root, string(h))
	if err != nil {
		fmt.Fprintf(Diagnostics, "events: receipts of %s: %v\n", h, err)
		return nil
	}
	seen, known := s.receipts[h]
	if !known {
		seen = map[string]bool{}
		s.receipts[h] = seen
	}
	var out []amq.Receipt
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if seeding {
			continue
		}
		rc, err := amq.ReadReceipt(root, string(h), n)
		if err != nil {
			fmt.Fprintf(Diagnostics, "events: %v\n", err)
			continue
		}
		if rc.Stage != "drained" || (!known && !rc.EmittedAt.After(s.start)) {
			continue
		}
		out = append(out, rc)
	}
	return out
}
