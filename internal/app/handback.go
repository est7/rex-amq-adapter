package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An Epic's lead ends its run by recording its final report with
// `rex-amq-adapter epic handback`: the whole text (review queue, PR links,
// Blocked on me, Decided for you, Changed, Found) goes into
// sessions/<id>/hand-back.json, the view carries it whole (hand_back), and
// the session's pulse marks the lead with the hand-back's time and digest,
// so the watcher wakes the narrator once the report is on disk, never
// before. A design Epic's lead hands back its drafts with it, byte for byte
// (--artifact): they are published read-only under the hand-back's Seq
// (store.PutArtifacts) before the hand-back is stored.

// handBackMax bounds a hand-back: the view holds it whole, so one too large
// is refused rather than cut.
const handBackMax = 256 << 10

// HandBackRequest is a hand-back: the report, the artifacts snapshot with
// it (SnapshotArtifacts), and the --op-id that names the request ("" for
// none).
type HandBackRequest struct {
	Body      string
	Artifacts []store.ArtifactBlob
	OpID      string
}

// RecordHandBack records body, with no artifacts, as the hand-back of the
// live lead of loc's session (RecordHandBackWith).
func (e Env) RecordHandBack(ctx context.Context, loc Location, body string) (store.HandBack, error) {
	h, _, err := e.RecordHandBackWith(ctx, loc, HandBackRequest{Body: body})
	return h, err
}

// RecordHandBackWith records r as the hand-back of the live lead of loc's
// session (sessionLead), replacing the one before, and marks the session's
// Epic lifecycle hand_back. The lifecycle numbers the hand-backs, and a Seq
// never changes its digest. The same report with the same artifacts
// recorded again (the same digest: store.HandBackDigest folded with the
// artifacts', store.HandBackWithArtifacts) leaves the stored one as it was,
// time included, so the narrator is not told twice and its Seq stays, and
// reports changed false; a different one takes the Seq after the
// lifecycle's latest, its artifacts published under that Seq, and is
// stored only once the lifecycle lists it beside the one it replaces. With
// an op id, a retry of the request it named is answered from the hand-back
// it recorded, and the op id with another request is a conflict. A retry
// after a failure part way finishes what is left: the same request again
// stores the one the lifecycle lists.
//
// Whenever the stored hand-back is the latest the lifecycle lists, it is
// kept in the Hub's inbox too (noteHandBackInHub), under the same lock and
// whether or not this run changed it.
func (e Env) RecordHandBackWith(ctx context.Context, loc Location, r HandBackRequest) (h store.HandBack, changed bool, err error) {
	rec, err := e.handBackCaller(loc, r.Body)
	if err != nil {
		return h, false, err
	}
	if err := ValidOpID(r.OpID); err != nil {
		return h, false, err
	}
	unlock, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return h, false, err
	}
	defer unlock()
	// Before and after: one an earlier run failed to keep is kept before
	// this one can replace it.
	before := e.noteHandBackInHub(ctx, rec.SessionID)
	h, changed, err = e.recordHandBack(rec, r)
	if ierr := e.noteHandBackInHub(ctx, rec.SessionID); ierr != nil {
		err = errors.Join(err, ierr)
	} else if before != nil {
		fmt.Fprintf(Diagnostics, "keep an earlier hand-back in the Hub's inbox: %v (kept now)\n", before)
	}
	return h, changed, err
}

// recordHandBack is RecordHandBackWith's record of r for the lead rec; the
// caller holds LockEpicLifecycle.
func (e Env) recordHandBack(rec agent, r HandBackRequest) (h store.HandBack, changed bool, err error) {
	entries := store.Entries(r.Artifacts)
	digest := store.HandBackWithArtifacts(store.HandBackDigest(r.Body), entries)
	opHash := ""
	if r.OpID != "" {
		opHash = store.ContractDigest([]byte(r.OpID))
	}
	stored, hasPrev, err := e.Store.HandBack(rec.SessionID)
	if err != nil {
		return h, false, err
	}
	lc, _, lcErr := e.Store.EpicLifecycle(rec.SessionID)
	now := e.Now().UTC()
	fresh := func(seq int) store.HandBack {
		return store.HandBack{By: rec.Handle, At: now, Body: r.Body, Seq: seq, Digest: digest, Artifacts: entries, OpHash: opHash}
	}
	if lcErr != nil {
		// With the history unreadable, only a first report is stored, as
		// Seq 1 is all it can be; any other waits for the lifecycle.
		switch {
		case hasPrev && opHash != "" && stored.OpHash == opHash && numbered(stored).Digest != digest:
			return h, false, errHandBackOpConflict(r.OpID)
		case !hasPrev:
			h, changed = fresh(1), true
			if err := e.storeHandBack(rec.SessionID, h, r.Artifacts); err != nil {
				return h, false, err
			}
		case numbered(stored).Digest == digest:
			h = numbered(stored)
		default:
			return h, false, fmt.Errorf("the hand-back is not stored, and %d stays: reading the Epic's lifecycle failed: %w", numbered(stored).Seq, lcErr)
		}
		return h, changed, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", lcErr)
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
	// A retry of an op id: the hand-back it recorded, or a conflict.
	if opHash != "" {
		if i := slices.IndexFunc(lc.HandBacks, func(r store.HandBackRef) bool { return r.Accepts(opHash) }); i >= 0 {
			ref := lc.HandBacks[i]
			if ref.Digest != digest {
				return h, false, errHandBackOpConflict(r.OpID)
			}
			if latest, _ := lc.LatestHandBack(); latest.Seq != ref.Seq || !hasPrev || prev.Seq != ref.Seq {
				// An earlier hand-back, since replaced: the op is done.
				return store.HandBack{By: ref.By, At: ref.At, Body: r.Body, Seq: ref.Seq, Digest: ref.Digest, Artifacts: entries, OpHash: ref.OpHash}, false, nil
			}
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
		h = fresh(1)
		if err := e.storeHandBack(rec.SessionID, h, r.Artifacts); err != nil {
			return h, false, err
		}
		lc.NoteHandBack(h)
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			return h, true, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", err)
		}
		return h, true, nil
	case latest.Digest == digest && hasPrev && prev.Seq == latest.Seq && prev.Digest == digest:
		// The same request again: left as it was, time included, so the
		// narrator is not told twice; a new op id is bound to it all the
		// same, so that op id with another report is a conflict.
		h = prev
		lc.NoteHandBackOp(h.Seq, opHash)
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			return h, false, fmt.Errorf("the hand-back is stored; recording it in the Epic's lifecycle failed (run hand-back again): %w", err)
		}
		if !sameHandBack(h, stored) {
			return h, false, e.storeHandBack(rec.SessionID, h, r.Artifacts)
		}
		return h, false, nil
	case latest.Digest == digest:
		// The latest the lifecycle lists, which an earlier run failed to
		// store: stored now, under its Seq and time.
		h = store.HandBack{By: latest.By, At: latest.At, Body: r.Body, Seq: latest.Seq, Digest: digest, Artifacts: entries, OpHash: latest.OpHash}
	default:
		h = fresh(latest.Seq + 1)
	}
	// Listed before it is stored, so the one it replaces is never lost from
	// the history; the request's op id is bound to it either way.
	lc.NoteHandBack(h)
	lc.NoteHandBackOp(h.Seq, opHash)
	if err := e.Store.PutEpicLifecycle(lc); err != nil {
		return h, false, fmt.Errorf("%s: recording it in the Epic's lifecycle failed: %w", stays, err)
	}
	if err := e.storeHandBack(rec.SessionID, h, r.Artifacts); err != nil {
		return h, false, fmt.Errorf("the Epic's lifecycle lists hand-back %d, but storing it failed (run hand-back again with the same report): %w", h.Seq, err)
	}
	return h, true, nil
}

// storeHandBack publishes h's artifacts under its Seq, then stores h: a
// reader that finds the hand-back finds its artifacts.
func (e Env) storeHandBack(session string, h store.HandBack, blobs []store.ArtifactBlob) error {
	if len(blobs) > 0 {
		m := store.Manifest{SessionID: session, Seq: h.Seq, By: h.By, At: h.At, ReportDigest: store.HandBackDigest(h.Body), Artifacts: h.Artifacts}
		if err := e.Store.PutArtifacts(m, blobs); err != nil {
			return fmt.Errorf("publish the artifacts of hand-back %d: %w", h.Seq, err)
		}
	}
	return e.Store.PutHandBack(session, h)
}

// sameHandBack reports whether a and b are the same record.
func sameHandBack(a, b store.HandBack) bool {
	return a.By == b.By && a.At.Equal(b.At) && a.Body == b.Body && a.Seq == b.Seq && a.Digest == b.Digest && a.OpHash == b.OpHash
}

func errHandBackOpConflict(opID string) error {
	return refusal(fault.Conflict, "op_id_reused", "--op-id %q already recorded another hand-back (another report or other artifacts); a new hand-back needs a new op id", opID)
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

// PreviewHandBack runs RecordHandBack's checks on the caller and the report
// and records nothing: what --dry-run shows is who would record it, its
// size, and its digest.
func (e Env) PreviewHandBack(loc Location, body string) (store.HandBack, error) {
	return e.PreviewHandBackWith(loc, HandBackRequest{Body: body})
}

// PreviewHandBackWith runs RecordHandBackWith's checks on the caller, the
// report, the artifacts, and the op id, and records nothing.
func (e Env) PreviewHandBackWith(loc Location, r HandBackRequest) (store.HandBack, error) {
	rec, err := e.handBackCaller(loc, r.Body)
	if err != nil {
		return store.HandBack{}, err
	}
	if err := ValidOpID(r.OpID); err != nil {
		return store.HandBack{}, err
	}
	entries := store.Entries(r.Artifacts)
	return store.HandBack{By: rec.Handle, At: e.Now().UTC(), Body: r.Body, Artifacts: entries,
		Digest: store.HandBackWithArtifacts(store.HandBackDigest(r.Body), entries)}, nil
}

// handBackCaller is the live lead in loc that may record body as its
// session's hand-back.
func (e Env) handBackCaller(loc Location, body string) (agent, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return agent{}, err
	}
	if !ok || !rec.Live() || !placed(rec) {
		return agent{}, fmt.Errorf("no AMQ identity in a Rex session for this block (%s), so there is no run to hand back", loc.Block)
	}
	lead, err := e.sessionLead(rec)
	if err != nil {
		return agent{}, err
	}
	if !lead {
		return agent{}, fmt.Errorf("%w: only the Epic's lead (the %s nobody summoned that the session recorded) records its hand-back, and %s is %s", ErrRefused, "agent of its lead role", rec.Handle, whoIs(rec))
	}
	if strings.TrimSpace(body) == "" {
		return agent{}, fmt.Errorf("%w: a hand-back needs its report: TEXT, @FILE, or - for stdin", ErrUsage)
	}
	if len(body) > handBackMax {
		return agent{}, fmt.Errorf("%w: the hand-back is %d bytes, over the %d the view holds whole; shorten it", ErrUsage, len(body), handBackMax)
	}
	return rec, nil
}
