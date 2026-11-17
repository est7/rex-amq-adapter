package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// fileKey names one file inside its directory for any id: the ids the
// adapter stores keep the key they always had, and an id that would name no
// file of its own, or another directory (".", "..", "", a slash or a
// backslash), gets a key that stays inside, distinct from every other id's.
func TestFileKeyStaysInItsDirectory(t *testing.T) {
	for id, want := range map[string]string{
		"session:01a1eac4-f222-7d79-9291-9a8a86d39ec3": "session_01a1eac4-f222-7d79-9291-9a8a86d39ec3",
		"block:a":  "block_a",
		"p2p/a__b": "p2p_a__b",
		`a\b`:      "a_b",
	} {
		if got := fileKey(id); got != want {
			t.Errorf("fileKey(%q) = %q, want %q", id, got, want)
		}
	}
	seen := map[string]string{}
	for _, id := range []string{"", ".", "..", "...", "/", `\`, "../..", `..\..`, "a", "h_x"} {
		k := fileKey(id)
		if k == "" || k == "." || k == ".." || strings.ContainsAny(k, `/\`) || filepath.Base(k) != k {
			t.Errorf("fileKey(%q) = %q names no file of its own", id, k)
		}
	}
	// The ids that are hashed are distinct from each other and from any
	// replaced id. (Replacement itself folds ":", "/", and "\\" alike, as
	// it always did; ids are validated before they reach the store.)
	for _, id := range []string{"", ".", "..", "..."} {
		k := fileKey(id)
		if prev, dup := seen[k]; dup {
			t.Errorf("fileKey(%q) = fileKey(%q) = %q", id, prev, k)
		}
		seen[k] = id
	}
}
