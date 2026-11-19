package screen

import "strings"

// promptGlyphs end a shell's prompt: sh and bash ($, # for root), zsh (%),
// fish and csh (>), and the glyphs common prompt themes draw (starship's ❯,
// oh-my-zsh's ➜, and the like).
var promptGlyphs = []string{"$", "%", "#", ">", "❯", "➜", "λ", "»", "›", "➤", "▶", "∴"}

// ShellPromptEmpty reports whether screen, a terminal's VT capture, shows a
// shell waiting at an empty prompt: its last line with any text on it ends
// in a prompt glyph, so nothing typed follows it. It fails closed: a prompt
// that ends otherwise (a right-hand prompt, a theme it does not know), a
// screen still blank, or a shell drawing something else reads as not
// empty, and nothing should be typed into it.
func ShellPromptEmpty(screen string) bool {
	lines := strings.Split(PlainText(screen), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		for _, g := range promptGlyphs {
			if strings.HasSuffix(l, g) {
				return true
			}
		}
		return false
	}
	return false
}
