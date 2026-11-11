package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// `tab <crew>` claims the tab it opens in the tab ledger: a crew tab under
// the crew its label leads with, a narrator tab under "narrator".
func TestCrewTabCommandClaimsTheTab(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "rex")
	script := `#!/bin/sh
case "$*" in
  *session.new_window*) echo '{"window_id":"window:new","block_ids":["block:new"]}' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", bin)
	t.Setenv("REX_AMQ_ADAPTER_HOME", dir)
	t.Setenv("REX_SERVER", "unix:///test.sock")
	st := store.Store{P: store.Paths{Home: dir}}
	for label, want := range map[string]store.Tab{
		"ship-topic | release": {WindowID: "window:new", SessionID: "session:t", Mode: core.WindowCrew, Crew: "ship-topic", LastLabel: "ship-topic | release"},
		"narrator":             {WindowID: "window:new", SessionID: "session:t", Mode: core.WindowNarrator, Crew: "narrator", LastLabel: "narrator"},
	} {
		if code := runTab([]string{label, "-s", "session:t", "-c", dir, "--", "true"}); code != 0 {
			t.Fatalf("tab %q exited %d", label, code)
		}
		if got, ok, err := st.Tab("window:new"); err != nil || !ok || got != want {
			t.Errorf("tab %q recorded %+v (ok=%v err=%v), want %+v", label, got, ok, err, want)
		}
	}
}
