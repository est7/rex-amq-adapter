package core

import (
	"slices"
	"testing"
	"time"
)

// A narrator and a personal agent are nobody's mates, never hear of a
// departure, and never stand in to report a stopped goal; their own
// departure is nobody's mail.
func TestNarratorStealthInSessionPolicy(t *testing.T) {
	windowOf := map[string]string{"b:o": "w", "b:i": "w", "b:n": "w2", "b:p": "w"}
	orch := Peer{Handle: "claude-01", Kind: Claude, Role: OrchestratorRole, Block: "b:o"}
	impl := Peer{Handle: "claude-02", Kind: Claude, Role: "implementer", Block: "b:i"}
	narr := Peer{Handle: "pi-03", Kind: Pi, Role: NarratorRole, Block: "b:n"}
	pers := Peer{Handle: "codex-04", Kind: Codex, Role: PersonalRole, Block: "b:p"}
	mates := SessionMates("b:i", windowOf, []Peer{orch, narr, pers})
	if len(mates) != 1 || mates[0].Handle != orch.Handle {
		t.Fatalf("mates of a worker: %+v", mates)
	}
	if !Stealth(NarratorRole) || !Stealth(PersonalRole) || Stealth(OrchestratorRole) || Stealth("") {
		t.Fatal("Stealth covers exactly the narrator and the personal agent")
	}

	// The narrator leaves: the orchestrator hears nothing.
	if to, _, _ := DepartureMail(narr, "sess", []Peer{orch, impl}); len(to) != 0 {
		t.Fatalf("a narrator's departure was mailed to %v", to)
	}
	// The orchestrator leaves with only a narrator (which notifies) and a
	// worker beside it: the worker reports, never the narrator.
	narr.Kind = Claude
	to, _, body := DepartureMail(orch, "", []Peer{narr, impl})
	if !slices.Equal(to, []Handle{impl.Handle}) || body == "" {
		t.Fatalf("reporting peer: %v", to)
	}
	// With only the narrator left, nobody is mailed.
	if to, _, _ := DepartureMail(orch, "", []Peer{narr, pers}); len(to) != 0 {
		t.Fatalf("the narrator was asked to report a stopped goal: %v", to)
	}
	// A worker leaves beside an orchestrator and a narrator: only the
	// orchestrator hears.
	if to, _, _ := DepartureMail(impl, "", []Peer{narr, orch}); !slices.Equal(to, []Handle{orch.Handle}) {
		t.Fatalf("departure mail went to %v", to)
	}
}

// A session's pulse is order-independent; a departure or new attention is
// urgent, a change of task normal, mail alone low, and no change is nothing.
func TestSessionWatcherPulse(t *testing.T) {
	a := PulseMember{Handle: "claude-01", Role: OrchestratorRole, Live: true, Task: "plan", LastMail: "m1"}
	b := PulseMember{Handle: "codex-02", Role: "reviewer", Live: true, LastMail: "m0", Attention: []string{"y@2", "x@1"}}
	p := SessionPulse{Members: []PulseMember{a, b}}
	q := SessionPulse{Members: []PulseMember{{Handle: b.Handle, Role: b.Role, Live: true, LastMail: "m0", Attention: []string{"x@1", "y@2"}}, a}}
	if p.Digest() != q.Digest() || ComparePulse(p, q).Priority != "" {
		t.Fatal("the pulse depends on order")
	}
	mail := a
	mail.LastMail = "m2"
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{mail, b}}); c.Priority != PriorityLow {
		t.Fatalf("mail: %+v", c)
	}
	task := a
	task.Task, task.LastMail = "build", "m2"
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{task, b}}); c.Priority != PriorityNormal {
		t.Fatalf("task: %+v", c)
	}
	titled := a
	titled.Task = "hand-back: login ships"
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{titled, b}}); c.Priority != PriorityNormal {
		t.Fatalf("a task that reads like a hand-back is routine: %+v", c)
	}
	gone := a
	gone.Live = false
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{gone, b}}); c.Priority != PriorityUrgent {
		t.Fatalf("departure: %+v", c)
	}
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{b}}); c.Priority != PriorityUrgent {
		t.Fatalf("a member collected away: %+v", c)
	}
	stuck := b
	stuck.Attention = append(slices.Clone(b.Attention), "turn_error@3")
	if c := ComparePulse(p, SessionPulse{Members: []PulseMember{a, stuck}}); c.Priority != PriorityUrgent || len(c.Lines) != 1 || c.Lines[0] != "codex-02: turn error" {
		t.Fatalf("attention: %+v", c)
	}
}

// The hook hold is amq's: arrival plus the priority's hold, urgent and
// future-dated mail at once, and a cohort due at its earliest message.
func TestNarratorHoldUntil(t *testing.T) {
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, c := range []struct {
		name  string
		fresh []Waiting
		held  bool
		until time.Time
	}{
		{"nothing waits", nil, false, time.Time{}},
		{"normal inside its hold", []Waiting{{Priority: PriorityNormal, Arrived: ago(30 * time.Second)}}, true, now.Add(90 * time.Second)},
		{"missing priority is normal", []Waiting{{Arrived: ago(30 * time.Second)}}, true, now.Add(90 * time.Second)},
		{"normal past its hold", []Waiting{{Priority: PriorityNormal, Arrived: ago(3 * time.Minute)}}, false, now.Add(-time.Minute)},
		{"low holds longer", []Waiting{{Priority: PriorityLow, Arrived: ago(3 * time.Minute)}}, true, now.Add(7 * time.Minute)},
		{"urgent skips the hold", []Waiting{{Priority: PriorityLow, Arrived: ago(time.Second)}, {Priority: PriorityUrgent, Arrived: ago(time.Second)}}, false, now},
		{"future-dated mail is not held", []Waiting{{Priority: PriorityNormal, Arrived: now.Add(time.Hour)}}, false, now},
		{"the earliest deadline rules", []Waiting{{Priority: PriorityLow, Arrived: ago(time.Minute)}, {Priority: PriorityNormal, Arrived: ago(time.Minute)}}, true, now.Add(time.Minute)},
	} {
		until, held := HoldUntil(c.fresh, 2*time.Minute, 10*time.Minute, now)
		if held != c.held || !until.Equal(c.until) {
			t.Errorf("%s: held %v until %v, want %v until %v", c.name, held, until, c.held, c.until)
		}
	}
	if _, held := HoldUntil([]Waiting{{Priority: PriorityNormal, Arrived: now}}, 0, 0, now); held {
		t.Error("no hold configured holds nothing")
	}
}

// A recorded hand-back marks its author in the pulse: urgent each time a new
// one is stored, whatever the author's task says, and kept by Normalize so
// the digest sees it.
func TestPulseHandBackMarker(t *testing.T) {
	a := PulseMember{Handle: "claude-01", Role: OrchestratorRole, Live: true, Task: "plan"}
	p := SessionPulse{Members: []PulseMember{a}}
	first := a
	first.HandBack = "2026-11-15T10:00:00Z"
	q := SessionPulse{Members: []PulseMember{first}}
	if p.Digest() == q.Digest() {
		t.Fatal("the digest does not see the hand-back")
	}
	if c := ComparePulse(p, q); c.Priority != PriorityUrgent || len(c.Lines) != 1 || c.Lines[0] != "claude-01 handed back: its report is in your view (hand_back)" {
		t.Fatalf("a recorded hand-back: %+v", c)
	}
	if c := ComparePulse(q, q); c.Priority != "" {
		t.Fatalf("the same hand-back again: %+v", c)
	}
	second := first
	second.HandBack = "2026-11-15T11:00:00Z"
	if c := ComparePulse(q, SessionPulse{Members: []PulseMember{second}}); c.Priority != PriorityUrgent {
		t.Fatalf("a second hand-back of the Epic: %+v", c)
	}
	gone := first
	gone.Live = false
	if c := ComparePulse(q, SessionPulse{Members: []PulseMember{gone}}); c.Priority != PriorityUrgent || len(c.Lines) != 1 || c.Lines[0] != "claude-01 (orchestrator) left" {
		t.Fatalf("a departure after the hand-back: %+v", c)
	}
}
