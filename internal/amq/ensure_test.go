package amq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// realAmq is the installed amq binary. Without one the test skips, except
// under REX_AMQ_REAL=1, which promises real amq and so fails instead.
func realAmq(t *testing.T) string {
	t.Helper()
	bin, err := ResolveBin()
	if err != nil {
		if os.Getenv("REX_AMQ_REAL") == "1" {
			t.Fatalf("REX_AMQ_REAL=1 but %v", err)
		}
		t.Skip(err)
	}
	return bin
}

// stubAmq is a script standing in for amq: it appends its argv to a log,
// one call per line, and then runs body (sh). It returns the script and a
// reader of the logged calls.
func stubAmq(t *testing.T, body string) (string, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "amq")
	text := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(script, []byte(text), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, func() []string {
		b, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
}

// A handle already registered with its mailbox made is found without any
// lock: a process holding the bootstrap lock (an older adapter, or a
// bootstrap in flight) does not hold up every hook that only checks its own
// mailbox.
func TestEnsureMailboxFastPathTakesNoLock(t *testing.T) {
	bin := realAmq(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := EnsureMailbox(context.Background(), bin, root, "alice"); err != nil {
		t.Fatal(err)
	}
	unlock, err := fsutil.Flock(context.Background(), filepath.Join(root, "meta", ".adapter-registry.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := EnsureMailbox(ctx, bin, root, "alice"); err != nil {
		t.Fatalf("the fast path waited for the bootstrap lock: %v", err)
	}
}

// A maker of a mailbox in an existing root asks amq to add just that
// handle; it never rewrites the roster. The first maker of a root creates
// it with a plain init.
func TestEnsureMailboxAddsToAnExistingRootAndNeverForces(t *testing.T) {
	bin := realAmq(t)
	script, calls := stubAmq(t, "exec '"+bin+"' \"$@\"")
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	for _, h := range []string{"alice", "bob", "alice"} {
		if err := EnsureMailbox(ctx, script, root, h); err != nil {
			t.Fatalf("mailbox %s: %v", h, err)
		}
	}
	got := calls()
	want := []string{
		"--no-update-check init --root " + root + " --agents alice",
		"--no-update-check init --root " + root + " --agents bob --add",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("amq calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An amq without --add is named as too old, with the version to upgrade
// to; any other refusal of the command is amq's own error.
func TestEnsureMailboxNamesAnAmqTooOldForAdd(t *testing.T) {
	for name, c := range map[string]struct {
		body   string
		tooOld bool
	}{
		"no --add flag": {"echo 'flag provided but not defined: -add' >&2; exit 2", true},
		"other usage":   {"echo '--add cannot be combined with --force' >&2; exit 2", false},
		"bad handle":    {"echo 'agent handle must match [a-z0-9_-]+' >&2; exit 1", false},
	} {
		t.Run(name, func(t *testing.T) {
			script, _ := stubAmq(t, c.body)
			root := t.TempDir()
			// A fixture standing in for the root amq made: only its
			// presence matters to the stub.
			if err := os.MkdirAll(filepath.Join(root, "meta"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "meta", "config.json"), []byte(`{"agents":["x"]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			err := EnsureMailbox(context.Background(), script, root, "alice")
			if err == nil || errors.Is(err, ErrAmqTooOld) != c.tooOld {
				t.Fatalf("err %v, too old %v, want too old %v", err, errors.Is(err, ErrAmqTooOld), c.tooOld)
			}
			if c.tooOld && !strings.Contains(err.Error(), "0.172.0") {
				t.Errorf("the error names no version to upgrade to: %v", err)
			}
		})
	}
}

// A root created by someone else while this maker bootstraps (amq run
// outside the adapter) refuses the plain init; the handle is then added.
// A bootstrap that fails with no root made is an error, after one try.
func TestEnsureMailboxBootstrapsOnce(t *testing.T) {
	t.Run("another creator won", func(t *testing.T) {
		root := t.TempDir()
		cfg := filepath.Join(root, "meta", "config.json")
		script, calls := stubAmq(t, `case "$*" in
*--add) exit 0 ;;
*) mkdir -p '`+filepath.Dir(cfg)+`'; echo '{"agents":["x"]}' > '`+cfg+`'; echo 'config already exists' >&2; exit 1 ;;
esac`)
		if err := EnsureMailbox(context.Background(), script, root, "alice"); err != nil {
			t.Fatal(err)
		}
		if got := calls(); len(got) != 2 || strings.HasSuffix(got[0], "--add") || !strings.HasSuffix(got[1], "--add") {
			t.Fatalf("amq calls %q: want a plain init, then --add", got)
		}
	})
	t.Run("init fails", func(t *testing.T) {
		script, calls := stubAmq(t, "echo 'disk full' >&2; exit 1")
		err := EnsureMailbox(context.Background(), script, t.TempDir(), "alice")
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("err %v, want amq's", err)
		}
		if got := calls(); len(got) != 1 {
			t.Fatalf("amq calls %q: want one plain init", got)
		}
	})
}

// Adding a handle keeps everything amq keeps: the roster's order, its
// creation time, and keys the adapter does not know.
func TestEnsureMailboxKeepsTheConfig(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "root")
			for _, h := range []string{"carol", "alice"} {
				if err := tc.q.EnsureMailbox(ctx, root, h); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "meta", "config.json")
			cfg := readConfig(t, path)
			// A fixture: a key a newer amq might write.
			cfg["x_extra"] = map[string]any{"k": 1.0}
			b, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.q.EnsureMailbox(ctx, root, "bob"); err != nil {
				t.Fatal(err)
			}
			after := readConfig(t, path)
			if got := fmt.Sprint(after["agents"]); got != "[carol alice bob]" {
				t.Errorf("roster %s, want [carol alice bob]", got)
			}
			if after["created_utc"] != cfg["created_utc"] || after["version"] != cfg["version"] {
				t.Errorf("created_utc/version changed: %v -> %v", cfg, after)
			}
			if fmt.Sprint(after["x_extra"]) != "map[k:1]" {
				t.Errorf("the unknown key was lost: %v", after)
			}
		})
	}
}

// Makers racing on one root, its creation included, each end with their
// handle registered once and its mailbox made.
func TestEnsureMailboxConcurrentMakers(t *testing.T) {
	for _, tc := range targets(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			root := filepath.Join(t.TempDir(), "root")
			var hs []string
			for i := 1; i <= 8; i++ {
				hs = append(hs, fmt.Sprintf("codex-%02d", i))
			}
			var wg sync.WaitGroup
			errs := make([]error, len(hs))
			for i, h := range hs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = tc.q.EnsureMailbox(ctx, root, h)
				}()
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			got, err := Agents(root)
			if err != nil {
				t.Fatal(err)
			}
			sorted := slices.Clone(got)
			slices.Sort(sorted)
			if !slices.Equal(sorted, hs) {
				t.Fatalf("roster %v, want each of %v once", got, hs)
			}
			for _, h := range hs {
				if !mailboxPresent(root, h) {
					t.Errorf("%s has no mailbox", h)
				}
			}
		})
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}
