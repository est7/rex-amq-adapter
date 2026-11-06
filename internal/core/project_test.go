package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func agentObs(h Handle) AgentObservation {
	return AgentObservation{Handle: h, Kind: Claude, Server: "unix:///s", SessionID: "session:1", WindowID: "window:1",
		Tab: "Work", BlockID: "block:" + string(h), Placed: true, AdoptedAt: t0}
}

func TestProjectedLifecycle(t *testing.T) {
	gone := t0.Add(time.Minute)
	for _, c := range []struct {
		name string
		edit func(*AgentObservation)
		want string
	}{
		{"live", func(*AgentObservation) {}, LifeLive},
		{"adopting", func(o *AgentObservation) { o.Adopting = true }, LifeAdopting},
		{"departing", func(o *AgentObservation) { o.DepartedAt, o.Departing = &gone, true }, LifeDeparting},
		{"departed", func(o *AgentObservation) { o.DepartedAt, o.DepartReason = &gone, "session_end" }, LifeDeparted},
	} {
		o := agentObs("claude-01")
		o.Role = "reviewer"
		c.edit(&o)
		a := Project(Observations{Agents: []AgentObservation{o}}).Agents[0]
		if a.Lifecycle.State != c.want {
			t.Errorf("%s: state %q, want %q", c.name, a.Lifecycle.State, c.want)
		}
		if a.Role != "reviewer" || a.Pane != "present" {
			t.Errorf("%s: role %q pane %q", c.name, a.Role, a.Pane)
		}
	}
	o := agentObs("claude-01")
	o.DepartedAt, o.DepartReason, o.Placed = &gone, "block_gone", false
	a := Project(Observations{Agents: []AgentObservation{o}}).Agents[0]
	if a.Lifecycle.DepartReason != "block_gone" || a.Pane != "gone" || a.Activity.State != "absent" || a.Activity.Source != "none" {
		t.Errorf("departed agent: %+v", a)
	}
}

func TestProjectedActivity(t *testing.T) {
	for _, c := range []struct {
		obs        *ActivityObservation
		state, kin string
	}{
		{nil, "absent", ""},
		{&ActivityObservation{State: StateWorking, Source: "rex_status", At: t0}, "working", ""},
		{&ActivityObservation{State: StateBlocked, Kind: "permission", Source: "rex_status", At: t0}, "blocked", "permission"},
		{&ActivityObservation{State: StateAbsent, Source: "rex_status", At: t0}, "absent", ""},
		{&ActivityObservation{Err: true, Source: "rex_status", At: t0}, "absent", "observe_failed"},
		{&ActivityObservation{State: "thinking", Source: "rex_status", At: t0}, "error", "unknown_state:thinking"},
		{&ActivityObservation{State: StateIdle, Source: "supervisor", At: t0}, "idle", ""},
	} {
		o := agentObs("claude-01")
		o.Activity = c.obs
		a := Project(Observations{Agents: []AgentObservation{o}}).Agents[0].Activity
		if a.State != c.state || a.Kind != c.kin {
			t.Errorf("%+v: got %+v, want %s/%s", c.obs, a, c.state, c.kin)
		}
	}
}

func TestProjectedDelivery(t *testing.T) {
	o := agentObs("codex-01")
	o.Kind = Codex
	o.Unread = []string{"m1", "m2"}
	o.Announced = map[string]bool{"m1": true, "gone": true}
	o.Doorbells = []Attempt{
		{At: t0, Outcome: Outcome{Progress: Accepted}},
		{At: t0.Add(time.Minute), Outcome: Outcome{Progress: Deferred, Code: "agent_working"}},
		{At: t0.Add(2 * time.Minute), Outcome: Outcome{Progress: Deferred, Code: "agent_working"}},
	}
	d := Project(Observations{Agents: []AgentObservation{o}}).Agents[0].Delivery
	if d.Unread != 2 || d.Announced != 1 || d.InProcess || !d.PendingDoorbell {
		t.Fatalf("delivery %+v", d)
	}
	if d.Doorbell == nil || d.Doorbell.Code != "agent_working" || !d.Doorbell.Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("doorbell %+v: want the run of agent_working since its first attempt", d.Doorbell)
	}
	o.Doorbells = append(o.Doorbells, Attempt{At: t0.Add(3 * time.Minute), Outcome: Outcome{Progress: Accepted}})
	if d := Project(Observations{Agents: []AgentObservation{o}}).Agents[0].Delivery; d.PendingDoorbell {
		t.Error("an accepted doorbell is still pending")
	}
	o.Kind = Pi
	if d := Project(Observations{Agents: []AgentObservation{o}}).Agents[0].Delivery; d.PendingDoorbell || !d.InProcess {
		t.Errorf("an in-process agent has no doorbell to wait for: %+v", d)
	}
}

// who's LAST DOORBELL column reads the projection; it must say what
// DeliveryStatus says of the same history.
func TestDoorbellSummaryIsDeliveryStatus(t *testing.T) {
	now := t0.Add(time.Hour)
	histories := [][]Attempt{
		nil,
		{{At: t0, Outcome: Outcome{Progress: Accepted}}},
		{{At: t0, Outcome: Outcome{Progress: Deferred, Code: "box_not_found"}}, {At: t0.Add(time.Minute), Outcome: Outcome{Progress: Deferred, Code: "box_not_found"}}},
	}
	for _, h := range histories {
		for _, unread := range []int{0, 1} {
			o := agentObs("codex-01")
			o.Kind = Codex
			o.Doorbells = h
			o.Unread = make([]string, unread)
			got := Project(Observations{Agents: []AgentObservation{o}}).Agents[0].DoorbellSummary(now)
			if want := DeliveryStatus(h, unread, now); got != want {
				t.Errorf("history %v unread %d: %q, want %q", h, unread, got, want)
			}
		}
	}
}

func TestProjectedMessageState(t *testing.T) {
	r := func(h string, b Box) RecipientObservation { return RecipientObservation{Handle: h, Box: b} }
	for _, c := range []struct {
		name string
		rs   []RecipientObservation
		want string
	}{
		{"in no mailbox in scope", nil, MsgSent},
		{"unread", []RecipientObservation{r("a", BoxUnread)}, MsgQueued},
		{"read by one of two", []RecipientObservation{r("a", BoxRead), r("b", BoxUnread)}, MsgQueued},
		{"read by all", []RecipientObservation{r("a", BoxRead), r("b", BoxRead)}, MsgDrained},
		{"dead-lettered by one", []RecipientObservation{r("a", BoxRead), r("b", BoxDead)}, MsgDeadLettered},
	} {
		m := Project(Observations{Messages: []MessageObservation{{ID: "m", Recipients: c.rs}}}).Messages[0]
		if m.State != c.want {
			t.Errorf("%s: %q, want %q", c.name, m.State, c.want)
		}
	}
	m := Project(Observations{Messages: []MessageObservation{{ID: "m", Recipients: []RecipientObservation{
		{Handle: "a", Box: BoxUnread, Announced: true, Departed: true},
	}}}}).Messages[0]
	if !m.Announced || !m.Bounced {
		t.Errorf("unread by a departed agent that was told: %+v", m)
	}
	m = Project(Observations{Messages: []MessageObservation{{ID: "m", Recipients: []RecipientObservation{
		{Handle: "a", Box: BoxRead, Departed: true},
	}}}}).Messages[0]
	if m.Bounced {
		t.Error("mail read before its recipient left bounced")
	}
}

func TestSanitizeSubject(t *testing.T) {
	long := strings.Repeat("é", 200)
	for _, c := range []struct{ in, want string }{
		{"Review ready", "Review ready"},
		{"line one\nline two\r\n", "line one line two"},
		{"\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"a\u0085b\u009bc\x7fd", "abcd"},
		{"evil‮gnp.exe", "evilgnp.exe"},
		{"iso⁦late⁩d ‏mark", "isolated mark"},
		{"bad \xff byte", "bad � byte"},
		{long, strings.Repeat("é", MaxSubject-1) + "…"},
	} {
		got := SanitizeSubject(c.in)
		if got != c.want {
			t.Errorf("SanitizeSubject(%q) = %q, want %q", c.in, got, c.want)
		}
		if n := len([]rune(got)); n > MaxSubject {
			t.Errorf("%q: %d runes", got, n)
		}
	}
}

func TestDiffIgnoresTheSamplingOfActivity(t *testing.T) {
	o := agentObs("claude-01")
	o.Activity = &ActivityObservation{State: StateIdle, Source: "rex_status", At: t0}
	prev := Project(Observations{Agents: []AgentObservation{o}})
	o.Activity = &ActivityObservation{State: StateIdle, Source: "supervisor", At: t0.Add(2 * time.Second)}
	if f := Diff(prev, Project(Observations{Agents: []AgentObservation{o}})); len(f) != 0 {
		t.Fatalf("a new sample of the same state emitted %+v", f)
	}
	o.Activity = &ActivityObservation{State: StateWorking, Source: "rex_status", At: t0.Add(4 * time.Second)}
	o.Unread = []string{"m1"}
	f := Diff(prev, Project(Observations{Agents: []AgentObservation{o}}))
	if len(f) != 1 || f[0].Type != "upsert" || f[0].Key != "claude-01" || !slices.Equal(f[0].Changed, []string{"activity", "delivery"}) {
		t.Fatalf("frames %+v", f)
	}
}

func TestDiffAddsAndRemoves(t *testing.T) {
	a, b := agentObs("claude-01"), agentObs("codex-01")
	prev := Project(Observations{Agents: []AgentObservation{a}, Messages: []MessageObservation{{ID: "m1"}}})
	next := Project(Observations{Agents: []AgentObservation{b}, Messages: []MessageObservation{{ID: "m2"}}})
	f := Diff(prev, next)
	var got []string
	for _, x := range f {
		got = append(got, x.Type+" "+x.Entity+" "+x.Key)
	}
	want := []string{"remove agent claude-01", "remove message m1", "upsert agent codex-01", "upsert message m2"}
	if !slices.Equal(got, want) {
		t.Fatalf("frames %v, want %v", got, want)
	}
	if !slices.Contains(f[2].Changed, "handle") || f[2].Value.(Agent).Handle != "codex-01" {
		t.Errorf("a new entity carries its value and every field: %+v", f[2])
	}
	if len(Diff(next, next)) != 0 {
		t.Error("an unchanged snapshot emitted frames")
	}
}

// The handle no longer carries the role, so notices name it beside the kind.
func TestNoticesNameTheRole(t *testing.T) {
	p := Peer{Handle: "codex-02", Kind: Codex, Role: "reviewer"}
	for _, n := range []string{MatesNotice([]Peer{p}), JoinedNotice(p), LeftNotice(p)} {
		if !strings.Contains(n, "codex-02 (codex, reviewer)") {
			t.Errorf("notice does not name the role: %q", n)
		}
	}
	if n := JoinedNotice(Peer{Handle: "pi-01", Kind: Pi}); !strings.Contains(n, "pi-01 (pi) joined") {
		t.Errorf("an agent without a role: %q", n)
	}
}
