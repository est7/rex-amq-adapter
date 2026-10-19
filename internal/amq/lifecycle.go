package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// WakeArgs is the argv for `amq wake`. --retry-until injected stops
// re-announcing a cohort once the injector proves acceptance; deferred keeps
// it on amq's ladder. The inject timeout leaves the injector room to confirm
// that the agent started after a typed doorbell.
func (w Waker) WakeArgs() []string {
	t := w.Target()
	args := []string{
		"wake",
		"--root", w.Root,
		"--me", w.Handle,
		"--inject-via", t.InjectVia,
	}
	for _, a := range t.InjectArgs {
		args = append(args, "--inject-arg", a)
	}
	return append(args,
		"--retry-until", t.RetryUntil,
		"--inject-timeout", "10s",
		"--interrupt-cmd", "none",
		"--no-self-upgrade",
	)
}

// Spawn starts the waker detached: its own session, stdio to a log file, no
// inherited pipes (the hook's stdout is read by the agent and must close when
// the hook exits). Starting is not adoption: a start that loses amq's lock
// race exits on its own, so callers must AwaitLive before recording it.
func Spawn(amqBin string, w Waker, logDir string) error {
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(logDir, w.Handle+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	fmt.Fprintf(logf, "\n=== %s spawn handle=%s block=%s server=%s\n", time.Now().Format(time.RFC3339), w.Handle, w.Block, w.Server)
	cmd := exec.Command(amqBin, append([]string{"--no-update-check"}, w.WakeArgs()...)...)
	cmd.Dir = w.Root
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start amq wake: %w", err)
	}
	return cmd.Process.Release()
}

// Ensure leaves exactly one live waker for w with w's target and returns it:
// keep a matching live one, repair a stale one with the same target, or
// retire whatever else holds the lock (by its exact identity and generation)
// and start a fresh one.
func Ensure(ctx context.Context, amqBin string, w Waker, logDir string) (WakeState, WakeDecision, error) {
	want := w.Target()
	st, err := WakeCheck(ctx, amqBin, w.Root, w.Handle)
	if err != nil {
		return WakeState{}, 0, err
	}
	decision := DecideWake(st, want)
	switch decision {
	case DecisionKeep:
		return st, decision, nil
	case DecisionRepair:
		if err := WakeRepair(ctx, amqBin, w.Root, w.Handle); err != nil {
			return WakeState{}, decision, err
		}
	case DecisionStart, DecisionReplace:
		if err := WakeRetire(ctx, amqBin, w.Root, w.Handle, st); err != nil {
			return WakeState{}, decision, err
		}
		if err := Spawn(amqBin, w, logDir); err != nil {
			return WakeState{}, decision, err
		}
	}
	live, err := AwaitLive(ctx, amqBin, w.Root, w.Handle, want)
	return live, decision, err
}

// RetireFor stops the waker holding handle if it wakes block, whatever its
// injector path or generation: the saved target names the block, so a waker
// for this block is the adapter's by construction, including one started by
// an adoption that died before recording it. A waker for another block (the
// handle was reused) is left alone. A lock that cannot be identified is an
// error, so the caller keeps its record as the credential to try again.
func RetireFor(ctx context.Context, amqBin, root, handle, block string) (retired bool, err error) {
	st, err := WakeCheck(ctx, amqBin, root, handle)
	if err != nil {
		return false, err
	}
	switch {
	case st.Status == "missing":
		return false, nil
	case st.Generation == "" || !st.HasTarget:
		return false, fmt.Errorf("wake lock for %s cannot be identified (generation %q, target present %v)", handle, st.Generation, st.HasTarget)
	case !wakes(st.Target, block, handle, root):
		return false, nil
	}
	if err := WakeRetire(ctx, amqBin, root, handle, st); err != nil {
		return false, err
	}
	return true, nil
}

// wakes reports whether t is an adapter target for block, handle and root
// (argv: inject <server> <block> <handle> <root>).
func wakes(t WakeTarget, block, handle, root string) bool {
	a := t.InjectArgs
	return len(a) == 5 && a[0] == "inject" && a[2] == block && a[3] == handle && canonPath(a[4]) == canonPath(root)
}

// EnsureMailbox makes handle a provisioned member of root. `amq init --force`
// is idempotent for directories and rewrites meta/config.json with the
// merged agent list; amq creates the mailbox directories with its own
// validation, and a waker without them never acquires its inbox watcher.
func EnsureMailbox(ctx context.Context, amqBin, root, handle string) error {
	unlock, err := lockRegistry(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	agents := []string{handle}
	b, err := os.ReadFile(filepath.Join(root, "meta", "config.json"))
	switch {
	case err == nil:
		merged, added, err := agentsWith(b, handle)
		if err != nil {
			return err
		}
		if !added && mailboxPresent(root, handle) {
			return nil
		}
		agents = merged
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	cmd := exec.CommandContext(ctx, amqBin, "--no-update-check", "init", "--root", root, "--agents", strings.Join(agents, ","), "--force")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("amq init --force %s: %w: %s", root, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Agents lists the handles registered in root; an uninitialised root has none.
func Agents(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "meta", "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("decode amq config: %w", err)
	}
	return cfg.Agents, nil
}

func agentsWith(configJSON []byte, handle string) ([]string, bool, error) {
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, false, fmt.Errorf("decode amq config: %w", err)
	}
	for _, a := range cfg.Agents {
		if a == handle {
			return cfg.Agents, false, nil
		}
	}
	agents := append(append([]string{}, cfg.Agents...), handle)
	sort.Strings(agents)
	return agents, true, nil
}

func mailboxPresent(root, handle string) bool {
	st, err := os.Stat(filepath.Join(root, "agents", handle, "inbox", "new"))
	return err == nil && st.IsDir()
}

// lockRegistry serialises every writer of meta/config.json.
func lockRegistry(ctx context.Context, root string) (func(), error) {
	dir := filepath.Join(root, "meta")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return fsutil.Flock(ctx, filepath.Join(dir, ".adapter-registry.lock"))
}

// Unread lists the ids of undrained messages in handle's inbox (amq's
// Maildir layout: agents/<handle>/inbox/new/<id>.md), sorted. A missing inbox
// has none. Only regular files count; the adapter follows no links.
func Unread(root, handle string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "agents", handle, "inbox", "new"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".md"); ok && e.Type().IsRegular() {
			ids = append(ids, name)
		}
	}
	return ids, nil
}

// ResolveBin finds amq on PATH, then in ~/.local/bin and Homebrew.
func ResolveBin() (string, error) {
	if p := os.Getenv("REX_AMQ_AMQ"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("amq"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".local", "bin", "amq"), "/opt/homebrew/bin/amq", "/usr/local/bin/amq"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("amq not found on PATH, ~/.local/bin, or Homebrew; set REX_AMQ_AMQ")
}
