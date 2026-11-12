package core

import "testing"

func TestCanExchangeMatrix(t *testing.T) {
	const srv = "unix:///rex.sock"
	at := func(h Handle, role, session string) Endpoint {
		return Endpoint{Handle: h, Role: role, Server: srv, SessionID: session}
	}
	personalS, epicA, epicB := "session:personal", "session:a", "session:b"
	me := at("claude-01", PersonalRole, personalS)
	narrA := at("agy-02", NarratorRole, epicA)
	orchA := at("claude-03", OrchestratorRole, epicA)
	implA := at("claude-04", "implementer", epicA)
	revA := at("codex-05", "reviewer", epicA)
	plainA := at("pi-06", "", epicA)
	narrB := at("agy-07", NarratorRole, epicB)
	orchB := at("claude-08", OrchestratorRole, epicB)
	linkA := []Link{{Personal: me.Session(), Epic: narrA.Session()}}
	send := Operation{Kind: OpSend}
	control := Operation{Kind: OpControl, ControlRequestID: "req-1"}

	for _, c := range []struct {
		name     string
		from, to Endpoint
		op       Operation
		links    []Link
		allow    bool
		reason   string
	}{
		{"personal to linked narrator", me, narrA, send, linkA, true, ReasonLinked},
		{"linked narrator replies to personal", narrA, me, Operation{Kind: OpReply}, linkA, true, ReasonLinked},
		{"personal to unlinked narrator", me, narrB, send, linkA, false, ReasonNotLinked},
		{"personal to narrator with no links", me, narrA, send, nil, false, ReasonNotLinked},
		{"personal to linked epic's orchestrator", me, orchA, send, linkA, false, ReasonPersonalOnly},
		{"personal to linked epic's worker", me, implA, send, linkA, false, ReasonPersonalOnly},
		{"orchestrator to personal", orchA, me, send, linkA, false, ReasonPersonalOnly},
		{"worker to personal", implA, me, send, linkA, false, ReasonPersonalOnly},
		{"personal control to orchestrator", me, orchA, control, linkA, false, ReasonPersonalOnly},
		{"cross-epic orchestrators", orchA, orchB, send, linkA, false, ReasonCrossEpic},
		{"cross-epic narrators", narrA, narrB, send, linkA, false, ReasonCrossEpic},
		{"cross-epic narrator to orchestrator", narrA, orchB, control, linkA, false, ReasonCrossEpic},
		{"cross-epic system mail", orchA, narrB, Operation{Kind: OpSend, System: true}, linkA, false, ReasonCrossEpic},
		{"narrator control to its orchestrator", narrA, orchA, control, nil, true, ReasonControl},
		{"narrator routine mail to its orchestrator", narrA, orchA, send, nil, false, ReasonNarratorRoutine},
		{"narrator control without a request id", narrA, orchA, Operation{Kind: OpControl}, nil, false, ReasonNarratorRoutine},
		{"narrator reply to its orchestrator", narrA, orchA, Operation{Kind: OpReply}, nil, false, ReasonNarratorRoutine},
		{"narrator to implementer", narrA, implA, send, nil, false, ReasonNarratorWorker},
		{"narrator control to reviewer", narrA, revA, control, nil, false, ReasonNarratorWorker},
		{"orchestrator to narrator", orchA, narrA, send, nil, false, ReasonNarratorObserves},
		{"worker to narrator", implA, narrA, send, nil, false, ReasonNarratorObserves},
		{"worker control to narrator", implA, narrA, control, nil, false, ReasonNarratorObserves},
		{"adapter system mail to narrator", orchA, narrA, Operation{Kind: OpSend, System: true}, nil, true, ReasonSystem},
		{"orchestrator to implementer", orchA, implA, send, nil, true, ReasonSameEpic},
		{"implementer to reviewer", implA, revA, send, nil, true, ReasonSameEpic},
		{"reviewer replies to implementer", revA, implA, Operation{Kind: OpReply}, nil, true, ReasonSameEpic},
		{"roleless peers", plainA, implA, send, nil, true, ReasonSameEpic},
		{"self", implA, implA, send, nil, true, ReasonSelf},
		{"unplaced sender", Endpoint{Handle: "alice"}, implA, send, nil, false, ReasonUnplaced},
		{"sender named system", Endpoint{Handle: "system", Role: OrchestratorRole}, narrA, Operation{Kind: OpSend}, nil, false, ReasonUnplaced},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := CanExchange(c.from, c.to, c.op, c.links)
			if d.Allow != c.allow || d.Reason != c.reason {
				t.Errorf("CanExchange = %+v, want allow=%v reason=%s", d, c.allow, c.reason)
			}
		})
	}
}

// A message already sent is seen only by the recipient where it was when
// the message was sent: not after its handle moved to another session, and
// not after it came back (a new generation).
func TestCanExchangeFencesPriorPlacements(t *testing.T) {
	const srv = "unix:///rex.sock"
	sender := Endpoint{Handle: "claude-01", Server: srv, SessionID: "session:a"}
	then := Endpoint{Handle: "codex-02", Server: srv, SessionID: "session:a", Generation: 1}
	sent := Operation{Kind: OpSend, Addressed: &then}
	for _, c := range []struct {
		name   string
		now    Endpoint
		allow  bool
		reason string
	}{
		{"unmoved", then, true, ReasonSameEpic},
		{"moved to another session", Endpoint{Handle: "codex-02", Server: srv, SessionID: "session:b", Generation: 2}, false, ReasonFenced},
		{"came back to the session", Endpoint{Handle: "codex-02", Server: srv, SessionID: "session:a", Generation: 3}, false, ReasonFenced},
		{"on another server", Endpoint{Handle: "codex-02", Server: "unix:///other.sock", SessionID: "session:a", Generation: 1}, false, ReasonFenced},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := CanExchange(sender, c.now, sent, nil)
			if d.Allow != c.allow || d.Reason != c.reason {
				t.Errorf("CanExchange = %+v, want allow=%v reason=%s", d, c.allow, c.reason)
			}
		})
	}
	// The sender's own copy is never fenced by where a recipient went.
	if d := CanExchange(sender, sender, Operation{Kind: OpSend}, nil); !d.Allow {
		t.Errorf("own copy: %+v", d)
	}
}

// Linking takes effect on the next decision and unlinking revokes it: the
// policy reads the grants it is given and keeps none.
func TestCanExchangeFollowsLinks(t *testing.T) {
	const srv = "unix:///rex.sock"
	me := Endpoint{Handle: "claude-01", Role: PersonalRole, Server: srv, SessionID: "session:p"}
	narr := Endpoint{Handle: "agy-02", Role: NarratorRole, Server: srv, SessionID: "session:a"}
	other := Link{Personal: me.Session(), Epic: SessionKey{Server: srv, SessionID: "session:z"}}
	var links []Link
	if CanExchange(me, narr, Operation{Kind: OpSend}, links).Allow {
		t.Fatal("allowed before link")
	}
	links = append(links, other, Link{Personal: me.Session(), Epic: narr.Session()})
	if d := CanExchange(me, narr, Operation{Kind: OpSend}, links); !d.Allow {
		t.Fatalf("after link: %+v", d)
	}
	links = links[:1]
	if CanExchange(narr, me, Operation{Kind: OpReply}, links).Allow {
		t.Fatal("allowed after unlink")
	}
	// A link is to the Epic on its server: the same session id elsewhere is
	// another Epic.
	elsewhere := narr
	elsewhere.Server = "unix:///other.sock"
	if CanExchange(me, elsewhere, Operation{Kind: OpSend}, []Link{{Personal: me.Session(), Epic: narr.Session()}}).Allow {
		t.Fatal("link matched another server")
	}
}
