package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A Hub's inbox (store.AppendHubInbox) keeps what its Epics told the
// Operator whether or not a personal agent was live to hear it: each
// hand-back as it is recorded (noteHandBackInHub), and each narrator's mail
// to the Hub's personal agent (inboxNarratorMail). The Operator reads it
// with `hub inbox`, from anywhere, Rex down included (ReadHubInbox).

// epicHub is the Hub the Epic session belongs to: the one its lifecycle
// records, else the Hub of a link to it; "" for an Epic of no Hub.
func (e Env) epicHub(session string) (core.HubKey, error) {
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil {
		return "", err
	}
	if ok && lc.Hub != "" {
		return lc.Hub, nil
	}
	links, err := e.linksToEpic(session)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(links, func(l core.Link) bool { return l.Hub != "" }); i >= 0 {
		return links[i].Hub, nil
	}
	return "", nil
}

// noteHandBackInHub keeps the Epic session's stored hand-back in its Hub's
// inbox, once: the report on disk, as the lifecycle lists it, whether or
// not a later one is listed (listed before it is stored, it may never be).
// A run that failed to keep it is made good by the next hand-back run,
// which keeps it before anything can replace it. The caller holds
// LockEpicLifecycle.
//
// Only a failure to write the inbox is its error: a hand-back or lifecycle
// it cannot read is the hand-back record's to report (recordHandBack reads
// both and says what stays), and leaves nothing to keep.
func (e Env) noteHandBackInHub(ctx context.Context, session string) error {
	stored, ok, err := e.Store.HandBack(session)
	if err != nil || !ok {
		return nil
	}
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok {
		return nil
	}
	h := numbered(stored)
	i := slices.IndexFunc(lc.HandBacks, func(r store.HandBackRef) bool { return r.Seq == h.Seq && r.Digest == h.Digest })
	if i < 0 {
		return nil
	}
	hub, err := e.epicHub(session)
	if err != nil || hub == "" {
		return err
	}
	ref := lc.HandBacks[i]
	if _, _, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{
		Epic: session, HubKey: hub, Kind: store.InboxHandBack, HandBackSeq: ref.Seq, Digest: ref.Digest, By: ref.By, At: ref.At,
		DedupeKey: "handback:" + session + "#" + strconv.Itoa(ref.Seq),
	}, e.Now()); err != nil {
		return fmt.Errorf("keep hand-back %d in the Hub's inbox: %w", ref.Seq, err)
	}
	return nil
}

// latestHandBackSeq is the Epic's latest hand-back's Seq, 0 for none.
func (e Env) latestHandBackSeq(session string) (int, error) {
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok {
		return 0, err
	}
	latest, _ := lc.LatestHandBack()
	return latest.Seq, nil
}

// narratorOpKey scopes a narrator's --op-id to its Epic inside a Hub's inbox.
// Length-prefixing the Epic session id makes (epic, opID) unambiguous even
// when either carries colons or hashes.
func narratorOpKey(epic, opID string) string {
	return "narrator:op:" + strconv.Itoa(len(epic)) + ":" + epic + ":" + opID
}

// narratorMailRequest is the immutable normalized request a narrator submits
// for Hub-linked delivery to personal: everything that affects the sent
// message, separate from amq-assigned Thread or dynamically selected live
// recipients.
type narratorMailRequest struct {
	To         []core.Handle `json:"to"`
	Subject    string        `json:"subject"`
	Thread     string        `json:"thread,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Priority   string        `json:"priority,omitempty"`
	Labels     []string      `json:"labels,omitempty"`
	Refs       []string      `json:"refs,omitempty"`
	Context    string        `json:"context,omitempty"`
	Body       string        `json:"body"`
	AllowEmpty bool          `json:"allow_empty,omitempty"`
}

func (r narratorMailRequest) hash() string {
	return requestHash(r)
}

// splitCSV splits a comma-separated flag value into trimmed non-empty items.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// canonicalContext validates that own (when non-empty) is a single JSON object
// and returns its key-sorted canonical JSON, or "" when empty or `{}`.
func canonicalContext(own json.RawMessage) (string, error) {
	if len(own) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(own))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return "", fmt.Errorf("%w: --context must be a JSON object", ErrUsage)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("%w: --context must be a JSON object", ErrUsage)
	}
	if len(obj) == 0 {
		return "", nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// keepNarratorMail keeps narrator n's message (req) to the personal agent of
// hub in the Hub's inbox, before any delivery: the inbox holds it whatever
// becomes of the delivery. Its Delivery token travels in the message's context
// (withDelivery), and its delivery outcome is recorded beside it once settled
// (noteNarratorDelivery), or found again by that token in the narrator's sent
// mail (hubInbox). Each send is its own entry, the same words twice are two,
// unless the sender names it (opID): under LockHubInboxOp, a retry scoped by
// Hub and Epic finds its entry, done when already settled (delivered or
// Hub-only), pending to retry with the same durable token, and the op id with
// another caller or normalized request is a conflict.
func (e Env) keepNarratorMail(ctx context.Context, n agent, hub core.HubKey, opID string, req narratorMailRequest) (x store.HubInboxEntry, done bool, err error) {
	key, reqHash := "", ""
	if opID != "" {
		key, reqHash = narratorOpKey(n.SessionID, opID), req.hash()
		all, err := e.hubInbox(hub.Dir())
		if err != nil {
			return x, false, err
		}
		if i := slices.IndexFunc(all, func(old store.HubInboxEntry) bool { return old.DedupeKey == key }); i >= 0 {
			return e.narratorRetry(all[i], n, req, reqHash, opID)
		}
	}
	seq, err := e.latestHandBackSeq(n.SessionID)
	if err != nil {
		return x, false, err
	}
	token, err := newToken()
	if err != nil {
		return x, false, err
	}
	if key == "" {
		key = "narrator:" + token
	}
	x, added, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{
		Epic: n.SessionID, HubKey: hub, Kind: store.InboxNarrator, Thread: req.Thread, Subject: req.Subject,
		HandBackSeq: seq, Body: req.Body, By: n.Handle, DedupeKey: key, RequestHash: reqHash, Delivery: token,
	}, e.Now())
	if err != nil {
		return x, false, fmt.Errorf("keep the message in the Hub's inbox (nothing was sent): %w", err)
	}
	if !added {
		// Re-read with resolved delivery state before deciding on retry.
		all, err := e.hubInbox(hub.Dir())
		if err != nil {
			return x, false, err
		}
		if i := slices.IndexFunc(all, func(old store.HubInboxEntry) bool { return old.DedupeKey == key }); i >= 0 {
			x = all[i]
		}
		return e.narratorRetry(x, n, req, reqHash, opID)
	}
	return x, false, nil
}

// narratorRetry answers a retry of the narrator message x kept under opID:
// checks caller ownership inside the (Hub, Epic) scope and compares the
// immutable normalized request, then reports whether delivery is already
// settled (delivered or Hub-only) or still pending.
func (e Env) narratorRetry(x store.HubInboxEntry, n agent, req narratorMailRequest, reqHash, opID string) (store.HubInboxEntry, bool, error) {
	sameRequest := x.RequestHash == reqHash
	if x.RequestHash == "" {
		sameRequest = x.Subject == req.Subject && x.Body == req.Body && x.Thread == req.Thread
	}
	if x.Epic != n.SessionID || x.By != n.Handle || !sameRequest {
		return x, false, refusal(fault.Conflict, "op_id_reused", "--op-id %q already names another message (Hub inbox entry %d)", opID, x.Seq)
	}
	settled := x.DeliveryState == store.DeliveryDelivered || x.DeliveryState == store.DeliveryHubOnly
	return x, settled, nil
}

// deliveryContextKey names, in a message's context, the Hub inbox entry it
// delivers (its Delivery token).
const deliveryContextKey = "rex_amq_adapter_delivery"

type deliveryContext struct {
	Token string `json:"token"`
}

// withDelivery is the context object own with the delivery token added.
func withDelivery(own json.RawMessage, token string) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if len(own) > 0 {
		if err := json.Unmarshal(own, &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("%w: --context must be a JSON object", ErrUsage)
		}
	}
	b, err := json.Marshal(deliveryContext{Token: token})
	if err != nil {
		return nil, err
	}
	obj[deliveryContextKey] = b
	return json.Marshal(obj)
}

// noteNarratorDelivery records what became of x, kept before it was sent:
// delivered as sent (DeliveryDelivered), or (attempted false) settled without
// a live personal agent (DeliveryHubOnly). A failed delivery attempt records
// nothing so x stays DeliveryPending with its durable token intact for retry
// or outbox recovery. A write failure is reported, never returned: the entry
// is already kept.
func (e Env) noteNarratorDelivery(x store.HubInboxEntry, sent amq.Sent, attempted bool, route *MailRoute) {
	if sent.ID == "" && attempted {
		return
	}
	var d store.HubInboxDelivery
	if !attempted {
		d = store.HubInboxDelivery{State: store.DeliveryHubOnly, RoutedTo: HubAddress}
		if route != nil {
			d.ReroutedFrom = route.ReroutedFrom
		}
	} else {
		routedTo := core.Handle("")
		if route != nil {
			routedTo = route.RoutedTo
		}
		if routedTo == "" && len(sent.To) > 0 {
			routedTo = core.Handle(sent.To[0])
		}
		d = store.HubInboxDelivery{State: store.DeliveryDelivered, MsgID: sent.ID, Thread: sent.Thread, RoutedTo: routedTo}
		if route != nil {
			d.ReroutedFrom = route.ReroutedFrom
		}
	}
	if err := e.Store.PutHubInboxDelivery(x.HubKey.Dir(), x.Delivery, d); err != nil {
		fmt.Fprintf(Diagnostics, "record what became of Hub inbox entry %d: %v\n", x.Seq, err)
	}
}

// hubInbox is the inbox of the Hub directory dir (store.HubInbox), with each
// pending entry kept before its delivery reconciled against its sender's sent
// mail by its durable Delivery token (one scan per sender per read, local
// files only). Settled entries (DeliveryDelivered, DeliveryHubOnly) are never
// scanned again.
func (e Env) hubInbox(dir string) ([]store.HubInboxEntry, error) {
	all, err := e.Store.HubInbox(dir)
	if err != nil {
		return nil, err
	}
	root := e.Paths.Root()
	sentBy := map[core.Handle]map[string]amq.Header{}
	for i := range all {
		x := &all[i]
		if x.Delivery == "" || x.DeliveryState != store.DeliveryPending {
			continue
		}
		byToken, ok := sentBy[x.By]
		if !ok {
			byToken = map[string]amq.Header{}
			ids, _ := amq.IDs(root, string(x.By), amq.BoxSent)
			for _, id := range ids {
				h, err := amq.ReadHeader(root, string(x.By), amq.BoxSent, id)
				var dc deliveryContext
				if err == nil && h.ContextOf(deliveryContextKey, &dc) && dc.Token != "" {
					byToken[dc.Token] = h
				}
			}
			sentBy[x.By] = byToken
		}
		if h, ok := byToken[x.Delivery]; ok {
			x.MsgID = h.ID
			if x.Thread == "" {
				x.Thread = h.Thread
			}
			x.DeliveryState = store.DeliveryDelivered
			if len(h.To) > 0 {
				x.DeliveryRoutedTo = core.Handle(h.To[0])
			}
		}
	}
	return all, nil
}

// HubInboxRequest is what `hub inbox` asks: the entries after Since, of one
// Epic and one kind when given, at most Limit; with Wait, until one comes
// or Wait passes.
type HubInboxRequest struct {
	Hub   string
	Since int64
	Wait  time.Duration
	Epic  string
	Kind  string
	Limit int
}

// HubInboxPage is a read of a Hub's inbox. NextSince is the --since that
// reads what comes after it.
type HubInboxPage struct {
	Hub       string                `json:"hub"`
	Items     []store.HubInboxEntry `json:"items"`
	NextSince int64                 `json:"next_since"`
	Truncated bool                  `json:"truncated"`
}

// hubInboxPoll is how often `hub inbox --wait` reads the inbox again.
const hubInboxPoll = 200 * time.Millisecond

// ReadHubInbox reads, for the Operator (ResolveOperator), the inbox of the
// Hub in its cockpit's scope (cockpitScope: a personal agent's own Hub,
// else --hub or the default Hub). An --epic must be an Epic of that Hub. It
// reads the inbox file alone, holding no lock, so it works with Rex down;
// with r.Wait it reads again until an entry comes or the wait ends.
func (e Env) ReadHubInbox(ctx context.Context, getenv func(string) string, r HubInboxRequest) (HubInboxPage, error) {
	op, err := e.ResolveOperator(getenv)
	if err != nil {
		return HubInboxPage{}, err
	}
	scope, err := e.cockpitScope(op, CockpitViewRequest{Hub: r.Hub})
	if err != nil {
		return HubInboxPage{}, err
	}
	if scope.hubDir == "" {
		return HubInboxPage{}, fmt.Errorf("%w: an inbox is a Hub's, and %s is of no Hub", ErrUsage, cockpitWhose(scope))
	}
	if r.Epic != "" {
		if ok, err := e.epicInScope(scope, r.Epic); err != nil {
			return HubInboxPage{}, err
		} else if !ok {
			return HubInboxPage{}, refused(fmt.Sprintf("the inbox of %s reading %s", cockpitWhose(scope), r.Epic), core.Decision{Reason: core.ReasonNotLinked})
		}
	}
	page := HubInboxPage{Hub: scope.hubDir, Items: []store.HubInboxEntry{}, NextSince: r.Since}
	deadline := time.Now().Add(r.Wait)
	for {
		all, err := e.hubInbox(scope.hubDir)
		if err != nil {
			return page, err
		}
		for _, x := range all {
			if x.Seq <= r.Since || r.Epic != "" && x.Epic != r.Epic || r.Kind != "" && x.Kind != r.Kind {
				continue
			}
			if r.Limit > 0 && len(page.Items) == r.Limit {
				page.Truncated = true
				break
			}
			page.Items = append(page.Items, x)
			page.NextSince = x.Seq
		}
		if len(page.Items) > 0 || r.Wait <= 0 || !time.Now().Before(deadline) {
			return page, nil
		}
		select {
		case <-ctx.Done():
			return page, nil
		case <-time.After(hubInboxPoll):
		}
	}
}

// epicInScope reports whether the Epic session is one of the Hub scope
// reads: linked to it (cockpitScope.matches), or, once its links are gone
// with its close, of that Hub by its lifecycle.
func (e Env) epicInScope(scope cockpitScope, session string) (bool, error) {
	links, err := e.linksToEpic(session)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(links, func(l core.Link) bool { return scope.matches(e, l) }) {
		return true, nil
	}
	hub, err := e.epicHub(session)
	return hub != "" && hub.Dir() == scope.hubDir, err
}
