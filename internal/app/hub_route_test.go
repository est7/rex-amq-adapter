package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// managedSend runs `mail send`/`mail reply` as the agent in block, with
// --json, and decodes what it printed.
func managedSend(t *testing.T, e Env, block, verb string, args ...string) (map[string]any, error) {
	t.Helper()
	var out, errOut strings.Builder
	_, err := e.ManagedMail(context.Background(), Location{Block: block}, verb, append(args, "--json"), nil, &out, &errOut)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if jerr := json.Unmarshal([]byte(out.String()), &v); jerr != nil {
		t.Fatalf("%s output %q: %v", verb, out.String(), jerr)
	}
	return v, nil
}

// routeWorld is a Hub with personal agents claude-01, claude-02, and
// claude-03 in sessions of their own, and narrator agy-05 of session:e,
// linked to the Hub with claude-02 preferred.
func routeWorld(t *testing.T) Env {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{})
	key := core.NewHubKey(fakeServer, "/hub")
	old := time.Now().Add(-time.Hour)
	for _, h := range []core.Handle{"claude-01", "claude-02", "claude-03"} {
		session := "session:" + string(h)
		put(t, e, seed{Handle: h, Kind: core.Claude, Role: core.PersonalRole, SessionID: session, AdoptedAt: old})
		putHub(t, e, session, key)
	}
	put(t, e, seed{Handle: "agy-05", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:e", AdoptedAt: old})
	for _, h := range []string{"claude-01", "claude-02", "claude-03", "agy-05"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:claude-02"}, PreferredPersonal: "claude-02", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:e"}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// A narrator mails its Hub's personal agent as `--to personal`: the
// preferred one while it is live and reachable, else the one the user spoke
// to last, else the preferred one's mailbox. A literal handle of one that
// departed takes the same path, and a reply to a departed one's message
// becomes a send to the live one on the reply's thread, subject, and kind,
// naming the original among its refs; the result names routed_to and
// rerouted_from, and the original's provenance is never rewritten.
func TestNarratorRoutesToLivePersonal(t *testing.T) {
	e := routeWorld(t)
	sent, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--kind", "status", "--subject", "progress", "--body", "half way")
	if err != nil || sent["routed_to"] != "claude-02" || sent["rerouted_from"] != nil {
		t.Fatalf("to the live preferred one: %v %v", sent, err)
	}
	q, err := managedSend(t, e, "block:claude-02", "send", "--to", "agy-05", "--kind", "question", "--subject", "status?", "--body", "how is it")
	if err != nil {
		t.Fatal(err)
	}
	origID := q["id"].(string)
	origProv, ok, err := e.Store.ProvenanceOf(origID, "", "claude-02", []string{"agy-05"})
	if err != nil || !ok {
		t.Fatalf("the question's provenance: %v %v", ok, err)
	}
	leave(t, e, "claude-02")
	e.touchActivity(recordNamed(t, e, "claude-03"))
	for _, to := range []string{"personal", "claude-02"} {
		sent, err := managedSend(t, e, "block:agy-05", "send", "--to", to, "--kind", "status", "--subject", "s", "--body", "b")
		if err != nil || sent["routed_to"] != "claude-03" || sent["rerouted_from"] != "claude-02" {
			t.Errorf("--to %s with the preferred one gone: %v %v", to, sent, err)
		}
	}
	rep, err := managedSend(t, e, "block:agy-05", "reply", "--id", origID, "--body", "two of three Slices are in")
	if err != nil || rep["routed_to"] != "claude-03" || rep["rerouted_from"] != "claude-02" {
		t.Fatalf("a reply to the departed one: %v %v", rep, err)
	}
	newID := rep["id"].(string)
	hdr, err := amq.ReadHeader(e.Paths.Root(), "claude-03", amq.BoxUnread, newID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), "claude-03"), newID+".md"))
	if hdr.From != "agy-05" || hdr.Subject != "Re: status?" || hdr.Kind != "question" || hdr.Thread != "p2p/agy-05__claude-02" || !strings.Contains(string(raw), origID) {
		t.Errorf("the converted reply: %+v\n%s", hdr, raw)
	}
	if kept, err := e.admit(recordNamed(t, e, "claude-03")); err != nil || len(kept) != 3 {
		t.Errorf("claude-03 admitted %v %v", kept, err)
	}
	if again, _, _ := e.Store.ProvenanceOf(origID, "", "claude-02", []string{"agy-05"}); again.From != origProv.From || len(again.To) != 1 || again.To[0].Handle != "agy-05" {
		t.Errorf("the original's provenance changed: %+v", again)
	}
	// No live personal agent: the preferred one's mailbox keeps it.
	leave(t, e, "claude-01")
	leave(t, e, "claude-03")
	if sent, err := managedSend(t, e, "block:agy-05", "send", "--to", "personal", "--subject", "s", "--body", "b"); err != nil || sent["routed_to"] != "claude-02" {
		t.Errorf("no live personal agent: %v %v", sent, err)
	}
}

// A personal agent that departs (or is collected) leaves its linked
// narrators' unread mail to the Hub's most recent personal agent, each
// message once, as a new message naming the original (rerouted_from); with
// no personal agent of the Hub live, it waits, and the next one adopted
// there gets it, with its start notice naming the pending hand-back.
func TestDepartedPersonalRedelivery(t *testing.T) {
	e, term, hubDir := hubWorld(t)
	key := core.NewHubKey(fakeServer, hubDir)
	hookAs(t, e, term, "block:a", "s1", "", HookInput{})
	p1 := recordOf(t, e, handleIn(t, e, "block:a"))
	put(t, e, seed{Handle: "claude-02", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:p2", SessionID: "session:p2", AgentPID: os.Getpid(), AdoptedAt: time.Now()})
	putHub(t, e, "session:p2", key)
	put(t, e, seed{Handle: "agy-05", Kind: core.Agy, Role: core.NarratorRole, BlockID: "block:n", SessionID: "session:e", AdoptedAt: time.Now()})
	for _, h := range []string{"claude-02", "agy-05"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p2"}, PreferredPersonal: "claude-02", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:e"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: "session:e", Server: fakeServer, Hub: key, State: store.EpicHandBack}); err != nil {
		t.Fatal(err)
	}
	sent, err := managedSend(t, e, "block:n", "send", "--to", "personal", "--kind", "status", "--subject", "hand-back ready", "--body", "the orchestrator handed back")
	if err != nil || sent["routed_to"] != "claude-02" {
		t.Fatalf("to the preferred one: %v %v", sent, err)
	}
	orig := sent["id"].(string)
	// claude-02's pane is gone: collection parks it, and its unread mail
	// goes to p1.
	term.mu.Lock()
	term.closed = append(term.closed, "block:p2")
	term.mu.Unlock()
	_ = e.Collect(context.Background())
	if recordNamed(t, e, "claude-02").Live() {
		t.Fatal("collection left claude-02 live")
	}
	r, ok, err := e.Store.Redelivered(orig)
	if err != nil || !ok || r.To != p1.Handle || r.Left != "claude-02" {
		t.Fatalf("redelivery %+v %v %v", r, ok, err)
	}
	hdr, err := amq.ReadHeader(e.Paths.Root(), string(p1.Handle), amq.BoxUnread, r.NewID)
	var from string
	if err != nil || hdr.From != "agy-05" || hdr.Subject != "hand-back ready" || !hdr.ContextOf("rerouted_from", &from) || from != orig {
		t.Fatalf("redelivered message %+v %v", hdr, err)
	}
	if kept, err := e.admit(recordNamed(t, e, p1.Handle)); err != nil || len(kept) != 1 {
		t.Fatalf("p1 admitted %v %v", kept, err)
	}
	// Once: finishing the departure again delivers nothing more.
	if err := e.redeliverFrom(context.Background(), recordNamed(t, e, "claude-02")); err != nil {
		t.Fatal(err)
	}
	if ids, _ := amq.Unread(e.Paths.Root(), string(p1.Handle)); len(ids) != 1 {
		t.Fatalf("p1 holds %v after a second try", ids)
	}
	// p1 departs too, with the copy unread and no personal agent left: it
	// waits; the next personal agent of the Hub gets it.
	if err := e.Depart(context.Background(), Location{Block: "block:a"}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if recordNamed(t, e, p1.Handle).Live() {
		t.Fatal("p1 did not depart")
	}
	if _, ok, _ := e.Store.Redelivered(r.NewID); ok {
		t.Fatal("redelivered with no personal agent live")
	}
	n := hookAs(t, e, term, "block:c", "c1", "", HookInput{})
	p3 := recordOf(t, e, handleIn(t, e, "block:c"))
	if p3.Role != core.PersonalRole {
		t.Fatalf("block:c adopted as %q", p3.Role)
	}
	if r2, ok, _ := e.Store.Redelivered(r.NewID); !ok || r2.To != p3.Handle {
		t.Fatalf("the pending mail on the next adoption: %+v %v", r2, ok)
	}
	if !strings.Contains(n.Notice, "1 linked Epic has a pending hand-back: `rex-amq-adapter epic list`") {
		t.Errorf("start notice lacks the obligation line:\n%s", n.Notice)
	}
}

// hub notify rings one live personal agent of the Hub: the one the user
// spoke to last (ties to the first handle), the one --personal names, and
// on a replay the one it rang first; with none live it says so, starting
// nothing.
func TestHubNotifyMostRecent(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, term := notifyWorld(t, root, root, root)
	now := time.Now()
	e.Now = func() time.Time { return now }
	key := core.NewHubKey(fakeServer, root)
	for _, h := range []string{"claude-01", "codex-02", "codex-03"} {
		putHub(t, e, "session:"+h, key)
		s := seedOf(recordNamed(t, e, core.Handle(h)))
		s.AdoptedAt = now.Add(-time.Hour)
		put(t, e, s)
	}
	if res, err := notify(t, e, NotifyRequest{Ref: "inbox:1", EventID: "ev-tie", Hub: root}); err != nil || res.To != "claude-01" {
		t.Fatalf("a tie: %+v %v", res, err)
	}
	e.touchActivity(recordNamed(t, e, "codex-03"))
	for _, c := range []struct {
		r    NotifyRequest
		want core.Handle
	}{
		{NotifyRequest{Ref: "inbox:2", EventID: "ev-2"}, "codex-03"},
		{NotifyRequest{Ref: "inbox:3", EventID: "ev-3", Hub: root}, "codex-03"},
		{NotifyRequest{Ref: "inbox:4", EventID: "ev-4", Personal: "codex-02"}, "codex-02"},
	} {
		if res, err := notify(t, e, c.r); err != nil || res.Status != NotifyNotified || res.To != c.want {
			t.Errorf("%+v: %+v %v", c.r, res, err)
		}
	}
	leave(t, e, "codex-03")
	if res, err := notify(t, e, NotifyRequest{Ref: "inbox:2", EventID: "ev-2"}); err != nil || !res.Deduped || res.To != "codex-03" {
		t.Errorf("a replay: %+v %v", res, err)
	}
	leave(t, e, "claude-01")
	leave(t, e, "codex-02")
	if res, err := notify(t, e, NotifyRequest{Ref: "inbox:5", EventID: "ev-5", Hub: root}); err != nil || res.Status != NotifyNoLivePersonal {
		t.Errorf("none live: %+v %v", res, err)
	}
	noLaunches(t, term)
}
