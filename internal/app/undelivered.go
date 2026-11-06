package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// checkSentFor is how long a batch of sent mail waits for its recipients to be
// observed. Past it the check gives up on them, so a recipient Rex cannot speak
// for neither holds the sender's other notices nor costs every tool call.
const checkSentFor = time.Minute

// A send to a departed agent succeeds in amq, and its mail waits for a resume
// that may never come, so nothing else would tell the sender. Sending is a tool
// call, so the sender's next hook is the moment to say so, with no process
// watching the mailboxes. One batch of sent mail goes through two steps:
//
//  1. checkableSent, with no ledger lock held, parks the recipients that are
//     gone: an agent that crashed or lost its pane sends no SessionEnd, so its
//     record says live until someone looks, and a send is why someone must.
//  2. takeUndelivered, under the sender's ledger lock, tells once what was left
//     unread in the inbox of an agent that has left.
//
// This is advice: a failure is reported, never costs the agent its own mail,
// and the same messages are checked again on the next hook until checkSentFor.

// checkableSent is the mail of rec's to tell about now: what it sent since the
// last check, once its recipients are settled. Mail that lands later is the
// next hook's. It reaches Rex only when there is new mail. It must run outside
// any ledger lock: parking takes the ledger locks of the parked agent's session,
// which can include rec's.
func (e Env) checkableSent(ctx context.Context, rec store.Record) []string {
	cursor, begun, err := e.Store.SentCursor(rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the sent cursor of %s: %v\n", rec.Handle, err)
		return nil
	}
	if !begun {
		// An agent adopted before this check existed: what it sent earlier was
		// never meant to be told about.
		e.seedSentCursor(rec.Handle)
		return nil
	}
	sent, err := amq.SentAfter(e.Paths.Root(), string(rec.Handle), cursor)
	if err != nil {
		fmt.Fprintf(Diagnostics, "list the mail %s sent: %v\n", rec.Handle, err)
		return nil
	}
	if len(sent) == 0 || e.settleRecipients(ctx, rec, sent) || e.checkOverdue(rec, sent[0]) {
		return sent
	}
	return nil
}

// settleRecipients parks the agents mailed in sent that are gone, and reports
// whether every one of them is now known to be live or gone.
func (e Env) settleRecipients(ctx context.Context, rec store.Record, sent []string) bool {
	ctx, cancel := context.WithTimeout(ctx, superviseFor)
	defer cancel()
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		fmt.Fprintf(Diagnostics, "check the agents %s mailed: %v\n", rec.Handle, err)
		return false
	}
	defer unlock()
	recs, err := e.Store.Records()
	if err != nil {
		fmt.Fprintf(Diagnostics, "check the agents %s mailed: %v\n", rec.Handle, err)
		return false
	}
	settled := true
	for _, r := range e.recipients(rec, sent, recs) {
		if !r.Live() {
			continue
		}
		observed, err := e.parkIfGone(ctx, r)
		if err != nil {
			fmt.Fprintf(Diagnostics, "check %s, mailed by %s: %v\n", r.Handle, rec.Handle, err)
		}
		settled = settled && observed && err == nil
	}
	return settled
}

// recipients are the records of the agents mailed in sent, other than rec. A
// message that cannot be read is reported and skipped, so that it does not hide
// the rest.
func (e Env) recipients(rec store.Record, sent []string, recs []store.Record) []store.Record {
	mailed := map[core.Handle]bool{}
	for _, id := range sent {
		to, err := amq.Recipients(e.Paths.Root(), string(rec.Handle), id)
		if err != nil {
			fmt.Fprintf(Diagnostics, "read %s, sent by %s: %v\n", id, rec.Handle, err)
			continue
		}
		for _, h := range to {
			mailed[core.Handle(h)] = true
		}
	}
	var out []store.Record
	for _, r := range recs {
		if mailed[r.Handle] && r.Handle != rec.Handle {
			out = append(out, r)
		}
	}
	return out
}

func (e Env) checkOverdue(rec store.Record, oldest string) bool {
	at, err := amq.SentAt(e.Paths.Root(), string(rec.Handle), oldest)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read %s, sent by %s: %v\n", oldest, rec.Handle, err)
		return true
	}
	if e.Now().Sub(at) < checkSentFor {
		return false
	}
	fmt.Fprintf(Diagnostics, "gave up checking the agents %s mailed in %s\n", rec.Handle, oldest)
	return true
}

// seedSentCursor starts checking h's mail after what its outbox holds now: an
// agent answers for what it sends from here. A previous owner of the handle
// wrote the rest, or the adapter did in h's name while it was away.
func (e Env) seedSentCursor(h core.Handle) {
	sent, err := amq.SentAfter(e.Paths.Root(), string(h), "")
	if err == nil {
		newest := ""
		if len(sent) > 0 {
			newest = sent[len(sent)-1]
		}
		err = e.Store.SetSentCursor(h, newest)
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "start checking the mail %s sends: %v\n", h, err)
	}
}

// takeUndelivered tells rec's agent about the mail in sent that no agent will
// read, once. The caller holds rec's ledger lock.
func (e Env) takeUndelivered(rec store.Record, sent []string) string {
	if len(sent) == 0 {
		return ""
	}
	notice, err := e.undeliveredNotice(rec, sent)
	if err != nil {
		fmt.Fprintf(Diagnostics, "check the mail %s sent: %v\n", rec.Handle, err)
		return ""
	}
	return notice
}

func (e Env) undeliveredNotice(rec store.Record, sent []string) (string, error) {
	cursor, _, err := e.Store.SentCursor(rec.Handle)
	if err != nil {
		return "", err
	}
	// Another hook of this agent, in a parallel tool call, may have told it.
	sent = slices.DeleteFunc(sent, func(id string) bool { return id <= cursor })
	if len(sent) == 0 {
		return "", nil
	}
	recs, err := e.Store.Records()
	if err != nil {
		return "", err
	}
	missed := e.departedRecipients(rec, sent, recs)
	if err := e.Store.SetSentCursor(rec.Handle, sent[len(sent)-1]); err != nil {
		return "", err
	}
	notices := make([]string, 0, len(missed))
	for _, r := range missed {
		notices = append(notices, core.UndeliveredNotice(r.Handle, occupant(recs, r.BlockID), sessionMates(recs, rec)))
	}
	return joinNotices(notices...), nil
}

// departedRecipients are the agents that left without reading a message in
// sent. Unread in a departed agent's inbox is the fact itself: it needs no
// comparison of when the mail went out and when the departure was noticed,
// which for a crash is much later.
func (e Env) departedRecipients(rec store.Record, sent []string, recs []store.Record) []store.Record {
	var missed []store.Record
	for _, r := range e.recipients(rec, sent, recs) {
		if r.Live() {
			continue
		}
		unread, err := amq.Unread(e.Paths.Root(), string(r.Handle))
		if err != nil {
			fmt.Fprintf(Diagnostics, "read the inbox of %s: %v\n", r.Handle, err)
			continue
		}
		if slices.ContainsFunc(sent, func(id string) bool { return slices.Contains(unread, id) }) {
			missed = append(missed, r)
		}
	}
	return missed
}

// occupant is the live agent in block, if any.
func occupant(recs []store.Record, block string) *core.Peer {
	for _, r := range recs {
		if r.Live() && r.BlockID == block {
			p := peerOf(r)
			return &p
		}
	}
	return nil
}

// sessionMates are the live agents recorded in rec's session, other than
// rec's.
func sessionMates(recs []store.Record, rec store.Record) []core.Peer {
	var mates []core.Peer
	for _, r := range recs {
		if r.Live() && r.Handle != rec.Handle && together(r, rec) {
			mates = append(mates, peerOf(r))
		}
	}
	return mates
}
