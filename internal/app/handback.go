package app

import (
	"context"
	"errors"
	"fmt"
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
// loc's session, replacing the one before, and marks the session's Epic
// lifecycle hand_back. The same report recorded again (the same
// store.HandBackDigest) leaves the stored one as it was, time included, so
// the narrator is not told twice and its Seq stays; a different one takes the
// next Seq, and is stored only once the lifecycle records it beside the one
// it replaces. A retry after a failure part way finishes what is left.
func (e Env) RecordHandBack(ctx context.Context, loc Location, body string) (store.HandBack, error) {
	var h store.HandBack
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return h, err
	}
	if !ok || !rec.Live() || !placed(rec) {
		return h, fmt.Errorf("no AMQ identity in a Rex session for this block (%s), so there is no run to hand back", loc.Block)
	}
	if rec.Role != core.OrchestratorRole {
		return h, fmt.Errorf("%w: only a session's %s records its hand-back, and %s is %q", ErrRefused, core.OrchestratorRole, rec.Handle, rec.Role)
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
	prev := numbered(stored)
	h = nextHandBack(prev, hasPrev, store.HandBack{By: rec.Handle, At: e.Now().UTC(), Body: body})
	// A different report replaces the stored one only once the lifecycle
	// holds both, so a lifecycle that cannot be written never loses the one
	// replaced. A first report, or the same one again, is stored at once:
	// the report matters more than its place in the history, which the next
	// hand-back fills in.
	replaces := hasPrev && h.Seq != prev.Seq
	if !replaces && (!hasPrev || h != stored) {
		if err := e.Store.PutHandBack(rec.SessionID, h); err != nil {
			return h, err
		}
	}
	notStored := "the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again)"
	if replaces {
		notStored = fmt.Sprintf("the hand-back is not stored, and %d stays: recording them both in the Epic's lifecycle failed", prev.Seq)
	}
	lc, _, err := e.Store.EpicLifecycle(rec.SessionID)
	if err != nil {
		return h, fmt.Errorf("%s: %w", notStored, err)
	}
	if lc.SessionID == "" {
		lc = store.EpicLifecycle{SessionID: rec.SessionID, Server: rec.Server}
	}
	if hasPrev {
		lc.NoteHandBack(prev)
	}
	lc.NoteHandBack(h)
	if err := e.Store.PutEpicLifecycle(lc); err != nil {
		return h, fmt.Errorf("%s: %w", notStored, err)
	}
	if replaces {
		if err := e.Store.PutHandBack(rec.SessionID, h); err != nil {
			return h, err
		}
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

// nextHandBack is the hand-back to store when next is recorded over prev
// (numbered): prev itself for the same report; otherwise next, numbered after
// prev.
func nextHandBack(prev store.HandBack, hasPrev bool, next store.HandBack) store.HandBack {
	next.Digest = store.HandBackDigest(next.Body)
	if !hasPrev {
		next.Seq = 1
		return next
	}
	if prev.Digest == next.Digest {
		return prev
	}
	next.Seq = prev.Seq + 1
	return next
}
