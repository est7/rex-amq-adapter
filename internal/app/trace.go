package app

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/amq"
)

// traceScope decides which of amq trace's evidence a managed trace shows.
// amq trace reads the whole root; an item of its evidence (any object in a
// list, under legs or follow) is shown only when it is scoped to the caller:
//
//   - every message it names (a copy, a reference, a reply, a receipt) is
//     the traced message, which the caller may read, or one the caller may
//     read in its own mailbox (exposed: provenance and its placement now);
//   - it lies in the caller's own mailbox, or in the mailbox of the sender
//     or a recipient of every message it names: a copy of mail the caller
//     may read, nobody else's.
//
// An item that names no mailbox and no message cannot be scoped and is left
// out: the trace fails closed.
type traceScope struct {
	m      managed
	traced located
}

// withheld replaces trace text that the scope cannot vouch for.
const withheld = "withheld: a managed trace shows only evidence scoped to you"

// traceLabels are the fields of trace structure (outside an evidence item)
// that hold a fixed vocabulary or the traced message itself, never text
// about other mail. Every other string there is diagnostic text from amq's
// scan of the whole root (a parse error naming another Epic's file) and is
// withheld.
var traceLabels = map[string]bool{"schema": true, "message_id": true, "status": true, "state": true, "root": true, "root_identity": true, "scope": true}

func (s traceScope) filter(v any) any { return s.walk(v, false) }

// walk scopes v: an evidence item (an object in a list) is kept or dropped
// whole by shows; structure outside items keeps only its labels.
func (s traceScope) walk(v any, inItem bool) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if str, ok := x.(string); ok && !inItem && !traceLabels[k] && str != "" {
				v[k] = withheld
				continue
			}
			v[k] = s.walk(x, inItem)
		}
		return v
	case []any:
		out := []any{}
		for _, x := range v {
			if obj, ok := x.(map[string]any); ok {
				if !s.shows(obj) {
					continue
				}
				x = s.walk(obj, true)
			} else if _, ok := x.(string); ok && !inItem {
				continue // a list of diagnostic text
			}
			out = append(out, x)
		}
		return out
	}
	return v
}

// shows applies the scope to one evidence item.
func (s traceScope) shows(obj map[string]any) bool {
	box, ids := evidenceOf(obj)
	if box == "" && len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		h, ok := s.readable(id)
		if !ok {
			return false
		}
		if box != "" && box != s.m.me() && box != h.From && !slices.Contains(h.To, box) {
			return false // in a mailbox that is not one of this message's
		}
	}
	return box == "" || box == s.m.me() || len(ids) > 0
}

// readable returns the header of message id when the caller may read it:
// the traced message, or one in the caller's own mailbox the gateway shows
// it (exposed: provenance and its placement now).
func (s traceScope) readable(id string) (amq.Header, bool) {
	if id == s.traced.id || id == s.traced.header.ID {
		return s.traced.header, true
	}
	msg, ok, err := s.m.e.findMessage(s.m.rec.Handle, id)
	if err != nil || !ok || s.m.e.exposed(s.m.rec.Handle, msg) != nil {
		return amq.Header{}, false
	}
	return msg.header, true
}

// evidenceOf names the mailbox an evidence item lies in and the messages it
// names. The keys are amq trace's (schema amq/trace/v1): agent and path
// ("agents/<h>/..."), follow's handle, a message's or reply's id, a
// relation's message_id, a receipt's msg_id.
func evidenceOf(obj map[string]any) (box string, ids []string) {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	box = str(obj, "agent")
	if box == "" {
		box = str(obj, "handle")
	}
	if p := str(obj, "path"); strings.HasPrefix(p, "agents/") {
		parts := strings.Split(p, "/")
		if box == "" && len(parts) > 1 {
			box = parts[1]
		}
		if len(parts) > 3 {
			switch parts[2] {
			case "inbox", "outbox", "dlq", "receipts":
				id, _, _ := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(parts[len(parts)-1], ".md"), ".json"), "__")
				ids = append(ids, id)
			}
		}
	}
	if id := str(obj, "id"); id != "" {
		ids = append(ids, id)
	}
	for _, k := range []string{"message", "relation", "receipt"} {
		if sub, ok := obj[k].(map[string]any); ok {
			for _, ik := range []string{"id", "message_id", "msg_id"} {
				if id := str(sub, ik); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return box, ids
}

// renderTrace writes a scoped trace for a reader: per leg, its status and
// the evidence left, by path or relation.
func renderTrace(w io.Writer, doc any) error {
	d, _ := doc.(map[string]any)
	fmt.Fprintf(w, "Trace %v: %v\n", d["message_id"], d["status"])
	legs, _ := d["legs"].(map[string]any)
	names := make([]string, 0, len(legs))
	for k := range legs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		leg, _ := legs[k].(map[string]any)
		ev, _ := leg["evidence"].([]any)
		fmt.Fprintf(w, "  %-12s %v (%d)\n", k, leg["status"], len(ev))
		for _, x := range ev {
			obj, _ := x.(map[string]any)
			line, _ := obj["path"].(string)
			if rel, ok := obj["relation"].(map[string]any); ok {
				line = fmt.Sprintf("%v %v", rel["relation"], rel["message_id"])
			}
			fmt.Fprintf(w, "    - %s\n", line)
		}
	}
	if f, ok := d["follow"].(map[string]any); ok {
		rs, _ := f["recipients"].([]any)
		for _, x := range rs {
			r, _ := x.(map[string]any)
			fmt.Fprintf(w, "  follow       %v: delivery %v, receipt %v\n", r["handle"], r["delivery"], r["receipt"])
		}
	}
	return nil
}
