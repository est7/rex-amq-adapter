package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/install"
)

// trustTemp records, in the world's own Claude store under TranscriptHome,
// the user's trust of the system temp directory, so the panes a test opens
// in its temp directories meet no folder dialog. A launch now preflights
// folder trust, and the user of these worlds has given it.
func trustTemp(t *testing.T, e Env) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.TrustClaudeProject(context.Background(), filepath.Join(e.TranscriptHome, ".claude.json"), tmp); err != nil {
		t.Fatal(err)
	}
}
