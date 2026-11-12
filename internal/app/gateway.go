package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The gateway applies core.CanExchange on the paths the adapter owns: the
// mail it sends (post), the mail an agent finds in its inbox (admit, before
// any doorbell or hook notice), and the managed read commands (exposed).

// adapterContext is the adapter's object in a message's context.
type adapterContext struct {
	Token string `json:"token"`
}

// endpointOf is h as the gateway sees it now; a handle without a record is
// in no session.
func (e Env) endpointOf(h core.Handle) (core.Endpoint, error) {
	r, ok, err := e.Store.RecordOf(h)
	if err != nil || !ok {
		return core.Endpoint{Handle: h}, err
	}
	return r.Endpoint(), nil
}

// judge decides whether reader may see a message it received, by what the
// adapter recorded at sending. A message the adapter has no record of (sent
// with amq directly) is judged by its sender's record now, as an ordinary
// send: its header's labels and sender name prove nothing.
func (e Env) judge(reader store.Record, h amq.Header, links []core.Link) (core.Decision, core.Endpoint, error) {
	var ac adapterContext
	h.ContextOf(store.ProvenanceContextKey, &ac)
	p, ok, err := e.Store.ProvenanceOf(h.ID, ac.Token, core.Handle(h.From), h.To)
	if err != nil {
		return core.Decision{}, core.Endpoint{}, err
	}
	from, op := p.From, p.Operation(reader.Handle)
	if !ok {
		if from, err = e.endpointOf(core.Handle(h.From)); err != nil {
			return core.Decision{}, core.Endpoint{}, err
		}
		op = core.Operation{Kind: core.OpSend}
	} else if op.Addressed == nil {
		// The adapter sent it to others: it is in this inbox by no send of
		// the adapter's.
		op.Addressed = &core.Endpoint{}
	}
	return core.CanExchange(from, reader.Endpoint(), op, links), from, nil
}

// admit is the arrival gate: it moves the unread mail rec may not receive
// into the quarantine, unread and without a receipt, and returns the ids
// left. For an agent in a session it fails closed: a file whose header
// cannot be read (corrupt, unclosed, of another schema) names no sender to
// judge, and is quarantined too. amq renames whole files into place, so none
// is read mid-write.
func (e Env) admit(rec store.Record) ([]string, error) {
	root, h := e.Paths.Root(), string(rec.Handle)
	ids, err := amq.Unread(root, h)
	if err != nil || len(ids) == 0 {
		return ids, err
	}
	links, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, id := range ids {
		var d core.Decision
		var from core.Endpoint
		hdr, err := amq.ReadHeader(root, h, amq.BoxUnread, id)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // drained or quarantined by another reader
		case err != nil && !rec.Endpoint().Placed():
			// A record in no session (a test's synthetic one) keeps it:
			// every readable sender is refused to it anyway.
			kept = append(kept, id)
			continue
		case err != nil:
			d = core.Decision{Reason: core.ReasonUnreadableHeader}
		default:
			if d, from, err = e.judge(rec, hdr, links); err != nil {
				return nil, err
			}
		}
		if d.Allow {
			kept = append(kept, id)
			continue
		}
		q := store.Quarantined{ID: id, Handle: rec.Handle, From: from, To: rec.Endpoint(), Decision: d, At: e.Now().UTC()}
		if _, err := e.Store.Quarantine(filepath.Join(amq.Inbox(root, h), id+".md"), q); err != nil {
			return nil, fmt.Errorf("quarantine %s for %s: %w", id, h, err)
		}
		fmt.Fprintf(Diagnostics, "quarantined %s for %s from %s: %s\n", id, h, hdr.From, d.Reason)
	}
	return kept, nil
}

// ErrRefused is a send or read the gateway refused.
var ErrRefused = errors.New("refused by the gateway")

func refused(what string, d core.Decision) error {
	return fmt.Errorf("%w: %s (%s)", ErrRefused, what, d.Reason)
}

// Outgoing is a message the adapter sends: To for a send, ReplyTo (a message
// id in From's mailbox) for a reply. Args are the rest of amq's flags;
// Context is the sender's own context object, if any.
type Outgoing struct {
	Op      core.Operation
	From    core.Handle
	To      []core.Handle
	ReplyTo string
	Args    []string
	Context json.RawMessage
	// Check applies the gateway before sending: an agent's own send. The
	// adapter's notices in an agent's name are judged on arrival only.
	Check bool
}

// post sends o through amq, recording its provenance first under a token
// the message carries, so the arrival gate can judge it before amq returns
// its id.
func (e Env) post(ctx context.Context, o Outgoing) (amq.Sent, []byte, error) {
	from, err := e.endpointOf(o.From)
	if err != nil {
		return amq.Sent{}, nil, err
	}
	to := o.To
	verb, args := "send", []string{"--me", string(o.From)}
	if o.ReplyTo != "" {
		verb = "reply"
		args = append(args, "--id", o.ReplyTo)
		orig, ok, err := e.findMessage(o.From, o.ReplyTo)
		if err != nil {
			return amq.Sent{}, nil, err
		}
		if !ok {
			return amq.Sent{}, nil, fmt.Errorf("no message %s in the mailbox of %s", o.ReplyTo, o.From)
		}
		if o.Check {
			if err := e.exposed(o.From, orig); err != nil {
				return amq.Sent{}, nil, err
			}
		}
		to = []core.Handle{core.Handle(orig.header.From)}
		if orig.box == amq.BoxSent {
			to = handles(orig.header.To)
		}
	} else {
		args = append(args, "--to", joinHandles(to))
	}
	links, err := e.Store.Links()
	if err != nil {
		return amq.Sent{}, nil, err
	}
	token, err := newToken()
	if err != nil {
		return amq.Sent{}, nil, err
	}
	p := store.Provenance{Token: token, Op: o.Op.Kind, ControlRequestID: o.Op.ControlRequestID, System: o.Op.System, From: from, At: e.Now().UTC()}
	for _, h := range to {
		ep, err := e.endpointOf(h)
		if err != nil {
			return amq.Sent{}, nil, err
		}
		if o.Check {
			if d := core.CanExchange(from, ep, o.Op, links); !d.Allow {
				return amq.Sent{}, nil, refused(fmt.Sprintf("%s to %s", o.From, h), d)
			}
		}
		p.To = append(p.To, ep)
	}
	ctxJSON, err := withToken(o.Context, p.Token)
	if err != nil {
		return amq.Sent{}, nil, err
	}
	if err := e.Store.PendProvenance(p); err != nil {
		return amq.Sent{}, nil, err
	}
	sent, out, err := amq.Post(ctx, e.AmqBin, e.Paths.Root(), verb, append(append(args, "--context", string(ctxJSON)), o.Args...)...)
	if err != nil && sent.ID == "" {
		// amq may have failed after delivering (it delivers before it
		// writes the outbox copy): the copy carries the token if it exists.
		sent.ID = e.sentWithToken(o.From, p.Token)
	}
	if sent.ID == "" {
		// Delivery cannot be ruled out: the pending record stays, and
		// vouches for a delivered copy by the token it carries.
		return sent, out, err
	}
	p.ID = sent.ID
	if cerr := e.Store.CommitProvenance(p); cerr != nil {
		// Delivered: the pending record still vouches for it.
		fmt.Fprintf(Diagnostics, "record the provenance of %s: %v\n", sent.ID, cerr)
	}
	return sent, out, err
}

// sentWithToken finds the message in h's outbox that carries token, ""
// when none does.
func (e Env) sentWithToken(h core.Handle, token string) string {
	ids, err := amq.IDs(e.Paths.Root(), string(h), amq.BoxSent)
	if err != nil {
		return ""
	}
	for i := len(ids) - 1; i >= 0; i-- {
		hdr, err := amq.ReadHeader(e.Paths.Root(), string(h), amq.BoxSent, ids[i])
		var ac adapterContext
		if err == nil && hdr.ContextOf(store.ProvenanceContextKey, &ac) && ac.Token == token {
			return hdr.ID
		}
	}
	return ""
}

// sendAs is the adapter's own send in from's name: a notice (system) or an
// agent's summon (not system).
func (e Env) sendAs(ctx context.Context, from, to core.Handle, system bool, args ...string) error {
	_, _, err := e.post(ctx, Outgoing{Op: core.Operation{Kind: core.OpSend, System: system}, From: from, To: []core.Handle{to}, Args: args})
	return err
}

// mailAs is an agent's own message sent through the adapter (summon): the
// gateway applies.
func (e Env) mailAs(ctx context.Context, from, to core.Handle, kind, subject, body string) error {
	_, _, err := e.post(ctx, Outgoing{Op: core.Operation{Kind: core.OpSend}, From: from, To: []core.Handle{to}, Check: true,
		Args: []string{"--kind", kind, "--priority", "normal", "--subject", subject, "--body", body}})
	return err
}

// withToken adds the adapter's object, carrying token, to a sender's
// context object.
func withToken(own json.RawMessage, token string) ([]byte, error) {
	obj := map[string]json.RawMessage{}
	if len(own) > 0 {
		// null decodes into a nil map without an error: refused too.
		if err := json.Unmarshal(own, &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("--context must be a JSON object, not %.40q", own)
		}
	}
	ac, err := json.Marshal(adapterContext{Token: token})
	if err != nil {
		return nil, err
	}
	obj[store.ProvenanceContextKey] = ac
	return json.Marshal(obj)
}

// located is a message in an agent's mailbox.
type located struct {
	id     string
	box    amq.Box
	header amq.Header
}

// findMessage finds message id in h's mailbox: received, dead-lettered, or
// sent.
func (e Env) findMessage(h core.Handle, id string) (located, bool, error) {
	for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained, amq.BoxDLQNew, amq.BoxDLQCur, amq.BoxSent} {
		hdr, err := amq.ReadHeader(e.Paths.Root(), string(h), box, id)
		if err == nil {
			return located{id: id, box: box, header: hdr}, true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return located{}, false, err
		}
	}
	return located{}, false, nil
}

// exposed refuses reader a message in its mailbox that the gateway would not
// show it now: one received that it would not deliver to it now (from
// another or an earlier session, or one it may not receive at all), or one
// it sent from a placement it has since left.
func (e Env) exposed(reader core.Handle, m located) error {
	rec, ok, err := e.Store.RecordOf(reader)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s has no record", ErrRefused, reader)
	}
	if m.box == amq.BoxSent {
		if !e.sentHere(rec, m.header) {
			return refused(fmt.Sprintf("%s reading %s, sent before it moved", reader, m.id), core.Decision{Reason: core.ReasonFenced})
		}
		return nil
	}
	links, err := e.Store.Links()
	if err != nil {
		return err
	}
	d, _, err := e.judge(rec, m.header, links)
	if err != nil {
		return err
	}
	if !d.Allow {
		return refused(fmt.Sprintf("%s reading %s", reader, m.id), d)
	}
	return nil
}

func handles(ss []string) []core.Handle {
	out := make([]core.Handle, len(ss))
	for i, s := range ss {
		out[i] = core.Handle(s)
	}
	return out
}

func joinHandles(hs []core.Handle) string {
	s := ""
	for i, h := range hs {
		if i > 0 {
			s += ","
		}
		s += string(h)
	}
	return s
}

// Link grants the Personal agent in loc's session and the Narrator of Epic
// session epic (on the same server) the right to exchange mail; unlink
// (link false) revokes it. Either takes effect on the next decision, for
// running wakers and hooks too. changed is false when nothing changed.
func (e Env) Link(ctx context.Context, loc Location, epic string, link bool) (l core.Link, changed bool, err error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return l, false, err
	}
	if !ok || !rec.Live() {
		return l, false, errors.New("no AMQ identity for this pane")
	}
	if rec.Role != core.PersonalRole {
		return l, false, fmt.Errorf("%w: only a %s agent links Epics, and %s is %q", ErrRefused, core.PersonalRole, rec.Handle, rec.Role)
	}
	if epic == "" || epic == rec.SessionID {
		return l, false, fmt.Errorf("%w: name an Epic session other than your own", ErrUsage)
	}
	l = core.Link{Personal: rec.Endpoint().Session(), Epic: core.SessionKey{Server: rec.Server, SessionID: epic}}
	if !link {
		changed, err = e.Store.DeleteLink(l)
		return l, changed, err
	}
	if _, err := e.Rex.InspectSession(ctx, epic); err != nil {
		return l, false, fmt.Errorf("Epic session %s: %w", epic, err)
	}
	return l, true, e.Store.PutLink(l)
}

// LinksOf lists the links of the Personal agent in loc.
func (e Env) LinksOf(loc Location) ([]core.Link, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil || !ok {
		return nil, err
	}
	all, err := e.Store.Links()
	var out []core.Link
	for _, l := range all {
		if l.Personal == rec.Endpoint().Session() {
			out = append(out, l)
		}
	}
	return out, err
}

// sentHere reports whether rec sent h from where it is now: its provenance
// names rec's current placement, or, without provenance, it was created
// after rec's last move.
func (e Env) sentHere(rec store.Record, h amq.Header) bool {
	var ac adapterContext
	h.ContextOf(store.ProvenanceContextKey, &ac)
	p, ok, err := e.Store.ProvenanceOf(h.ID, ac.Token, rec.Handle, h.To)
	if err != nil {
		return false
	}
	if ok {
		now := rec.Endpoint()
		return p.From.Server == now.Server && p.From.SessionID == now.SessionID && p.From.Generation == now.Generation
	}
	return rec.Generation == 0 || !h.Created.Before(rec.PlacedAt)
}
