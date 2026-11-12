package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func sameWindow(blocks ...string) *fakeTerm {
	windows := map[string]string{}
	for _, b := range blocks {
		windows[b] = "window:w"
	}
	return &fakeTerm{windows: windows}
}

func sendMail(t *testing.T, e Env, from, to core.Handle) {
	t.Helper()
	if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), string(from), string(to), "subject", "body"); err != nil {
		t.Fatal(err)
	}
}

// drainMail moves h's unread mail to cur, as `amq drain` does.
func drainMail(t *testing.T, e Env, h core.Handle) {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), string(h))
	if err != nil {
		t.Fatal(err)
	}
	inbox := amq.Inbox(e.Paths.Root(), string(h))
	for _, id := range ids {
		if err := os.Rename(filepath.Join(inbox, id+".md"), filepath.Join(inbox, "..", "cur", id+".md")); err != nil {
			t.Fatal(err)
		}
	}
}

func afterTool(t *testing.T, e Env, rec store.Record) Reply {
	t.Helper()
	r, err := e.Hook(context.Background(), EventPostTool, rec.Kind, Location{Block: rec.BlockID}, HookInput{SessionID: rec.AgentSessionID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Mail to an agent that left would sit unread with nothing to tell the sender.
// The sender hears it once, on the hook after its send, with who took the
// pane and who is in its tab.
func TestSenderHearsItsMailWentToADepartedAgent(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b", "block:c"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	bystander := adoptIn(t, e, "block:c", "s-bystander", 0)
	depart(t, e, "block:b", "s-left")
	successor := adoptIn(t, e, "block:b", "s-successor", 0)

	sendMail(t, e, sender.Handle, left.Handle)

	r := afterTool(t, e, sender)
	for _, want := range []string{
		"mail to " + string(left.Handle),
		string(successor.Handle) + " (codex) now runs in its pane",
		string(bystander.Handle) + " (codex)",
	} {
		if !strings.Contains(r.Notice, want) {
			t.Errorf("notice lacks %q: %q", want, r.Notice)
		}
	}
	if strings.Contains(r.Notice, string(sender.Handle)) {
		t.Errorf("notice names the sender as a peer: %q", r.Notice)
	}
	if again := afterTool(t, e, sender); again.Notice != "" {
		t.Errorf("told twice: %q", again.Notice)
	}
}

func TestMailToALiveAgentIsNotBounced(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	peer := adoptIn(t, e, "block:b", "s-peer", 0)

	sendMail(t, e, sender.Handle, peer.Handle)

	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("live recipient bounced: %q", r.Notice)
	}
}

// Mail the recipient read before it left needs no bounce, however it left.
func TestMailReadBeforeTheRecipientLeftIsNotBounced(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	peer := adoptIn(t, e, "block:b", "s-peer", 0)

	sendMail(t, e, sender.Handle, peer.Handle)
	drainMail(t, e, peer.Handle)
	depart(t, e, "block:b", "s-peer")

	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("read mail bounced: %q", r.Notice)
	}
}

func TestUnreadMailToAnAgentThatLeftBeforeTheHookIsBounced(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	peer := adoptIn(t, e, "block:b", "s-peer", 0)

	sendMail(t, e, sender.Handle, peer.Handle)
	depart(t, e, "block:b", "s-peer")

	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(peer.Handle)) {
		t.Fatalf("no bounce: %q", r.Notice)
	}
}

// An agent that crashed or lost its pane sends no SessionEnd, so its record
// still says live when the mail goes out. The send is what makes the sender
// look: only the agents it mailed are checked, and an unrelated dead agent is
// left for collection.
func TestMailToACrashedAgentIsBounced(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b", "block:c"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	crashed := adoptIn(t, e, "block:b", "s-crashed", exitedPID)
	unrelated := adoptIn(t, e, "block:c", "s-unrelated", exitedPID)

	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("bounce without a send: %q", r.Notice)
	}
	if !recordOf(t, e, crashed.Handle).Live() {
		t.Fatal("a hook with no new mail checked the recipients")
	}

	sendMail(t, e, sender.Handle, crashed.Handle)

	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(crashed.Handle)) {
		t.Fatalf("no bounce: %q", r.Notice)
	}
	if recordOf(t, e, crashed.Handle).Live() {
		t.Fatal("the crashed recipient was not parked")
	}
	if !recordOf(t, e, unrelated.Handle).Live() {
		t.Fatal("an agent nobody mailed was checked")
	}
}

// While the recipients cannot be checked nothing is concluded: the same mail
// is checked on the next hook.
func TestBusyLifecycleLockDelaysTheBounce(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	crashed := adoptIn(t, e, "block:b", "s-crashed", exitedPID)

	sendMail(t, e, sender.Handle, crashed.Handle)

	unlock, err := e.Store.LockLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("concluded without checking: %q", r.Notice)
	}
	unlock()

	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(crashed.Handle)) {
		t.Fatalf("not retried: %q", r.Notice)
	}
}

// An agent whose last act was the send still hears it before its turn ends,
// and only once, so the Stop hook cannot loop.
func TestTurnEndCarriesTheBounceOnce(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	depart(t, e, "block:b", "s-left")

	sendMail(t, e, sender.Handle, left.Handle)

	stop := func() Reply {
		t.Helper()
		r, err := e.Hook(context.Background(), EventStop, sender.Kind, Location{Block: "block:a"}, HookInput{SessionID: "s-sender"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := stop(); !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
		t.Fatalf("no bounce at turn end: %q", r.Notice)
	}
	if r := stop(); r.Notice != "" {
		t.Fatalf("Stop would block again: %q", r.Notice)
	}
}

// One message that cannot be read does not hide the others.
func TestUnreadableSentMessageIsSkipped(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	depart(t, e, "block:b", "s-left")

	sendMail(t, e, sender.Handle, left.Handle)
	torn := filepath.Join(e.Paths.Root(), "agents", string(sender.Handle), "outbox", "sent", "0000-torn.md")
	if err := os.WriteFile(torn, []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}

	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
		t.Fatalf("real bounce lost: %q", r.Notice)
	}
}

// The bounce is advice: failing to check the outbox never costs the agent its
// own mail, and the check runs again on the next hook.
func TestBrokenOutboxDoesNotSilenceMail(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	depart(t, e, "block:b", "s-left")

	sendMail(t, e, sender.Handle, left.Handle)
	sendMail(t, e, left.Handle, sender.Handle)

	sent := filepath.Join(e.Paths.Root(), "agents", string(sender.Handle), "outbox", "sent")
	moved := sent + ".moved"
	if err := os.Rename(sent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "unread") || strings.Contains(r.Notice, "mail to") {
		t.Fatalf("own mail not announced alone: %q", r.Notice)
	}

	if err := os.Remove(sent); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, sent); err != nil {
		t.Fatal(err)
	}
	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
		t.Fatalf("bounce not retried: %q", r.Notice)
	}
}

// failingFg makes Rex unreachable for the agent in one block only.
type failingFg struct {
	*fakeTerm
	block string
	down  bool
}

func (f *failingFg) Foreground(ctx context.Context, block string) (rex.Process, bool, error) {
	if f.down && block == f.block {
		return rex.Process{}, false, errors.New("rex is down")
	}
	return f.fakeTerm.Foreground(ctx, block)
}

// A recipient whose state cannot be observed proves nothing: the mail is
// checked again, and a crash is reported once Rex answers.
func TestUnobservedRecipientIsCheckedAgain(t *testing.T) {
	term := &failingFg{fakeTerm: sameWindow("block:a", "block:b"), block: "block:b"}
	e := realAmqEnv(t, term.fakeTerm)
	e.Rex, e.RexOn = term, func(string) Terminal { return term }
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	crashed := adoptIn(t, e, "block:b", "s-crashed", exitedPID)

	term.down = true
	sendMail(t, e, sender.Handle, crashed.Handle)
	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("concluded without observing: %q", r.Notice)
	}

	term.down = false
	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(crashed.Handle)) {
		t.Fatalf("not checked again: %q", r.Notice)
	}
}

// A recipient that stays unobservable does not hold the sender's other
// bounces, or the cost of checking it, for ever.
func TestUnobservableRecipientStopsBeingChecked(t *testing.T) {
	term := &failingFg{fakeTerm: sameWindow("block:a", "block:b", "block:c"), block: "block:b"}
	e := realAmqEnv(t, term.fakeTerm)
	e.Rex, e.RexOn = term, func(string) Terminal { return term }
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	silent := adoptIn(t, e, "block:b", "s-silent", 0)
	left := adoptIn(t, e, "block:c", "s-left", 0)
	depart(t, e, "block:c", "s-left")
	term.down = true

	sendMail(t, e, sender.Handle, silent.Handle)
	sendMail(t, e, sender.Handle, left.Handle)
	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("told before the check gave up: %q", r.Notice)
	}

	e.Now = func() time.Time { return time.Now().Add(checkSentFor + time.Second) }
	r := afterTool(t, e, sender)
	if !strings.Contains(r.Notice, "mail to "+string(left.Handle)) || strings.Contains(r.Notice, "mail to "+string(silent.Handle)) {
		t.Fatalf("after giving up: %q", r.Notice)
	}
	cursor, _, err := e.Store.SentCursor(sender.Handle)
	if err != nil || cursor == "" {
		t.Fatalf("still checking: %q %v", cursor, err)
	}
}

// Mail that lands while the recipients are being checked is the next hook's:
// it must not be passed over unchecked.
func TestMailSentDuringTheCheckIsNotSkipped(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b", "block:c"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	first := adoptIn(t, e, "block:b", "s-first", 0)
	second := adoptIn(t, e, "block:c", "s-second", exitedPID)

	sendMail(t, e, sender.Handle, first.Handle)
	batch := e.checkableSent(context.Background(), sender)
	sendMail(t, e, sender.Handle, second.Handle)

	unlock, err := e.Store.LockLedger(context.Background(), sender.Handle)
	if err != nil {
		t.Fatal(err)
	}
	notice := e.takeUndelivered(sender, batch)
	unlock()
	if notice != "" {
		t.Fatalf("batch told: %q", notice)
	}

	if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(second.Handle)) {
		t.Fatalf("later mail skipped: %q", r.Notice)
	}
}

// What a handle's previous owner sent is not the new agent's concern, and an
// install upgrading to this check has no cursor yet: old outboxes stay quiet.
func TestOldOutboxIsNotBounced(t *testing.T) {
	t.Run("handle reused", func(t *testing.T) {
		e := realAmqEnv(t, sameWindow("block:a", "block:b"))
		old := adoptIn(t, e, "block:a", "s-old", 0)
		left := adoptIn(t, e, "block:b", "s-left", 0)
		depart(t, e, "block:b", "s-left")
		sendMail(t, e, old.Handle, left.Handle)
		depart(t, e, "block:a", "s-old")
		if err := e.Store.DeleteRecord(old.Handle); err != nil {
			t.Fatal(err)
		}

		next := adoptIn(t, e, "block:a", "s-next", 0)
		if next.Handle != old.Handle {
			t.Fatalf("handle not reused: %s", next.Handle)
		}
		if r := afterTool(t, e, next); strings.Contains(r.Notice, "mail to") {
			t.Fatalf("told of a stranger's mail: %q", r.Notice)
		}
	})
	t.Run("upgrade", func(t *testing.T) {
		e := realAmqEnv(t, sameWindow("block:a", "block:b"))
		sender := adoptIn(t, e, "block:a", "s-sender", 0)
		left := adoptIn(t, e, "block:b", "s-left", 0)
		depart(t, e, "block:b", "s-left")
		sendMail(t, e, sender.Handle, left.Handle)
		if err := os.Remove(filepath.Join(e.Paths.Home, "ledger", string(sender.Handle)+".sent")); err != nil {
			t.Fatal(err)
		}
		if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
			t.Fatalf("old mail bounced: %q", r.Notice)
		}
		sendMail(t, e, sender.Handle, left.Handle)
		if r := afterTool(t, e, sender); !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
			t.Fatalf("new mail not bounced: %q", r.Notice)
		}
	})
}

// An orchestrator is told of a departure by mail in the departed agent's name. If
// that agent resumes it did not write that mail, and is not told about it.
func TestResumedAgentIsNotToldOfMailSentInItsName(t *testing.T) {
	term := sameWindow("block:a", "block:b")
	e := realAmqEnv(t, term)
	agent := adoptIn(t, e, "block:a", "s-agent", 0)
	term.label = core.OrchestratorRole
	orch := adoptIn(t, e, "block:b", "s-goal", 0)
	term.label = ""
	depart(t, e, "block:a", "s-agent")
	unread, err := amq.Unread(e.Paths.Root(), string(orch.Handle))
	if err != nil || len(unread) == 0 {
		t.Fatalf("orchestrator was not mailed: %v %v", unread, err)
	}
	depart(t, e, "block:b", "s-goal")

	resumed := adoptIn(t, e, "block:a", "s-agent", 0)
	if resumed.Handle != agent.Handle {
		t.Fatalf("handle changed: %s", resumed.Handle)
	}
	if r := afterTool(t, e, resumed); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("told of mail it did not send: %q", r.Notice)
	}
}

// An agent whose record was parked while its process ran is adopted again by
// its next hook; mail it sent meanwhile is still its own.
func TestReadoptedAgentKeepsItsLatestSend(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	depart(t, e, "block:b", "s-left")
	depart(t, e, "block:a", "s-sender")

	sendMail(t, e, sender.Handle, left.Handle)
	r, err := e.Hook(context.Background(), EventPostTool, sender.Kind, Location{Block: "block:a"}, HookInput{SessionID: "s-sender"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
		t.Fatalf("send lost on re-adoption: %q", r.Notice)
	}
}

func TestMailToItselfIsNotChecked(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a"))
	self := adoptIn(t, e, "block:a", "s-self", exitedPID)

	out, err := exec.Command(realBin(t, e), "--no-update-check", "send", "--root", e.Paths.Root(), "--me", string(self.Handle), "--to", string(self.Handle), "--allow-self", "--body", "note").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r := afterTool(t, e, self); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("told of its own note: %q", r.Notice)
	}
	if !recordOf(t, e, self.Handle).Live() {
		t.Fatal("parked itself")
	}
}

func TestEachDepartedRecipientIsNamedOnce(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b", "block:c", "block:d"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	one := adoptIn(t, e, "block:b", "s-one", 0)
	two := adoptIn(t, e, "block:c", "s-two", 0)
	live := adoptIn(t, e, "block:d", "s-live", 0)
	depart(t, e, "block:b", "s-one")
	depart(t, e, "block:c", "s-two")

	to := string(one.Handle) + "," + string(two.Handle) + "," + string(live.Handle)
	out, err := exec.Command(realBin(t, e), "--no-update-check", "send", "--root", e.Paths.Root(), "--me", string(sender.Handle), "--to", to, "--thread", "team", "--body", "all").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	sendMail(t, e, sender.Handle, one.Handle)

	n := afterTool(t, e, sender).Notice
	for h, want := range map[core.Handle]int{one.Handle: 1, two.Handle: 1, live.Handle: 0} {
		if got := strings.Count(n, "mail to "+string(h)); got != want {
			t.Errorf("%s named %d times, want %d: %q", h, got, want, n)
		}
	}
}

// Every kind reaches the check through its own hook.
func TestEveryKindHearsTheBounce(t *testing.T) {
	cases := []struct {
		name  string
		kind  core.Kind
		event string
		in    HookInput
	}{
		{"claude tool", core.Claude, EventPostTool, HookInput{SessionID: "s"}},
		{"codex prompt", core.Codex, EventPrompt, HookInput{SessionID: "s"}},
		{"agy model call", core.Agy, EventPreInvocation, HookInput{ConversationID: "s"}},
		{"pi settle", core.Pi, EventSettle, HookInput{SessionID: "s", AgentPID: os.Getpid()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := realAmqEnv(t, sameWindow("block:a", "block:b"))
			ctx := context.Background()
			loc := Location{Block: "block:a"}
			if _, err := e.Hook(ctx, EventSessionStart, c.kind, loc, c.in); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Depart(context.Background(), loc, HookInput{}) })
			sender, _, err := e.Store.Record("block:a")
			if err != nil {
				t.Fatal(err)
			}
			left := adoptIn(t, e, "block:b", "s-left", 0)
			depart(t, e, "block:b", "s-left")
			sendMail(t, e, sender.Handle, left.Handle)

			r, err := e.Hook(ctx, c.event, c.kind, loc, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
				t.Fatalf("no bounce: %q", r.Notice)
			}
			again, err := e.Hook(ctx, c.event, c.kind, loc, c.in)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(again.Notice, "mail to") {
				t.Fatalf("told twice: %q", again.Notice)
			}
		})
	}
}

// Pi's mail watcher turns any notice into a turn of its own: a bounce waits for
// the agent's next run, as window notes do.
func TestPiMailWatcherLeavesTheBounceForTheNextRun(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	ctx := context.Background()
	loc := Location{Block: "block:a"}
	in := HookInput{SessionID: "s", AgentPID: os.Getpid(), Idle: true}
	if _, err := e.Hook(ctx, EventSessionStart, core.Pi, loc, in); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), loc, HookInput{}) })
	sender, _, _ := e.Store.Record("block:a")
	left := adoptIn(t, e, "block:b", "s-left", 0)
	depart(t, e, "block:b", "s-left")
	sendMail(t, e, sender.Handle, left.Handle)

	r, err := e.Hook(ctx, EventMail, core.Pi, loc, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Notice, "mail to") {
		t.Fatalf("the watcher would start a turn for it: %q", r.Notice)
	}
	r, err = e.Hook(ctx, EventSettle, core.Pi, loc, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Notice, "mail to "+string(left.Handle)) {
		t.Fatalf("not told on the next run: %q", r.Notice)
	}
}

// Mail the adapter sent in an agent's name is never bounced to that agent,
// even to a recipient the agent mailed itself before it left.
func TestSystemMailInAnAgentsNameIsNeverBounced(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	sendMail(t, e, sender.Handle, left.Handle)
	drainMail(t, e, left.Handle)
	if err := e.sendSystem(context.Background(), sender.Handle, left.Handle, "watchdog: x", "body"); err != nil {
		t.Fatal(err)
	}
	depart(t, e, "block:b", "s-left")
	if r := afterTool(t, e, sender); strings.Contains(r.Notice, "mail to") {
		t.Fatalf("bounced mail the adapter sent: %q", r.Notice)
	}
}

// System mail in an agent's name does not hide the agent's own mail to an
// agent that left without reading it.
func TestSystemMailDoesNotHideAPendingBounce(t *testing.T) {
	e := realAmqEnv(t, sameWindow("block:a", "block:b", "block:c"))
	sender := adoptIn(t, e, "block:a", "s-sender", 0)
	left := adoptIn(t, e, "block:b", "s-left", 0)
	other := adoptIn(t, e, "block:c", "s-other", 0)
	sendMail(t, e, sender.Handle, left.Handle)
	depart(t, e, "block:b", "s-left")
	if err := e.sendSystem(context.Background(), sender.Handle, other.Handle, "watchdog: x", "body"); err != nil {
		t.Fatal(err)
	}
	r := afterTool(t, e, sender)
	if !strings.Contains(r.Notice, "mail to "+string(left.Handle)) || strings.Contains(r.Notice, "mail to "+string(other.Handle)) {
		t.Fatalf("notice %q: want the bounce of the agent's own mail only", r.Notice)
	}
}
