// Package e2e drives the adapter's real binary against a real Rex server and
// real amq wakers. The agents are scripts in real terminals that report OSC
// 7501 and draw Claude Code's input box, so faults can be scripted without a
// model. Every test gets its own server, home, and state, and never touches
// the machine's Rex or AMQ.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// agentScript is a scripted agent. It adopts itself with a real hook, reports
// idle, draws an empty box, and for each line typed reports working, appends
// the line to <name>.got, and reports idle again. Sending it `quit` runs
// SessionEnd; `crash` exits without running any hook.
const agentScript = `name=$1
status() { printf '\033]7501;state=%s:app=claude-code\033\\' "$1"; }
rule() { printf '%s\r\n' '────────────────────────────────────────'; }
draw() { printf '\033[2J\033[H'; printf 'scripted agent %s\r\n' "$name"; rule; printf '❯ \r\n'; rule; printf '\033[3;3H'; }
hook() { printf '{"session_id":"%s"}' "$name" | "$ADAPTER" hook "$1" --agent claude >>"$DIR/$name.hook" 2>&1; }
stty -echo
hook session-start
status idle
draw
: >"$DIR/$name.ready"
while read -r line; do
	status working
	printf '%s\n' "$line" >>"$DIR/$name.got"
	case "$line" in
	*quit*) hook session-end; exit 0 ;;
	*crash*) exit 1 ;;
	esac
	sleep 0.3
	status idle
	draw
done
`

// world is one isolated Rex server, adapter state, and amq root.
type world struct {
	t       *testing.T
	dir     string // short: a Unix socket lives below it
	server  string
	rexBin  string
	amqBin  string
	adapter string
	home    string // REX_AMQ_ADAPTER_HOME
	env     []string
	// lastBlock is shown when a test fails.
	lastBlock string
}

// adapterBin is built once per test binary.
var (
	adapterBin string
	adapterErr error
	buildOnce  sync.Once
)

func TestMain(m *testing.M) {
	code := m.Run()
	if adapterBin != "" {
		_ = os.Remove(adapterBin)
	}
	os.Exit(code)
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("needs real Rex and amq servers")
	}
	t.Parallel() // each world is private
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("rex is not installed")
	}
	amqBin, err := exec.LookPath("amq")
	if err != nil {
		t.Skip("amq is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "rae") // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	buildOnce.Do(func() {
		adapterBin = filepath.Join(os.TempDir(), fmt.Sprintf("rae-adapter-%d", os.Getpid()))
		if out, err := exec.Command("go", "build", "-o", adapterBin, "github.com/est7/rex-amq-adapter/cmd/rex-amq-adapter").CombinedOutput(); err != nil {
			adapterErr = fmt.Errorf("build adapter: %w\n%s", err, out)
		}
	})
	if adapterErr != nil {
		t.Fatal(adapterErr)
	}
	w := &world{
		t: t, dir: dir, server: "unix://" + filepath.Join(dir, "s"),
		rexBin: rexBin, amqBin: amqBin, adapter: adapterBin,
		home: filepath.Join(t.TempDir(), "state"), // amq refuses an injector under /tmp
	}
	config := filepath.Join(dir, "init.lua")
	script := filepath.Join(dir, "agent.sh")
	w.write(config, "")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "REX_") && !strings.HasPrefix(name, "AM_") && name != "HOME" {
			w.env = append(w.env, kv)
		}
	}
	w.env = append(w.env, "HOME="+dir, "REX_CONFIG="+config, "REX_AMQ_ADAPTER_HOME="+w.home,
		"REX_AMQ_AMQ="+amqBin, "REX_AMQ_REX="+rexBin, "ADAPTER="+adapterBin, "DIR="+dir, "AMQ_NO_UPDATE_CHECK=1")
	// A terminal starts from Rex's own environment, so the script carries its own.
	preamble := "#!/bin/sh\n"
	for _, kv := range []string{"HOME=" + dir, "REX_AMQ_ADAPTER_HOME=" + w.home, "REX_AMQ_AMQ=" + amqBin, "REX_AMQ_REX=" + rexBin, "ADAPTER=" + adapterBin, "DIR=" + dir, "REX_SERVER=" + w.server} {
		preamble += "export '" + kv + "'\n"
	}
	w.write(script, preamble+agentScript)
	w.startServer()
	return w
}

func (w *world) write(path, text string) {
	w.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) startServer() {
	w.t.Helper()
	cmd := exec.Command(w.rexBin, "--autostart=false", "server", "run", "--listen", w.server,
		"--state-dir", filepath.Join(w.dir, "rex"), "--restore=false")
	cmd.Env = w.env
	log, err := os.Create(filepath.Join(w.dir, "server.log"))
	if err != nil {
		w.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() {
		w.stopWakers()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Close()
	})
	w.until("the Rex server to answer", 10*time.Second, func() bool {
		_, err := w.rexErr("whoami", "--json")
		return err == nil
	})
}

// stopWakers kills what the agents started, so nothing outlives the test.
func (w *world) stopWakers() {
	_ = exec.Command("pkill", "-f", w.watcherPattern()).Run()
	entries, _ := os.ReadDir(filepath.Join(w.home, "amq-root", "agents"))
	for _, e := range entries {
		if p := w.wakerPID(e.Name()); p > 0 {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	}
}

func (w *world) watcherPattern() string { return "__internal watch " + w.server + " " }

func (w *world) watcherRuns() bool {
	return exec.Command("pgrep", "-f", w.watcherPattern()).Run() == nil
}

func (w *world) wakerPID(handle string) int {
	out, _ := exec.Command(w.amqBin, "--no-update-check", "wake", "check", "--root", w.root(), "--me", handle, "--json", "--json-schema", "2").Output()
	var doc struct {
		Wake struct {
			PID int `json:"pid"`
		} `json:"wake"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return 0
	}
	return doc.Wake.PID
}

// killWaker is a waker dying on its own, its lock left behind.
func (w *world) killWaker(handle string) {
	w.t.Helper()
	pid := w.wakerPID(handle)
	if pid <= 0 {
		w.t.Fatalf("%s has no waker\n%s", handle, w.dump())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		w.t.Fatal(err)
	}
	w.until("the waker to die", 5*time.Second, func() bool { return syscall.Kill(pid, 0) != nil })
}

// who runs the adapter's who as the agent in block would.
func (w *world) who(block string, args ...string) (string, int) {
	w.t.Helper()
	cmd := exec.Command(w.adapter, append([]string{"who"}, args...)...)
	cmd.Env = append(w.env[:len(w.env):len(w.env)], "REX_SERVER="+w.server, "REX_BLOCK="+block)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	return string(out), 0
}

func (w *world) root() string { return filepath.Join(w.home, "amq-root") }

func (w *world) rexErr(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.rexBin, append([]string{"--autostart=false", "-S", w.server}, args...)...)
	cmd.Env = w.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("rex %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func (w *world) rex(args ...string) string {
	w.t.Helper()
	out, err := w.rexErr(args...)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// until fails the test when cond stays false for d.
func (w *world) until(what string, d time.Duration, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s\n%s", what, w.dump())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (w *world) dump() string {
	var b strings.Builder
	sessions, _ := w.rexErr("ls", "--json")
	fmt.Fprintf(&b, "--- sessions\n%s\n", sessions)
	if w.lastBlock != "" {
		screen, err := w.rexErr("capture", "-b", w.lastBlock)
		fmt.Fprintf(&b, "--- screen of %s (%v)\n%s\n", w.lastBlock, err, screen)
	}
	files, _ := filepath.Glob(filepath.Join(w.dir, "*.hook"))
	more, _ := filepath.Glob(filepath.Join(w.home, "logs", "*"))
	inject, _ := filepath.Glob(filepath.Join(w.home, "inject", "*"))
	for _, f := range append(append(files, more...), inject...) {
		text, _ := os.ReadFile(f)
		fmt.Fprintf(&b, "--- %s\n%s\n", f, text)
	}
	return b.String()
}

// tab is one Rex tab with its first agent running.
type tab struct {
	w     *world
	first string
}

func firstBlock(w *world, out []byte) string {
	w.t.Helper()
	var doc struct {
		Blocks []string `json:"block_ids"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || len(doc.Blocks) != 1 {
		w.t.Fatalf("decode %q: %v", out, err)
	}
	return doc.Blocks[0]
}

// newTab opens a tab with an agent called name in a pane labelled role.
func (w *world) newTab(name, role string) *tab {
	w.t.Helper()
	var doc struct {
		Session string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(w.rex("new", "e2e-"+name, "-c", w.dir, "--json")), &doc); err != nil {
		w.t.Fatal(err)
	}
	w.lastBlock = firstBlock(w, []byte(w.rex(w.start("run", name, role, "-s", doc.Session)...)))
	return &tab{w: w, first: w.lastBlock}
}

// start is the rex command line that puts an agent in a new block. Rex would
// otherwise start it through the user's login shell, in this empty home.
func (w *world) start(verb, name, role string, where ...string) []string {
	args := append([]string{verb}, where...)
	return append(args, "--shell=none", "--focus=false", "--label", role, "-c", w.dir, "--json", "--", "/bin/sh", filepath.Join(w.dir, "agent.sh"), name)
}

// split adds an agent called name beside the tab's first one.
func (tb *tab) split(name, role string) string {
	tb.w.t.Helper()
	return firstBlock(tb.w, []byte(tb.w.rex(tb.w.start("split", name, role, "-b", tb.first, "--split=right")...)))
}

func (w *world) ready(name string) {
	w.t.Helper()
	w.until(name+" to adopt", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(w.dir, name+".ready"))
		return err == nil
	})
}

func (w *world) got(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.dir, name+".got"))
	return string(b)
}

// send mails to from a peer's handle through real amq.
func (w *world) send(from, to, body string) {
	w.t.Helper()
	cmd := exec.Command(w.amqBin, "--no-update-check", "send", "--root", w.root(), "--me", from, "--to", to, "--subject", "e2e", "--body", body)
	cmd.Env = w.env
	if out, err := cmd.CombinedOutput(); err != nil {
		w.t.Fatalf("amq send: %v\n%s\n%s", err, out, w.dump())
	}
}

// adapterRun runs the adapter from outside any block.
func (w *world) adapterRun(args ...string) (string, int) {
	w.t.Helper()
	cmd := exec.Command(w.adapter, args...)
	cmd.Env = append(w.env[:len(w.env):len(w.env)], "REX_SERVER="+w.server)
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	w.t.Fatal(err)
	return "", 0
}

// program starts a script that is no agent, in a new tab of the first session.
func (w *world) program(script string) string {
	w.t.Helper()
	var doc struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(w.rex("ls", "--json")), &doc); err != nil {
		w.t.Fatal(err)
	}
	args := []string{"new", "e2e-program", "-c", w.dir, "--json"}
	session := ""
	if len(doc.Sessions) > 0 {
		session = doc.Sessions[0].ID
	} else {
		var created struct {
			Session string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(w.rex(args...)), &created); err != nil {
			w.t.Fatal(err)
		}
		session = created.Session
	}
	return firstBlock(w, []byte(w.rex("run", "-s", session, "--shell=none", "--focus=false", "-c", w.dir, "--json", "--", "/bin/sh", "-c", script)))
}

// typeInto gives a scripted agent a line, as a person would.
func (w *world) typeInto(block, line string) {
	w.t.Helper()
	w.rex("block", "call", "-b", block, "write", fmt.Sprintf(`{"data":%q}`, line+"\r"))
}
