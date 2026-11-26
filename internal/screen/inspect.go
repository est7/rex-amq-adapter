package screen

// BoxState is what an agent's input box on screen holds. The zero value is
// BoxMissing so an uninspected Surface never reads as an empty prompt box.
type BoxState int

const (
	// BoxMissing: no input box was found on screen (a menu or dialog, a scrolled
	// view, an unknown layout, or an unsupported kind).
	BoxMissing BoxState = iota
	// BoxEmpty: the input box is visible and holds nothing typed.
	BoxEmpty
	// BoxDraft: the input box is visible and holds unsent typed text.
	BoxDraft
)

func (b BoxState) String() string {
	switch b {
	case BoxEmpty:
		return "empty"
	case BoxDraft:
		return "draft"
	default:
		return "missing"
	}
}

// Surface is the single parsed snapshot of an agent's terminal screen.
type Surface struct {
	Box                  BoxState
	OursInBox            bool
	TrustPhrase          string
	SelfDenyingCountdown bool

	boxText string
}

// Holds reports whether the input box holds exactly text, ignoring whitespace
// so wrapped lines still match.
func (s Surface) Holds(text string) bool {
	return s.Box == BoxDraft && s.boxText != "" && text != "" && squash(s.boxText) == squash(text)
}

// Inspect reads vt once for an agent of kind and extracts every surface fact
// the activity assessor needs. Pass an optional expected doorbell string to
// populate OursInBox.
func Inspect(kind, vt string, ours ...string) Surface {
	var s Surface
	if text, ok := boxText(kind, vt); ok {
		if text == "" {
			s.Box = BoxEmpty
		} else {
			s.Box = BoxDraft
			s.boxText = text
			if len(ours) > 0 && ours[0] != "" {
				s.OursInBox = squash(text) == squash(ours[0])
			}
		}
	}
	if vt == "" {
		return s
	}
	plain := PlainText(vt)
	if s.Box != BoxEmpty {
		s.TrustPhrase, _ = trustPhraseFromPlain(plain)
	}
	s.SelfDenyingCountdown = selfDenyingCountdownFromPlain(plain)
	return s
}
