package core

import (
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/screen"
)

func set(hs ...Handle) map[Handle]bool {
	m := map[Handle]bool{}
	for _, h := range hs {
		m[h] = true
	}
	return m
}

func TestNameHandle(t *testing.T) {
	cases := []struct {
		name  string
		kind  Kind
		label string
		taken map[Handle]bool
		want  Handle
	}{
		{"default label gets the kind and a number", Codex, "shell", nil, "codex-01"},
		{"empty label counts as default", Claude, "", nil, "claude-01"},
		{"a label is the role", Codex, "reviewer", nil, "codex-reviewer-01"},
		{"the role is lowercased and folded", Claude, "Lark Impl", nil, "claude-lark-impl-01"},
		{"taken handles are skipped, never shared", Codex, "reviewer", set("codex-reviewer-01", "codex-reviewer-02"), "codex-reviewer-03"},
		{"the lowest free number is reused", Claude, "shell", set("claude-02"), "claude-01"},
		{"a composed handle reads back as its role", Codex, "codex-reviewer-01", nil, "codex-reviewer-01"},
		{"a pane relabeled for another kind keeps the role, not the kind", Claude, "codex-reviewer-01", nil, "claude-reviewer-01"},
		{"an old automatic name carries no role", Claude, "codex-10", nil, "claude-01"},
		{"a bare kind carries no role", Pi, "codex", nil, "pi-01"},
		{"a peer's own numbering is dropped", Codex, "reviewer-2", nil, "codex-reviewer-01"},
		{"inner numbers stay part of the role", Agy, "v2-audit", nil, "agy-v2-audit-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NameHandle(c.kind, c.label, c.taken); got != c.want {
				t.Fatalf("NameHandle(%s, %q) = %q, want %q", c.kind, c.label, got, c.want)
			}
		})
	}
}

func TestLongRoleStaysAValidHandle(t *testing.T) {
	long := strings.Repeat("ab-", 20)
	taken := map[Handle]bool{}
	for i := 0; i < 120; i++ {
		h := NameHandle(Claude, long, taken)
		if _, err := ParseHandle(string(h)); err != nil {
			t.Fatalf("handle %q is invalid: %v", h, err)
		}
		if !strings.HasPrefix(string(h), "claude-ab-") || taken[h] {
			t.Fatalf("handle %q lost its role or was shared", h)
		}
		taken[h] = true
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
		{Readiness{State: StateBlocked, Box: screen.Typed}, false, "agent_blocked"}, // Codex approval: no kind
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
		{"an idle agent is reminded of undrained mail after RemindAfter", AnnounceInput{Event: PromptSubmitted, Unread: []string{"m1"}, Announced: ann, Idle: true, Since: RemindAfter}, true, 0},
		{"an idle agent is not reminded sooner", AnnounceInput{Event: PromptSubmitted, Unread: []string{"m1"}, Announced: ann, Idle: true, Since: RemindAfter - time.Second}, false, 0},
		{"a busy agent is not reminded", AnnounceInput{Event: PromptSubmitted, Unread: []string{"m1"}, Announced: ann, Since: time.Hour}, false, 0},
		{"no mail, no reminder", AnnounceInput{Event: PromptSubmitted, Announced: ann, Idle: true, Since: time.Hour}, false, 0},
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

func TestBlockedNoteNamesTheKindOnlyWhenKnown(t *testing.T) {
	if o, _ := DecideWrite(Readiness{State: StateBlocked}); o.Note != "the agent waits on the user" {
		t.Errorf("no kind: %q", o.Note)
	}
	if o, _ := DecideWrite(Readiness{State: StateBlocked, BlockedKind: "permission"}); o.Note != "the agent waits on the user (permission)" {
		t.Errorf("with kind: %q", o.Note)
	}
}

func TestLastDelivery(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	at := func(min int, p Progress, code string) Attempt {
		return Attempt{At: t0.Add(time.Duration(min) * time.Minute), Outcome: Outcome{Progress: p, Code: code}}
	}
	now := t0.Add(30 * time.Minute)
	cases := []struct {
		name    string
		history []Attempt
		want    string
	}{
		{"no doorbell yet", nil, "-"},
		{"accepted", []Attempt{at(28, Accepted, "")}, "accepted 2m"},
		// The streak starts at the first of the identical outcomes, not the last.
		{"stuck on the same reason", []Attempt{at(1, Accepted, ""), at(7, Deferred, "box_not_found"), at(15, Deferred, "box_not_found"), at(29, Deferred, "box_not_found")}, "deferred box_not_found 23m"},
		{"a different reason restarts the streak", []Attempt{at(2, Deferred, "agent_working"), at(26, Deferred, "draft_in_box")}, "deferred draft_in_box 4m"},
		{"seconds", []Attempt{{At: now.Add(-40 * time.Second), Outcome: Outcome{Progress: Failed, Code: "block_gone"}}}, "failed block_gone 40s"},
	}
	for _, c := range cases {
		if got := LastDelivery(c.history, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if Ago(3*24*time.Hour) != "3d" || Ago(5*time.Hour) != "5h" {
		t.Errorf("Ago: %s %s", Ago(3*24*time.Hour), Ago(5*time.Hour))
	}
}

func TestWindowMatesAreTheOtherAgentsInTheSameWindow(t *testing.T) {
	windowOf := map[string]string{"b:self": "w1", "b:right": "w1", "b:below": "w1", "b:tab2": "w2"}
	peers := []Peer{
		{Handle: "codex-01", Kind: Codex, Block: "b:right"},
		{Handle: "pi-01", Kind: Pi, Block: "b:tab2"},
		{Handle: "claude-02", Kind: Claude, Block: "b:self"}, // replaced in this block
		{Handle: "agy-01", Kind: Agy, Block: "b:below"},
		{Handle: "codex-02", Kind: Codex, Block: "b:other-session"},
	}
	got := WindowMates("b:self", "w1", windowOf, peers)
	if len(got) != 2 || got[0].Handle != "codex-01" || got[1].Handle != "agy-01" {
		t.Fatalf("got %+v, want codex-01 and agy-01", got)
	}
	if got := WindowMates("b:self", "", windowOf, peers); got != nil {
		t.Fatalf("an unknown window has no mates, got %+v", got)
	}
	if s := MatesNotice(got); !strings.Contains(s, "codex-01 (codex), agy-01 (agy)") || !strings.Contains(s, "amq send") {
		t.Fatalf("mates notice %q", s)
	}
	if MatesNotice(nil) != "" {
		t.Fatal("no mates must say nothing")
	}
	if s := JoinedNotice(got[0]); !strings.Contains(s, "codex-01 (codex) joined") || !strings.Contains(s, "amq send --to codex-01") {
		t.Fatalf("joined notice %q", s)
	}
}

func TestBriefNoticeCarriesConventionsThenRole(t *testing.T) {
	b := Briefing{Crew: "pair", Conventions: "Review before commit.", Role: "reviewer", RolePrompt: "You are the reviewer."}
	s := BriefNotice(b)
	conv, role := strings.Index(s, "Review before commit."), strings.Index(s, "You are the reviewer.")
	if conv < 0 || role < conv || !strings.Contains(s, `crew "pair"`) || !strings.Contains(s, "role here is reviewer") {
		t.Fatalf("briefing %q", s)
	}
	if BriefNotice(Briefing{Role: "reviewer"}) != "" {
		t.Fatal("a role without a prompt says nothing")
	}
	long := strings.Repeat("x", MaxInline+1)
	if s := BriefNotice(Briefing{Role: "r", RolePrompt: long, RolePath: "/p/r.md"}); strings.Contains(s, long) || !strings.Contains(s, "read /p/r.md") {
		t.Fatalf("a long file must be named, not inlined: %.80q", s)
	}
}
