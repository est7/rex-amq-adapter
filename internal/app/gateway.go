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
	"github.com/est7/rex-amq-adapter/internal/fault"
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
	r, ok, err := e.agentOf(h)
	if err != nil || !ok {
		return core.Endpoint{Handle: h}, err
	}
	return e.gatewayEndpoint(r)
}

// gatewayEndpoint is rec as the gateway sees it now: its endpoint, marked
// Lead when rec leads its Epic (sessionLead), which is whom a narrator's
// control mail may reach.
//
// A personal agent with a valid lease in a Hub session is a member of that
// Hub (Endpoint.Hub), from the session's recorded policy alone: never from
// an environment variable or a mail header.
func (e Env) gatewayEndpoint(rec agent) (core.Endpoint, error) {
	ep := rec.Endpoint()
	if rec.Role == core.PersonalRole {
		if rec.Live() && placed(rec) {
			hub, err := e.hubOfSession(rec.SessionID)
			ep.Hub = hub
			return ep, err
		}
		return ep, nil
	}
	lead, err := e.sessionLead(rec)
	ep.Lead = lead
	return ep, err
}

// judge decides whether reader may see a message it received, by what the
// adapter recorded at sending. A message the adapter has no record of (sent
// with amq directly) is judged by its sender's record now, as an unverified
// send: its header's labels and sender name prove nothing, so it never
// reaches a personal agent or a narrator.
func (e Env) judge(reader core.Endpoint, h amq.Header, links []core.Link) (core.Decision, core.Endpoint, error) {
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
		op = core.Operation{Kind: core.OpSend, Unverified: true}
	} else if op.Addressed == nil {
		// The adapter sent it to others: it is in this inbox by no send of
		// the adapter's.
		op.Addressed = &core.Endpoint{}
	}
	return core.CanExchange(from, reader, op, links), from, nil
}

// admit is the arrival gate: it moves the unread mail rec may not receive
// into the quarantine, unread and without a receipt, and returns the ids
// left. For an agent in a session it fails closed: a file whose header
// cannot be read (corrupt, unclosed, of another schema) names no sender to
// judge, and is quarantined too. amq renames whole files into place, so none
// is read mid-write. The caller holds no lock the lifecycle lock is taken
// under (a handle's, an injector's, or a consumer's may be held; the ledger's
// may not: `agent join` takes it under the lifecycle lock).
func (e Env) admit(ctx context.Context, rec agent) ([]string, error) {
	root, h := e.Paths.Root(), string(rec.Handle)
	ids, err := amq.Unread(root, h)
	if err != nil || len(ids) == 0 {
		return ids, err
	}
	links, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	reader, err := e.gatewayEndpoint(rec)
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
			if d, from, err = e.judge(reader, hdr, links); err != nil {
				return nil, err
			}
		}
		if d.Allow {
			kept = append(kept, id)
			continue
		}
		quarantined, err := e.quarantineFenced(ctx, rec, id, hdr, from, d)
		if err != nil {
			return nil, err
		}
		if !quarantined {
			kept = append(kept, id)
		}
	}
	return kept, nil
}

// errReaderMoved is an arrival gate whose reader was moved, or replaced,
// while it judged: what is left unread is judged again by the next gate.
var errReaderMoved = errors.New("the reader's placement changed during the arrival gate")

// quarantineFenced moves message id out of rec's inbox for denial d. Its
// reason is written first with no lock (the fsync is the slow part); the
// move itself, which cannot be undone, happens under the lifecycle lock that
// every placement writer holds, only while rec is still placed as it was
// read, and after a readable message is judged again there by the endpoints
// and links as they are then: one they allow is kept, its reason dropped.
func (e Env) quarantineFenced(ctx context.Context, rec agent, id string, hdr amq.Header, from core.Endpoint, d core.Decision) (bool, error) {
	root, h := e.Paths.Root(), string(rec.Handle)
	reader, err := e.gatewayEndpoint(rec)
	if err != nil {
		return false, err
	}
	q := store.Quarantined{ID: id, Handle: rec.Handle, From: from, To: reader, Decision: d, At: e.Now().UTC()}
	if err := e.Store.PrepareQuarantine(q); err != nil {
		return false, fmt.Errorf("quarantine %s for %s: %w", id, h, err)
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	now, ok, err := e.agentOf(rec.Handle)
	if err != nil {
		return false, err
	}
	if !ok || !placedAsRead(now, rec) {
		_ = e.Store.DropQuarantineReason(rec.Handle, id)
		return false, errReaderMoved
	}
	if d.Reason != core.ReasonUnreadableHeader {
		links, err := e.Store.Links()
		if err != nil {
			return false, err
		}
		again, now, err := e.judge(reader, hdr, links)
		if err != nil {
			return false, err
		}
		if again.Allow {
			return false, e.Store.DropQuarantineReason(rec.Handle, id)
		}
		if again != d || now != from {
			// What decided changed meanwhile: the reason says what did.
			q.Decision, q.From = again, now
			if err := e.Store.PrepareQuarantine(q); err != nil {
				return false, err
			}
		}
	}
	if _, err := e.Store.MoveToQuarantine(filepath.Join(amq.Inbox(root, h), id+".md"), q); err != nil {
		return false, fmt.Errorf("quarantine %s for %s: %w", id, h, err)
	}
	fmt.Fprintf(Diagnostics, "quarantined %s for %s from %s: %s\n", id, h, hdr.From, q.Decision.Reason)
	return true, nil
}

// placedAsRead reports whether now, read again, is placed as rec was read:
// the same live placement (samePlacement), or for a departed record the
// same last place.
func placedAsRead(now, rec agent) bool {
	if rec.Live() {
		return samePlacement(now, rec)
	}
	return !now.Live() && now.Place == rec.Place && now.Fence == rec.Fence
}

// ErrRefused is a send or read the gateway refused.
var ErrRefused = errors.New("refused by the gateway")

func refused(what string, d core.Decision) error {
	return refusal(fault.PermissionDenied, string(d.Reason), "%s (%s)", what, d.Reason)
}

// refusal is a refusal carrying its code and details.reason from where it
// is decided, its message the same "refused by the gateway: …" text, and
// still matching ErrRefused.
func refusal(code fault.Code, reason, format string, a ...any) *fault.Error {
	return fault.New(code, "%s: %s", ErrRefused, fmt.Sprintf(format, a...)).WithCause(ErrRefused).With("reason", reason)
}

// refusedSend is refused for from's send to to, pointing a member that tried
// to mail a personal agent at the path that reaches one: an Epic lead's
// hand-back, which the Epic's narrator relays.
func refusedSend(from, to core.Endpoint, d core.Decision) error {
	err := refused(fmt.Sprintf("%s to %s", from.Handle, to.Handle), d)
	if d.Reason == core.ReasonPersonalOnly && to.Role == core.PersonalRole && from.Role != core.PersonalRole {
		return fmt.Errorf(`%w: report with "rex-amq-adapter epic handback @<file>" (an Epic lead's path; the Epic's narrator relays it to the personal agent)`, err)
	}
	return err
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
	// Strict (amq's --strict, which errors on unknown handles) also refuses
	// a recipient with no mailbox: amq keeps a retired handle in its roster
	// after its mailbox is removed (roster.go), so it would not.
	Strict bool
}

// post sends o through amq, recording its provenance first under a token
// the message carries, so the arrival gate can judge it before amq returns
// its id.
// checkedRecipients are to as the gateway sees them, each checked for o's sender
// (from) when o.Check: the first one it refuses is the error.
func (e Env) checkedRecipients(from core.Endpoint, to []core.Handle, o Outgoing, links []core.Link) ([]core.Endpoint, error) {
	var eps []core.Endpoint
	for _, h := range to {
		ep, err := e.endpointOf(h)
		if err != nil {
			return nil, err
		}
		if o.Check {
			if d := core.CanExchange(from, ep, o.Op, links); !d.Allow {
				return nil, refusedSend(from, ep, d)
			}
		}
		if o.Strict && !mailboxExists(e.Paths.Root(), h) {
			return nil, fault.New(fault.NotFound, "%s has no mailbox (retired, or never registered); --strict refuses it", h).With("kind", "mailbox")
		}
		eps = append(eps, ep)
	}
	return eps, nil
}

// admits is post's gateway check of send o, run before anything is sent.
func (e Env) admits(o Outgoing) error {
	from, err := e.endpointOf(o.From)
	if err != nil {
		return err
	}
	links, err := e.Store.Links()
	if err != nil {
		return err
	}
	_, err = e.checkedRecipients(from, o.To, o, links)
	return err
}

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
			return amq.Sent{}, nil, fault.New(fault.NotFound, "no message %s in the mailbox of %s", o.ReplyTo, o.From).With("kind", "message")
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
	p := store.Provenance{Token: token, Op: o.Op.Kind, ControlRequestID: o.Op.ControlRequestID, System: o.Op.System, Hub: o.Op.Hub, From: from, At: e.Now().UTC()}
	if p.To, err = e.checkedRecipients(from, to, o, links); err != nil {
		return amq.Sent{}, nil, err
	}
	ctxJSON, err := withToken(o.Context, p.Token)
	if err != nil {
		return amq.Sent{}, nil, err
	}
	if err := e.Store.PendProvenance(p); err != nil {
		return amq.Sent{}, nil, err
	}
	sent, out, err := e.AMQ.Post(ctx, e.Paths.Root(), verb, append(append(args, "--context", string(ctxJSON)), o.Args...)...)
	if err != nil && sent.ID == "" {
		// amq may have failed after delivering (it delivers before it
		// writes the outbox copy): the copy carries the token if it exists.
		if hdr, ok := e.sentHeaderWithToken(o.From, p.Token); ok {
			sent = amq.Sent{ID: hdr.ID, Thread: hdr.Thread, To: hdr.To}
		}
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

// sentHeaderWithToken finds the message header in h's outbox that carries token.
func (e Env) sentHeaderWithToken(h core.Handle, token string) (amq.Header, bool) {
	ids, err := amq.IDs(e.Paths.Root(), string(h), amq.BoxSent)
	if err != nil {
		return amq.Header{}, false
	}
	for i := len(ids) - 1; i >= 0; i-- {
		hdr, err := amq.ReadHeader(e.Paths.Root(), string(h), amq.BoxSent, ids[i])
		var ac adapterContext
		if err == nil && hdr.ContextOf(store.ProvenanceContextKey, &ac) && ac.Token == token {
			return hdr, true
		}
	}
	return amq.Header{}, false
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
	rec, ok, err := e.agentOf(reader)
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
	ep, err := e.gatewayEndpoint(rec)
	if err != nil {
		return err
	}
	d, _, err := e.judge(ep, m.header, links)
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
	return e.link(ctx, loc, epic, link, false)
}

// PreviewLink is Link's every check with nothing written: changed says
// whether Link would change the record.
func (e Env) PreviewLink(ctx context.Context, loc Location, epic string, link bool) (core.Link, bool, error) {
	return e.link(ctx, loc, epic, link, true)
}

// A personal agent of a Hub links the Epic to its Hub, so every personal
// agent of the Hub shares it (core.LinkMatchesPersonal), naming itself the
// narrator's preferred recipient; one of no Hub links its own session.
// Linking an Epic its Hub already has changes nothing. Unlinking removes
// every link that grants the caller the Epic: its Hub's and its session's.
func (e Env) link(ctx context.Context, loc Location, epic string, link, dryRun bool) (l core.Link, changed bool, err error) {
	rec, ok, err := e.agentIn(loc)
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
	me, err := e.gatewayEndpoint(rec)
	if err != nil {
		return l, false, err
	}
	l = core.Link{Hub: me.Hub, Personal: me.Session(), PreferredPersonal: rec.Handle, Epic: core.SessionKey{Server: rec.Server, SessionID: epic}, CreatedBy: rec.Handle, CreatedAt: e.Now().UTC()}
	held, err := e.linksFor(me, l.Epic)
	if err != nil {
		return l, false, err
	}
	if !link {
		if dryRun || len(held) == 0 {
			return l, len(held) > 0, nil
		}
		for _, h := range held {
			if _, err := e.Store.DeleteLink(h); err != nil {
				return l, true, err
			}
		}
		return held[0], true, nil
	}
	if len(held) > 0 {
		return held[0], false, nil
	}
	if _, err := e.Rex.InspectSession(ctx, epic); err != nil {
		return l, false, fmt.Errorf("Epic session %s: %w", epic, err)
	}
	if dryRun {
		return l, true, nil
	}
	return l, true, e.Store.PutLink(l)
}

// linksFor is every recorded link that grants personal agent p the Epic
// epic (core.LinkMatchesPersonal).
func (e Env) linksFor(p core.Endpoint, epic core.SessionKey) ([]core.Link, error) {
	all, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var out []core.Link
	for _, l := range all {
		if l.Epic == epic && core.LinkMatchesPersonal(l, p) {
			out = append(out, l)
		}
	}
	return out, nil
}

// linkedEpics is every Epic linked to personal agent p, once each, with the
// link that grants it (a Hub's before a session's).
func (e Env) linkedEpics(p core.Endpoint) ([]core.Link, error) {
	all, err := e.Store.Links()
	if err != nil {
		return nil, err
	}
	var out []core.Link
	seen := map[core.SessionKey]int{}
	for _, l := range all {
		if !core.LinkMatchesPersonal(l, p) {
			continue
		}
		if i, ok := seen[l.Epic]; ok {
			if out[i].Hub == "" && l.Hub != "" {
				out[i] = l
			}
			continue
		}
		seen[l.Epic] = len(out)
		out = append(out, l)
	}
	return out, nil
}

// personalLinked reports whether personal agent rec may reach the Epic
// session (core.LinkMatchesPersonal), on any server.
func (e Env) personalLinked(rec agent, session string) (bool, error) {
	me, err := e.gatewayEndpoint(rec)
	if err != nil {
		return false, err
	}
	links, err := e.linkedEpics(me)
	if err != nil {
		return false, err
	}
	for _, l := range links {
		if l.Epic.SessionID == session {
			return true, nil
		}
	}
	return false, nil
}

// LinksOf lists the links of the Personal agent in loc.
func (e Env) LinksOf(loc Location) ([]core.Link, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil || !ok {
		return nil, err
	}
	me, err := e.gatewayEndpoint(rec)
	if err != nil {
		return nil, err
	}
	return e.linkedEpics(me)
}

// sentHere reports whether rec sent h from where it is now: its provenance
// names rec's current placement, or, without provenance, it was created
// after rec's last move.
func (e Env) sentHere(rec agent, h amq.Header) bool {
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
	return rec.Fence.Generation == 0 || !h.Created.Before(rec.Fence.PlacedAt)
}
