package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/out"
)

// hostVars are the variables of the host a test must never inherit: the
// Rex pane and harness session of the agent running the tests, its
// unattended marker, its trust stores, and its Hub.
var hostVars = []string{
	"REX_BLOCK", "REX_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID",
	"ME_UNATTENDED", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR", "ME_HUB", "AGENT_TRACE_ID", "REX_AMQ_ADAPTER_TRACE_ID",
	"REX_AMQ_SUMMON", "CLAUDE_PID", "REX_AMQ_REX", "REX_AMQ_AMQ",
}

// TestMain runs the package with no access to the host's adapter state,
// home, config, or Rex pane, whatever the shell running `go test` holds: a
// test that forgets isolate still cannot act on the live system.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "rae-cmd-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for k, v := range map[string]string{
		"REX_AMQ_ADAPTER_HOME": filepath.Join(root, "state"),
		"HOME":                 filepath.Join(root, "home"),
		"XDG_CONFIG_HOME":      filepath.Join(root, "config"),
		"XDG_STATE_HOME":       filepath.Join(root, "xdg-state"),
		"REX_SERVER":           "unix:///nonexistent/rex.sock",
	} {
		os.Setenv(k, v)
	}
	for _, v := range hostVars {
		os.Unsetenv(v)
	}
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// isolate points the adapter at a fresh empty state directory, home, and
// config, and no Rex pane, for one test.
func isolate(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_ADAPTER_HOME", filepath.Join(home, "state"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("REX_SERVER", "unix:///nonexistent/rex.sock")
	for _, v := range hostVars {
		t.Setenv(v, "")
	}
	// Crews come from the working directory's parents: not the host's.
	t.Chdir(t.TempDir())
	return home
}

// stubTools points rex and amq at a script that fails every call, so a
// test proves a command never reached either.
func stubTools(t *testing.T) {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho stub called >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
}

// result is one invocation's exit status and streams.
type result struct {
	code           int
	stdout, stderr string
}

// envelope is the v:2 envelope as a caller decodes it.
type envelope struct {
	OK    bool            `json:"ok"`
	V     int             `json:"v"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Retryable  bool           `json:"retryable"`
		Hint       string         `json:"hint"`
		NextAction *string        `json:"next_action"`
		Details    map[string]any `json:"details"`
	} `json:"error"`
	Meta struct {
		Verb    string `json:"verb"`
		TraceID string `json:"trace_id"`
	} `json:"meta"`
	Warnings []string `json:"warnings"`
}

// invoke runs the CLI in process, off a terminal, with stdin.
func invoke(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var o, e bytes.Buffer
	var in io.Reader = strings.NewReader(stdin)
	code := run(args, out.Streams{Stdout: &o, Stderr: &e}, os.Getenv, in)
	return result{code: code, stdout: o.String(), stderr: e.String()}
}

// machine runs args and decodes its one envelope line, failing the test
// when machine mode broke its contract: anything on stderr, or anything
// but one JSON line on stdout.
func machine(t *testing.T, args ...string) (int, envelope) {
	t.Helper()
	r := invoke(t, "", args...)
	return r.code, decode(t, args, r)
}

func decode(t *testing.T, args []string, r result) envelope {
	t.Helper()
	if r.stderr != "" {
		t.Errorf("%q: stderr in machine mode: %q", args, r.stderr)
	}
	if strings.Count(r.stdout, "\n") != 1 || !strings.HasSuffix(r.stdout, "\n") {
		t.Errorf("%q: stdout is not one line: %q", args, r.stdout)
	}
	var env envelope
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("%q: stdout is not an envelope: %v\n%s", args, err, r.stdout)
	}
	if env.V != 2 {
		t.Errorf("%q: v=%d", args, env.V)
	}
	if env.OK != (env.Error == nil) {
		t.Errorf("%q: ok=%v with error %+v", args, env.OK, env.Error)
	}
	return env
}

// code is an envelope's error code, "" on success.
func (e envelope) code() string {
	if e.Error == nil {
		return ""
	}
	return e.Error.Code
}
