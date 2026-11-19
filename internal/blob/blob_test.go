package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A body over the cap comes back as a valid UTF-8 prefix of at most max
// bytes, with its whole text spilled once to <dir>/<sha256>.txt (0600); a
// body within the cap comes back as it is and spills nothing.
func TestCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache", "blobs")
	small, err := Cap(dir, "short", 16)
	if err != nil || small.Body != "short" || small.Truncated || small.FullPath != "" || small.TotalBytes != 5 {
		t.Fatalf("small: %+v %v", small, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a body within the cap spilled")
	}
	body := strings.Repeat("a", 9) + strings.Repeat("é", 10) // é is 2 bytes: byte 10 falls inside one
	c, err := Cap(dir, body, 10)
	if err != nil || !c.Truncated || c.TotalBytes != len(body) || len(c.Body) > 10 || !utf8.ValidString(c.Body) || !strings.HasPrefix(body, c.Body) {
		t.Fatalf("capped: %+v %v", c, err)
	}
	sum := sha256.Sum256([]byte(body))
	if want := filepath.Join(dir, hex.EncodeToString(sum[:])+".txt"); c.FullPath != want {
		t.Errorf("full_path %s, want %s", c.FullPath, want)
	}
	b, err := os.ReadFile(c.FullPath)
	if err != nil || string(b) != body {
		t.Fatalf("spilled %q %v", b, err)
	}
	if st, _ := os.Stat(c.FullPath); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if again, err := Cap(dir, body, 10); err != nil || again != c {
		t.Errorf("the same body capped again: %+v %v", again, err)
	}
}
