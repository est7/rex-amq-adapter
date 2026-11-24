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

// noteHandBackInHub keeps the Epic session's stored hand-back in its Hub's
// inbox, once, while it is the latest the lifecycle lists: a retry of a
// hand-back that stored the report and failed before this, whether or not
// the report changed, keeps it then. The caller holds LockEpicLifecycle.
func (e Env) noteHandBackInHub(ctx context.Context, session string) error {
	stored, ok, err := e.Store.HandBack(session)
	if err != nil || !ok {
		return err
	}
	lc, ok, err := e.Store.EpicLifecycle(session)
	if err != nil || !ok {
		return err
	}
	h := numbered(stored)
	latest, ok := lc.LatestHandBack()
	if !ok || latest.Seq != h.Seq || latest.Digest != h.Digest {
		return nil
	}
	hub, err := e.epicHub(session)
	if err != nil || hub == "" {
		return err
	}
	_, _, err = e.Store.AppendHubInbox(ctx, hub.Dir(), store.HubInboxEntry{
		Epic: session, HubKey: hub, Kind: store.InboxHandBack, HandBackSeq: h.Seq, Digest: h.Digest, By: h.By, At: h.At,
		DedupeKey: "handback:" + session + "#" + strconv.Itoa(h.Seq),
	}, e.Now())
	if err != nil {
		return fmt.Errorf("the hand-back is stored; keeping it in the Hub's inbox failed (run hand-back again): %w", err)
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

// narratorMail is narrator mail to its Hub's personal agent, ready to keep
// in the Hub's inbox once it is sent.
type narratorMail struct {
	entry store.HubInboxEntry
}

// prepareNarratorMail is narrator n's message (subject, thread as given,
// body) to the personal agent of hub, with the Epic's latest hand-back.
func (e Env) prepareNarratorMail(n agent, hub core.HubKey, subject, thread, body string) (narratorMail, error) {
	seq, err := e.latestHandBackSeq(n.SessionID)
	if err != nil {
		return narratorMail{}, err
	}
	return narratorMail{entry: store.HubInboxEntry{Epic: n.SessionID, HubKey: hub, Kind: store.InboxNarrator, Thread: thread, Subject: subject,
		HandBackSeq: seq, Body: body, By: n.Handle}}, nil
}

// keepNarratorMail keeps nm in its Hub's inbox, with the amq message sent
// that carried it to a personal agent (none when no personal agent of the
// Hub, live or recorded, could take it). It runs after the delivery, which
// gives the message its id, and holds no lock across it. The message is
// its key, so keeping one message again keeps it once; two messages with
// the same words are two entries.
func (e Env) keepNarratorMail(ctx context.Context, nm narratorMail, sent amq.Sent) (store.HubInboxEntry, error) {
	x := nm.entry
	x.MsgID = sent.ID
	if sent.Thread != "" {
		x.Thread = sent.Thread
	}
	if x.MsgID != "" {
		x.DedupeKey = "narrator:" + x.MsgID
	} else {
		token, err := newToken()
		if err != nil {
			return x, err
		}
		x.DedupeKey = "narrator:hub:" + token
	}
	got, _, err := e.Store.AppendHubInbox(ctx, x.HubKey.Dir(), x, e.Now())
	if err != nil {
		if sent.ID != "" {
			return got, fmt.Errorf("the message was delivered as %s; keeping it in the Hub's inbox failed: %w", sent.ID, err)
		}
		return got, fmt.Errorf("no personal agent of the Hub could take the message, and keeping it in the Hub's inbox failed: %w", err)
	}
	return got, nil
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
