package screen

import "testing"

// A restored pane's shell is typed into only at the empty prompt of a
// freshly started shell. The fish screens are `rex capture --format=vt` of
// real login shells: a starship prompt, and the restored block of a private
// Rex server after a restart.
func TestShellPromptEmpty(t *testing.T) {
	fish := "Last login: Wed Nov 18 21:29:49 on ttys001\r\n\r\n\x1b[0m\x1b[1m\x1b[38;2;131;192;146m/tmp\x1b[0m at \x1b[0m\x1b[38;2;133;146;137m21:38\x1b[0m \r\n\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m \r\n\r\n\r\n\r\n"
	fishDraft := "Last login: Wed Nov 18 21:29:49 on ttys001\r\n\r\n\x1b[0m\x1b[1m\x1b[38;2;131;192;146m/tmp\x1b[0m at \x1b[0m\x1b[38;2;133;146;137m21:38\x1b[0m \r\n\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m echo \x1b[0m\x1b[38;5;6mdra\x1b[0m\r\n\r\n\r\n"
	restored := "Last login: Thu Nov 19 00:19:25 on ttys003\r\nWelcome to fish, the friendly interactive shell\r\nType \x1b[0m\x1b[38;5;2mhelp\x1b[0m for instructions on how to use fish\r\n\x1b[0m\x1b[38;5;10mest9\x1b[0m@est9s-MacBook-Pro \x1b[0m\x1b[38;5;2m/p/t/r/app\x1b[0m> \r\n\r\n\r\n\r\n"
	// Rex's Clear Terminal (block method clear) after `ls /`: the scrollback
	// goes, one prompt is left. Ctrl+L would leave the listing above it.
	cleared := "\r\n\r\n\r\n\x1b[0m\x1b[1m\x1b[38;2;131;192;146m/tmp\x1b[0m via \x1b[0m\x1b[1m\x1b[38;2;131;192;146m\ue627 v1.27.4\x1b[0m at \x1b[0m\x1b[38;2;133;146;137m00:30\x1b[0m \r\n\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m \r\n\r\n"
	glyphDraft := "\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m echo \x1b[0m\x1b[38;5;6mxxxxxxxx\x1b[0m \x1b[0m\x1b[38;5;6mfoo\x1b[0m \x1b[0m\x1b[38;5;1m#\x1b[0m\r\n\r\n"
	for name, c := range map[string]struct {
		screen string
		empty  bool
	}{
		"fish at its prompt":            {fish, true},
		"a restored fish login shell":   {restored, true},
		"bash":                          {"user@host:~/app$ \r\n", true},
		"zsh":                           {"host% ", true},
		"root":                          {"root@host:/# ", true},
		"a cleared pane":                {cleared, true},
		"a cursor-moved prompt":         {"\x1b[1m~/app\x1b[0m\x1b[2C❯\x1b[1C\r\n", true},
		"fish with a draft":             {fishDraft, false},
		"bash with a draft":             {"user@host:~/app$ rm -rf build\r\n", false},
		"a draft ending in a comment":   {"❯ echo pending # \r\n", false},
		"a styled draft ending in #":    {glyphDraft, false},
		"a draft ending in $":           {"user@host:~$ echo $\r\n", false},
		"a draft ending in >":           {"host> cat >\r\n", false},
		"a draft ending in %":           {"host% printf 100%\r\n", false},
		"bash's continuation prompt":    {"user@host:~$ echo \"pending\r\n> \r\n", false},
		"zsh's continuation prompt":     {"host% echo \"pending\r\ndquote> \r\n", false},
		"fish's multi-line edit":        {"❯ echo 'a\r\n  b #\r\n", false},
		"a command run before":          {"❯ ls\r\nREADME.md\r\n❯ \r\n", false},
		"a right-hand prompt":           {"❯                     main 21:38\r\n", false},
		"a glyph in the prompt's text":  {"~/100%/app ❯ \r\n", false},
		"a blank screen":                {"\r\n\r\n\r\n", false},
		"a program's output":            {"Compiling...\r\n 42% done\r\n", false},
		"a pager":                       {"line 1\r\nline 2\r\n(END)\r\n", false},
		"a prompt then output":          {"$ \r\nhello\r\n", false},
		"macOS bash":                    {"mac:~ est9$ \r\n", true},
		"Fedora bash":                   {"[est9@box app]$ \r\n", true},
		"a bare path":                   {"~/app ❯ \r\n", true},
		"an unknown prompt's draft":     {"app: echo pending # \r\n", false},
		"an unknown prompt, empty":      {"app: \r\n", false},
		"an unknown prompt and a glyph": {"app ❯ \r\n", false},
		"a draft after a bare hostname": {"host% ls\r\n", false},
		"a glyph the shape never draws": {"host❯ \r\n", false},
	} {
		if got := ShellPromptEmpty(c.screen); got != c.empty {
			t.Errorf("%s: ShellPromptEmpty = %v, want %v", name, got, c.empty)
		}
	}
}
