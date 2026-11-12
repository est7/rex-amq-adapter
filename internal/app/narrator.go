package app

import (
	"context"
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
// ones, from their records, mailbox listings, and health marks.
func (e Env) sessionPulse(key core.SessionKey) (core.SessionPulse, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return core.SessionPulse{}, err
	}
	var p core.SessionPulse
	for _, r := range recs {
		if r.Server != key.Server || r.SessionID != key.SessionID || core.Stealth(r.Role) {
			continue
		}
		m := core.PulseMember{Handle: r.Handle, Role: r.Role, Live: r.Live(), Task: r.Task}
		if m.LastMail, err = e.lastMail(r.Handle); err != nil {
			return p, err
		}
		if r.Live() {
			mark, err := e.Store.HealthMark(r.BlockID)
			if err != nil {
				return p, err
			}
			m.Attention = attention(r, mark)
		}
		p.Members = append(p.Members, m)
	}
	return p.Normalize(), nil
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
func attention(r store.Record, m store.HealthMark) []string {
	var out []string
	at := func(what string, t time.Time) { out = append(out, what+"@"+t.UTC().Format(time.RFC3339)) }
	if !m.TurnErroredAt.IsZero() && m.TurnErroredBy == turnKey(r) {
		at("turn_error", m.TurnErroredAt)
	}
	if !m.BlockedSince.IsZero() && m.BlockedBy == turnKey(r) {
		at("waiting_on_the_human", m.BlockedSince)
	}
	if !m.LastStallAt.IsZero() {
		at("stalled", m.LastStallAt)
	}
	return out
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
		return e.Store.PutPulse(key.SessionID, store.PulseCursor{Pulse: now, At: at})
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
	body := strings.Join(c.Lines, "\n") + "\n\nRefresh your view with `" + e.SelfBin + " view refresh`, then say what changed in `--summary`."
	sent, _, err := e.post(ctx, Outgoing{
		Op: core.Operation{Kind: core.OpSend, System: true}, From: narr.Handle, To: []core.Handle{narr.Handle},
		Args: []string{"--allow-self", "--kind", "status", "--priority", c.Priority, "--labels", amq.SystemLabel, "--subject", subject, "--body", body},
	})
	if err != nil {
		return fmt.Errorf("tell the narrator %s: %w", narr.Handle, err)
	}
	return e.Store.PutPulse(key.SessionID, store.PulseCursor{Pulse: now, Notice: sent.ID, At: at})
}
