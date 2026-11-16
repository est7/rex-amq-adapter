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
// loc's session, or of a pair Epic's lead (pairLead), replacing the one
// before.
func (e Env) RecordHandBack(ctx context.Context, loc Location, body string) (store.HandBack, error) {
	var h store.HandBack
	rec, ok, err := e.Store.Record(loc.Block)
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
	h = store.HandBack{By: rec.Handle, At: e.Now().UTC(), Body: body}
	return h, e.Store.PutHandBack(rec.SessionID, h)
}

// pairLead reports whether rec leads its pair Epic: an implementer nobody
// summoned, in a session working as crew pair that has no orchestrator, live
// or departed. It runs the goal in the orchestrator's place, so it records
// the hand-back and reads the view; an implementer an orchestrator summoned,
// or one in an Epic whose orchestrator left, does neither.
func (e Env) pairLead(rec store.Record) (bool, error) {
	if rec.Role != implementerRole || rec.Parent != "" || !placed(rec) {
		return false, nil
	}
	c, ok, err := e.Store.SessionCrew(rec.SessionID)
	if err != nil || !ok || c.Crew != pairCrew {
		return false, err
	}
	recs, err := e.Store.Records()
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
