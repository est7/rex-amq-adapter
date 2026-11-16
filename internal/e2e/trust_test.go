package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/est7/rex-amq-adapter/internal/install"
)

// project makes a directory under the world's home that the user trusts in
// Claude (the world's own ~/.claude.json) and Antigravity (its
// settings.json), as an Epic's repository is: epic checks folder trust before
// it opens a pane, and Claude never records trust in the home itself. Both
// key it by the physical path (/private/tmp, not /tmp, on macOS).
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
	if _, err := install.TrustClaudeProject(context.Background(), filepath.Join(w.dir, ".claude.json"), phys); err != nil {
		w.t.Fatal(err)
	}
	w.trustAgy(phys)
	return dir
}

// trustAgy adds dir to the world's Antigravity trustedWorkspaces.
func (w *world) trustAgy(dir string) {
	w.t.Helper()
	path := filepath.Join(w.dir, ".gemini", "antigravity-cli", "settings.json")
	var settings struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			w.t.Fatal(err)
		}
	}
	settings.Trusted = append(settings.Trusted, dir)
	b, err := json.Marshal(settings)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		w.t.Fatal(err)
	}
	w.write(path, string(b))
}
