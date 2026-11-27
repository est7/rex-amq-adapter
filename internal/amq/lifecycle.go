package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	fmt.Fprintf(logf, "\n=== %s spawn handle=%s\n", time.Now().Format(time.RFC3339), w.Handle)
	cmd := exec.Command(amqBin, append([]string{"--no-update-check"}, w.WakeArgs()...)...)
	cmd.Env = amqEnv()
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

// RetireHandle stops the adapter's waker holding handle under root, whatever
// its injector path or generation: the saved target names the handle and
// root, so such a waker is the adapter's by construction, including one
// started by an adoption that died before leasing. A waker that is not the
// adapter's for handle and root is left alone. A lock that cannot be
// identified is an error, so the caller keeps its lease as the credential to
// try again.
func RetireHandle(ctx context.Context, amqBin, root, handle string) (retired bool, err error) {
	st, err := WakeCheck(ctx, amqBin, root, handle)
	if err != nil {
		return false, err
	}
	switch {
	case st.Status == "missing":
		return false, nil
	case st.Generation == "" || !st.HasTarget:
		return false, fmt.Errorf("wake lock for %s cannot be identified (generation %q, target present %v)", handle, st.Generation, st.HasTarget)
	case !wakes(st.Target, handle, root):
		return false, nil
	}
	if err := WakeRetire(ctx, amqBin, root, handle, st); err != nil {
		return false, err
	}
	return true, nil
}

// wakes reports whether t is the adapter's target for handle and root
// (argv: inject <handle> <root>).
func wakes(t WakeTarget, handle, root string) bool {
	a := t.InjectArgs
	return len(a) == 3 && a[0] == "inject" && a[1] == handle && canonPath(a[2]) == canonPath(root)
}

// ErrAmqTooOld is an amq without `init --add`, which the adapter needs to
// make a mailbox in an existing root.
var ErrAmqTooOld = errors.New("amq is too old: rex-amq-adapter needs amq >= 0.172.0 (amq init --add); run `brew update && brew upgrade amq`")

// EnsureMailbox makes handle a registered member of root with its mailbox
// made. A handle already listed in meta/config.json whose mailbox is in
// place (mailboxPresent) is done with no lock and no amq run. Otherwise,
// under the registry lock, `amq init --add` registers it and makes (or
// repairs) its mailbox, touching no other handle; a root with no queue yet
// is created by a plain `amq init`. amq stays the only writer of
// meta/config.json and the mailboxes.
func EnsureMailbox(ctx context.Context, amqBin, root, handle string) error {
	// A config that cannot be read or decoded is amq's to judge: --add
	// validates it and says what is wrong.
	if b, err := os.ReadFile(configPath(root)); err == nil && listed(b, handle) && mailboxPresent(root, handle) {
		return nil
	}
	unlock, err := lockRegistry(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	exists, err := configExists(root)
	if err != nil {
		return err
	}
	if !exists {
		out, _, initErr := runInit(ctx, amqBin, root, handle)
		if initErr == nil {
			return nil
		}
		// A root created meanwhile by amq run outside the adapter refuses
		// the plain init; it only needs handle added.
		if exists, err = configExists(root); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("amq init %s: %w: %s", root, initErr, out)
		}
	}
	return addMailbox(ctx, amqBin, root, handle)
}

// addMailbox runs `amq init --add` for handle in an existing root.
func addMailbox(ctx context.Context, amqBin, root, handle string) error {
	out, code, err := runInit(ctx, amqBin, root, handle, "--add")
	switch {
	case err == nil:
		return nil
	case code == 2 && strings.Contains(out, "flag provided but not defined: -add"):
		return fmt.Errorf("%w: amq init --add %s: %s", ErrAmqTooOld, root, out)
	}
	return fmt.Errorf("amq init --add %s: %w: %s", root, err, out)
}

// runInit runs `amq init` for handle under root with extra flags, and
// returns its combined output and exit code.
func runInit(ctx context.Context, amqBin, root, handle string, extra ...string) (string, int, error) {
	args := append([]string{"--no-update-check", "init", "--root", root, "--agents", handle}, extra...)
	cmd := exec.CommandContext(ctx, amqBin, args...)
	cmd.Env = amqEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	return strings.TrimSpace(out.String()), code, err
}

func configPath(root string) string { return filepath.Join(root, "meta", "config.json") }

func configExists(root string) (bool, error) {
	_, err := os.Lstat(configPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Agents lists the handles registered in root; an uninitialised root has none.
func Agents(root string) ([]string, error) {
	b, err := os.ReadFile(configPath(root))
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

// listed reports whether configJSON decodes and lists handle.
func listed(configJSON []byte, handle string) bool {
	var cfg struct {
		Agents []string `json:"agents"`
	}
	return json.Unmarshal(configJSON, &cfg) == nil && slices.Contains(cfg.Agents, handle)
}

// mailboxPresent reports whether handle's mailbox directory, its inbox,
// and inbox/new are real directories (Lstat: a symlink is not). It checks
// no other leaf: a mailbox missing another of amq's directories, or one
// whose directories hold symlinks further down, passes, and is amq's to
// repair when it next runs for the handle.
func mailboxPresent(root, handle string) bool {
	for _, d := range []string{filepath.Join(root, "agents", handle), filepath.Join(root, "agents", handle, "inbox"), Inbox(root, handle)} {
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// lockRegistry serialises the adapter's runs of `amq init` on root. amq's
// own lock serialises the writes; this one is for older adapters, which
// read the roster and later rewrite all of it under this same lock, so an
// older one still running during an upgrade cannot write back a roster
// read before a handle was added here. The fast path never takes it.
func lockRegistry(ctx context.Context, root string) (func(), error) {
	dir := filepath.Join(root, "meta")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return fsutil.Flock(ctx, filepath.Join(dir, ".adapter-registry.lock"))
}

// Inbox is the directory holding handle's undrained messages (amq's Maildir
// layout: agents/<handle>/inbox/new/<id>.md).
func Inbox(root, handle string) string {
	return filepath.Join(root, "agents", handle, "inbox", "new")
}

// Unread lists the ids of undrained messages in handle's inbox, sorted. A
// missing inbox has none. Only regular files count; the adapter follows no
// links.
func Unread(root, handle string) ([]string, error) {
	return messageIDs(Inbox(root, handle))
}

func outbox(root, handle string) string {
	return filepath.Join(root, "agents", handle, "outbox", "sent")
}

// SentAfter lists the ids of the messages handle sent after the message with
// id after, oldest first: ids start with their timestamp. A missing outbox
// has none.
func SentAfter(root, handle, after string) ([]string, error) {
	ids, err := messageIDs(outbox(root, handle))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ids, func(id string) bool { return id <= after }), nil
}

// SentAt is when a message handle sent reached its outbox.
func SentAt(root, handle, id string) (time.Time, error) {
	st, err := os.Stat(filepath.Join(outbox(root, handle), id+".md"))
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// SentHeader reads the header of message id in handle's outbox.
func SentHeader(root, handle, id string) (Header, error) {
	return ReadHeader(root, handle, BoxSent, id)
}

func messageIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
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

// ErrNoBinary is no amq binary found.
var ErrNoBinary = errors.New("amq not found")

// ResolveBin finds amq on PATH, then in ~/.local/bin and Homebrew.
func ResolveBin() (string, error) {
	if p := os.Getenv("REX_AMQ_AMQ"); p != "" {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return "", fmt.Errorf("%w: REX_AMQ_AMQ=%q names no amq binary", ErrNoBinary, p)
		}
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
	return "", fmt.Errorf("%w on PATH, ~/.local/bin, or Homebrew; set REX_AMQ_AMQ", ErrNoBinary)
}

// Poster sends mail (amq send or reply): CLI and Fake are Posters.
type Poster interface {
	Post(ctx context.Context, root, verb string, args ...string) (Sent, []byte, error)
}

// Send mails body to to, as from, as a status notice.
func Send(ctx context.Context, p Poster, root, from, to, subject, body string) error {
	return SendKind(ctx, p, root, from, to, "status", subject, body)
}

// SendSystem mails body to to, in from's name, as system mail: labelled
// SystemLabel, so the bounce check never reads it as from's own.
func SendSystem(ctx context.Context, p Poster, root, from, to, subject, body string) error {
	_, _, err := p.Post(ctx, root, "send", "--me", from, "--to", to, "--kind", "status", "--priority", "normal", "--labels", SystemLabel, "--subject", subject, "--body", body)
	return err
}

// SendKind mails body to to, as from, with an amq message kind.
func SendKind(ctx context.Context, p Poster, root, from, to, kind, subject, body string) error {
	_, _, err := p.Post(ctx, root, "send", "--me", from, "--to", to, "--kind", kind, "--priority", "normal", "--subject", subject, "--body", body)
	return err
}

// Sent is what amq send and amq reply report with --json.
type Sent struct {
	ID     string   `json:"id"`
	To     []string `json:"to"`
	Thread string   `json:"thread"`
}

// Post runs amq send or amq reply (verb) with args, which name neither the
// root nor --json, and returns what was sent and amq's JSON report. A send
// that failed after delivering (a --wait-for that timed out) returns its id
// with the error.
func Post(ctx context.Context, amqBin, root, verb string, args ...string) (Sent, []byte, error) {
	args, body := literalBody(args)
	out, err := runAmqIn(ctx, amqBin, body, append([]string{verb, "--root", root, "--json"}, args...)...)
	var s Sent
	_ = json.Unmarshal(out, &s)
	if err != nil {
		return s, out, err
	}
	if s.ID == "" {
		return Sent{}, out, fmt.Errorf("amq %s reported no message id: %q", verb, out)
	}
	return s, out, nil
}

// Run runs amq with args, its output going to stdout and stderr, and
// returns its exit status. err is set only when amq could not be run.
func Run(ctx context.Context, amqBin string, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, amqBin, append([]string{"--no-update-check"}, args...)...)
	cmd.Env = amqEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrPermission) {
		// The binary itself cannot be started: as unavailable as no binary.
		return 1, fmt.Errorf("%w: %w", ErrNoBinary, err)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// literalBody moves the --body value out of args, to go on stdin under
// --body -: amq reads its value as @FILE or - for stdin, and a body here is
// already the text to send, so a body starting with "@" must never make
// amq read that file. The body's bytes travel as they are.
func literalBody(args []string) ([]string, io.Reader) {
	out := make([]string, 0, len(args))
	var body io.Reader
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--body" && i+1 < len(args):
			body = strings.NewReader(args[i+1])
			i++
		case strings.HasPrefix(a, "--body="):
			body = strings.NewReader(strings.TrimPrefix(a, "--body="))
		default:
			out = append(out, a)
			continue
		}
		out = append(out, "--body=-")
	}
	return out, body
}
