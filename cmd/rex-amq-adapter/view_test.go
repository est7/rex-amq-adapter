package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// view parses its three forms, reads --summary from a file, and refuses the
// rest as usage errors.
func TestSessionViewArgs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(file, []byte("review pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want viewArgs
	}{
		{nil, viewArgs{}},
		{[]string{"--session", "session:a"}, viewArgs{session: "session:a"}},
		{[]string{"refresh"}, viewArgs{verb: "refresh"}},
		{[]string{"refresh", "--summary", "all green"}, viewArgs{verb: "refresh", summary: "all green"}},
		{[]string{"refresh", "--summary", "@" + file}, viewArgs{verb: "refresh", summary: "review pending"}},
		{[]string{"read", "--ids", "a, b,,c"}, viewArgs{verb: "read", ids: []string{"a", "b", "c"}}},
	} {
		got, err := parseViewArgs(c.args)
		if err != nil || got.verb != c.want.verb || got.session != c.want.session || got.summary != c.want.summary || !slices.Equal(got.ids, c.want.ids) {
			t.Errorf("%v: %+v %v", c.args, got, err)
		}
	}
	for _, bad := range [][]string{{"read"}, {"read", "--ids", ""}, {"drain"}, {"refresh", "--session", "x"}, {"--summary", "x"}, {"refresh", "extra"}} {
		if _, err := parseViewArgs(bad); err == nil {
			t.Errorf("%v parsed", bad)
		}
	}
}
