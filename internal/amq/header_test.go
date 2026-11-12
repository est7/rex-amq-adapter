package amq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A header is read however long it is, as amq reads it, and one amq would
// not accept is an error.
func TestReadHeaderReadsLongAndCRLFHeaders(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "b", "inbox", "new")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 70<<10)
	for name, c := range map[string]struct {
		text string
		ok   bool
	}{
		"long":     {"---json\n{\"schema\":1,\"id\":\"long\",\"from\":\"a\",\"context\":{\"pad\":\"" + big + "\"}}\n---\nbody\n", true},
		"crlf":     {"---json\r\n{\"schema\":1,\"id\":\"crlf\",\"from\":\"a\"}\r\n---\r\nbody\r\n", true},
		"unclosed": {"---json\n{\"schema\":1,\"id\":\"u\",\"from\":\"a\"}\nbody\n", false},
		"corrupt":  {"---json\n{\"schema\":1,\"id\"\n---\n", false},
		"none":     {"x", false},
		"schema":   {"---json\n{\"schema\":2,\"id\":\"s\",\"from\":\"a\"}\n---\n", false},
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(c.text), 0o600); err != nil {
			t.Fatal(err)
		}
		h, err := ReadHeader(root, "b", BoxUnread, name)
		if (err == nil) != c.ok || c.ok && h.From != "a" {
			t.Errorf("%s: %+v %v", name, h, err)
		}
	}
	if h, _ := ReadHeader(root, "b", BoxUnread, "long"); len(h.Context["pad"]) < 70<<10 {
		t.Errorf("long context cut: %d", len(h.Context["pad"]))
	}
}
