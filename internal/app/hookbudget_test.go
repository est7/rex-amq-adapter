package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// The whole hook reply stays within core.ContextCap, not only its start
// notice (codex-12 Slice 4 R1 S1): a design lead adopted at its first prompt,
// with the longest grant receipt that still fits a start notice, five
// ordinary session-mates' join notes queued while it adopted (tellMates), and
// a mail notice, composed through withWindowNotes into the final
// Reply.Notice. Whatever the user's words, the receipt is whole or withheld:
// complete: true with every quoted line, or complete: false with none of
// them.
func TestHookReplyBudget(t *testing.T) {
	joined := []core.Peer{
		{Handle: "codex-03", Kind: core.Codex, Role: core.OrchestratorRole},
		{Handle: "codex-04", Kind: core.Codex, Role: "implementer"},
		{Handle: "codex-05", Kind: core.Codex, Role: "reviewer"},
		{Handle: "codex-06", Kind: core.Codex, Role: "qa"},
		{Handle: "codex-07", Kind: core.Codex, Role: "writer"},
	}
	e, _, lead, _ := receiptWorld(t, "design", architectRole, "run it")
	base := e.identityParts(context.Background(), Adoption{Agent: lead, Role: lead.Role, Session: lead.SessionID}, 0)
	if base.Receipt == nil {
		t.Fatal("the design lead has no grant receipt")
	}
	mail := core.HookNotice(core.Announcement{Unread: 3, Fresh: 3, Speak: true}, lead.Handle, e.SelfBin, core.PromptSubmitted)
	// The notes tellMates queues (an orchestrator's join adds who
	// coordinates), and with the mail what an unbounded composition would
	// add to the start notice.
	queue := func() {
		for _, p := range joined {
			e.tellMates(context.Background(), p, []core.Peer{peerOf(lead)}, core.JoinedNotice(p))
		}
	}
	queue()
	notes, err := e.Store.TakeWindowNotes(lead.Handle)
	if err != nil || len(notes) != len(joined) {
		t.Fatalf("queued notes %v %v", notes, err)
	}
	rest := core.ContextLen(" " + strings.Join(append(slices.Clone(notes), mail), " "))
	withAsked := func(n int) core.StartParts {
		p := base
		r := *base.Receipt
		r.Asked = strings.Repeat("a", n)
		p.Receipt = &r
		return p
	}
	// Every asked length whose complete start notice the notes and mail
	// would push past ContextCap (codex-12's 9,000 + 1,338 among them), the
	// longest complete notice of each step of the allocator, and lengths
	// whose receipt fits only withheld.
	var cases []int
	longest := 0
	for n := 1; n <= 8000; n++ {
		alone := core.StartNotice(withAsked(n))
		complete := strings.Contains(alone, "- complete: true")
		over := core.ContextLen(alone)+rest > core.ContextCap
		if complete && core.ContextLen(alone) > longest {
			longest = core.ContextLen(alone)
		}
		if (complete && over && n%11 == 0) || (complete && !strings.Contains(core.StartNotice(withAsked(n+1)), "- complete: true")) || n%997 == 0 {
			cases = append(cases, n)
		}
	}
	if longest != core.StartBudget {
		t.Fatalf("the longest complete start notice is %d, not StartBudget %d: the cases miss codex-12's", longest, core.StartBudget)
	}
	sawComplete, sawWithheld, sawOver := false, false, false
	for _, n := range cases {
		parts := withAsked(n)
		asked := parts.Receipt.Asked
		queue()
		r, err := e.withWindowNotes(lead.Handle, true, &parts, mail, core.ContextCap)
		if err != nil {
			t.Fatal(err)
		}
		got := r.Notice
		if l := core.ContextLen(got); l > core.ContextCap {
			t.Fatalf("asked %d: the hook reply is %d units, over ContextCap %d", n, l, core.ContextCap)
		}
		alone := core.StartNotice(parts)
		sawOver = sawOver || strings.Contains(alone, "- complete: true") && core.ContextLen(alone)+rest > core.ContextCap
		if !strings.HasPrefix(got, "AMQ: you are claude-02.") || !strings.HasSuffix(got, mail) {
			t.Errorf("asked %d: identity or mail left the reply:\n%.200s … %.300s", n, got, got[max(0, len(got)-300):])
		}
		if !strings.Contains(got, "Grant: "+e.Paths.GrantPath(lead.SessionID)) {
			t.Errorf("asked %d: the Grant line left the reply", n)
		}
		switch {
		case strings.Contains(got, "- complete: true"):
			sawComplete = true
			if !strings.Contains(got, "- asked_verbatim:\n> "+asked+"\n") {
				t.Errorf("asked %d: a complete receipt without the user's words whole", n)
			}
		case strings.Contains(got, "- complete: false"):
			sawWithheld = true
			if strings.Contains(got, "asked_verbatim") || strings.Contains(got, "> aaaa") || !strings.Contains(got, "- next_action: rex-amq-adapter epic grant "+lead.SessionID) {
				t.Errorf("asked %d: a withheld receipt carries part of the words or no next_action:\n%s", n, got)
			}
		default:
			t.Errorf("asked %d: no receipt:\n%s", n, got)
		}
		for _, note := range notes {
			if !strings.Contains(got, note) {
				t.Errorf("asked %d: a join note was dropped: %s", n, note)
			}
		}
		if left, _ := e.Store.TakeWindowNotes(lead.Handle); len(left) != 0 {
			t.Errorf("asked %d: notes left queued: %v", n, left)
		}
	}
	if !sawComplete || !sawWithheld || !sawOver {
		t.Errorf("the cases did not cover the edge: complete %v, withheld %v, over the cap without HookReply %v", sawComplete, sawWithheld, sawOver)
	}
}

// Window notes that no reply could hold whole give way: the newest that fit
// stay, the older ones become one line counting them, and the start notice
// and mail stay inline.
func TestHookReplyBoundsQueuedNotes(t *testing.T) {
	e, _, lead, _ := receiptWorld(t, "design", architectRole, "run it")
	parts := e.identityParts(context.Background(), Adoption{Agent: lead, Role: lead.Role, Session: lead.SessionID}, 0)
	for i := 0; i < 200; i++ {
		p := core.Peer{Handle: core.Handle("codex-" + strings.Repeat("9", 1+i%5) + string(rune('a'+i%26))), Kind: core.Codex, Role: "implementer"}
		e.tellMates(context.Background(), p, []core.Peer{peerOf(lead)}, core.JoinedNotice(p))
	}
	mail := core.HookNotice(core.Announcement{Unread: 1, Fresh: 1, Speak: true}, lead.Handle, e.SelfBin, core.PromptSubmitted)
	r, err := e.withWindowNotes(lead.Handle, true, &parts, mail, core.ContextCap)
	if err != nil {
		t.Fatal(err)
	}
	if l := core.ContextLen(r.Notice); l > core.ContextCap {
		t.Fatalf("reply %d > %d", l, core.ContextCap)
	}
	if !strings.Contains(r.Notice, "earlier note(s) about agents joining or leaving") || !strings.Contains(r.Notice, "- complete: true") || !strings.HasSuffix(r.Notice, mail) {
		t.Errorf("reply:\n%s", r.Notice)
	}
}
