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
// loc's session, replacing the one before.
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
	h = store.HandBack{By: rec.Handle, At: e.Now().UTC(), Body: body}
	return h, e.Store.PutHandBack(rec.SessionID, h)
}
