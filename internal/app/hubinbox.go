package app

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
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

// noteHandBackInHub keeps in the Epic session's Hub inbox, once each, every
// hand-back its lifecycle lists that was stored: those before the stored
// one, which replaced them, and the stored one while it is the latest
// listed (a later one is listed before it is stored, and is kept once it
// is). A run that failed to keep one is made good by the next hand-back
// run, whether it repeats that one or replaces it. The caller holds
// LockEpicLifecycle.
func (e Env) noteHandBackInHub(ctx context.Context, session string) error {
	stored, ok, err := e.Store.HandBack(session)
	if err != nil || !ok {
		return err
	}
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok {
		return err
	}
	hub, err := e.epicHub(session)
	if err != nil || hub == "" {
		return err
	}
	h := numbered(stored)
	refs := slices.Clone(lc.HandBacks)
	slices.SortFunc(refs, func(a, b store.HandBackRef) int { return a.Seq - b.Seq })
	for _, ref := range refs {
		if ref.Seq > h.Seq || ref.Seq == h.Seq && ref.Digest != h.Digest {
			continue
		}
		if _, _, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{
			Epic: session, HubKey: hub, Kind: store.InboxHandBack, HandBackSeq: ref.Seq, Digest: ref.Digest, By: ref.By, At: ref.At,
			DedupeKey: "handback:" + session + "#" + strconv.Itoa(ref.Seq),
		}, e.Now()); err != nil {
			return fmt.Errorf("the hand-back is stored; keeping it in the Hub's inbox failed (run hand-back again): %w", err)
		}
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
// delivery: the inbox holds it whatever becomes of the delivery, whose amq
// id is recorded beside it once there is one (PutHubInboxDelivery under the
// entry's Delivery). Each send is its own entry, the same words twice are
// two.
func (e Env) keepNarratorMail(ctx context.Context, n agent, hub core.HubKey, subject, thread, body string) (store.HubInboxEntry, error) {
	seq, err := e.latestHandBackSeq(n.SessionID)
	if err != nil {
		return store.HubInboxEntry{}, err
	}
	token, err := newToken()
	if err != nil {
		return store.HubInboxEntry{}, err
	}
	x, _, err := e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{Epic: n.SessionID, HubKey: hub, Kind: store.InboxNarrator, Thread: thread, Subject: subject,
		HandBackSeq: seq, Body: body, By: n.Handle, DedupeKey: "narrator:" + token, Delivery: token}, e.Now())
	if err != nil {
		return x, fmt.Errorf("keep the message in the Hub's inbox (nothing was sent): %w", err)
	}
	return x, nil
}

// noteNarratorDelivery records that x, kept before it was sent, was
// delivered as sent. A failure only leaves the entry without its id, so it
// is reported, never returned: the message is kept and delivered.
func (e Env) noteNarratorDelivery(x store.HubInboxEntry, sent amq.Sent) {
	if sent.ID == "" {
		return
	}
	if err := e.Store.PutHubInboxDelivery(x.HubKey.Dir(), x.Delivery, sent.ID, sent.Thread); err != nil {
		fmt.Fprintf(Diagnostics, "record that Hub inbox entry %d was delivered as %s: %v\n", x.Seq, sent.ID, err)
	}
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
		all, err := e.Store.HubInbox(scope.hubDir)
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
