// Package blob bounds the message bodies a detail view returns: a body over
// the cap comes back as a UTF-8-safe prefix, and its full text is written
// once to <state>/cache/blobs/<sha256>.txt (mode 0600) for an explicit read.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// DefaultMax is a detail view's default body cap.
const DefaultMax = 16 << 10

// Capped is a body as a detail view returns it.
type Capped struct {
	Body       string `json:"body"`
	Truncated  bool   `json:"truncated"`
	FullPath   string `json:"full_path,omitempty"`
	TotalBytes int    `json:"total_bytes"`
}

// Cap is body bounded to max bytes. A longer body is cut on a rune
// boundary and spilled whole to dir, named by its SHA-256, so the same body
// is written once however often it is read.
func Cap(dir, body string, max int) (Capped, error) {
	c := Capped{Body: body, TotalBytes: len(body)}
	if max <= 0 || len(body) <= max {
		return c, nil
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	sum := sha256.Sum256([]byte(body))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".txt")
	if _, err := os.Stat(path); err != nil {
		if err := fsutil.WriteAtomic(path, []byte(body)); err != nil {
			return c, err
		}
	}
	c.Body, c.Truncated, c.FullPath = body[:cut], true, path
	return c, nil
}
