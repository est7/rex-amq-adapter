package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// leadSeat is a single-seat brainstorm request from the design lead, in
// dir when it is not "".
func leadSeat(dir, harness string) func(*SummonRequest) {
	return func(r *SummonRequest) {
		r.Kind, r.Cwd, r.Harness, r.Asked = "brainstorm", dir, harness, "a second architect on codex"
	}
}

// A design Epic's lead architect (claude, depth 0) seats one architect on
// another harness without --council, in its own directory or without
// --cwd: the seat starts at depth 1 on the harness asked. That seat may not
// summon an architect (a cycle, with --harness or --cwd), and the lead never
// summons itself by handle.
func TestSummonLeadSeatsOneArchitectOnAnotherHarness(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")

	res := summon(t, e, loc, architectRole, leadSeat(dir, "codex"))
	if res.Action != "start" || res.Harness != core.Codex || res.Depth != 1 || len(term.launches) != 1 || launchProgram(term.launches[0])[0] != "codex" || term.launches[0].Dir != dir {
		t.Fatalf("a codex seat in the lead's directory: %+v launches %+v", res, term.launches)
	}
	seat := adoptLaunch(t, e, term, term.launches[0], core.Codex, "arch-codex")
	if seat.Role != architectRole || seat.Parent != "claude-01" || seat.Depth != 1 {
		t.Fatalf("seat %+v", seat)
	}

	res = summon(t, e, loc, architectRole, leadSeat("", "agy"))
	if res.Action != "start" || res.Harness != core.Agy || len(term.launches) != 2 || launchProgram(term.launches[1])[0] != "agy" || term.launches[1].Dir != dir {
		t.Fatalf("an agy seat without --cwd: %+v", res)
	}

	at := Location{Block: seat.BlockID}
	for _, extra := range []func(*SummonRequest){leadSeat("", "agy"), leadSeat(workDir(t, e, "other"), "")} {
		got := summon(t, e, at, architectRole, extra)
		if got.Action != "refused" || !strings.Contains(got.Reason, "cycle") {
			t.Fatalf("the depth-1 seat summoning an architect: %+v", got)
		}
	}
	self := summon(t, e, loc, "claude-01", leadSeat("", ""))
	if self.Action != "refused" || len(term.launches) != 2 || len(unread(t, e, "claude-01")) != 0 {
		t.Fatalf("the lead summoning itself: %+v", self)
	}
}

// In its own directory the lead holds its own harness's seat: naming that
// harness there is refused with nothing mailed or launched, with --cwd or
// without; in another directory it starts.
func TestSummonLeadSeatRefusesItsOwnHarnessInItsDirectory(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	for _, cwd := range []string{dir, ""} {
		res := summon(t, e, loc, architectRole, leadSeat(cwd, "claude"))
		if res.Action != "refused" || !strings.Contains(res.Reason, "claude-01 holds the claude seat of architect in "+dir) || len(term.launches) != 0 {
			t.Fatalf("its own harness, --cwd %q: %+v", cwd, res)
		}
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("a refusal reserved %d", n)
	}
	other := workDir(t, e, "other")
	res := summon(t, e, loc, architectRole, leadSeat(other, "claude"))
	if res.Action != "start" || res.Harness != core.Claude || len(term.launches) != 1 || term.launches[0].Dir != other {
		t.Fatalf("its own harness in another directory: %+v", res)
	}
}

// With no --harness in its own directory, the lead's seat starts on the
// next routed harness that is free there: codex, then agy once codex sits,
// never claude, and a departed claude architect of that directory is not
// resumed in its place. Every harness seated, it is refused.
func TestSummonLeadSeatWithoutHarnessPassesOverItsOwn(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	add(t, e, departed(seed{Handle: "claude-03", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: architectRole, Parent: "claude-01", Depth: 1, AgentPID: 3, AgentSessionID: "thread-3", AdoptedAt: e.Now(), Cwd: dir}, e.Now()))

	res := summon(t, e, loc, architectRole, leadSeat("", ""))
	if res.Action != "start" || res.Harness != core.Codex || len(term.launches) != 1 || launchProgram(term.launches[0])[0] != "codex" || len(unread(t, e, "claude-03")) != 0 {
		t.Fatalf("no --harness, no --cwd: %+v", res)
	}
	// The seats as adopted in dir (a fake hook records no directory).
	seat := func(h core.Handle, k core.Kind, n int) seed {
		return seed{Handle: h, Kind: k, Server: "unix:///fake.sock", BlockID: "block:" + string(h), SessionID: "session:s", Role: architectRole, Parent: "claude-01", Depth: 1, AgentPID: 20 + n, AgentSessionID: string(h), AdoptedAt: e.Now(), Cwd: dir}
	}
	add(t, e, seat("codex-04", core.Codex, 4))
	if err := e.Store.DeleteSummon(tokenOf(term.launches[0])); err != nil { // adoption consumes it
		t.Fatal(err)
	}

	res = summon(t, e, loc, architectRole, leadSeat(dir, ""))
	if res.Action != "start" || res.Harness != core.Agy || len(term.launches) != 2 || launchProgram(term.launches[1])[0] != "agy" {
		t.Fatalf("no --harness with codex seated: %+v", res)
	}
	add(t, e, seat("agy-05", core.Agy, 5))
	if err := e.Store.DeleteSummon(tokenOf(term.launches[1])); err != nil {
		t.Fatal(err)
	}

	res = summon(t, e, loc, architectRole, leadSeat(dir, ""))
	if res.Action != "refused" || !strings.Contains(res.Reason, "already has a live architect") || len(term.launches) != 2 {
		t.Fatalf("every harness seated: %+v", res)
	}
}

// The exemption is a read-only lead's: a pair Epic's lead implementer
// summoning an implementer is a cycle, in another directory too.
func TestSummonPairLeadOwnRoleIsACycle(t *testing.T) {
	e, term, loc := summonWorld(t)
	rec, _, _ := e.agentAt(loc.Block)
	rec.Role = implementerRole
	add(t, e, seedOf(rec))
	p := epicPolicy("session:s", "unix:///fake.sock", pairCrew, "claude-09", e.Now())
	p.LeadHandle = rec.Handle
	if err := e.Store.PutSessionPolicy(p); err != nil {
		t.Fatal(err)
	}
	if lead, err := e.sessionLead(recordOf(t, e, rec.Handle)); err != nil || !lead {
		t.Fatalf("the pair lead does not lead: %v %v", lead, err)
	}
	for _, h := range []string{"", "codex"} {
		res := summon(t, e, loc, implementerRole, func(r *SummonRequest) {
			r.Kind, r.Cwd, r.Harness, r.Asked = "todo", workDir(t, e, "other"), h, "another implementer"
		})
		if res.Action != "refused" || !strings.Contains(res.Reason, "cycle") || len(term.launches) != 0 {
			t.Fatalf("the pair lead, --harness %q: %+v", h, res)
		}
	}
}

// A departed seat of the lead's harness whose record names no directory is
// judged by the one a resume would use, its transcript's: one in the lead's
// directory is the lead's own seat, so it is neither mailed nor resumed.
func TestSummonLeadSeatSkipsADepartedSeatByItsTranscriptDirectory(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	p := filepath.Join(e.TranscriptHome, ".claude/projects/-work-app/thread-3.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user","cwd":"`+dir+`","sessionId":"thread-3"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	add(t, e, departed(seed{Handle: "claude-03", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: architectRole, Parent: "claude-01", Depth: 1, AgentPID: 3, AgentSessionID: "thread-3", AdoptedAt: e.Now()}, e.Now()))

	res := summon(t, e, loc, architectRole, leadSeat("", "claude"))
	if res.Action != "refused" || !strings.Contains(res.Reason, "claude-01 holds the claude seat") || len(term.launches) != 0 || len(unread(t, e, "claude-03")) != 0 {
		t.Fatalf("its own harness: %+v", res)
	}
	res = summon(t, e, loc, architectRole, leadSeat("", ""))
	if res.Action != "start" || res.Harness != core.Codex || len(term.launches) != 1 || len(unread(t, e, "claude-03")) != 0 {
		t.Fatalf("no --harness: %+v", res)
	}
}

// A seat the lead summoned that is not adopted yet (a startup that timed
// out, a trust screen) still holds its place: until it is adopted or its
// reservation lapses, the lead's next single seat of the role is refused
// with nothing mailed or launched, whatever harness it names, so a retry
// never opens a second pane for the same seat. Once adopted, the next seat
// starts.
func TestSummonLeadSeatWaitsForAPendingSeat(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	res := summon(t, e, loc, architectRole, leadSeat(dir, "codex"))
	if res.Action != "start" || len(term.launches) != 1 {
		t.Fatalf("the first seat: %+v", res)
	}
	for _, h := range []string{"codex", "", "agy"} {
		again := summon(t, e, loc, architectRole, leadSeat(dir, h))
		if again.Action != "refused" || !strings.Contains(again.Reason, "not adopted yet") || len(term.launches) != 1 {
			t.Fatalf("a retry with --harness %q while the seat is pending: %+v", h, again)
		}
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 1 {
		t.Fatalf("pending %d, want the first seat's alone", n)
	}
	adoptLaunch(t, e, term, term.launches[0], core.Codex, "arch-codex")
	res = summon(t, e, loc, architectRole, leadSeat(dir, "agy"))
	if res.Action != "start" || res.Harness != core.Agy || len(term.launches) != 2 {
		t.Fatalf("the next seat once the first is adopted: %+v", res)
	}
}

// Two single seats the lead asks for at once meet under the lifecycle
// lock: the first to reserve claims the seat, and the other is refused,
// however the two interleave before it, so one pane opens.
func TestSummonLeadSeatClaimsTheSeatUnderTheLock(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	dir := workDir(t, e, "app")
	unlock, err := e.Store.LockLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan SummonResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := SummonRequest{Target: architectRole, Subject: "s", Body: "b"}
			leadSeat(dir, "codex")(&r)
			res, err := e.Summon(context.Background(), loc, r)
			results <- res
			errs <- err
		}()
	}
	time.Sleep(300 * time.Millisecond) // both plan and wait at reserve's lock
	unlock()
	actions := map[string]int{}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		res := <-results
		actions[res.Action]++
		if res.Action == "refused" && !strings.Contains(res.Reason, "not adopted yet") {
			t.Errorf("the refusal: %q", res.Reason)
		}
	}
	term.mu.Lock()
	n := len(term.launches)
	term.mu.Unlock()
	if actions["start"] != 1 || actions["refused"] != 1 || n != 1 {
		t.Fatalf("actions %v launches %d", actions, n)
	}
}

// A resumed seat keeps its own lineage (an architect the user started has
// no parent), yet its pending launch still holds the seat: a serial retry
// neither mails it again nor resumes it twice.
func TestSummonLeadSeatWaitsForAPendingResume(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	add(t, e, departed(seed{Handle: "codex-03", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: architectRole, AgentPID: 3, AgentSessionID: "thread-3", AdoptedAt: e.Now(), Cwd: workDir(t, e, "other")}, e.Now()))
	first := summon(t, e, loc, architectRole, leadSeat("", "codex"))
	if first.Action != "resume" || len(term.launches) != 1 || len(unread(t, e, "codex-03")) != 1 {
		t.Fatalf("the first resume: %+v", first)
	}
	again := summon(t, e, loc, architectRole, leadSeat("", "codex"))
	if again.Action != "refused" || !strings.Contains(again.Reason, "not adopted yet") || len(term.launches) != 1 || len(unread(t, e, "codex-03")) != 1 {
		t.Fatalf("a retry while the resume is pending: %+v", again)
	}
}

// An agent of the role adopted after the summon was planned, and so not
// weighed in choosing the seat, refuses the claim under the lock.
func TestSummonLeadSeatClaimRefusesASeatAdoptedSincePlanned(t *testing.T) {
	e, _, loc := designLeadWorld(t)
	lead, _, _ := e.agentAt(loc.Block)
	s, err := e.FindSession(context.Background(), loc.Block)
	if err != nil {
		t.Fatal(err)
	}
	claim := &seatClaim{role: architectRole, seen: map[core.Handle]bool{lead.Handle: true}}
	if _, err := e.reserve(context.Background(), lead, s, "brainstorm", []core.Peer{peerOf(lead)}, architectRole, []*agent{nil}, true, claim, true); err != nil {
		t.Fatalf("nothing adopted since: %v", err)
	}
	add(t, e, seed{Handle: "codex-04", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:codex-04", SessionID: "session:s", Role: architectRole, Parent: lead.Handle, Depth: 1, AgentPID: 24, AgentSessionID: "codex-04", AdoptedAt: e.Now(), Cwd: lead.Cwd})
	s, _ = e.FindSession(context.Background(), loc.Block)
	_, err = e.reserve(context.Background(), lead, s, "brainstorm", []core.Peer{peerOf(lead)}, architectRole, []*agent{nil}, true, claim, true)
	var refusal core.SummonCheck
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "codex-04") {
		t.Fatalf("a seat adopted since planned: %v", err)
	}
}

// A resume the lead asked for by handle holds the seat too: its pending
// launch names the handle, not the role, and still counts, so a role
// summon for the same seat neither mails nor resumes it again.
func TestSummonLeadSeatWaitsForAResumeAskedByHandle(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	add(t, e, departed(seed{Handle: "codex-06", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: architectRole, Parent: "claude-01", Depth: 1, AgentPID: 6, AgentSessionID: "thread-6", AdoptedAt: e.Now(), Cwd: workDir(t, e, "app")}, e.Now()))
	first := summon(t, e, loc, "codex-06", leadSeat("", ""))
	if first.Action != "resume" || len(term.launches) != 1 || len(unread(t, e, "codex-06")) != 1 {
		t.Fatalf("the resume by handle: %+v", first)
	}
	again := summon(t, e, loc, architectRole, leadSeat("", "codex"))
	if again.Action != "refused" || !strings.Contains(again.Reason, "not adopted yet") || len(term.launches) != 1 || len(unread(t, e, "codex-06")) != 1 {
		t.Fatalf("a role summon while the resume by handle is pending: %+v", again)
	}
}

// The lead's seat is claimed before its mail: of two resumes of one seat
// racing to reserve, the refused one mails nobody.
func TestSummonLeadSeatClaimsAResumeBeforeItsMail(t *testing.T) {
	e, term, loc := designLeadWorld(t)
	add(t, e, departed(seed{Handle: "codex-03", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: "block:gone", SessionID: "session:s", Role: architectRole, Parent: "claude-01", Depth: 1, AgentPID: 3, AgentSessionID: "thread-3", AdoptedAt: e.Now(), Cwd: workDir(t, e, "other")}, e.Now()))
	unlock, err := e.Store.LockLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan SummonResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := SummonRequest{Target: architectRole, Subject: "s", Body: "b"}
			leadSeat("", "codex")(&r)
			res, err := e.Summon(context.Background(), loc, r)
			results <- res
			errs <- err
		}()
	}
	time.Sleep(300 * time.Millisecond) // both plan and wait at reserve's lock
	unlock()
	actions := map[string]int{}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		res := <-results
		actions[res.Action]++
		if res.Action == "refused" && (res.Mailed || strings.Contains(res.Reason, "mailbox")) {
			t.Errorf("the refused resume mailed: %+v", res)
		}
	}
	term.mu.Lock()
	n := len(term.launches)
	term.mu.Unlock()
	if actions["resume"] != 1 || actions["refused"] != 1 || n != 1 || len(unread(t, e, "codex-03")) != 1 {
		t.Fatalf("actions %v launches %d mails %d", actions, n, len(unread(t, e, "codex-03")))
	}
}
