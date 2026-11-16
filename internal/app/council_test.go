package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

const scoreAsk = "SCORE: <n>/10"

func councilOf(extra ...func(*SummonRequest)) func(*SummonRequest) {
	return func(r *SummonRequest) {
		r.Council, r.Kind = true, "review_request"
		for _, f := range extra {
			f(r)
		}
	}
}

func seatActions(res SummonResult) string {
	parts := make([]string, len(res.Seats))
	for i, s := range res.Seats {
		parts[i] = string(s.Harness) + ":" + s.Action
	}
	return strings.Join(parts, " ")
}

// lastBody is the body of the newest unread mail of h.
func lastBody(t *testing.T, e Env, h core.Handle) string {
	t.Helper()
	ids := unread(t, e, string(h))
	if len(ids) == 0 {
		t.Fatalf("%s has no mail", h)
	}
	return mailBody(t, e, string(h), ids[len(ids)-1])
}

// A council with no seat yet starts one reviewer per harness of the
// routing, in order, the caller's own family included, each briefed with a
// request for a score; once adopted, each gets the request in its mailbox.
func TestSummonCouncilStartsEverySeat(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	res := summon(t, e, loc, "reviewer", councilOf(func(r *SummonRequest) { r.Asked = "council 审一下" }))
	if res.Action != "council" || seatActions(res) != "codex:start agy:start claude:start" {
		t.Fatalf("%+v", res)
	}
	if len(term.launches) != 3 {
		t.Fatalf("launches %d", len(term.launches))
	}
	for i, want := range []core.Kind{core.Codex, core.Agy, core.Claude} {
		l := term.launches[i]
		if launchProgram(l)[0] != string(want) || res.Seats[i].Block != l.Block || l.Dir != workDir(t, e, "app") {
			t.Fatalf("seat %d: %+v launch %+v", i, res.Seats[i], l)
		}
		if brief := l.Command[len(l.Command)-1]; !strings.Contains(brief, "please review #42") || !strings.Contains(brief, scoreAsk) || !strings.Contains(brief, "codex, agy, claude") {
			t.Fatalf("seat %d brief: %q", i, brief)
		}
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 3 {
		t.Fatalf("pending reservations: %d", n)
	}
	var handles []core.Handle
	for i, k := range []core.Kind{core.Codex, core.Agy, core.Claude} {
		rec := adoptLaunch(t, e, term, term.launches[i], k, fmt.Sprintf("seat-%d", i))
		if rec.Role != "reviewer" || rec.Parent != "claude-01" {
			t.Fatalf("adopted %+v", rec)
		}
		handles = append(handles, rec.Handle)
	}
	if err := e.AwaitCouncil(context.Background(), loc, SummonRequest{Kind: "review_request", Subject: "look at #42"}, &res, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(res.Handles) != 3 {
		t.Fatalf("handles %v", res.Handles)
	}
	for i, h := range handles {
		if s := res.Seats[i]; s.Handle != h || s.Startup != core.StartupAdopted || !s.Mailed {
			t.Fatalf("seat %d: %+v", i, s)
		}
		if n := len(unread(t, e, string(h))); n != 1 {
			t.Fatalf("%s has %d mails", h, n)
		}
		if b := lastBody(t, e, h); !strings.Contains(b, "please review #42") || !strings.Contains(b, scoreAsk) {
			t.Fatalf("%s mail: %s", h, b)
		}
	}
	if lines, _ := e.Summons("claude-01"); len(lines) != 1 || !strings.Contains(lines[0], "council reviewer") {
		t.Fatalf("summon log: %v", lines)
	}
}

// The next round goes to the seats still live: no pane opens and each seat
// gets the request with the score asked again; a seat that departed is
// resumed with its mail waiting, and with --cwd only the seats in that
// worktree count.
func TestSummonCouncilRoundTwoDeliversToLiveSeats(t *testing.T) {
	e, term, loc := summonWorld(t)
	e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
	wt := workDir(t, e, "app")
	var seats []store.Record
	for i, k := range []core.Kind{core.Codex, core.Agy, core.Claude} {
		rec := store.Record{Handle: core.Handle(fmt.Sprintf("%s-1%d", k, i)), Kind: k, Server: "unix:///fake.sock", BlockID: fmt.Sprintf("block:seat-%d", i), SessionID: "session:s", Role: "reviewer", AgentPID: 20 + i, AgentSessionID: fmt.Sprintf("seat-%d", i), AdoptedAt: e.Now(), Cwd: wt}
		add(t, e, rec)
		seats = append(seats, rec)
	}
	res := summon(t, e, loc, "reviewer", councilOf(func(r *SummonRequest) { r.Cwd = wt }))
	if res.Action != "council" || seatActions(res) != "codex:deliver agy:deliver claude:deliver" || len(term.launches) != 0 || len(res.Handles) != 3 {
		t.Fatalf("round 2 with --cwd: %+v launches %d", res, len(term.launches))
	}
	for _, s := range seats {
		if n := len(unread(t, e, string(s.Handle))); n != 1 || !strings.Contains(lastBody(t, e, s.Handle), scoreAsk) {
			t.Fatalf("%s: %d mails", s.Handle, n)
		}
	}
	// AwaitCouncil has nothing to wait for and mails nobody twice.
	if err := e.AwaitCouncil(context.Background(), loc, SummonRequest{Kind: "review_request"}, &res, time.Second); err != nil || len(res.Handles) != 3 {
		t.Fatalf("%v %v", err, res.Handles)
	}
	for _, s := range seats {
		if n := len(unread(t, e, string(s.Handle))); n != 1 {
			t.Fatalf("%s mailed again: %d", s.Handle, n)
		}
	}
	// The agy seat departed: without --cwd it is resumed, mailed first.
	add(t, e, departed(seats[1], e.Now()))
	res = summon(t, e, loc, "reviewer", councilOf())
	if seatActions(res) != "codex:deliver agy:resume claude:deliver" || len(term.launches) != 1 || launchProgram(term.launches[0])[0] != "agy" || !res.Seats[1].Mailed {
		t.Fatalf("round 3: %+v launches %+v", res, term.launches)
	}
	if n := len(unread(t, e, string(seats[1].Handle))); n != 2 {
		t.Fatalf("resumed seat has %d mails", n)
	}
	if rec := adoptLaunch(t, e, term, term.launches[0], core.Agy, "seat-1"); rec.Handle != seats[1].Handle {
		t.Fatalf("resumed as %s", rec.Handle)
	}
	if err := e.AwaitCouncil(context.Background(), loc, SummonRequest{Kind: "review_request"}, &res, time.Second); err != nil || res.Seats[1].Startup != core.StartupAdopted || len(unread(t, e, string(seats[1].Handle))) != 2 {
		t.Fatalf("%v %+v", err, res.Seats[1])
	}
}

// When any seat cannot be seated (a harness not installed, a preflight
// refusal, too many seats of one harness, the session cap, no authority to
// start), the council is refused naming why, and nothing is mailed,
// launched or reserved: a later single summon under the cap still starts.
func TestSummonCouncilRefusesWithoutPartialLaunch(t *testing.T) {
	asked := func(r *SummonRequest) { r.Asked = "council" }
	seatOn := func(k core.Kind, n int) store.Record {
		return store.Record{Handle: core.Handle(fmt.Sprintf("%s-%d", k, n)), Kind: k, Server: "unix:///fake.sock", BlockID: fmt.Sprintf("block:%s-%d", k, n), SessionID: "session:s", Role: "reviewer", AgentPID: 30 + n, AdoptedAt: time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)}
	}
	nothing := func(t *testing.T, e Env, term *fakeTerm, live store.Record, res SummonResult, want string) {
		t.Helper()
		if res.Action != "refused" || !strings.Contains(res.Reason, want) || len(res.Seats) != 0 || len(res.Handles) != 0 {
			t.Fatalf("want a refusal naming %q: %+v", want, res)
		}
		if len(term.launches) != 0 {
			t.Fatalf("launched %+v", term.launches)
		}
		if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
			t.Fatalf("pending reservations: %d", n)
		}
		if n := len(unread(t, e, string(live.Handle))); n != 0 {
			t.Fatalf("the live seat %s got %d mails", live.Handle, n)
		}
	}
	for _, c := range []struct {
		name, want string
		live       store.Record
		setup      func(t *testing.T, e *Env)
	}{
		{"agy not installed", "agy: not installed", seatOn(core.Codex, 12), func(t *testing.T, e *Env) { e.LookPath = installedOnly(core.Codex, core.Claude) }},
		{"codex hooks unapproved", "codex: unapproved_hooks", seatOn(core.Claude, 12), func(t *testing.T, e *Env) { unapprovedCodexHook(t, *e) }},
		{"two live codex seats", "codex: 2 live reviewer seats", seatOn(core.Codex, 12), func(t *testing.T, e *Env) { add(t, *e, seatOn(core.Codex, 13)) }},
		{"no authority to start", "user's own words", seatOn(core.Codex, 12), nil},
		{"departed agy seat not installed", "agy: not installed", seatOn(core.Codex, 12), func(t *testing.T, e *Env) {
			gone := seatOn(core.Agy, 14)
			gone.AgentSessionID, gone.Cwd = "agy-14", workDir(t, *e, "app")
			add(t, *e, departed(gone, e.Now()))
			e.LookPath = installedOnly(core.Codex, core.Claude)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, term, loc := summonWorld(t)
			e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
			add(t, e, c.live)
			req := councilOf(asked)
			if c.setup != nil {
				c.setup(t, &e)
			} else {
				req = councilOf()
			}
			nothing(t, e, term, c.live, summon(t, e, loc, "reviewer", req), c.want)
		})
	}
	t.Run("over the cap", func(t *testing.T) {
		e, term, loc := summonWorld(t)
		e.LookPath = installedOnly(core.Codex, core.Agy, core.Claude)
		live := seatOn(core.Codex, 12)
		add(t, e, live)
		// The caller, the codex seat and three more: two seats to start would make 7.
		for i := 0; i < 3; i++ {
			add(t, e, store.Record{Handle: core.Handle(fmt.Sprintf("pi-%02d", i+2)), Kind: core.Pi, Server: "unix:///fake.sock", BlockID: fmt.Sprintf("block:peer-%d", i), SessionID: "session:s", AgentPID: i + 10, AdoptedAt: e.Now()})
		}
		nothing(t, e, term, live, summon(t, e, loc, "reviewer", councilOf(asked)), "cap")
		// One more agent still fits: the refused council reserved nothing.
		if res := summon(t, e, loc, "editor", asked); res.Action != "start" {
			t.Fatalf("a single start under the cap: %+v", res)
		}
	})
}

// --council is a review role's: a writer role, a handle, or an explicit
// harness is a usage error, and nothing is mailed or launched.
func TestSummonCouncilRejectsNonReviewRoles(t *testing.T) {
	e, term, loc := summonWorld(t)
	add(t, e, store.Record{Handle: "codex-02", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:b", SessionID: "session:s", Role: "reviewer", AgentPID: 2, AdoptedAt: e.Now()})
	for _, r := range []SummonRequest{
		{Target: "implementer"},
		{Target: "writer"},
		{Target: "codex-02"},
		{Target: "reviewer", Harness: "codex"},
	} {
		r.Council, r.Kind, r.Subject, r.Body, r.Asked = true, "review_request", "s", "b", "yes"
		if _, err := e.Summon(context.Background(), loc, r); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v, want a usage error", r, err)
		}
	}
	if len(term.launches) != 0 || len(unread(t, e, "codex-02")) != 0 {
		t.Fatalf("launches %d, mail %v", len(term.launches), unread(t, e, "codex-02"))
	}
}
