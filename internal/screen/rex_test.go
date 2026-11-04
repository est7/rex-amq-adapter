package screen

import (
	"strings"
	"testing"
)

// Rex's VT capture carries the same SGR styling the classifier reads, so the
// Herdr-era logic applies unchanged.
func TestRexCapturesClassify(t *testing.T) {
	cases := []struct {
		kind, file string
		want       Draft
	}{
		{"claude", "rex-claude-empty", Empty},
		{"claude", "rex-claude-draft", Typed},
		{"claude", "rex-claude-question", Unknown},
		{"codex", "rex-codex-empty", Empty},
	}
	for _, c := range cases {
		if got := Check(c.kind, fixture(t, c.file)); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.kind, c.file, got, c.want)
		}
	}
}

func TestBoxHoldsComparesOnlyTheBox(t *testing.T) {
	draft := fixture(t, "rex-claude-draft")
	if !BoxHolds("claude", draft, "half typed draft") {
		t.Fatal("the draft in the box must match")
	}
	if !BoxHolds("claude", draft, "half typed  draft ") {
		t.Fatal("whitespace differences must not matter")
	}
	if BoxHolds("claude", draft, "half typed") {
		t.Fatal("a prefix is not the box's content")
	}
	// The empty capture's transcript contains earlier prompts; none of them is in the box.
	if BoxHolds("claude", fixture(t, "rex-claude-empty"), "Reply with exactly the word PONG1 and nothing else.") {
		t.Fatal("text in the transcript must not match an empty box")
	}
}

// Codex wraps long input onto indented rows; the box spans them, so a
// doorbell left in a narrow pane is still recognised as exactly ours.
func TestCodexWrappedBox(t *testing.T) {
	wrapped := fixture(t, "rex-codex-wrapped-draft")
	text := `AMQ doorbell: 2 unread message(s) for codex-2. Run eval "$(/Users/dev1/EstProjects/rex-amq-adapter/bin/rex-amq-adapter env)" && amq drain --include-body and act on them.`
	if got := Check("codex", wrapped); got != Typed {
		t.Fatalf("wrapped draft: got %v", got)
	}
	if !BoxHolds("codex", wrapped, text) {
		t.Fatal("the wrapped box must hold the whole doorbell")
	}
	if BoxHolds("codex", wrapped, text[:60]) {
		t.Fatal("only the first row matched: continuation rows were dropped")
	}
	if got := Check("codex", fixture(t, "rex-codex-cleared")); got != Empty {
		t.Fatalf("after ctrl+u: got %v", got)
	}
}

func TestAgyBox(t *testing.T) {
	if got := Check("agy", fixture(t, "rex-agy-empty")); got != Empty {
		t.Fatalf("empty agy box: got %v", got)
	}
	draft := fixture(t, "rex-agy-draft")
	if got := Check("agy", draft); got != Typed || !BoxHolds("agy", draft, "half typed draft") {
		t.Fatalf("agy draft: got %v", got)
	}
}

// An idle agy box shows its mode's hint in grey (not dim), cut to the pane's
// width with "..."; Auto mode, agy's default, held every doorbell back as a
// draft (2026-11-04). The captures are of a 54-column pane.
func TestAgyModeHintIsNoDraft(t *testing.T) {
	for _, name := range []string{"rex-agy-auto-mode", "rex-agy-plan-mode", "rex-agy-accept-edits-mode"} {
		if got := Check("agy", fixture(t, name)); got != Empty {
			t.Errorf("%s: got %v, want empty", name, got)
		}
	}
}

// The hint is told by its grey, not its words: a reworded hint is still one,
// and a hint's words typed (default colour, or a slash command's blue) are
// still the user's, as is typed text that a grey suggestion follows.
func TestAgyHintIsToldByColour(t *testing.T) {
	box := func(text string) string {
		rule := "\x1b[0m\x1b[38;5;8m" + strings.Repeat("─", 54) + "\x1b[0m\n"
		return rule + "\x1b[0m\x1b[38;5;12m>\x1b[0m " + text + "\n" + rule
	}
	const grey = "\x1b[0m\x1b[38;5;8m"
	cases := []struct {
		text string
		want Draft
	}{
		{grey + "Some future mode: reworded hint (...\x1b[0m", Empty},
		{"\x1b[0mAuto mode: edits & safe commands auto-approved (...", Typed},
		{"\x1b[0m\x1b[38;5;12m/agy-customizations\x1b[0m", Typed},
		{"\x1b[0mhalf typed" + grey + " suggestion\x1b[0m", Typed},
		{grey + "hint\x1b[0m\x1b[37m typed after an explicit colour\x1b[0m", Typed},
	}
	for _, c := range cases {
		if got := Check("agy", box(c.text)); got != c.want {
			t.Errorf("%q: got %v, want %v", c.text, got, c.want)
		}
	}
	// Grey means nothing for another kind.
	if got := Check("claude", strings.ReplaceAll(box(grey+"grey text\x1b[0m"), ">", "❯")); got != Typed {
		t.Errorf("claude grey text: got %v, want typed", got)
	}
}
