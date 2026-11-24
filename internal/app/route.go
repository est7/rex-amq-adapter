package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A Hub's personal agents share its Epics (C26), so a narrator's mail goes
// to whichever of them is with the user, not to the one tab that opened the
// Epic: resolved when it is sent (routeNarratorMail), and delivered again
// when the personal agent it waits for departs or is collected
// (redeliverFrom), or, with none live then, when the next one of the Hub is
// adopted (redeliverPending). Delivery is at least once, deduplicated by the
// original message's id; historical provenance is never rewritten.

// PersonalAddress is the role address a narrator mails its Hub's personal
// agent by: `mail send --to personal`.
const PersonalAddress = core.PersonalRole

// maxRedeliverBody bounds the body read back for a redelivery: a message
// body is at most cli.BodyMax, well under it.
const maxRedeliverBody = 4 << 20

// TouchActivity records that the user spoke to personal agent rec now (its
// user-prompt-submit hook): what makes it the Hub's most recent. A failure
// only leaves the recency older, so it is reported, never returned.
func (e Env) touchActivity(rec agent) {
	if rec.Role != core.PersonalRole {
		return
	}
	if err := e.Store.TouchActivity(rec.Handle, e.Now()); err != nil {
		fmt.Fprintf(Diagnostics, "record the activity of %s: %v\n", rec.Handle, err)
	}
}

// recency is when personal agent r was last known to be with the user: its
// last prompt, or when its lease was taken, whichever is later.
func (e Env) recency(r agent) time.Time {
	at := r.adoptedAt()
	if a, ok, err := e.Store.ActivityAt(r.Handle); err == nil && ok && a.After(at) {
		at = a
	}
	return at
}

// reachable reports whether personal agent r runs (a valid lease) where
// Rex still shows its block.
func (e Env) reachable(ctx context.Context, r agent) bool {
	if !r.Live() || r.Role != core.PersonalRole || !placed(r) {
		return false
	}
	on := e.Rex
	if e.RexOn != nil && r.Server != "" {
		on = e.RexOn(r.Server)
	}
	_, err := on.InspectBlock(ctx, r.BlockID)
	return err == nil
}

// mostRecent is the candidate of the latest recency, ties going to the
// lexicographically first handle; ok is false with no candidate.
func (e Env) mostRecent(cands []agent) (best agent, ok bool) {
	var bestAt time.Time
	for _, r := range cands {
		at := e.recency(r)
		if !ok || at.After(bestAt) || at.Equal(bestAt) && r.Handle < best.Handle {
			best, bestAt, ok = r, at, true
		}
	}
	return best, ok
}

// mostRecentPersonal is the Hub's personal agent the user spoke to last:
// among the live, reachable personal agents member accepts (their gateway
// endpoints, so membership is the app's, from session policy), the one of
// the latest recency.
func (e Env) mostRecentPersonal(ctx context.Context, member func(core.Endpoint) bool) (agent, bool, error) {
	recs, err := e.agents()
	if err != nil {
		return agent{}, false, err
	}
	var cands []agent
	for _, r := range recs {
		if r.Role != core.PersonalRole || !r.Live() || !placed(r) {
			continue
		}
		ep, err := e.gatewayEndpoint(r)
		if err != nil {
			return agent{}, false, err
		}
		if member(ep) && e.reachable(ctx, r) {
			cands = append(cands, r)
		}
	}
	best, ok := e.mostRecent(cands)
	return best, ok, nil
}

// hubMember is membership in the Hub l grants (or its session, for a link
// without a Hub).
func hubMember(l core.Link) func(core.Endpoint) bool {
	return func(ep core.Endpoint) bool { return core.LinkMatchesPersonal(l, ep) }
}

// narratorLink is the link that grants the Epic of narrator n to a Hub: a
// Hub's before a session-keyed one; ok is false when it is linked to none.
func (e Env) narratorLink(n agent) (core.Link, bool, error) {
	all, err := e.Store.Links()
	if err != nil {
		return core.Link{}, false, err
	}
	var found core.Link
	ok := false
	for _, l := range all {
		if l.Epic == n.Endpoint().Session() && (!ok || found.Hub == "" && l.Hub != "") {
			found, ok = l, true
		}
	}
	return found, ok, nil
}

// resolvePersonal is where mail for the Hub l grants goes, preferred first:
// preferred while it is live, reachable, and of the Hub; else the Hub's
// most recent personal agent; else preferred, whose mailbox keeps the mail
// until it is back.
func (e Env) resolvePersonal(ctx context.Context, l core.Link, preferred core.Handle) (core.Handle, error) {
	if to, ok, err := e.livePersonal(ctx, l, preferred); err != nil || ok {
		return to, err
	}
	if preferred == "" {
		return "", fault.New(fault.NotFound, "no personal agent of this Epic's Hub, live or recorded, to mail").With("kind", "agent")
	}
	return preferred, nil
}

// livePersonal is where mail for the Hub l grants goes now: preferred
// while it is live, reachable, and of the Hub, else the Hub's most recent
// personal agent; ok is false with none of them live.
func (e Env) livePersonal(ctx context.Context, l core.Link, preferred core.Handle) (core.Handle, bool, error) {
	member := hubMember(l)
	if preferred != "" {
		r, ok, err := e.agentOf(preferred)
		if err != nil {
			return "", false, err
		}
		if ok && e.reachable(ctx, r) {
			ep, err := e.gatewayEndpoint(r)
			if err != nil {
				return "", false, err
			}
			if member(ep) {
				return preferred, true, nil
			}
		}
	}
	r, ok, err := e.mostRecentPersonal(ctx, member)
	return r.Handle, ok, err
}

// preferredOf is the personal agent the Epic of l prefers: the link's, else
// the one its lifecycle records as having opened it.
func (e Env) preferredOf(l core.Link) core.Handle {
	if l.PreferredPersonal != "" {
		return l.PreferredPersonal
	}
	if lc, ok, err := e.Store.EpicLifecycle(l.Epic.SessionID); err == nil && ok {
		return lc.PersonalHandle
	}
	return ""
}

// MailRoute is where a narrator's mail to its Hub's personal agent went:
// RoutedTo the personal agent it reached, ReroutedFrom the one it was
// addressed to (the preferred one, the departed one it named, or the one
// its reply answered) when that is not where it went.
type MailRoute struct {
	RoutedTo     core.Handle `json:"routed_to"`
	ReroutedFrom core.Handle `json:"rerouted_from,omitempty"`
	// hub is the Hub whose inbox keeps the message: that of a Hub link,
	// "" for a session-keyed one, which amq alone carries.
	hub core.HubKey
	// InboxSeq is the message's entry in that inbox, once kept.
	InboxSeq int64 `json:"inbox_seq,omitempty"`
	// NoHubDedupe is true when a narrator passed --op-id on a session-keyed
	// link without a Hub: delivered via AMQ only without Hub deduplication.
	NoHubDedupe bool `json:"no_hub_dedupe,omitempty"`
}

// HubAddress is routed_to for a narrator's message no personal agent of
// its Hub, live or recorded, could take: the Hub's inbox alone keeps it.
const HubAddress core.Handle = "hub"

// routeNarratorMail resolves, for narrator n's outgoing message o, the mail
// it addresses to its Hub's personal agent: the role address
// PersonalAddress, a personal agent's handle that is no longer live and
// reachable (an older narrator's literal handle), or a reply to such a
// one's message, which crosses mailboxes and so becomes a send keeping the
// reply's thread, subject, and kind, with the original among its refs.
// given reports whether a flag was given. A route is nil when nothing was
// resolved. Mail to a Hub link's personal agent names the Hub's inbox,
// which keeps it (inboxNarratorMail); with no personal agent of the Hub,
// live or recorded, the inbox alone does: that recipient leaves o.To and
// the route is HubAddress.
func (e Env) routeNarratorMail(ctx context.Context, n agent, o *Outgoing, given func(string) bool) (*MailRoute, error) {
	if o.ReplyTo != "" {
		return e.routeNarratorReply(ctx, n, o, given)
	}
	var route *MailRoute
	kept := o.To[:0:0]
	defer func() { o.To = kept }()
	for i, h := range o.To {
		kept = append(kept, h)
		addressed := h
		if h != PersonalAddress {
			r, ok, err := e.agentOf(h)
			if err != nil {
				return nil, err
			}
			if !ok || r.Role != core.PersonalRole || e.reachable(ctx, r) {
				continue
			}
		}
		l, ok, err := e.narratorLink(n)
		if err != nil {
			return nil, err
		}
		if !ok {
			if h == PersonalAddress {
				return nil, refused(fmt.Sprintf("%s to %s", n.Handle, PersonalAddress), core.Decision{Reason: core.ReasonNotLinked})
			}
			continue
		}
		if h == PersonalAddress {
			addressed = e.preferredOf(l)
		}
		var to core.Handle
		if l.Hub != "" {
			// A Hub's inbox keeps what no live personal agent of the Hub
			// could take; nothing waits in a departed one's mailbox.
			live, ok, err := e.livePersonal(ctx, l, addressed)
			if err != nil {
				return nil, err
			}
			if !ok {
				kept = kept[:len(kept)-1]
				route = &MailRoute{RoutedTo: HubAddress, hub: l.Hub}
				if addressed != "" {
					route.ReroutedFrom = addressed
				}
				continue
			}
			to = live
		} else if to, err = e.resolvePersonal(ctx, l, addressed); err != nil {
			return nil, err
		}
		o.To[i], kept[len(kept)-1] = to, to
		route = &MailRoute{RoutedTo: to, hub: l.Hub}
		if addressed != "" && addressed != to {
			route.ReroutedFrom = addressed
		}
	}
	return route, nil
}

// routeNarratorReply turns narrator n's reply to a personal agent no longer
// live and reachable into a send to the Hub's personal agent, when the
// resolution lands elsewhere; otherwise the reply stands and waits.
func (e Env) routeNarratorReply(ctx context.Context, n agent, o *Outgoing, given func(string) bool) (*MailRoute, error) {
	orig, ok, err := e.findMessage(n.Handle, o.ReplyTo)
	if err != nil || !ok || orig.box == amq.BoxSent {
		return nil, err
	}
	from := core.Handle(orig.header.From)
	r, ok, err := e.agentOf(from)
	if err != nil || !ok || r.Role != core.PersonalRole || e.reachable(ctx, r) {
		return nil, err
	}
	l, ok, err := e.narratorLink(n)
	if err != nil || !ok {
		return nil, err
	}
	to, err := e.resolvePersonal(ctx, l, from)
	if err != nil || to == from {
		return nil, err
	}
	// The narrator may still answer it: the gateway's read check first.
	if err := e.exposed(n.Handle, orig); err != nil {
		return nil, err
	}
	o.ReplyTo, o.To, o.Op.Kind = "", []core.Handle{to}, core.OpSend
	if orig.header.Thread != "" && !given("thread") {
		o.Args = append(o.Args, "--thread="+orig.header.Thread)
	}
	if !given("subject") {
		o.Args = append(o.Args, "--subject="+replySubject(orig.header.Subject))
	}
	if !given("kind") && orig.header.Kind != "" {
		o.Args = append(o.Args, "--kind="+orig.header.Kind)
	}
	o.Args = append(o.Args, "--refs="+orig.id)
	return &MailRoute{RoutedTo: to, ReroutedFrom: from}, nil
}

// replySubject is amq's subject for a reply to subject.
func replySubject(subject string) string {
	if strings.HasPrefix(subject, "Re: ") {
		return subject
	}
	return "Re: " + subject
}

// withRoute adds route's routed_to and rerouted_from to amq's JSON output.
func withRoute(out []byte, route *MailRoute) []byte {
	if route == nil {
		return out
	}
	obj := map[string]any{}
	if err := json.Unmarshal(out, &obj); err != nil {
		return out
	}
	obj["routed_to"] = route.RoutedTo
	if route.ReroutedFrom != "" {
		obj["rerouted_from"] = route.ReroutedFrom
	}
	if route.InboxSeq != 0 {
		obj["inbox_seq"] = route.InboxSeq
	}
	if route.NoHubDedupe {
		obj["no_hub_dedupe"] = true
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return out
	}
	return append(b, '\n')
}

// reroutedContext is the context a redelivered message carries.
type reroutedContext struct {
	ReroutedFrom string `json:"rerouted_from"`
}

// redeliverFrom delivers again, to the Hub's most recent personal agent,
// each unread message in left's mailbox (a personal agent that departed or
// was collected) that a narrator of an Epic linked to its Hub sent: a new
// message in the narrator's name, on the same thread and subject, whose
// context and refs name the original (rerouted_from). One delivered again
// already is skipped. With no live personal agent of the Hub the mail
// stays in left's mailbox, pending, for redeliverPending. Each message is
// delivered again under its own lock (LockRedelivery). An error leaves the rest for the next try (the departure
// stays unfinished, so collection retries it).
func (e Env) redeliverFrom(ctx context.Context, left agent) error {
	if left.Role != core.PersonalRole || left.Live() {
		return nil
	}
	root, h := e.Paths.Root(), string(left.Handle)
	ids, err := amq.Unread(root, h)
	if err != nil || len(ids) == 0 {
		return err
	}
	ep := left.Endpoint()
	if ep.Hub, err = e.hubOfSession(left.SessionID); err != nil {
		return err
	}
	links, err := e.linkedEpics(ep)
	if err != nil || len(links) == 0 {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := e.redeliverOne(ctx, left, id, links); err != nil {
			errs = append(errs, fmt.Errorf("redeliver %s from %s: %w", id, left.Handle, err))
		}
	}
	return errors.Join(errs...)
}

func (e Env) redeliverOne(ctx context.Context, left agent, id string, links []core.Link) error {
	// A departure's finisher and a personal agent's adoption may both try
	// it, with no lifecycle lock held.
	unlock, err := e.Store.LockRedelivery(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	root, h := e.Paths.Root(), string(left.Handle)
	if _, done, err := e.Store.Redelivered(id); err != nil || done {
		return err
	}
	hdr, err := amq.ReadHeader(root, h, amq.BoxUnread, id)
	if err != nil {
		return nil // drained, quarantined, or unreadable: nothing to deliver again
	}
	var ac adapterContext
	hdr.ContextOf(store.ProvenanceContextKey, &ac)
	p, ok, err := e.Store.ProvenanceOf(hdr.ID, ac.Token, core.Handle(hdr.From), hdr.To)
	if err != nil || !ok || p.From.Role != core.NarratorRole {
		return err
	}
	var link *core.Link
	for i := range links {
		if links[i].Epic == p.From.Session() {
			link = &links[i]
		}
	}
	if link == nil {
		return nil
	}
	target, ok, err := e.mostRecentPersonal(ctx, hubMember(*link))
	if err != nil || !ok || target.Handle == left.Handle {
		return err
	}
	body, err := amq.BodyPrefix(root, h, amq.BoxUnread, id, maxRedeliverBody)
	if err != nil {
		return nil
	}
	body = strings.TrimSuffix(body, "\n")
	args := []string{"--subject", hdr.Subject, "--refs", id}
	if hdr.Thread != "" {
		args = append(args, "--thread", hdr.Thread)
	}
	if hdr.Kind != "" {
		args = append(args, "--kind", hdr.Kind)
	}
	if hdr.Priority != "" {
		args = append(args, "--priority", hdr.Priority)
	}
	rc, err := json.Marshal(reroutedContext{ReroutedFrom: id})
	if err != nil {
		return err
	}
	sent, _, err := e.post(ctx, Outgoing{Op: core.Operation{Kind: core.OpSend}, From: p.From.Handle, To: []core.Handle{target.Handle}, Context: rc, Args: append(args, "--body", body)})
	if sent.ID == "" {
		if err == nil {
			err = errors.New("amq returned no message id")
		}
		return err
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "redeliver %s: delivered %s, then: %v\n", id, sent.ID, err)
	}
	return e.Store.PutRedelivery(store.Redelivery{OrigID: id, From: p.From.Handle, Left: left.Handle, NewID: sent.ID, To: target.Handle, At: e.Now().UTC()})
}

// redeliverPending delivers again the narrators' mail that departed
// personal agents of rec's Hub (or session) left unread while no personal
// agent of it was live: rec, just adopted there, is one now. A failure is
// reported, and the mail stays pending.
func (e Env) redeliverPending(ctx context.Context, rec agent) {
	me, err := e.gatewayEndpoint(rec)
	if err != nil {
		fmt.Fprintf(Diagnostics, "redeliver to %s: %v\n", rec.Handle, err)
		return
	}
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "redeliver to %s: %v\n", rec.Handle, err)
		return
	}
	for _, r := range recs {
		if r.Live() || r.Role != core.PersonalRole || r.Handle == rec.Handle || !placed(r) {
			continue
		}
		hub, err := e.hubOfSession(r.SessionID)
		if err != nil || !(hub != "" && hub == me.Hub || r.Endpoint().Session() == me.Session()) {
			continue
		}
		if err := e.redeliverFrom(ctx, r); err != nil {
			fmt.Fprintf(Diagnostics, "%v\n", err)
		}
	}
}
