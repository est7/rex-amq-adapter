package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// agy stops asking after repeated failures, so the title command always
// answers: outside Rex, for an unknown kind, or with unreadable input it
// names the directory, or prints an empty line.
func TestPaneTitleAlwaysAnswers(t *testing.T) {
	t.Setenv("REX_BLOCK", "")
	if got := paneTitle("agy", strings.NewReader(`{"cwd":"/w/proj","conversation_id":"c"}`)); got != "proj" {
		t.Errorf("outside Rex: %q", got)
	}
	if got := paneTitle("nope", strings.NewReader(`{"cwd":"/w/proj"}`)); got != "proj" {
		t.Errorf("unknown kind: %q", got)
	}
	if got := paneTitle("agy", strings.NewReader(`not json`)); got != "" {
		t.Errorf("unreadable input: %q", got)
	}
}

// `eval ""` succeeds, so a failing env must print a failing command: agents
// run `eval "$(rex-amq-adapter env)" && …` and must not carry on without an
// identity.
func TestAFailingEnvStopsTheEvalChain(t *testing.T) {
	t.Setenv("REX_BLOCK", "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	code := runEnv()
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	out, _ := io.ReadAll(r)
	if code == 0 || !strings.HasPrefix(string(out), "false\n") {
		t.Errorf("env outside Rex: exit %d, output %q", code, out)
	}
}
