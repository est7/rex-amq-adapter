package core

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// cfBidi and zeroWidth are the format characters that hide or reorder text.
func cfBidi(r rune) bool    { return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 }
func zeroWidth(r rune) bool { return r >= 0x200b && r <= 0x200d || r == 0x2060 || r == 0xfeff }
func c1(r rune) bool        { return r >= 0x80 && r < 0xa0 }

// LineClean refuses what one line of text may not hold: C0 and C1 controls
// (newlines and ANSI escapes included), bidi controls, zero-width characters,
// and invalid UTF-8.
func LineClean(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("is not valid UTF-8")
	}
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || c1(r) || r == 0x2028 || r == 0x2029:
			return fmt.Errorf("holds a control character (%U)", r)
		case cfBidi(r):
			return fmt.Errorf("holds a bidi control (%U)", r)
		case zeroWidth(r):
			return fmt.Errorf("holds a zero-width character (%U)", r)
		}
	}
	return nil
}
