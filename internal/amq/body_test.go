package amq

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A body reaches amq as bytes, never as amq's own @FILE or - syntax: a
// resolved body that starts with "@<path>" or is "-" is delivered as that
// text, and the file it names is never read.
func TestMailBodyLeadingAtSentLiterally(t *testing.T) {
	bin, err := ResolveBin()
	if err != nil {
		t.Skip(err)
	}
	root := filepath.Join(t.TempDir(), "root")
	ctx := context.Background()
	for _, h := range []string{"alice", "bob"} {
		if err := EnsureMailbox(ctx, bin, root, h); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"@" + secret, "-", "@" + secret + "\nmore"} {
		for _, form := range [][]string{{"--body", body}, {"--body=" + body}} {
			args := append([]string{"--me", "alice", "--to", "bob", "--subject", "s"}, form...)
			sent, out, err := CLI{Bin: bin}.Post(ctx, root, "send", args...)
			if err != nil {
				t.Fatalf("%q: %v %s", form, err, out)
			}
			b, err := os.ReadFile(filepath.Join(Inbox(root, "bob"), sent.ID+".md"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "TOP SECRET") || !strings.Contains(string(b), body) {
				t.Errorf("%q delivered:\n%s", form, b)
			}
		}
	}
}
