package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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

// keepNarratorMail keeps narrator n's message (subject, thread as given,
// body) to the personal agent of hub in the Hub's inbox, before any
// delivery: the inbox holds it whatever becomes of the delivery. Its
// Delivery token travels in the message's context (withDelivery), and its
// amq id is recorded beside it once there is one (noteNarratorDelivery),
// or found again by that token in the narrator's sent mail (hubInbox).
// Each send is its own entry, the same words twice are two, unless the
// sender names it (opID): a retry under the same op id finds its entry,
// done when it was delivered already, and the op id with another message
// is a conflict.
func (e Env) keepNarratorMail(ctx context.Context, n agent, hub core.HubKey, opID, subject, thread, body string) (x store.HubInboxEntry, done bool, err error) {
	key := ""
	if opID != "" {
		key = "narrator:op:" + opID
		all, err := e.hubInbox(hub.Dir())
		if err != nil {
			return x, false, err
		}
		if i := slices.IndexFunc(all, func(old store.HubInboxEntry) bool { return old.DedupeKey == key }); i >= 0 {
			return e.narratorRetry(all[i], n, subject, body, opID)
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
	x, added, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{Epic: n.SessionID, HubKey: hub, Kind: store.InboxNarrator, Thread: thread, Subject: subject,
		HandBackSeq: seq, Body: body, By: n.Handle, DedupeKey: key, Delivery: token}, e.Now())
	if err != nil {
		return x, false, fmt.Errorf("keep the message in the Hub's inbox (nothing was sent): %w", err)
	}
	if !added {
		// A run of the same op id kept it meanwhile.
		return e.narratorRetry(x, n, subject, body, opID)
	}
	return x, false, nil
}

// narratorRetry answers a retry of the narrator message x kept under
// opID: the same message, delivered (done) or still to send, or a
// conflict.
func (e Env) narratorRetry(x store.HubInboxEntry, n agent, subject, body, opID string) (store.HubInboxEntry, bool, error) {
	if x.Epic != n.SessionID || x.By != n.Handle || x.Subject != subject || x.Body != body {
		return x, false, refusal(fault.Conflict, "op_id_reused", "--op-id %q already names another message (Hub inbox entry %d)", opID, x.Seq)
	}
	return x, x.MsgID != "", nil
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

// noteNarratorDelivery records what became of x, kept before it was
// sent: delivered as sent, or (attempted false) sent to nobody. A failed
// send records nothing, so hubInbox looks for it in the sender's sent mail.
// A failure only leaves the record out, so it is reported, never
// returned: the message is kept.
func (e Env) noteNarratorDelivery(x store.HubInboxEntry, sent amq.Sent, attempted bool) {
	if sent.ID == "" && attempted {
		return
	}
	if err := e.Store.PutHubInboxDelivery(x.HubKey.Dir(), x.Delivery, sent.ID, sent.Thread); err != nil {
		fmt.Fprintf(Diagnostics, "record what became of Hub inbox entry %d: %v\n", x.Seq, err)
	}
}

// hubInbox is the inbox of the Hub directory dir (store.HubInbox), each
// entry kept before its delivery with the amq id it was delivered as: its
// delivery record's, else that of the message in its sender's sent mail
// whose context names its Delivery token, each sender's read once. It reads
// local files only.
func (e Env) hubInbox(dir string) ([]store.HubInboxEntry, error) {
	all, err := e.Store.HubInbox(dir)
	if err != nil {
		return nil, err
	}
	root := e.Paths.Root()
	sentBy := map[core.Handle]map[string]amq.Header{}
	for i := range all {
		x := &all[i]
		if x.Delivery == "" || x.MsgID != "" {
			continue
		}
		byToken, ok := sentBy[x.By]
		if !ok {
			byToken = map[string]amq.Header{}
			ids, _ := amq.IDs(root, string(x.By), amq.BoxSent)
			for _, id := range ids {
				h, err := amq.ReadHeader(root, string(x.By), amq.BoxSent, id)
				var dc deliveryContext
				if err == nil && h.ContextOf(deliveryContextKey, &dc) {
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
