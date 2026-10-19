package screen

import "testing"

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
