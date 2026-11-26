package screen

import (
	"strings"
	"testing"
)

func TestInspectExtractsSurfaceFactsInOnePass(t *testing.T) {
	t.Run("zero value is BoxMissing", func(t *testing.T) {
		var s Surface
		if s.Box != BoxMissing || s.Box.String() != "missing" || s.OursInBox || s.TrustPhrase != "" || s.SelfDenyingCountdown {
			t.Fatalf("zero Surface = %+v, want BoxMissing and all false/empty", s)
		}
	})

	t.Run("empty agy fixtures suppress trust and report BoxEmpty", func(t *testing.T) {
		for _, name := range []string{"rex-agy-empty", "rex-agy-auto-mode", "rex-agy-plan-mode"} {
			s := Inspect("agy", fixture(t, name))
			if s.Box != BoxEmpty || s.TrustPhrase != "" || s.SelfDenyingCountdown || s.OursInBox {
				t.Errorf("%s: got %+v, want BoxEmpty", name, s)
			}
		}
	})

	t.Run("agy draft reports BoxDraft and matches expected text", func(t *testing.T) {
		vt := fixture(t, "rex-agy-draft")
		s := Inspect("agy", vt)
		if s.Box != BoxDraft || s.Box.String() != "draft" || s.OursInBox {
			t.Fatalf("got %+v, want BoxDraft without OursInBox", s)
		}
		rule := strings.Repeat("─", 40)
		doorbell := "AMQ doorbell: 1 unread message(s) for agy-01"
		wrapped := rule + "\n> AMQ doorbell: 1 unread\n  message(s) for agy-01\n" + rule + "\n"
		s2 := Inspect("agy", wrapped, doorbell)
		if s2.Box != BoxDraft || !s2.OursInBox || !s2.Holds(doorbell) || s2.Holds("other text") {
			t.Fatalf("wrapped doorbell: got %+v", s2)
		}
	})

	t.Run("trust dialog detected when box missing or draft, suppressed when box empty", func(t *testing.T) {
		rule := strings.Repeat("─", 40)
		quoted := "the dialog said \"Do you trust the files in this folder?\"\n" + rule + "\n❯ \n" + rule + "\n"
		if s := Inspect("claude", quoted); s.Box != BoxEmpty || s.TrustPhrase != "" {
			t.Errorf("quoted trust above empty box: got %+v", s)
		}
		codexHooks := "  Hooks need review\n  1 hook is new or changed.\n› 1. Review hooks\n"
		if s := Inspect("", codexHooks); s.Box != BoxMissing || s.TrustPhrase != "Hooks need review" {
			t.Errorf("unadopted hooks review: got %+v", s)
		}
	})

	t.Run("claude self-denying countdown detected", func(t *testing.T) {
		dlg := "╌╌╌╌\n│ rm -rf /tmp/x\n╌╌╌╌\n" +
			"⚠ Claude Code will automatically block this request in 1:24, to avoid holding up an unattended session\n" +
			"Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel · Tab to amend\n"
		s := Inspect("claude", dlg)
		if s.Box != BoxMissing || !s.SelfDenyingCountdown {
			t.Fatalf("countdown dialog: got %+v", s)
		}
	})
}
