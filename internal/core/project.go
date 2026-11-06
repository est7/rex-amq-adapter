package core

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The projection turns what the adapter observes of its agents and their mail
// into entities a reader can keep: agents keyed by handle, messages by id.
// `who` renders it once; `events --json` renders it, then the differences
// between successive projections. Both are read-only views: nothing here
// decides delivery or lifecycle.

// Observations is what was seen of one scope at one moment.
type Observations struct {
	Agents   []AgentObservation
	Messages []MessageObservation
}

// AgentObservation is one record, the layout it was found in, and its mail.
type AgentObservation struct {
	Handle                               Handle
	Kind                                 Kind
	Server, SessionID, WindowID, BlockID string
	Tab, Role                            string
	Placed                               bool // the block is in the observed layout
	Task                                 string
	Adopting, Departing                  bool // a transition the record is in the middle of
	AdoptedAt                            time.Time
	DepartedAt                           *time.Time
	DepartReason                         string
	Activity                             *ActivityObservation // nil: not observed
	Unread                               []string
	Announced                            map[string]bool // ids the agent was told of
	Doorbells                            []Attempt       // oldest first
}

// ActivityObservation is what the agent reported doing. Source names where it
// came from: Rex's program status, or Claude Code's supervisor for a
// background job.
type ActivityObservation struct {
	State  AgentState
	Kind   string
	Source string
	Err    bool // the observation failed
	At     time.Time
}

// MessageObservation is one message and where each recipient in the scope
// holds it.
type MessageObservation struct {
	ID, From, Thread, Subject string
	To                        []string
	Created                   time.Time
	Recipients                []RecipientObservation
}

// RecipientObservation is one recipient's copy of a message.
type RecipientObservation struct {
	Handle    string
	Box       Box
	Announced bool
	Departed  bool
}

// Box is where a recipient holds a message.
type Box string

const (
	BoxNone   Box = ""    // not in this recipient's mailbox (or outside the scope)
	BoxUnread Box = "new" // inbox/new
	BoxRead   Box = "cur" // inbox/cur: drained
	BoxDead   Box = "dlq" // dead-lettered
)

// Snapshot is a projection: entities sorted by key.
type Snapshot struct {
	Agents   []Agent   `json:"agents"`
	Messages []Message `json:"messages"`
}

// Agent is an agent entity, keyed by Handle.
type Agent struct {
	Handle    Handle    `json:"handle"`
	Kind      Kind      `json:"kind"`
	Server    string    `json:"server"`
	SessionID string    `json:"session_id"`
	WindowID  string    `json:"window_id"`
	Tab       string    `json:"tab"`
	BlockID   string    `json:"block_id"`
	Pane      string    `json:"pane"` // "present" | "gone"
	Task      string    `json:"task"`
	Role      string    `json:"role"`
	Lifecycle Lifecycle `json:"lifecycle"`
	Activity  Activity  `json:"activity"`
	Delivery  Delivery  `json:"delivery"`
}

// Lifecycle states of an agent.
const (
	LifeAdopting  = "adopting"
	LifeLive      = "live"
	LifeDeparting = "departing"
	LifeDeparted  = "departed"
)

type Lifecycle struct {
	State        string     `json:"state"`
	DepartReason string     `json:"depart_reason,omitempty"`
	AdoptedAt    time.Time  `json:"adopted_at"`
	DepartedAt   *time.Time `json:"departed_at,omitempty"`
}

// Activity is what the agent reports doing. ObservedAt and Source describe the
// sample, not the agent, and do not count as a change.
type Activity struct {
	State      string     `json:"state"` // absent | idle | working | blocked | done | error
	Kind       string     `json:"kind,omitempty"`
	Source     string     `json:"source"` // rex_status | supervisor | none
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Delivery is the state of the agent's mail. PendingDoorbell: unread mail
// whose doorbell has not been accepted yet; an in-process agent's extension
// delivers instead, so it has none.
type Delivery struct {
	Unread          int            `json:"unread"`
	Announced       int            `json:"announced"`
	Doorbell        *DoorbellState `json:"doorbell"`
	PendingDoorbell bool           `json:"pending_doorbell"`
	InProcess       bool           `json:"in_process"`
}

// DoorbellState is the latest injector outcome and since when the same outcome
// has repeated.
type DoorbellState struct {
	Progress Progress  `json:"progress"`
	Code     string    `json:"code,omitempty"`
	Since    time.Time `json:"since"`
}

// Message states.
const (
	MsgSent         = "sent"
	MsgQueued       = "queued"
	MsgDrained      = "drained"
	MsgDeadLettered = "dead_lettered"
)

// Message is a message entity, keyed by ID. It never carries the body.
type Message struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        []string  `json:"to"`
	Thread    string    `json:"thread"`
	Subject   string    `json:"subject"`
	State     string    `json:"state"`
	Announced bool      `json:"announced"`
	Bounced   bool      `json:"bounced"`
	CreatedAt time.Time `json:"created_at"`
}

// Project builds the snapshot of obs.
func Project(obs Observations) Snapshot {
	s := Snapshot{Agents: make([]Agent, 0, len(obs.Agents)), Messages: make([]Message, 0, len(obs.Messages))}
	for _, a := range obs.Agents {
		s.Agents = append(s.Agents, projectAgent(a))
	}
	slices.SortFunc(s.Agents, func(a, b Agent) int { return strings.Compare(string(a.Handle), string(b.Handle)) })
	for _, m := range obs.Messages {
		s.Messages = append(s.Messages, projectMessage(m))
	}
	slices.SortFunc(s.Messages, func(a, b Message) int { return strings.Compare(a.ID, b.ID) })
	return s
}

func projectAgent(o AgentObservation) Agent {
	a := Agent{
		Handle: o.Handle, Kind: o.Kind, Server: o.Server, SessionID: o.SessionID, WindowID: o.WindowID,
		Tab: o.Tab, BlockID: o.BlockID, Pane: "gone", Task: o.Task, Role: o.Role,
	}
	if o.Placed {
		a.Pane = "present"
	}
	a.Lifecycle = Lifecycle{AdoptedAt: o.AdoptedAt, DepartedAt: o.DepartedAt, DepartReason: o.DepartReason}
	switch {
	case o.DepartedAt != nil && o.Departing:
		a.Lifecycle.State = LifeDeparting
	case o.DepartedAt != nil:
		a.Lifecycle.State = LifeDeparted
	case o.Adopting:
		a.Lifecycle.State = LifeAdopting
	default:
		a.Lifecycle.State = LifeLive
	}
	a.Activity = projectActivity(o)
	d := Delivery{Unread: len(o.Unread), InProcess: o.Kind.InProcess()}
	for _, id := range o.Unread {
		if o.Announced[id] {
			d.Announced++
		}
	}
	if last, since, ok := lastRun(o.Doorbells); ok {
		d.Doorbell = &DoorbellState{Progress: last.Progress, Code: last.Code, Since: since}
	}
	d.PendingDoorbell = d.Unread > 0 && !d.InProcess && (d.Doorbell == nil || d.Doorbell.Progress != Accepted)
	a.Delivery = d
	return a
}

func projectActivity(o AgentObservation) Activity {
	if o.DepartedAt != nil || o.Activity == nil {
		return Activity{State: "absent", Source: "none"}
	}
	at := o.Activity.At
	act := Activity{Kind: o.Activity.Kind, Source: o.Activity.Source, ObservedAt: &at}
	switch {
	case o.Activity.Err:
		act.State, act.Kind = "absent", "observe_failed"
	case o.Activity.State == StateAbsent:
		act.State = "absent"
	case slices.Contains([]AgentState{StateIdle, StateWorking, StateBlocked, StateDone, StateError}, o.Activity.State):
		act.State = string(o.Activity.State)
	default:
		act.State, act.Kind = string(StateError), "unknown_state:"+string(o.Activity.State)
	}
	return act
}

func projectMessage(o MessageObservation) Message {
	m := Message{
		ID: o.ID, From: o.From, To: o.To, Thread: o.Thread, Subject: SanitizeSubject(o.Subject),
		CreatedAt: o.Created, State: MsgSent,
	}
	if m.To == nil {
		m.To = []string{}
	}
	// The state is the least advanced copy among the recipients in scope: a
	// message is drained only once every one of them has read it.
	held, read, unread, dead := 0, 0, 0, 0
	for _, r := range o.Recipients {
		switch r.Box {
		case BoxUnread:
			held++
			unread++
			m.Announced = m.Announced || r.Announced
			m.Bounced = m.Bounced || r.Departed
		case BoxRead:
			held++
			read++
		case BoxDead:
			held++
			dead++
		}
	}
	switch {
	case dead > 0:
		m.State = MsgDeadLettered
	case unread > 0:
		m.State = MsgQueued
	case held > 0 && read == held:
		m.State = MsgDrained
	}
	return m
}

// lastRun is the latest attempt in history and when the run of attempts with
// its outcome began.
func lastRun(history []Attempt) (last Attempt, since time.Time, ok bool) {
	if len(history) == 0 {
		return Attempt{}, time.Time{}, false
	}
	last = history[len(history)-1]
	since = last.At
	for i := len(history) - 2; i >= 0; i-- {
		a := history[i]
		if a.Progress != last.Progress || a.Code != last.Code {
			break
		}
		since = a.At
	}
	return last, since, true
}

// DoorbellSummary is who's LAST DOORBELL column for a: see DeliveryStatus.
func (a Agent) DoorbellSummary(now time.Time) string {
	if a.Delivery.InProcess {
		return "in-process" // its extension delivers; no doorbell is typed
	}
	return summarize(a.Delivery.Doorbell, a.Delivery.Unread, now)
}

func summarize(d *DoorbellState, unread int, now time.Time) string {
	if d == nil || unread == 0 && d.Progress != Accepted {
		return "-"
	}
	what := string(d.Progress)
	if d.Code != "" {
		what += " " + d.Code
	}
	return what + " " + Ago(now.Sub(d.Since))
}

// MaxSubject is the longest subject a message entity carries, in runes.
const MaxSubject = 120

// SanitizeSubject makes a sender's subject safe to show: one line, no control
// characters (C0, DEL, C1) or bidirectional overrides and isolates, at most
// MaxSubject runes. A subject is another agent's text; a terminal or log that
// shows it must not be steered by it.
func SanitizeSubject(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f':
			space = true
			continue
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			continue
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
			continue
		}
		if space && n > 0 {
			b.WriteRune(' ')
			n++
		}
		space = false
		b.WriteRune(r)
		n++
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) <= MaxSubject {
		return out
	}
	runes := []rune(out)
	return string(runes[:MaxSubject-1]) + "…"
}

// Frame is one change between two snapshots, before the stream's envelope.
type Frame struct {
	Type    string   `json:"type"`   // upsert | remove
	Entity  string   `json:"entity"` // agent | message
	Key     string   `json:"key"`
	Value   any      `json:"value,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// Diff lists the frames that turn prev into next: removals, then upserts,
// agents before messages, each sorted by key. An agent whose only difference
// is the sampling of its activity (ObservedAt, Source) is unchanged, so
// polling alone emits nothing.
func Diff(prev, next Snapshot) []Frame {
	var out []Frame
	out = append(out, diffEntities("agent", prev.Agents, next.Agents, func(a Agent) string { return string(a.Handle) }, comparableAgent)...)
	out = append(out, diffEntities("message", prev.Messages, next.Messages, func(m Message) string { return m.ID }, func(m Message) any { return m })...)
	slices.SortStableFunc(out, func(a, b Frame) int {
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type) // remove before upsert
		}
		return 0
	})
	return out
}

func comparableAgent(a Agent) any {
	a.Activity.ObservedAt, a.Activity.Source = nil, ""
	return a
}

func diffEntities[T any](entity string, prev, next []T, key func(T) string, cmp func(T) any) []Frame {
	before := make(map[string]T, len(prev))
	for _, e := range prev {
		before[key(e)] = e
	}
	var out []Frame
	seen := make(map[string]bool, len(next))
	for _, e := range next {
		k := key(e)
		seen[k] = true
		old, had := before[k]
		if !had {
			out = append(out, Frame{Type: "upsert", Entity: entity, Key: k, Value: e, Changed: fieldsOf(e)})
			continue
		}
		if changed := changedFields(cmp(old), cmp(e)); len(changed) > 0 {
			out = append(out, Frame{Type: "upsert", Entity: entity, Key: k, Value: e, Changed: changed})
		}
	}
	for _, e := range prev {
		if k := key(e); !seen[k] {
			out = append(out, Frame{Type: "remove", Entity: entity, Key: k})
		}
	}
	return out
}

// changedFields are the top-level JSON fields that differ between a and b.
func changedFields(a, b any) []string {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	fa, fb := jsonFields(a), jsonFields(b)
	var out []string
	for name, va := range fa {
		if string(va) != string(fb[name]) {
			out = append(out, name)
		}
	}
	for name := range fb {
		if _, ok := fa[name]; !ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func fieldsOf(v any) []string {
	var out []string
	for name := range jsonFields(v) {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func jsonFields(v any) map[string]json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return m
}
