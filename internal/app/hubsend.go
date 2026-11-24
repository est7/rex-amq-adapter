package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The Operator writes to an Epic's narrator from anywhere (`hub send`, a new
// topic; `hub reply`, an answer to an inbox entry): each message is kept in
// the Hub's inbox (kind operator) and then mailed to the narrator in its own
// name as an OpOperatorRelay, which the gateway admits only for an Epic
// linked to that Hub (core.ReasonOperatorRelay). The narrator relays it to
// its lead as a control directive under `hub:<seq>`, the entry's seq, which
// the message's subject names. A personal agent's reply to a narrator is
// kept too (kind reply, mirrorPersonalMail).

// relayContextKey is the context key of the relay message: the Hub inbox
// entry it carries.
const relayContextKey = "rex_amq_adapter_hub"

// relayContext is what a relay message names in its context.
type relayContext struct {
	Hub string `json:"hub"`
	Seq int64  `json:"seq"`
}

// HubSendRequest is a message of the Operator to an Epic's narrator: to
// Session (hub send), or answering the inbox entry ReplyTo (hub reply),
// whose Epic, thread, and message it takes. OpID names it, so a retry
// sends it once.
type HubSendRequest struct {
	Hub     string
	Session string
	ReplyTo int64
	Body    string
	OpID    string
	DryRun  bool
}

// HubSendResult is what was kept and sent.
type HubSendResult struct {
	Hub       string      `json:"hub"`
	Seq       int64       `json:"seq,omitempty"`
	Epic      string      `json:"epic"`
	Narrator  core.Handle `json:"narrator"`
	MsgID     string      `json:"msg_id,omitempty"`
	Thread    string      `json:"thread"`
	InReplyTo int64       `json:"in_reply_to,omitempty"`
	Control   string      `json:"control,omitempty"`
	Replayed  bool        `json:"replayed,omitempty"`
	DryRun    bool        `json:"dry_run,omitempty"`
}

// SendToNarrator keeps the Operator's (ResolveOperator) message in the
// Hub's inbox and mails it to the narrator of its Epic, which must be an
// Epic linked to the Hub in the Operator's scope, with a live narrator. A
// retry under the same OpID finds the entry it kept, and mails it again
// only when the narrator's mailbox holds no relay of it.
func (e Env) SendToNarrator(ctx context.Context, getenv func(string) string, r HubSendRequest) (HubSendResult, error) {
	op, err := e.ResolveOperator(getenv)
	if err != nil {
		return HubSendResult{}, err
	}
	if err := ValidOpID(r.OpID); err != nil {
		return HubSendResult{}, err
	}
	if strings.TrimSpace(r.Body) == "" {
		return HubSendResult{}, fmt.Errorf("%w: a message to the narrator needs a body", ErrUsage)
	}
	scope, err := e.cockpitScope(op, CockpitViewRequest{Hub: r.Hub})
	if err != nil {
		return HubSendResult{}, err
	}
	if scope.hubDir == "" {
		return HubSendResult{}, fmt.Errorf("%w: the Operator's messages go through a Hub, and %s is of no Hub", ErrUsage, cockpitWhose(scope))
	}
	res := HubSendResult{Hub: scope.hubDir, Epic: r.Session, DryRun: r.DryRun}
	var parent *store.HubInboxEntry
	if r.ReplyTo > 0 {
		all, err := e.Store.HubInbox(scope.hubDir)
		if err != nil {
			return res, err
		}
		i := slices.IndexFunc(all, func(x store.HubInboxEntry) bool { return x.Seq == r.ReplyTo })
		if i < 0 {
			return res, fault.New(fault.NotFound, "the inbox of the Hub %s has no entry %d", scope.hubDir, r.ReplyTo).With("kind", "inbox_entry")
		}
		parent, res.Epic, res.InReplyTo = &all[i], all[i].Epic, r.ReplyTo
	}
	link, err := e.relayLink(scope, res.Epic)
	if err != nil {
		return res, err
	}
	narr, err := e.liveNarrator(link.Epic)
	if err != nil {
		return res, err
	}
	res.Narrator = narr.Handle
	if parent != nil && parent.Thread != "" {
		res.Thread = parent.Thread
	}
	if r.DryRun {
		return res, nil
	}
	name := r.OpID
	if name == "" {
		if name, err = newToken(); err != nil {
			return res, err
		}
	}
	key := "operator:" + name
	if res.Thread == "" {
		// A new topic's own thread, the same for its retries.
		sum := sha256.Sum256([]byte(key))
		res.Thread = "hub/" + strings.TrimPrefix(res.Epic, "session:") + "/" + hex.EncodeToString(sum[:6])
	}
	x, added, err := e.Store.AppendHubInbox(ctx, scope.hubDir, store.HubInboxEntry{Epic: res.Epic, HubKey: link.Hub, Kind: store.InboxOperator,
		Thread: res.Thread, Body: r.Body, InReplyTo: res.InReplyTo, By: op.By(), DedupeKey: key}, e.Now())
	if err != nil {
		return res, err
	}
	if !added && (x.Epic != res.Epic || x.Body != r.Body || x.InReplyTo != res.InReplyTo) {
		return res, refusal(fault.Conflict, "op_id_reused", "--op-id %q already sent another message (inbox entry %d)", r.OpID, x.Seq)
	}
	res.Seq, res.Replayed, res.Control = x.Seq, !added, "hub:"+strconv.FormatInt(x.Seq, 10)
	if !added {
		if id, ok, err := e.relayedAlready(narr.Handle, scope.hubDir, x.Seq); err != nil || ok {
			res.MsgID = id
			return res, err
		}
	}
	rc, err := json.Marshal(map[string]relayContext{relayContextKey: {Hub: scope.hubDir, Seq: x.Seq}})
	if err != nil {
		return res, err
	}
	args := []string{"--allow-self", "--labels", amq.SystemLabel, "--thread", res.Thread,
		"--subject", fmt.Sprintf("operator %s: %s", res.Control, summaryLine(r.Body)), "--body", r.Body}
	if parent != nil && parent.MsgID != "" {
		args = append(args, "--refs", parent.MsgID)
	}
	sent, _, err := e.post(ctx, Outgoing{From: narr.Handle, To: []core.Handle{narr.Handle}, Check: true, Context: rc,
		Op: core.Operation{Kind: core.OpOperatorRelay, System: true, Hub: link.Hub}, Args: args})
	if sent.ID == "" {
		if err == nil {
			err = fmt.Errorf("amq returned no message id")
		}
		return res, fmt.Errorf("the message is inbox entry %d; mailing it to %s failed (run the same command with the same --op-id to send it): %w", x.Seq, narr.Handle, err)
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "relay %d to %s: delivered %s, then: %v\n", x.Seq, narr.Handle, sent.ID, err)
	}
	res.MsgID = sent.ID
	if err := e.ensureWaker(ctx, narr); err != nil {
		fmt.Fprintf(Diagnostics, "relay %d to %s: the waker: %v\n", x.Seq, narr.Handle, err)
	}
	return res, nil
}

// relayLink is the link of the Epic session to the Hub scope reads: the
// one an Operator's message travels under. An Epic of another Hub, or of
// none, or closed (its links gone), takes none.
func (e Env) relayLink(scope cockpitScope, session string) (core.Link, error) {
	links, err := e.linksToEpic(session)
	if err != nil {
		return core.Link{}, err
	}
	for _, l := range links {
		if l.Hub != "" && l.Hub.Dir() == scope.hubDir && scope.matches(e, l) {
			return l, nil
		}
	}
	return core.Link{}, refused(fmt.Sprintf("the Operator of %s writing to %s", cockpitWhose(scope), session), core.Decision{Reason: core.ReasonRelayNotLinked})
}

// liveNarrator is the live narrator of the Epic session.
func (e Env) liveNarrator(epic core.SessionKey) (agent, error) {
	recs, err := e.agents()
	if err != nil {
		return agent{}, err
	}
	for _, r := range recs {
		if r.Live() && r.Role == core.NarratorRole && r.SessionID == epic.SessionID && (epic.Server == "" || r.Server == epic.Server) {
			return r, nil
		}
	}
	return agent{}, fault.New(fault.FailedPrecondition, "the Epic %s has no live narrator to take the message (`rex-amq-adapter epic revive %s` brings it back)", epic.SessionID, epic.SessionID).
		With("reason", "no_live_narrator")
}

// relayedAlready is the id of the relay of inbox entry seq of the Hub dir
// in narrator n's mailbox, read or not: a retry that finds one sends it
// not again.
func (e Env) relayedAlready(n core.Handle, dir string, seq int64) (string, bool, error) {
	root := e.Paths.Root()
	for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained} {
		ids, err := amq.IDs(root, string(n), box)
		if err != nil {
			return "", false, err
		}
		for _, id := range ids {
			h, err := amq.ReadHeader(root, string(n), box, id)
			if err != nil {
				continue
			}
			var rc relayContext
			if h.From == string(n) && h.ContextOf(relayContextKey, &rc) && rc.Hub == dir && rc.Seq == seq {
				return id, true, nil
			}
		}
	}
	return "", false, nil
}

// summaryLine is the first line of body, cut for a subject.
func summaryLine(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if r := []rune(line); len(r) > 60 {
		line = string(r[:60]) + "…"
	}
	return line
}

// mirrorPersonalMail keeps personal agent p's message to a narrator, once
// amq delivered it as sent, in the inbox of the Hub the narrator's Epic is
// of: kind reply, answering the inbox entry of the narrator's message it
// names. That message is found by id only, through the replies and
// deliveries again that name it (originalIDs), never guessed from the
// thread. A failure is reported, never the send's: the mail is delivered.
func (e Env) mirrorPersonalMail(ctx context.Context, p agent, narrator core.Handle, sent amq.Sent, replyTo string, refs []string, body string) {
	n, ok, err := e.agentOf(narrator)
	if err != nil || !ok || n.Role != core.NarratorRole || sent.ID == "" {
		return
	}
	hub, err := e.epicHub(n.SessionID)
	if err != nil || hub == "" {
		return
	}
	all, err := e.Store.HubInbox(hub.Dir())
	if err != nil {
		fmt.Fprintf(Diagnostics, "keep %s's reply %s in the Hub's inbox: %v\n", p.Handle, sent.ID, err)
		return
	}
	var inReplyTo int64
	for _, id := range e.originalIDs(p.Handle, append([]string{replyTo}, refs...)) {
		if i := slices.IndexFunc(all, func(x store.HubInboxEntry) bool { return x.MsgID == id && x.Epic == n.SessionID }); i >= 0 {
			inReplyTo = all[i].Seq
			break
		}
	}
	seq, _ := e.latestHandBackSeq(n.SessionID)
	if _, _, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{Epic: n.SessionID, HubKey: hub, Kind: store.InboxReply, MsgID: sent.ID, Thread: sent.Thread,
		HandBackSeq: seq, Body: body, InReplyTo: inReplyTo, By: p.Handle, DedupeKey: "reply:" + sent.ID}, e.Now()); err != nil {
		fmt.Fprintf(Diagnostics, "keep %s's reply %s in the Hub's inbox: %v\n", p.Handle, sent.ID, err)
	}
}

// originalIDs are ids and, nearest first, the messages they name in h's
// mailbox: a reply's refs, a delivery again's original (rerouted_from), a
// converted reply's refs, up to a few hops.
func (e Env) originalIDs(h core.Handle, ids []string) []string {
	var out []string
	queue := slices.Clone(ids)
	for hops := 0; len(queue) > 0 && hops < 16; hops++ {
		id := queue[0]
		queue = queue[1:]
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
		m, ok, err := e.findMessage(h, id)
		if err != nil || !ok {
			continue
		}
		var from string
		if raw, ok := m.header.Context["rerouted_from"]; ok && json.Unmarshal(raw, &from) == nil {
			queue = append(queue, from)
		}
		queue = append(queue, m.header.Refs...)
	}
	return out
}
