package screen

import "testing"

// A restored pane's shell is typed into only at an empty prompt. The fish
// screens are `rex capture --format=vt` of a real restored login shell.
func TestShellPromptEmpty(t *testing.T) {
	fish := "Last login: Wed Nov 18 21:29:49 on ttys001\r\n\r\n\x1b[0m\x1b[1m\x1b[38;2;131;192;146m/tmp\x1b[0m at \x1b[0m\x1b[38;2;133;146;137m21:38\x1b[0m \r\n\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m \r\n\r\n\r\n\r\n"
	fishDraft := "Last login: Wed Nov 18 21:29:49 on ttys001\r\n\r\n\x1b[0m\x1b[1m\x1b[38;2;131;192;146m/tmp\x1b[0m at \x1b[0m\x1b[38;2;133;146;137m21:38\x1b[0m \r\n\x1b[0m\x1b[1m\x1b[38;2;167;192;128m❯\x1b[0m echo \x1b[0m\x1b[38;5;6mdra\x1b[0m\r\n\r\n\r\n"
	for name, c := range map[string]struct {
		screen string
		empty  bool
	}{
		"fish at its prompt":    {fish, true},
		"fish with a draft":     {fishDraft, false},
		"bash":                  {"user@host:~/app$ \r\n", true},
		"zsh":                   {"host% ", true},
		"root":                  {"root@host:/# ", true},
		"bash with a draft":     {"user@host:~/app$ rm -rf build\r\n", false},
		"a right-hand prompt":   {"❯                     main 21:38\r\n", false},
		"a blank screen":        {"\r\n\r\n\r\n", false},
		"a program's output":    {"Compiling...\r\n 42% done\r\n", false},
		"a pager":               {"line 1\r\nline 2\r\n(END)\r\n", false},
		"a prompt then output":  {"$ \r\nhello\r\n", false},
		"a cursor-moved prompt": {"\x1b[1m~/app\x1b[0m\x1b[2C❯\x1b[1C\r\n", true},
	} {
		if got := ShellPromptEmpty(c.screen); got != c.empty {
			t.Errorf("%s: ShellPromptEmpty = %v, want %v", name, got, c.empty)
		}
	}
}
