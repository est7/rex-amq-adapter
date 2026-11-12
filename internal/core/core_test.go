package core

import (
	"regexp"
	"slices"
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

// A handle is the agent's kind and the lowest free number; the role lives in
// the record, so a pane label never shapes it.
func TestNameHandle(t *testing.T) {
	cases := []struct {
		name  string
		kind  Kind
		taken map[Handle]bool
		want  Handle
	}{
		{"the first of a kind", Codex, nil, "codex-01"},
		{"taken handles are skipped, never shared", Codex, set("codex-01", "codex-02"), "codex-03"},
		{"the lowest free number is reused", Claude, set("claude-02"), "claude-01"},
		{"other kinds' numbers do not count", Pi, set("claude-01", "codex-01"), "pi-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NameHandle(c.kind, c.taken); got != c.want {
				t.Fatalf("NameHandle(%s) = %q, want %q", c.kind, got, c.want)
			}
		})
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
	d := Doorbell(2, "claude-2", "/x/rex-amq-adapter")
	want := `AMQ doorbell: 2 unread message(s) for claude-2 (you are claude-2 in Rex; skill: rex-amq-adapter). Run /x/rex-amq-adapter drain --include-body, then act on each message.`
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
	n := HookNotice(Announcement{Speak: true, Unread: 3, Fresh: 1}, "codex", "/x/rex-amq-adapter", TurnEnding)
	if bareAmq.MatchString(n) {
		t.Errorf("notice teaches bare amq mail, which skips the gateway: %q", n)
	}
	for _, want := range []string{"3 unread", "(1 new)", "codex", "/x/rex-amq-adapter drain --include-body", "before ending your turn", "skill: rex-amq-adapter"} {
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

func TestSessionMatesAreTheOtherAgentsInTheSameSession(t *testing.T) {
	windowOf := map[string]string{"b:self": "w1", "b:right": "w1", "b:below": "w1", "b:tab2": "w2"}
	peers := []Peer{
		{Handle: "codex-01", Kind: Codex, Block: "b:right"},
		{Handle: "pi-01", Kind: Pi, Block: "b:tab2"},
		{Handle: "claude-02", Kind: Claude, Block: "b:self"}, // replaced in this block
		{Handle: "agy-01", Kind: Agy, Block: "b:below"},
		{Handle: "codex-02", Kind: Codex, Block: "b:other-session"},
	}
	got := SessionMates("b:self", windowOf, peers)
	if len(got) != 3 || got[0].Handle != "codex-01" || got[1].Handle != "pi-01" || got[2].Handle != "agy-01" {
		t.Fatalf("got %+v, want codex-01, pi-01 (another tab), and agy-01", got)
	}
	if got := SessionMates("b:self", nil, peers); got != nil {
		t.Fatalf("an unknown session has no mates, got %+v", got)
	}
	got = []Peer{got[0], got[2]}
	if s := MatesNotice(got); !strings.Contains(s, "codex-01 (codex), agy-01 (agy)") || !strings.Contains(s, "rex-amq-adapter send --to <handle>") {
		t.Fatalf("mates notice %q", s)
	}
	if MatesNotice(nil) != "" {
		t.Fatal("no mates must say nothing")
	}
	if s := JoinedNotice(got[0]); !strings.Contains(s, "codex-01 (codex) joined") || !strings.Contains(s, "rex-amq-adapter send --to codex-01") {
		t.Fatalf("joined notice %q", s)
	}
}

func TestUndeliveredNoticeSaysOnlyWhatIsKnown(t *testing.T) {
	alone := UndeliveredNotice("claude-26", nil, nil)
	if !strings.Contains(alone, "mail to claude-26") || !strings.Contains(alone, "rex-amq-adapter who") || strings.Contains(alone, "pane") {
		t.Errorf("alone: %q", alone)
	}
	next := Peer{Handle: "claude-33", Kind: Claude}
	full := UndeliveredNotice("claude-26", &next, []Peer{next})
	if !strings.Contains(full, "claude-33 (claude) now runs in its pane") || !strings.Contains(full, "Agents beside you") || strings.Contains(full, "rex-amq-adapter who") {
		t.Errorf("replaced: %q", full)
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

func TestDeliveryStatusOnlyCountsARefusedDoorbellWhileMailWaits(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	at := func(ago time.Duration, p Progress, code string) []Attempt {
		return []Attempt{{At: now.Add(-ago), Outcome: Outcome{Progress: p, Code: code}}}
	}
	cases := []struct {
		name    string
		history []Attempt
		unread  int
		want    string
	}{
		{"drained after a deferral", at(48*time.Hour, Deferred, "agent_working"), 0, "-"},
		{"mail still waits", at(23*time.Minute, Deferred, "box_not_found"), 2, "deferred box_not_found 23m"},
		{"drained after a failure", at(time.Hour, Failed, "block_gone"), 0, "-"},
		{"an accepted doorbell stays", at(2*time.Minute, Accepted, ""), 0, "accepted 2m"},
		{"no doorbell yet", nil, 0, "-"},
	}
	for _, c := range cases {
		if got := DeliveryStatus(c.history, c.unread, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecideStartup(t *testing.T) {
	cases := []struct {
		name string
		o    StartupObservation
		want StartupStatus
		done bool
	}{
		{"adopted wins over a stale dialog on screen", StartupObservation{Handle: "claude-01", TrustPhrase: "Hooks need review"}, StartupAdopted, true},
		{"block gone", StartupObservation{BlockGone: true}, StartupExited, true},
		{"trust dialog", StartupObservation{TrustPhrase: "Trust this folder?", State: StateBlocked}, StartupTrust, true},
		{"blocked", StartupObservation{State: StateBlocked, BlockedKind: "permission"}, StartupBlocked, true},
		{"working: keep waiting", StartupObservation{State: StateWorking}, "", false},
		{"nothing yet: keep waiting", StartupObservation{}, "", false},
	}
	for _, c := range cases {
		got, done := DecideStartup(c.o)
		if done != c.done || got.Status != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got.Status, done, c.want, c.done)
		}
	}
}

func TestDepartureMailGoesToTheOrchestratorOrItsReportingPeer(t *testing.T) {
	orch := Peer{Handle: "claude-02", Kind: Claude, Role: OrchestratorRole}
	goal2 := Peer{Handle: "codex-02", Kind: Codex, Role: OrchestratorRole}
	pi := Peer{Handle: "pi-01", Kind: Pi}
	agy := Peer{Handle: "agy-01", Kind: Agy}
	claude := Peer{Handle: "claude-01", Kind: Claude}
	codex := Peer{Handle: "codex-01", Kind: Codex}
	for name, tc := range map[string]struct {
		left    Peer
		others  []Peer
		to      []Handle
		subject string
	}{
		"a peer leaves":            {claude, []Peer{pi, orch}, []Handle{orch.Handle}, "claude-01 left your Rex session"},
		"every orchestrator hears": {claude, []Peer{orch, goal2}, []Handle{orch.Handle, goal2.Handle}, "claude-01 left your Rex session"},
		"no orchestrator":          {claude, []Peer{pi, codex}, nil, ""},
		"goal leaves: prefers one": {orch, []Peer{pi, claude}, []Handle{claude.Handle}, "claude-02 left your Rex session"},
		"goal leaves: codex too":   {orch, []Peer{agy, pi, codex}, []Handle{codex.Handle}, "claude-02 left your Rex session"},
		"goal leaves: keeps first": {orch, []Peer{codex, claude}, []Handle{codex.Handle}, "claude-02 left your Rex session"},
		"goal leaves: none notify": {orch, []Peer{agy, pi}, []Handle{agy.Handle}, "claude-02 left your Rex session"},
		"goal leaves alone":        {orch, nil, nil, ""},
	} {
		to, subject, body := DepartureMail(tc.left, "s", tc.others)
		if !slices.Equal(to, tc.to) || subject != tc.subject {
			t.Errorf("%s: got %v %q want %v %q", name, to, subject, tc.to, tc.subject)
		}
		if (len(to) > 0) != (body != "") {
			t.Errorf("%s: body %q for %v", name, body, to)
		}
	}
	if _, _, body := DepartureMail(orch, "s", []Peer{codex}); !strings.Contains(body, "do not take over") {
		t.Errorf("a peer is not told to stay out of the goal: %q", body)
	}
	if _, _, body := DepartureMail(codex, "sess-1", []Peer{orch}); !strings.Contains(body, "sess-1") {
		t.Errorf("the orchestrator is not told the session: %q", body)
	}
}

func TestPaneLabelIsTheRoleAndHandleThenTheTask(t *testing.T) {
	for _, c := range []struct {
		h          Handle
		role, task string
		want       string
	}{
		{"claude-04", "", "", "claude-04"},
		{"claude-04", "", "fix login", "claude-04 | fix login"},
		{"claude-04", "architect", "", "architect(claude-04)"},
		{"claude-04", "architect", "stream design", "architect(claude-04) | stream design"},
	} {
		if got := PaneLabel(c.h, c.role, c.task); got != c.want {
			t.Errorf("PaneLabel(%s, %q, %q) = %q, want %q", c.h, c.role, c.task, got, c.want)
		}
	}
}

// What the adapter writes reads back as written: the role it shows, the
// handle it names, and no task for the next agent in the pane.
func TestAdapterLabelsReadBack(t *testing.T) {
	for _, c := range []struct {
		h          Handle
		role, task string
	}{{"claude-04", "", "x"}, {"codex-12", "reviewer", "fix login"}, {"agy-01", "v2-audit", ""}, {"pi-03", "orchestrator", "ship"}} {
		label := PaneLabel(c.h, c.role, c.task)
		if got := Role(label); got != c.role {
			t.Errorf("Role(%q) = %q, want %q", label, got, c.role)
		}
		if LauncherLabel(label) || InitialTask(label) != "" {
			t.Errorf("%q was taken for a launcher's label", label)
		}
		if !TitleNamesHandle("✳ "+label, c.h) {
			t.Errorf("title %q does not name %s", label, c.h)
		}
		if other := Handle(string(c.h) + "9"); TitleNamesHandle(label, other) {
			t.Errorf("title %q names %s", label, other)
		}
	}
	// A role taken since the harness drew its title still names the agent.
	if !TitleNamesHandle("claude-04 | old", "claude-04") || !TitleNamesHandle("implementer(claude-04) | old", "claude-04") {
		t.Error("a title drawn before a role change no longer names its agent")
	}
	if TitleNamesHandle("(claude-04)", "claude-04") || TitleNamesHandle("fix claude-04", "claude-04") {
		t.Error("a title that is not a pane label named an agent")
	}
}

func TestTaskFitsAPaneLabel(t *testing.T) {
	for in, want := range map[string]string{
		"  fix   login\nredirect ":          "fix login redirect",
		"a | b":                             "a / b",
		"   ":                               "",
		"exactly thirty-two columns long!":  "exactly thirty-two columns long!",
		"one column too long for the label": "one column too long for the lab…",
		"修复登录跳转并补上回归测试然后提交合并请求": "修复登录跳转并补上回归测试然后…",
		strings.Repeat("🚀", 17): strings.Repeat("🚀", 15) + "…",
		strings.Repeat("🚀", 16): strings.Repeat("🚀", 16),
	} {
		if got := Task(in); got != want {
			t.Errorf("Task(%q) = %q, want %q", in, got, want)
		}
	}
}

// A launcher's label names the role; its task, the agent's numbering, and
// Rex's defaults do not.
func TestRoleOfALaunchersLabel(t *testing.T) {
	for label, want := range map[string]string{
		"reviewer | 修复登录": "reviewer",
		"reviewer|x":      "reviewer",
		"Reviewer":        "reviewer",
		"reviewer-2":      "reviewer",
		"Lark Impl":       "lark-impl",
		"v2-audit":        "v2-audit",
		"orchestrator":    "orchestrator",
		"shell":           "",
		"codex":           "",
		"":                "",
		"| fix login":     "",
	} {
		if got := Role(label); got != want {
			t.Errorf("Role(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestShownTitleDropsTheStateGlyph(t *testing.T) {
	for in, want := range map[string]string{
		"✳ claude-07 | fix login":             "claude-07 | fix login",
		"◐ Adapter spike brief for claude-89": "Adapter spike brief for claude-89",
		"probe job j1":                        "probe job j1",
		"  claude-07  ":                       "claude-07",
		"2 awaiting input · claude agents":    "2 awaiting input · claude agents",
		"":                                    "",
	} {
		if got := ShownTitle(in); got != want {
			t.Errorf("ShownTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAShownBackgroundJobIsBound(t *testing.T) {
	if _, ok := CheckBinding(Binding{ForegroundPID: 10, RecordPID: 20, JobShown: true}); !ok {
		t.Error("a job its pane's client shows is bound")
	}
	if o, ok := CheckBinding(Binding{ForegroundPID: 10, RecordPID: 20}); ok || o.Code != "agent_not_foreground" {
		t.Errorf("a job its pane does not show: %+v %v", o, ok)
	}
}

func TestAgentViewTitle(t *testing.T) {
	for _, title := range []string{"2 awaiting input · claude agents", "✳ 1 awaiting input · claude agents", "claude agents"} {
		if !AgentViewTitle(title) {
			t.Errorf("agent view %q not recognised", title)
		}
	}
	for _, title := range []string{"claude-07 | write the claude agents doc", "probe job j1", "notes · claude agents", "many awaiting input · claude agents"} {
		if AgentViewTitle(title) {
			t.Errorf("a conversation's title %q taken for agent view", title)
		}
	}
	if !TitleNamesHandle("✳ claude-07", "claude-07") || !TitleNamesHandle("claude-07 | old task", "claude-07") ||
		TitleNamesHandle("claude-070", "claude-07") || TitleNamesHandle("claude-07x | t", "claude-07") {
		t.Error("TitleNamesHandle")
	}
}

func TestAPaneThatShowsSomethingElse(t *testing.T) {
	if o, ok := CheckBinding(Binding{ForegroundPID: 10, RecordPID: 10, LeftPane: true}); ok || o.Progress != Failed || o.Code != "agent_left_pane" {
		t.Errorf("agent view in the agent's own process: %+v %v", o, ok)
	}
}

func TestOrchestratorNoticeNamesEveryOrchestratorAndAFallback(t *testing.T) {
	if OrchestratorNotice([]Peer{{Handle: "codex-01", Role: "reviewer"}}) != "" {
		t.Fatal("a session without an orchestrator got its notice")
	}
	one := OrchestratorNotice([]Peer{{Handle: "codex-01", Role: "reviewer"}, {Handle: "agy-02", Role: OrchestratorRole}})
	if !strings.Contains(one, "send it to agy-02 instead") || !strings.Contains(one, "overrides those texts") {
		t.Fatalf("one orchestrator: %q", one)
	}
	for _, want := range []string{"the orchestrator's to say", "nobody approves it or works around it", "report chain"} {
		if !strings.Contains(one, want) {
			t.Errorf("the notice lacks %q: %q", want, one)
		}
	}
	two := OrchestratorNotice([]Peer{{Handle: "agy-02", Role: OrchestratorRole}, {Handle: "claude-03", Role: OrchestratorRole}})
	if !strings.Contains(two, "one of agy-02, claude-03; if neither did, agy-02") {
		t.Fatalf("two orchestrators: %q", two)
	}
}

// startParts is a start notice's fixed parts as a hook composes them: the
// identity and mail line with five mates and unread mail, the task line,
// and the orchestrator's routing line.
func startParts(role string) (head, tail string) {
	mates := []Peer{{Handle: "claude-01", Kind: Claude, Role: OrchestratorRole}, {Handle: "codex-03", Kind: Codex, Role: "implementer"}, {Handle: "codex-04", Kind: Codex, Role: "qa"}, {Handle: "claude-05", Kind: Claude, Role: "writer"}, {Handle: "claude-06", Kind: Claude, Role: "architect"}}
	bin := "/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter"
	head = SessionNotice("codex-02", role, bin, 1) + " " + MatesNotice(mates) + "\n" + TaskNotice("codex-02", role, bin+" title", "review guidance")
	return head, OrchestratorNotice(mates)
}

// Claude Code replaces a hook's context longer than ContextCap with a file
// path and a 2,000-character preview, losing what follows. A start notice
// therefore keeps within StartBudget: identity, mail and routing stay
// inline, and a prompt that does not fit is named by its file (codex-01's
// s1 review: two 4,000-character custom prompts made 10,075).
func TestStartNoticeKeepsWithinTheHookBudget(t *testing.T) {
	head, tail := startParts("reviewer")
	conv, role := strings.Repeat("x", MaxInline), strings.Repeat("y", MaxInline)
	b := Briefing{Crew: "custom", Conventions: conv, ConventionsPath: "/c/team-conventions.md", Role: "reviewer", RolePrompt: role, RolePath: "/c/reviewer.md"}
	n := StartNotice(head, b, tail)
	if l := ContextLen(n); l > StartBudget {
		t.Fatalf("notice is %d long, budget %d", l, StartBudget)
	}
	if !strings.HasPrefix(n, head) || !strings.HasSuffix(n, tail) || tail == "" {
		t.Fatalf("identity, mail or routing left the notice:\n%.300s … %.300s", n, n[max(0, len(n)-300):])
	}
	inline, named := 0, 0
	for _, p := range []struct{ text, path string }{{conv, b.ConventionsPath}, {role, b.RolePath}} {
		switch {
		case strings.Contains(n, p.text):
			inline++
		case strings.Contains(n, "read "+p.path):
			named++
		}
	}
	if inline != 1 || named != 1 {
		t.Errorf("want one prompt inline and one named by its file, got %d inline, %d named", inline, named)
	}
	// What fits is composed as before: head, briefing, tail.
	small := Briefing{Crew: "pair", Conventions: "Review first.", Role: "reviewer", RolePrompt: "You review."}
	if got, want := StartNotice(head, small, tail), head+"\n\n"+BriefNotice(small)+"\n\n"+tail; got != want {
		t.Errorf("a notice within budget changed:\n%q\nwant\n%q", got, want)
	}
	if got := StartNotice(head, Briefing{}, ""); got != head {
		t.Errorf("no briefing and no routing: %q", got)
	}
}

func TestContextLenCountsUTF16Units(t *testing.T) {
	if ContextLen("ab") != 2 || ContextLen("队列") != 2 || ContextLen("🙂") != 2 {
		t.Error("ContextLen counts as JavaScript's String length does")
	}
}

// bareAmq matches an instruction to run amq's own mail commands, which skip
// the adapter's gateway (ARCH-1).
var bareAmq = regexp.MustCompile(`\bamq (drain|send|reply)\b`)
