package amq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The observer reads amq's files directly, so a schema it does not know is
// an error, never a guess.
func TestMailboxReadersRequireTheirSchema(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, "agents", "a", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("inbox/new/m1.md", "---json\n{\"schema\":1,\"id\":\"m1\",\"from\":\"b\",\"to\":[\"a\"],\"subject\":\"hi\"}\n---\nbody\n")
	write("inbox/new/m2.md", "---json\n{\"schema\":2,\"id\":\"m2\",\"from\":\"b\",\"to\":[\"a\"]}\n---\nbody\n")
	write("inbox/new/m3.md", "---json\n{\"id\":\"m3\",\"to\":[\"a\"]}\n---\nbody\n")
	write("receipts/m1__a__drained.json", `{"schema":1,"msg_id":"m1","consumer":"a","stage":"drained"}`)
	write("receipts/m2__a__drained.json", `{"schema":2,"msg_id":"m2","consumer":"a","stage":"drained"}`)

	if h, err := ReadHeader(root, "a", BoxUnread, "m1"); err != nil || h.Subject != "hi" {
		t.Errorf("schema 1 header: %+v %v", h, err)
	}
	for _, id := range []string{"m2", "m3"} {
		if _, err := ReadHeader(root, "a", BoxUnread, id); err == nil || !strings.Contains(err.Error(), "schema") {
			t.Errorf("%s: err %v, want a schema error", id, err)
		}
	}
	if r, err := ReadReceipt(root, "a", "m1__a__drained.json"); err != nil || r.MsgID != "m1" {
		t.Errorf("schema 1 receipt: %+v %v", r, err)
	}
	if _, err := ReadReceipt(root, "a", "m2__a__drained.json"); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("schema 2 receipt: err %v, want a schema error", err)
	}
}
