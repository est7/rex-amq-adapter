package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The managed mail verbs answer with amq's exit status, 2 for flags that are
// the adapter's to set, and 1 for what the gateway refuses.
func TestManagedMailExitCodes(t *testing.T) {
	bin, err := amq.ResolveBin()
	if err != nil {
		t.Skip(err)
	}
	paths := store.Paths{Home: t.TempDir()}
	env := app.Env{Paths: paths, Store: store.Store{P: paths}, AMQ: amq.CLI{Bin: bin}, Now: time.Now}
	for _, r := range []store.Record{
		{Handle: "codex-01", Server: "unix:///t.sock", BlockID: "block:a", SessionID: "session:a"},
		{Handle: "claude-02", Server: "unix:///t.sock", BlockID: "block:b", SessionID: "session:a"},
		{Handle: "claude-09", Server: "unix:///t.sock", BlockID: "block:c", SessionID: "session:b"},
	} {
		if err := env.Store.PutRecord(r); err != nil {
			t.Fatal(err)
		}
		if err := amq.EnsureMailbox(context.Background(), bin, paths.Root(), string(r.Handle)); err != nil {
			t.Fatal(err)
		}
	}
	at := app.Location{Block: "block:a"}
	for _, c := range []struct {
		verb string
		args []string
		code int
		out  string
	}{
		{"send", []string{"--to", "claude-02", "--subject", "s", "--body", "b"}, 0, "Sent "},
		{"send", []string{"--to", "claude-09", "--subject", "s", "--body", "b"}, 1, "refused by the gateway"},
		{"send", []string{"--me", "claude-02", "--to", "claude-02", "--body", "b"}, 2, "--me"},
		{"drain", []string{"--root", "/tmp/x"}, 2, "--root"},
		{"read", nil, 2, "--id"},
		{"tap", nil, 2, "not a managed mail command"},
		{"dlq", []string{"list"}, 0, ""},
	} {
		var out bytes.Buffer
		code := managedMail(env, at, c.verb, c.args, strings.NewReader(""), &out, &out)
		if code != c.code || !strings.Contains(out.String(), c.out) {
			t.Errorf("%s %v: exit %d %q, want %d %q", c.verb, c.args, code, out.String(), c.code, c.out)
		}
	}
	if ids, _ := amq.Unread(paths.Root(), "claude-09"); len(ids) != 0 {
		t.Fatalf("delivered across Epics: %v", ids)
	}
}
