package amq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Two read-only amq commands serve an observer. Neither moves a message,
// writes a receipt, or wakes anyone: `thread open` scans envelopes for open
// obligations (no body is read), and `thread --message-ids` reads exactly
// the named messages, body included, from every copy the named agents hold
// (inbox new and cur, outbox sent), so a message is found whether or not it
// was drained or its sender crashed after delivering.

// Party is one side of an obligation.
type Party struct {
	Handle string `json:"handle"`
}

// Opener is the message that opened an obligation.
type Opener struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	From     string    `json:"from"`
	To       []string  `json:"to"`
	Subject  string    `json:"subject"`
	Created  time.Time `json:"created"`
	Priority string    `json:"priority"`
	Labels   []string  `json:"labels"`
}

// Obligation is a request, question, or todo still waiting for its reply.
type Obligation struct {
	Key        string `json:"key"`
	Thread     string `json:"thread"`
	Opener     Opener `json:"opener"`
	OwedBy     Party  `json:"owed_by"`
	OwedTo     Party  `json:"owed_to"`
	State      string `json:"state"`
	AgeSeconds int64  `json:"age_seconds"`
}

// OpenObligations is `amq thread open --json`, with what it left out.
type OpenObligations struct {
	Obligations []Obligation `json:"obligations"`
	Coverage    struct {
		MaxAgeSeconds    int64 `json:"max_age_seconds"`
		TruncatedByLimit struct {
			Limit   int `json:"limit"`
			Omitted int `json:"omitted"`
		} `json:"truncated_by_limit"`
		Skipped json.RawMessage `json:"skipped"`
		Expired json.RawMessage `json:"expired"`
	} `json:"coverage"`
}

// ThreadOpen lists the open obligations owed by any of handles and those owed
// to any of them (amq ANDs the two filters, so they are two scans), each
// once. me is the reader amq runs as; it reads no mailbox as me.
func ThreadOpen(ctx context.Context, amqBin, root, me string, handles []string) (OpenObligations, error) {
	var all OpenObligations
	if len(handles) == 0 {
		return all, nil
	}
	seen := map[string]bool{}
	for _, filter := range []string{"--owed-by", "--owed-to"} {
		out, err := runAmq(ctx, amqBin, "thread", "open", "--root", root, "--me", me, filter, strings.Join(handles, ","), "--json")
		if err != nil {
			return all, fmt.Errorf("amq thread open %s: %w", filter, err)
		}
		var o OpenObligations
		if err := json.Unmarshal(out, &o); err != nil {
			return all, fmt.Errorf("decode amq thread open: %w", err)
		}
		for _, ob := range o.Obligations {
			if !seen[ob.Key] {
				seen[ob.Key] = true
				all.Obligations = append(all.Obligations, ob)
			}
		}
		all.Coverage.MaxAgeSeconds = o.Coverage.MaxAgeSeconds
		all.Coverage.TruncatedByLimit.Omitted += o.Coverage.TruncatedByLimit.Omitted
		if len(o.Coverage.Skipped) > 2 {
			all.Coverage.Skipped = o.Coverage.Skipped
		}
		if len(o.Coverage.Expired) > 2 {
			all.Coverage.Expired = o.Coverage.Expired
		}
	}
	return all, nil
}

// ThreadMessage is one message as `amq thread --include-body --json` prints it.
type ThreadMessage struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	To       []string  `json:"to"`
	Thread   string    `json:"thread"`
	Subject  string    `json:"subject"`
	Created  time.Time `json:"created"`
	Body     string    `json:"body"`
	Priority string    `json:"priority,omitempty"`
	Kind     string    `json:"kind,omitempty"`
}

// ThreadRead reads exactly ids, all of thread, from the copies agents hold.
// amq fails the whole read when one id is not found.
func ThreadRead(ctx context.Context, amqBin, root, me, thread string, agents, ids []string) ([]ThreadMessage, error) {
	out, err := runAmq(ctx, amqBin, "thread", "--root", root, "--me", me, "--id", thread,
		"--agents", strings.Join(agents, ","), "--message-ids", strings.Join(ids, ","), "--include-body", "--json")
	if err != nil {
		return nil, fmt.Errorf("amq thread %s: %w", thread, err)
	}
	var msgs []ThreadMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		return nil, fmt.Errorf("decode amq thread: %w", err)
	}
	return msgs, nil
}
