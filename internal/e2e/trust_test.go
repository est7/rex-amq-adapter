package e2e

import (
	"os"
	"path/filepath"
	"strconv"
)

// project makes a directory under the world's home that the user trusts in
// Claude (the world's own ~/.claude.json), as an Epic's repository is: epic
// checks folder trust before it opens a pane, and Claude never records
// trust in the home itself.
func (w *world) project(name string) string {
	w.t.Helper()
	dir := filepath.Join(w.dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.t.Fatal(err)
	}
	phys, err := filepath.EvalSymlinks(dir)
	if err != nil {
		w.t.Fatal(err)
	}
	w.write(filepath.Join(w.dir, ".claude.json"), `{"projects": {`+strconv.Quote(phys)+`: {"hasTrustDialogAccepted": true}}}`)
	return dir
}
