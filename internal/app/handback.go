package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An orchestrator ends its run by recording its final report with
// `rex-amq-adapter hand-back`: the whole text (review queue, PR links,
// Blocked on me, Decided for you, Changed, Found) goes into
// sessions/<id>/hand-back.json, the view carries it whole (hand_back), and
// the session's pulse marks the orchestrator with the hand-back's time and
// body digest, so
// the watcher wakes the narrator once the report is on disk, never before.

// handBackMax bounds a hand-back: the view holds it whole, so one too large
// is refused rather than cut.
const handBackMax = 256 << 10

// RecordHandBack records body as the hand-back of the live orchestrator in
// loc's session, or of a pair Epic's lead (pairLead), replacing the one
// before, and marks the session's Epic lifecycle hand_back. The lifecycle numbers the hand-backs, and a Seq never
// changes its digest. The same report recorded again (the same
// store.HandBackDigest) leaves the stored one as it was, time included, so
// the narrator is not told twice and its Seq stays; a different one takes the
// Seq after the lifecycle's latest, and is stored only once the lifecycle
// lists it beside the one it replaces. A retry after a failure part way
// finishes what is left: the same report again stores the one the lifecycle
// lists.
func (e Env) RecordHandBack(ctx context.Context, loc Location, body string) (store.HandBack, error) {
	var h store.HandBack
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return h, err
	}
	if !ok || !rec.Live() || !placed(rec) {
		return h, fmt.Errorf("no AMQ identity in a Rex session for this block (%s), so there is no run to hand back", loc.Block)
	}
	lead, err := e.pairLead(rec)
	if err != nil {
		return h, err
	}
	if rec.Role != core.OrchestratorRole && !lead {
		return h, fmt.Errorf("%w: only a session's %s, or a pair Epic's own %s, records its hand-back, and %s is %q", ErrRefused, core.OrchestratorRole, implementerRole, rec.Handle, rec.Role)
	}
	if strings.TrimSpace(body) == "" {
		return h, errors.New("a hand-back needs its report: TEXT, @FILE, or - for stdin")
	}
	if len(body) > handBackMax {
		return h, fmt.Errorf("the hand-back is %d bytes, over the %d the view holds whole; shorten it", len(body), handBackMax)
	}
	unlock, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return h, err
	}
	defer unlock()
	stored, hasPrev, err := e.Store.HandBack(rec.SessionID)
	if err != nil {
		return h, err
	}
	lc, _, lcErr := e.Store.EpicLifecycle(rec.SessionID)
	digest := store.HandBackDigest(body)
	now := e.Now().UTC()
	if lcErr != nil {
		// With the history unreadable, only a first report is stored, as
		// Seq 1 is all it can be; any other waits for the lifecycle.
		switch {
		case !hasPrev:
			h = store.HandBack{By: rec.Handle, At: now, Body: body, Seq: 1, Digest: digest}
			if err := e.Store.PutHandBack(rec.SessionID, h); err != nil {
				return h, err
			}
		case numbered(stored).Digest == digest:
			h = numbered(stored)
		default:
			return h, fmt.Errorf("the hand-back is not stored, and %d stays: reading the Epic's lifecycle failed: %w", numbered(stored).Seq, lcErr)
		}
		return h, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", lcErr)
	}
	if lc.SessionID == "" {
		lc = store.EpicLifecycle{SessionID: rec.SessionID, Server: rec.Server}
	}
	// The lifecycle numbers hand-backs, and a Seq keeps its digest once
	// given: the stored one is listed when an earlier failure left it out
	// (after the latest, should its Seq have gone to another report).
	var prev store.HandBack
	if hasPrev {
		prev = numbered(stored)
		if !slices.ContainsFunc(lc.HandBacks, func(r store.HandBackRef) bool { return r.Seq == prev.Seq && r.Digest == prev.Digest }) {
			if latest, ok := lc.LatestHandBack(); ok && slices.ContainsFunc(lc.HandBacks, func(r store.HandBackRef) bool { return r.Seq == prev.Seq }) {
				prev.Seq = latest.Seq + 1
			}
			lc.NoteHandBack(prev)
		}
	}
	latest, hasLatest := lc.LatestHandBack()
	stays := "the hand-back is not stored"
	if hasPrev {
		stays = fmt.Sprintf("the hand-back is not stored, and %d stays", prev.Seq)
	}
	switch {
	case !hasLatest:
		// The first: stored at once, as the report matters more than its
		// place in the history, which the next hand-back fills in.
		h = store.HandBack{By: rec.Handle, At: now, Body: body, Seq: 1, Digest: digest}
		if err := e.Store.PutHandBack(rec.SessionID, h); err != nil {
			return h, err
		}
		lc.NoteHandBack(h)
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			return h, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", err)
		}
		return h, nil
	case latest.Digest == digest && hasPrev && prev.Seq == latest.Seq && prev.Digest == digest:
		// The same report again: left as it was, time included, so the
		// narrator is not told twice.
		h = prev
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			return h, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", err)
		}
		if h != stored {
			return h, e.Store.PutHandBack(rec.SessionID, h)
		}
		return h, nil
	case latest.Digest == digest:
		// The latest the lifecycle lists, which an earlier run failed to
		// store: stored now, under its Seq and time.
		h = store.HandBack{By: latest.By, At: latest.At, Body: body, Seq: latest.Seq, Digest: digest}
	default:
		h = store.HandBack{By: rec.Handle, At: now, Body: body, Seq: latest.Seq + 1, Digest: digest}
	}
	// Listed before it is stored, so the one it replaces is never lost from
	// the history.
	lc.NoteHandBack(h)
	if err := e.Store.PutEpicLifecycle(lc); err != nil {
		return h, fmt.Errorf("%s: recording it in the Epic's lifecycle failed: %w", stays, err)
	}
	if err := e.Store.PutHandBack(rec.SessionID, h); err != nil {
		return h, fmt.Errorf("the Epic's lifecycle lists hand-back %d, but storing it failed (run hand-back again with the same report): %w", h.Seq, err)
	}
	return h, nil
}

// numbered is a stored hand-back with its Seq and digest; one stored before
// they were kept counts as the first.
func numbered(h store.HandBack) store.HandBack {
	h.Seq = max(h.Seq, 1)
	if h.Digest == "" {
		h.Digest = store.HandBackDigest(h.Body)
	}
	return h
}

// pairLead reports whether rec leads its pair Epic: an implementer nobody
// summoned, in a session working as crew pair that has no orchestrator, live
// or departed. It runs the goal in the orchestrator's place, so it records
// the hand-back and reads the view; an implementer an orchestrator summoned,
// or one in an Epic whose orchestrator left, does neither.
func (e Env) pairLead(rec agent) (bool, error) {
	if rec.Role != implementerRole || rec.Parent != "" || !placed(rec) {
		return false, nil
	}
	c, ok, err := e.Store.SessionCrew(rec.SessionID)
	if err != nil || !ok || c.Crew != pairCrew {
		return false, err
	}
	recs, err := e.agents()
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.Role == core.OrchestratorRole && r.Server == rec.Server && r.SessionID == rec.SessionID {
			return false, nil
		}
	}
	return true, nil
}
