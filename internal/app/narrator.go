package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A narrator takes no member mail (core.CanExchange): what wakes it is the
// adapter's own system mail, which the session's watcher sends when the
// session's pulse changes. An attention change goes urgent and skips the
// narrator's doorbell hold; routine progress goes normal or low, and while
// one routine notice waits unread, later routine changes ride along with it.

// sessionPulse is session key's pulse now: its agents other than the stealth
// ones, from their records, mailbox listings, health marks, and the
// session's recorded hand-back.
func (e Env) sessionPulse(key core.SessionKey) (core.SessionPulse, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return core.SessionPulse{}, err
	}
	hb, hasHB, err := e.Store.HandBack(key.SessionID)
	if err != nil {
		return core.SessionPulse{}, err
	}
	var p core.SessionPulse
	for _, r := range recs {
		if r.Server != key.Server || r.SessionID != key.SessionID || core.Stealth(r.Role) {
			continue
		}
		m := core.PulseMember{Handle: r.Handle, Role: r.Role, Live: r.Live(), Task: r.Task, Retired: !r.Live() && r.DepartReason == store.DepartRetired}
		if hasHB && hb.By == r.Handle {
			sum := sha256.Sum256([]byte(hb.Body))
			m.HandBack = hb.At.UTC().Format(time.RFC3339Nano) + "#" + hex.EncodeToString(sum[:4])
		}
		if m.LastMail, err = e.lastMail(r.Handle); err != nil {
			return p, err
		}
		if r.Live() {
			if m.Attention, err = e.attention(r); err != nil {
				return p, err
			}
		}
		p.Members = append(p.Members, m)
	}
	return p.Normalize(), nil
}

// withoutHandBacks is p with no hand-back marked.
func withoutHandBacks(p core.SessionPulse) core.SessionPulse {
	out := p.Normalize()
	for i := range out.Members {
		out.Members[i].HandBack = ""
	}
	return out
}

// lastMail is the newest message id h holds or sent; ids sort by time.
func (e Env) lastMail(h core.Handle) (string, error) {
	newest := ""
	for _, box := range []amq.Box{amq.BoxUnread, amq.BoxDrained, amq.BoxSent} {
		ids, err := amq.IDs(e.Paths.Root(), string(h), box)
		if err != nil {
			return "", err
		}
		if n := len(ids); n > 0 && ids[n-1] > newest {
			newest = ids[n-1]
		}
	}
	return newest, nil
}

// attention is what waits on someone in r's block: its own turn that ended in
// an error, its own run waiting on the human, a stall.
func (e Env) attention(r store.Record) ([]string, error) {
	mark, err := e.Store.HealthMark(r.BlockID)
	if err != nil {
		return nil, err
	}
	te, hasErr, err := e.Store.TurnError(r.BlockID)
	if err != nil {
		return nil, err
	}
	var out []string
	at := func(what string, t time.Time) { out = append(out, what+"@"+t.UTC().Format(time.RFC3339)) }
	if hasErr && te.By == turnKey(r) {
		at("turn_error", te.At)
	}
	if !mark.BlockedSince.IsZero() && mark.BlockedBy == turnKey(r) {
		at("waiting_on_the_human", mark.BlockedSince)
	}
	if !mark.LastStallAt.IsZero() && (mark.HealthBy == "" || mark.HealthBy == turnKey(r)) {
		at("stalled", mark.LastStallAt)
	}
	return out, nil
}

// tellNarrator mails narr, the session's narrator, what changed in session
// key since it was last told. The first look only records the pulse: a
// narrator reads its view when it starts.
func (e Env) tellNarrator(ctx context.Context, key core.SessionKey, narr store.Record) error {
	now, err := e.sessionPulse(key)
	if err != nil {
		return err
	}
	cur, ok, err := e.Store.Pulse(key.SessionID)
	if err != nil {
		return err
	}
	at := e.Now().UTC()
	if !ok {
		// The first look only records the session, except a hand-back
		// already stored: the narrator may have read its view before it was,
		// so it is told as new.
		cur.Pulse = withoutHandBacks(now)
		if cur.Pulse.Digest() == now.Digest() {
			return e.Store.PutPulse(key.SessionID, store.PulseCursor{Pulse: now, At: at})
		}
	}
	c := core.ComparePulse(cur.Pulse, now)
	if c.Priority == "" {
		return nil
	}
	if c.Priority != core.PriorityUrgent && cur.Notice != "" {
		unread, err := amq.Unread(e.Paths.Root(), string(narr.Handle))
		if err != nil {
			return err
		}
		if slices.Contains(unread, cur.Notice) {
			// The waiting notice sends the narrator to its view, which
			// shows this change too.
			cur.Pulse, cur.At = now, at
			return e.Store.PutPulse(key.SessionID, cur)
		}
	}
	subject := "Your Rex session changed"
	if c.Priority == core.PriorityUrgent {
		subject = "Your Rex session needs attention"
	}
	body := strings.Join(c.Lines, "\n") + "\n\nRefresh your view with `" + e.SelfBin + " view refresh`, then say what changed in `--summary`. Mail the personal agent only when your role prompt names the case (its question, a hand-back or orchestrator departure, a wait on the human); otherwise end your turn without mailing anyone."
	sent, _, err := e.post(ctx, Outgoing{
		Op: core.Operation{Kind: core.OpSend, System: true}, From: narr.Handle, To: []core.Handle{narr.Handle},
		Args: []string{"--allow-self", "--kind", "status", "--priority", c.Priority, "--labels", amq.SystemLabel, "--subject", subject, "--body", body},
	})
	if err != nil {
		return fmt.Errorf("tell the narrator %s: %w", narr.Handle, err)
	}
	return e.Store.PutPulse(key.SessionID, store.PulseCursor{Pulse: now, Notice: sent.ID, At: at})
}

// A narrator's doorbell holds: routine mail (a status question, a routine
// notice) rings it after at most these, batched; urgent mail at once.
const (
	narratorHoldNormal = 2 * time.Minute
	narratorHoldLow    = 10 * time.Minute
)

// alignHolds gives a narrator its doorbell holds in .wake.settings, which its
// waker and the adapter's hooks both apply, and takes them from an agent
// that was one (was) and no longer is. A failure is reported, never fatal:
// holds batch mail, they guard nothing.
func (e Env) alignHolds(ctx context.Context, rec store.Record, was string) {
	var normal, low time.Duration
	switch {
	case rec.Role == core.NarratorRole:
		normal, low = narratorHoldNormal, narratorHoldLow
	case was != core.NarratorRole:
		return
	}
	h := string(rec.Handle)
	if n, l, err := amq.ReadHolds(e.Paths.Root(), h); err == nil && n == normal && l == low {
		return
	}
	if err := e.AMQ.ConfigureHolds(ctx, e.Paths.Root(), h, normal, low); err != nil {
		fmt.Fprintf(Diagnostics, "set the doorbell hold of %s: %v\n", h, err)
	}
}
