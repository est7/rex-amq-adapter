package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/cli"
)

// The black-box contract, run against the built binary as a separate
// process (so stdout is a real pipe, never a terminal) with an environment
// built from nothing: the gates of verify-cli-contract.sh, with cases
// derived from the registry so a new command is covered the day it lands.
// It needs no Rex or amq and runs in CI (go test -short).
func TestBlackBoxContract(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, "rex-amq-adapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state := filepath.Join(work, "state")
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "REX_AMQ_ADAPTER_HOME=" + state, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"), "REX_SERVER=unix:///nonexistent/rex.sock", "REX_AMQ_REX=" + filepath.Join(work, "no-rex"),
		"REX_AMQ_AMQ=" + filepath.Join(work, "no-amq")}
	type proc struct {
		code           int
		stdout, stderr string
	}
	runBin := func(extra []string, args ...string) proc {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(slices.Clone(base), extra...)
		cmd.Dir = work
		var o, e bytes.Buffer
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &o, &e
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%q blocked past 10s", args)
		}
		return proc{cmd.ProcessState.ExitCode(), o.String(), e.String()}
	}
	envelopeOf := func(args []string, p proc) map[string]any {
		t.Helper()
		var m map[string]any
		if p.stderr != "" {
			t.Errorf("%q: stderr in machine mode: %q", args, p.stderr)
		}
		if err := json.Unmarshal([]byte(p.stdout), &m); err != nil || strings.Count(p.stdout, "\n") != 1 {
			t.Errorf("%q: stdout is not one JSON line: %q", args, p.stdout)
			return nil
		}
		if strings.ContainsRune(p.stdout, 0x1b) {
			t.Errorf("%q: ANSI on stdout", args)
		}
		if m["v"] != float64(2) {
			t.Errorf("%q: v=%v", args, m["v"])
		}
		return m
	}
	errCode := func(m map[string]any) string {
		e, _ := m["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}

	// Missing required input: exit 2, JSON on stdout, nothing on stderr.
	for _, cmd := range registry().Commands {
		required := slices.ContainsFunc(cmd.Args, func(a cli.Arg) bool { return a.Required }) ||
			slices.ContainsFunc(cmd.Flags, func(f cli.Flag) bool { return f.Required })
		if !required {
			continue
		}
		p := runBin(nil, cmd.Path...)
		if m := envelopeOf(cmd.Path, p); p.code != 2 || errCode(m) != "usage" {
			t.Errorf("%q with nothing: exit %d %q, want 2 usage", cmd.Path, p.code, errCode(m))
		}
	}
	// Every legacy verb is a v:2 usage error.
	for _, v := range []string{"who", "env", "send", "view", "hand-back", "notify-personal", "events", "panel", "setup"} {
		p := runBin(nil, v)
		if m := envelopeOf([]string{v}, p); p.code != 2 || errCode(m) != "usage" {
			t.Errorf("legacy %s: exit %d %q", v, p.code, errCode(m))
		}
	}
	// Human-only commands under ME_UNATTENDED=1: exit 6.
	for _, cmd := range registry().Commands {
		if cmd.Guard != cli.GuardHuman {
			continue
		}
		args := invocation(t, cmd, "", "", "")
		p := runBin([]string{"ME_UNATTENDED=1"}, args...)
		if m := envelopeOf(args, p); p.code != 6 || errCode(m) != "guard_denied" {
			t.Errorf("%q unattended: exit %d %q, want 6", args, p.code, errCode(m))
		}
	}
	// --text failures: nothing on stdout, a diagnostic on stderr, no ANSI.
	for _, args := range [][]string{{"nonesuch", "--text"}, {"skill", "show", "nope", "--text"}, {"agent", "retire", "../x", "--text"}} {
		p := runBin(nil, args...)
		if p.code == 0 || p.stdout != "" || !strings.HasPrefix(p.stderr, "rex-amq-adapter: ") || strings.ContainsRune(p.stderr, 0x1b) {
			t.Errorf("%q: exit %d stdout %q stderr %q", args, p.code, p.stdout, p.stderr)
		}
	}
	// schema needs no home, config, or PATH.
	empty := t.TempDir()
	for _, args := range [][]string{{"schema"}, {"schema", "errors"}, {"schema", "agent", "summon"}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"HOME=" + empty, "XDG_CONFIG_HOME=" + filepath.Join(empty, "c"), "XDG_STATE_HOME=" + filepath.Join(empty, "s"), "PATH=" + filepath.Join(empty, "p")}
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		if m := envelopeOf(args, proc{0, o.String(), e.String()}); err != nil || m["ok"] != true {
			t.Errorf("%q offline: %v %s", args, err, o.String())
		}
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("schema wrote to an empty home: %v", entries)
	}
	// Nothing above created state: the pre-flight failures wrote nothing.
	if _, err := os.Stat(state); err == nil {
		entries, _ := os.ReadDir(state)
		t.Errorf("the state directory was created: %v", entries)
	}
}

// A finite command a signal ends answers, after cleanup, with one
// envelope naming the signal: terminated (143) for SIGTERM, interrupted
// (130) for SIGINT, never the failure its cleanup produced.
func TestSignalEndsAFiniteCommandWithItsCode(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(work, "rex-amq-adapter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// The stub marks that the adapter is waiting on it (past its signal
	// setup), then becomes the sleep, so a kill ends it.
	slow, marker := filepath.Join(work, "rex"), filepath.Join(work, "asked")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\ntouch "+marker+"\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sig  syscall.Signal
		code string
		exit int
	}{{syscall.SIGTERM, "terminated", 143}, {syscall.SIGINT, "interrupted", 130}} {
		cmd := exec.Command(bin, "agent", "await", "block:01a1eac4-f234-7985-8bef-2ea92c753265", "--timeout", "60s")
		cmd.Env = []string{"HOME=" + work, "PATH=/usr/bin:/bin", "REX_AMQ_ADAPTER_HOME=" + filepath.Join(work, "state"), "REX_AMQ_REX=" + slow, "REX_AMQ_AMQ=" + slow, "REX_SERVER=unix:///nonexistent/rex.sock"}
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		_ = os.Remove(marker)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(end) {
				_ = cmd.Process.Kill()
				t.Fatalf("%s: the adapter never asked rex", c.sig)
			}
		}
		if err := cmd.Process.Signal(c.sig); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(o.Bytes(), &env) != nil || cmd.ProcessState.ExitCode() != c.exit || env.Error.Code != c.code || strings.Count(o.String(), "\n") != 1 || e.Len() != 0 {
			t.Errorf("%s: exit %d stdout %q stderr %q", c.sig, cmd.ProcessState.ExitCode(), o.String(), e.String())
		}
	}
}
