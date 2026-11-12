package screen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEverySupportedKindTellsAnEmptyBoxFromADraft(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "cursor", "gemini", "opencode", "pi"} {
		if got := Check(kind, fixture(t, kind+"-empty")); got != Empty {
			t.Errorf("%s empty: got %v", kind, got)
		}
		if got := Check(kind, fixture(t, kind+"-draft")); got != Typed {
			t.Errorf("%s draft: got %v", kind, got)
		}
	}
	for _, tc := range []struct{ kind, name string }{
		{"claude", "claude-empty-after-turn"},
		{"opencode", "opencode-empty-session"},
	} {
		if got := Check(tc.kind, fixture(t, tc.name)); got != Empty {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

// Claude keeps its input box while working: the dim "Press up to edit queued
// messages" hint is not a draft, text the user types meanwhile is.
func TestClaudeWhileWorking(t *testing.T) {
	for name, want := range map[string]Draft{
		"claude-working":        Empty,
		"claude-working-queued": Empty,
		"claude-working-draft":  Typed,
	} {
		if got := Check("claude", fixture(t, name)); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
}

func TestScreenWithoutTheBoxOrUnknownKindIsUnknown(t *testing.T) {
	if got := Check("copilot", fixture(t, "claude-empty")); got != Unknown {
		t.Errorf("unknown kind: got %v", got)
	}
	if got := Check("claude", "some output\nno box here\n"); got != Unknown {
		t.Errorf("no box: got %v", got)
	}
	if got := Check("codex", fixture(t, "claude-empty")); got != Unknown {
		t.Errorf("wrong kind layout: got %v", got)
	}
}

func TestMenusAndMultiLineDraftsCountAsTyped(t *testing.T) {
	rule := strings.Repeat("─", 40)
	for name, tc := range map[string]struct {
		kind, screen string
		want         Draft
	}{
		// Codex's trust menu uses the same `›` for its selection.
		"codex menu":  {"codex", "  Do you trust the contents of this directory?\n\x1b[1m›\x1b[0m 1. Yes, continue\n  2. No, quit\n", Typed},
		"multi-line":  {"claude", rule + "\n❯ \n  second line typed\n" + rule + "\n", Typed},
		"placeholder": {"claude", rule + "\n❯ \x1b[2mTry \"fix lint\"\x1b[0m\n" + rule + "\n", Empty},
		// A transcript line with `❯` that is not under a rule is not the box.
		"transcript": {"claude", "❯ an earlier prompt\n\n" + rule + "\n❯ \n" + rule + "\n", Empty},
		// A typed character under the harness cursor is text; a cursor on a
		// dim placeholder is not.
		"cursor on text":        {"pi", rule + "\n\x1b[7mx\x1b[0m   \n" + rule + "\n", Typed},
		"cursor on placeholder": {"cursor", "  \x1b[2m→ \x1b[0m\x1b[7mP\x1b[0m\x1b[2mlan, search\x1b[0m\n", Empty},
	} {
		if got := Check(tc.kind, tc.screen); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

func TestEachHarnessTrustScreenIsRecognized(t *testing.T) {
	for name, tc := range map[string]struct{ kind, screen, want string }{
		"claude": {"claude", "╭────╮\n│ Accessing workspace:\n│ Quick safety check: Is this a project you created or one you trust? (Like your own code)\n│ \x1b[1m❯\x1b[0m 1. Yes, I trust this folder\n│   2. No, exit\n╰────╯\n", "Quick safety check: Is this a project you created or one you trust"},
		// Rex's VT capture draws a space between words as a cursor move.
		"claude capture": {"", "\x1b[1CQuick\x1b[1Csafety\x1b[1Ccheck:\x1b[1CIs\x1b[1Cthis\x1b[1Ca\x1b[1Cproject\x1b[1Cyou\x1b[1Ccreated\x1b[1Cor\x1b[1Cone\x1b[1Cyou\x1b[1Ctrust?\r\n\x1b[1C\x1b[1m\x1b[38;2;153;153;153m❯\x1b[1CNo,\x1b[1Cexit\x1b[0m\r\n\x1b[3CYes,\x1b[1CI\x1b[1Ctrust\x1b[1Cthis\x1b[1Cfolder\r\n", "Quick safety check: Is this a project you created or one you trust"},
		"codex folder":   {"codex", "  Trust this folder? Codex can read, edit, and run files here.\n\x1b[1m›\x1b[0m 1. Trust and continue\n  2. Open restricted\n", "Trust this folder?"},
		"codex hooks":    {"codex", "  Hooks need review\n  1 hook is new or changed.\n› 1. Review hooks\n  2. Trust all and continue\n", "Hooks need review"},
		"old codex":      {"codex", "  Do you trust the contents of this directory?\n\x1b[1m›\x1b[0m 1. Yes, continue\n  2. No, quit\n", "Do you trust the contents of this directory?"},
		"gemini":         {"gemini", "│ Do you trust this folder?\n│ ● 1. Trust folder (demo)\n", "Do you trust this folder?"},
		"copilot":        {"copilot", "Confirm folder trust\n❯ 1. Yes\n", "Confirm folder trust"},
		// Styling and padding between words do not hide it.
		"styled": {"kiro", "Hooks \x1b[1mneed\x1b[0m   review\n", "Hooks need review"},
	} {
		if got, ok := TrustScreen(tc.kind, tc.screen); !ok || got != tc.want {
			t.Errorf("%s: got %q %v want %q", name, got, ok, tc.want)
		}
	}
}

func TestOrdinaryScreensAndQuotedPhrasesAreNotTrustScreens(t *testing.T) {
	rule := strings.Repeat("─", 40)
	for name, tc := range map[string]struct{ kind, screen string }{
		// A transcript quoting the dialog above an empty input box.
		"quoted":     {"claude", "the dialog said \"Do you trust the files in this folder?\"\n" + rule + "\n❯ \n" + rule + "\n"},
		"sign in":    {"codex", "  Welcome to Codex\n› 1. Sign in with ChatGPT\n"},
		"permission": {"claude", "Allow this edit?\n❯ 1. Yes\n  2. No\n"},
		"empty":      {"claude", ""},
		"idle":       {"claude", fixture(t, "claude-empty")},
	} {
		if got, ok := TrustScreen(tc.kind, tc.screen); ok {
			t.Errorf("%s: detected %q", name, got)
		}
	}
}

// A truncated OpenCode box (two `┃` rows, the first empty, then `╹`) must
// read as a layout, not crash the injector.
func TestTruncatedOpencodeBoxDoesNotPanic(t *testing.T) {
	if got := Check("opencode", "┃\n┃ \n╹▀▀▀\n"); got != Empty {
		t.Errorf("got %v", got)
	}
}

// Claude Code shows the session title in the input box's top border
// (`──── <title> ─`). The box is still found, so a draft there is seen.
func TestClaudeBoxWithATitledBorder(t *testing.T) {
	rule := strings.Repeat("─", 60)
	top := "\x1b[38;2;136;136;136m" + rule + " Herdr-projects 项目探索 ─\x1b[0m"
	screen := func(box string) string {
		return "✶ Batting yarn… (51s)\n\n" + top + "\n" + box + "\n" + rule + "\n  status line\n"
	}
	for name, tc := range map[string]struct {
		box  string
		want Draft
	}{
		"queued hint": {"❯ \x1b[2mPress up to edit queued messages\x1b[0m", Empty},
		"empty":       {"❯ ", Empty},
		"cjk draft":   {"❯ 你好", Typed},
		"draft":       {"❯ amq 的 config 里:", Typed},
	} {
		if got := Check("claude", screen(tc.box)); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
	// A transcript line that only starts with dashes is not the border.
	if got := Check("claude", "── note\n❯ old prompt\n"+rule+"\n"); got != Unknown {
		t.Errorf("short dash line taken as a border: %v", got)
	}
	if got := Check("claude", "──────────── note\n❯ old prompt\n"+rule+"\n"); got != Unknown {
		t.Errorf("uncapped dash line taken as a border: %v", got)
	}
}

// On a narrow phone pane (e.g. 54 cols >= TinyColumns), Claude Code's Vst
// prompt border leaves 0..9 leading `─` before the padded session title and
// closes the right edge with ` ─`, while the bottom edge stays a plain rule.
func TestClaudeNarrowTitledBorder(t *testing.T) {
	rule54 := strings.Repeat("─", 54)
	// Exact SGR and NBSP layout emitted by `rex capture --format=vt` on a
	// 54-column phone pane when the adapter's title fills the top border (F=0).
	phoneTop := "\x1b[0m\x1b[38;2;138;138;138m orchestrator(claude-03) | [rex-amq-adapter] v0.13.… ─\x1b[0m"
	phoneBottom := "\x1b[0m\x1b[38;2;138;138;138m" + rule54 + "\x1b[0m"
	phoneStatus := "\x1b[2C\x1b[0m\x1b[38;5;14mrex-amq-adapter\x1b[0m\n"
	// Verbatim 54-col live phone capture (`rex capture --trim`) from claude-03.
	livePhoneCap := "⏺ 现在 pane 是 54 列宽，正好落在 agy 说的 40–59\n" +
		"  列区间里。再抓输入框那几行：\n" +
		"  ● Bash(eval \"$(/opt/homebrew/opt/rex-amq-adapter…)\n\n" +
		"✳ Finding a sunbeam… (16s · thought for 1s)\n" +
		"⚠ 这里值得改 · 报告里的编号和证据文件的编号对不…  [-]\n" +
		"1: 看看完整   2: 追问   3: 忽略\n" +
		"✦ 顺便懂一点 · 每个被拉起的 peer 都是一个独立的…\n" +
		"4: 看看解释   5: 我懂了   6: 追问   7: 忽略\n\n" +
		" orchestrator(claude-03) | [rex-amq-adapter] v0.13.… ─\n" +
		"❯\u00a0握\n" +
		rule54 + "\n" +
		"  rex-amq-adapter (main*) | Opus 5.5 | eff high | w…\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

	if got := Check("claude", livePhoneCap); got != Typed || !BoxHolds("claude", livePhoneCap, "握") {
		t.Fatalf("live 54-col phone capture with draft: got %v, holds=%v", got, BoxHolds("claude", livePhoneCap, "握"))
	}
	if got := Check("claude", strings.Replace(livePhoneCap, "❯\u00a0握", "❯\u00a0", 1)); got != Empty {
		t.Fatalf("live 54-col phone capture when idle: got %v, want empty", got)
	}

	cases := []struct {
		name string
		kind string
		vt   string
		want Draft
	}{
		{
			name: "54-col F=0 phone VT empty",
			kind: "claude",
			vt:   "✶ Churned…\n\n" + phoneTop + "\n\x1b[0m\x1b[38;2;102;102;102m❯\u00a0\x1b[0m\n" + phoneBottom + "\n" + phoneStatus,
			want: Empty,
		},
		{
			name: "54-col F=0 phone VT draft",
			kind: "claude",
			vt:   phoneTop + "\n\x1b[0m\x1b[38;2;102;102;102m❯\u00a0\x1b[0mhalf typed on phone\n" + phoneBottom + "\n" + phoneStatus,
			want: Typed,
		},
		{
			name: "54-col F=0 unstyled CSI 1C spaces",
			kind: "claude",
			vt:   "\x1b[1Corchestrator(claude-03)\x1b[1C|\x1b[1C[rex-amq-adapter]\x1b[1Cv0.13.…\x1b[1C─\n❯\u00a0\n" + rule54 + "\n",
			want: Empty,
		},
		{
			name: "54-col F=1 CJK title truncation",
			kind: "claude",
			vt:   "─ orchestrator(claude-03) | [rex-amq-adapter] 项目探索… ─\n❯\u00a0\n" + rule54 + "\n",
			want: Empty,
		},
		{
			name: "62-col F=9 boundary",
			kind: "claude",
			vt:   strings.Repeat("─", 9) + " orchestrator(claude-03) | [rex-amq-adapter] v0.13.0 ─\n❯\u00a0\n" + strings.Repeat("─", 62) + "\n",
			want: Empty,
		},
		{
			name: "40-col TinyColumns floor",
			kind: "claude",
			vt:   " orchestrator(claude-03) | [rex-amq-… ─\n❯\u00a0\n" + strings.Repeat("─", 40) + "\n",
			want: Empty,
		},
		{
			name: "30-col narrowest titled box",
			kind: "claude",
			vt:   " orchestrator(claude-03) | … ─\n❯\u00a0draft\n" + strings.Repeat("─", 30) + "\n",
			want: Typed,
		},
		{
			name: "Ink start-aligned borderText",
			kind: "claude",
			vt:   "─── dim hint ─────────────────────────────────────────\n❯\u00a0\n" + rule54 + "\n",
			want: Empty,
		},
		{
			name: "multiline draft with space-dash or leading dashes inside box",
			kind: "claude",
			vt:   phoneTop + "\n❯ first line\n  second line ─\n──────────── not a bottom rule\n" + phoneBottom + "\n",
			want: Typed,
		},
		{
			name: "2-space indented transcript line ending in space-dash is not a border",
			kind: "claude",
			vt:   "  assistant prose ending with ─\n❯ old prompt\n" + rule54 + "\n",
			want: Unknown,
		},
		{
			name: "Claude Insight banner is not a border",
			kind: "claude",
			vt:   "★ Insight ─────────────────────────────────────\n❯ old prompt\n" + rule54 + "\n",
			want: Unknown,
		},
		{
			name: "markdown hash rule is not a border",
			kind: "claude",
			vt:   "# ─── Write your reply below ──────────────────\n❯ old prompt\n" + rule54 + "\n",
			want: Unknown,
		},
		{
			name: "agy blockquote under prose ending in space-dash is not a box",
			kind: "agy",
			vt:   " prose ending with ─\n> quoted line\n" + rule54 + "\n",
			want: Unknown,
		},
	}
	for _, c := range cases {
		if got := Check(c.kind, c.vt); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !BoxHolds("claude", phoneTop+"\n❯\u00a0first line\n  second line ─\n──────────── not a bottom rule\n"+phoneBottom+"\n", "first line second line ─ ──────────── not a bottom rule") {
		t.Fatal("multiline draft ending with ` ─` or leading `─`s truncated the box early")
	}
	// Default/zero CUF advances one space; private CSI sequences ending in C
	// emit no phantom space and huge counts stay bounded.
	if got := Check("claude", "\x1b[Corchestrator(claude-03) | [rex-amq-adapter] v0.13.…\x1b[0C─\n❯\u00a0\n"+rule54+"\n"); got != Empty {
		t.Errorf("default/zero CUF titled border: got %v, want empty", got)
	}
	if got := Check("claude", "\x1b[?25C★ Insight ────────────────────────── ─\n❯ old\n"+rule54+"\n"); got != Unknown {
		t.Errorf("private CSI ?C injected phantom leading space: got %v, want unknown", got)
	}
	if got := Check("claude", " orchestrator(claude-03) | [rex-amq-adapter] v0.13.… ─\n❯\u00a0\x1b[999999C\n"+rule54+"\n"); got != Empty {
		t.Errorf("clamped CUF inside empty box: got %v, want empty", got)
	}
}
