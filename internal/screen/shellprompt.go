package screen

import "strings"

// promptGlyphs end a shell's prompt: sh and bash ($, # for root), zsh (%),
// fish and csh (>), and the glyphs common prompt themes draw (starship's ❯,
// oh-my-zsh's ➜, and the like).
var promptGlyphs = []string{"$", "%", "#", ">", "❯", "➜", "λ", "»", "›", "➤", "▶", "∴"}

// ShellPromptEmpty reports whether screen, a terminal's VT capture, shows a
// freshly started shell waiting at an empty prompt: the whole screen holds
// exactly one prompt glyph, and it ends the last line with text on it.
//
// One glyph is the boundary a suffix alone cannot give: a draft ending in a
// glyph (`❯ echo pending # `) shows the prompt's glyph and the draft's, a
// multi-line draft or a shell's continuation prompt (bash's `> `, zsh's
// `dquote> `) shows the first line's prompt above it, and so does any
// command run in the pane before. It fails closed: a prompt that ends
// otherwise (a right-hand prompt, a theme it does not know), a glyph in the
// prompt's own text, a blank screen, and anything else read as not empty,
// and nothing should be typed into it.
func ShellPromptEmpty(screen string) bool {
	lines := strings.Split(PlainText(screen), "\n")
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
			break
		}
	}
	if last < 0 {
		return false
	}
	glyphs := 0
	for _, l := range lines[:last+1] {
		for _, g := range promptGlyphs {
			glyphs += strings.Count(l, g)
		}
	}
	if glyphs != 1 {
		return false
	}
	end := strings.TrimSpace(lines[last])
	for _, g := range promptGlyphs {
		if strings.HasSuffix(end, g) {
			return true
		}
	}
	return false
}
