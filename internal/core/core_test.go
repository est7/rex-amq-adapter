package core

import (
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/screen"
)

func set(hs ...Handle) map[Handle]bool {
	m := map[Handle]bool{}
	for _, h := range hs {
		m[h] = true
	}
	return m
}

func TestChooseHandle(t *testing.T) {
	cases := []struct {
		name string
		req  HandleRequest
		want HandleChoice
	}{
		{"default label gets the kind and a visible label",
			HandleRequest{Kind: Codex, Label: "shell"},
			HandleChoice{Handle: "codex", SetLabel: true}},
		{"empty label counts as default",
			HandleRequest{Kind: Claude, Label: ""},
			HandleChoice{Handle: "claude", SetLabel: true}},
		{"kind taken by a live agent gets a suffix",
			HandleRequest{Kind: Codex, Label: "shell", Live: set("codex", "codex-2")},
			HandleChoice{Handle: "codex-3", SetLabel: true}},
		{"automatic names skip encumbered handles",
			HandleRequest{Kind: Claude, Label: "shell", Encumbered: set("claude")},
			HandleChoice{Handle: "claude-2", SetLabel: true}},
		{"explicit label wins verbatim, lowercased, even with mail waiting",
			HandleRequest{Kind: Codex, Label: "Reviewer", Encumbered: set("reviewer")},
			HandleChoice{Handle: "reviewer"}},
		{"explicit label held by a live agent is suffixed and relabeled",
			HandleRequest{Kind: Codex, Label: "reviewer", Live: set("reviewer")},
			HandleChoice{Handle: "reviewer-2", SetLabel: true}},
		{"a suffixed explicit label skips handles with someone's unread mail",
			HandleRequest{Kind: Codex, Label: "reviewer", Live: set("reviewer"), Encumbered: set("reviewer-2")},
			HandleChoice{Handle: "reviewer-3", SetLabel: true}},
		{"explicit label beats the block's previous handle",
			HandleRequest{Kind: Codex, Label: "auditor", Sticky: "codex-2"},
			HandleChoice{Handle: "auditor"}},
		{"sticky handle is kept for the same block",
			HandleRequest{Kind: Claude, Label: "shell", Sticky: "claude-4", Encumbered: set("claude-4")},
			HandleChoice{Handle: "claude-4", SetLabel: true}},
		{"sticky handle taken by another live agent is not shared",
			HandleRequest{Kind: Claude, Label: "shell", Sticky: "claude", Live: set("claude")},
			HandleChoice{Handle: "claude-2", SetLabel: true}},
		{"custom label that is not a handle is never overwritten",
			HandleRequest{Kind: Codex, Label: "my build pane"},
			HandleChoice{Handle: "codex"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ChooseHandle(c.req); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSuffixedHandleStaysValid(t *testing.T) {
	long := strings.Repeat("a", 32)
	got := ChooseHandle(HandleRequest{Kind: Codex, Label: long, Live: set(Handle(long))})
	if _, err := ParseHandle(string(got.Handle)); err != nil {
		t.Fatalf("suffixed handle %q is invalid: %v", got.Handle, err)
	}
	if got.Handle == Handle(long) {
		t.Fatalf("live handle was shared")
	}
}

func TestParseHandle(t *testing.T) {
	for _, ok := range []string{"codex", "codex-2", "a_b", "1abc"} {
		if _, err := ParseHandle(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Codex", "x.y", "-lead", "has space", strings.Repeat("a", 33)} {
		if _, err := ParseHandle(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckBinding(t *testing.T) {
	cases := []struct {
		b    Binding
		ok   bool
		want Progress
		code string
	}{
		{Binding{BlockGone: true}, false, Failed, "block_gone"},
		{Binding{ForegroundPID: 9, RecordPID: 9}, true, "", ""},
		{Binding{ForegroundPID: 7, RecordPID: 9, AgentGone: true}, false, Failed, "agent_gone"},
		// The agent opened an editor or pager: transient, retried.
		{Binding{ForegroundPID: 7, RecordPID: 9}, false, Deferred, "agent_not_foreground"},
	}
	for _, c := range cases {
		o, ok := CheckBinding(c.b)
		if ok != c.ok || o.Progress != c.want || o.Code != c.code {
			t.Errorf("%+v: got %+v ok=%v", c.b, o, ok)
		}
	}
}

func TestDecideWrite(t *testing.T) {
	cases := []struct {
		r     Readiness
		write bool
		code  string
	}{
		{Readiness{State: StateIdle, Box: screen.Empty}, true, ""},
		{Readiness{State: StateDone, Box: screen.Empty}, true, ""},
		{Readiness{State: StateError, Box: screen.Empty}, true, ""},
		{Readiness{State: StateAbsent, Box: screen.Empty}, true, ""},
		{Readiness{State: StateWorking, Box: screen.Empty}, false, "agent_working"},
		{Readiness{State: StateBlocked, BlockedKind: "question", Box: screen.Unknown}, false, "agent_blocked"},
		{Readiness{State: "paused", Box: screen.Empty}, false, "agent_state_unknown"},
		{Readiness{State: StateIdle, Box: screen.Typed}, false, "draft_in_box"},
		{Readiness{State: StateIdle, Box: screen.Unknown}, false, "box_not_found"},
		{Readiness{State: StateIdle, Box: screen.Empty, Trust: true}, false, "trust_screen"},
	}
	for _, c := range cases {
		o, write := DecideWrite(c.r)
		if write != c.write || o.Code != c.code {
			t.Errorf("%+v: got write=%v code=%q, want write=%v code=%q", c.r, write, o.Code, c.write, c.code)
		}
		if !write && o.Progress != Deferred {
			t.Errorf("%+v: refusal must defer, got %s", c.r, o.Progress)
		}
	}
}

func TestDecideConfirm(t *testing.T) {
	cases := []struct {
		c       Confirmation
		want    Progress
		cleanup Cleanup
	}{
		{Confirmation{Reports: true, Started: true}, Accepted, NoCleanup},
		{Confirmation{Reports: true, Box: screen.Typed, OursInBox: true}, Deferred, ClearOurText},
		{Confirmation{Reports: true, Box: screen.Typed}, Deferred, NoCleanup},
		{Confirmation{Reports: true, Box: screen.Empty}, Deferred, NoCleanup},
		// Without status reports one empty poll may precede the harness drawing the paste.
		{Confirmation{Reports: false, Box: screen.Empty}, Deferred, NoCleanup},
		{Confirmation{Reports: false, Box: screen.Empty, EmptiedTwice: true}, Accepted, NoCleanup},
		{Confirmation{Reports: true, Box: screen.Empty, EmptiedTwice: true}, Deferred, NoCleanup},
		{Confirmation{Reports: false, Box: screen.Typed, OursInBox: true}, Deferred, ClearOurText},
	}
	for _, c := range cases {
		o, cl := DecideConfirm(c.c)
		if o.Progress != c.want || cl != c.cleanup {
			t.Errorf("%+v: got %s/%v, want %s/%v", c.c, o.Progress, cl, c.want, c.cleanup)
		}
	}
}

func TestOutcomeExitCode(t *testing.T) {
	if (Outcome{Progress: Accepted}).ExitCode() != 0 {
		t.Error("accepted must exit 0")
	}
	if (Outcome{Progress: Deferred}).ExitCode() == 0 || (Outcome{Progress: Failed}).ExitCode() == 0 {
		t.Error("deferred and failed must exit non-zero")
	}
}

func TestDoorbellCarriesNoMessageContent(t *testing.T) {
	d := Doorbell(2, "claude-2", "/x/rex-amq-adapter env")
	want := `AMQ doorbell: 2 unread message(s) for claude-2 (you are claude-2 in Rex; skill: rex-amq-adapter). Run eval "$(/x/rex-amq-adapter env)" && amq drain --include-body, then act on each message.`
	if d != want {
		t.Fatalf("got %q\nwant %q", d, want)
	}
	if strings.ContainsAny(d, "\n\r\t\x1b") {
		t.Fatal("doorbell must be one line")
	}
}

func TestDecideAnnounce(t *testing.T) {
	ann := map[string]bool{"m1": true}
	cases := []struct {
		name  string
		in    AnnounceInput
		speak bool
		fresh int
	}{
		{"no mail, no notice", AnnounceInput{Event: ToolFinished}, false, 0},
		{"new mail mid-turn is announced", AnnounceInput{Event: ToolFinished, Unread: []string{"m1", "m2"}, Announced: ann}, true, 1},
		{"announced mail is not repeated mid-turn", AnnounceInput{Event: ToolFinished, Unread: []string{"m1"}, Announced: ann}, false, 0},
		{"announced mail is not repeated on a prompt", AnnounceInput{Event: PromptSubmitted, Unread: []string{"m1"}, Announced: ann}, false, 0},
		{"stopping with unread mail blocks once", AnnounceInput{Event: TurnEnding, Unread: []string{"m1"}, Announced: ann}, true, 0},
		{"an active stop block does not loop on the same mail", AnnounceInput{Event: TurnEnding, Unread: []string{"m1"}, Announced: ann, StopHookActive: true}, false, 0},
		{"an active stop block still announces new mail", AnnounceInput{Event: TurnEnding, Unread: []string{"m1", "m3"}, Announced: ann, StopHookActive: true}, true, 1},
		{"stopping with no mail does not block", AnnounceInput{Event: TurnEnding, Announced: ann}, false, 0},
	}
	for _, c := range cases {
		a := DecideAnnounce(c.in)
		if a.Speak != c.speak || a.Fresh != c.fresh || a.Unread != len(c.in.Unread) {
			t.Errorf("%s: got %+v, want speak=%v fresh=%d", c.name, a, c.speak, c.fresh)
		}
	}
}

func TestMergeAnnouncedDropsDrainedIDs(t *testing.T) {
	got := MergeAnnounced(map[string]bool{"old": true, "m1": true}, []string{"m3", "m2"}, []string{"m3", "m1", "m2", "m4"})
	if strings.Join(got, ",") != "m1,m2,m3" {
		t.Fatalf("got %v", got)
	}
}

func TestHookNoticeCarriesNoMessageContent(t *testing.T) {
	n := HookNotice(Announcement{Speak: true, Unread: 3, Fresh: 1}, "codex", "/x env", TurnEnding)
	for _, want := range []string{"3 unread", "(1 new)", "codex", `eval "$(/x env)"`, "amq drain --include-body", "before ending your turn", "skill: rex-amq-adapter"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice %q lacks %q", n, want)
		}
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range []string{"claude", "codex", "agy", "pi"} {
		if got, err := ParseKind(k); err != nil || string(got) != k {
			t.Errorf("%s: %v %v", k, got, err)
		}
	}
	for _, k := range []string{"", "gemini", "Claude"} {
		if _, err := ParseKind(k); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestOnlyPiDeliversInProcess(t *testing.T) {
	for _, k := range []Kind{Claude, Codex, Agy} {
		if k.InProcess() {
			t.Errorf("%s must be woken by amq wake", k)
		}
	}
	if !Pi.InProcess() {
		t.Error("pi delivers in-process")
	}
}
