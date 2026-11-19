package screen

import (
	"regexp"
	"strings"
)

// promptGlyphs end a shell's prompt: sh and bash ($, # for root), zsh (%),
// fish and csh (>), and the glyphs common prompt themes draw (starship's ❯,
// oh-my-zsh's ➜, and the like).
var promptGlyphs = []string{"$", "%", "#", ">", "❯", "➜", "λ", "»", "›", "➤", "▶", "∴"}

// promptShapes are the prompts ShellPromptEmpty recognises, each the text
// before its glyph and the glyphs that shell draws after it: a theme's
// glyph alone, its context on the line above (starship, oh-my-zsh, pure);
// user@host and a path (bash's Linux default with $ or #, fish's with >); a
// bracketed one (Fedora's bash); macOS bash's host:dir user; a bare
// hostname (zsh's default, % or #); and a bare path (fish's and themes').
var promptShapes = []struct {
	before *regexp.Regexp
	glyphs string
}{
	{regexp.MustCompile(`^$`), "$%#>❯➜λ»›➤▶∴"},
	{regexp.MustCompile(`^[\w.-]+@[\w.-]+:[~/]\S*$`), "$#"},
	{regexp.MustCompile(`^[\w.-]+@[\w.-]+ [~/]\S*$`), ">$#"},
	{regexp.MustCompile(`^\[[\w.-]+@[\w.-]+ [^\]\s]+\]$`), "$#"},
	{regexp.MustCompile(`^[\w.-]+:\S+ [\w.-]+$`), "$#"},
	{regexp.MustCompile(`^[\w.-]+$`), "%#"},
	{regexp.MustCompile(`^[~/]\S*$`), ">$%❯➜"},
}

// ShellPromptEmpty reports whether screen, a terminal's VT capture, shows a
// freshly started shell waiting at an empty prompt it positively
// recognises: the whole screen holds exactly one prompt glyph, it ends the
// last line with text on it, and what precedes it on that line is a prompt
// shape it knows (promptShapes).
//
// The screen alone cannot tell a prompt from a draft typed after one, so
// it accepts only what it can tell: a second glyph means a draft that ends
// in one (`❯ echo pending # `), a multi-line draft or a continuation prompt
// (bash's `> `, zsh's `dquote> `), or a command run in the pane before;
// a line it does not recognise (`app: echo pending # ` after PS1='app: ')
// may hold a draft, whatever it ends in. Both, a right-hand prompt, a blank
// screen, and anything else read as not empty, and nothing should be typed
// into it. What it still cannot tell is a draft that, with the prompt
// before it, makes up a whole prompt of a recognised shape (typing `%`
// after an empty PS1 draws `%`): typing it takes keys no command needs.
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
		if before, ok := strings.CutSuffix(end, g); ok {
			before = strings.TrimSpace(before)
			for _, shape := range promptShapes {
				if strings.Contains(shape.glyphs, g) && shape.before.MatchString(before) {
					return true
				}
			}
			return false
		}
	}
	return false
}
