package main

import (
	"strings"
	"testing"
)

// agy stops asking after repeated failures, so the title command always
// answers: outside Rex, for an unknown kind, or with unreadable input it
// names the directory, or prints an empty line.
func TestPaneTitleAlwaysAnswers(t *testing.T) {
	t.Setenv("REX_BLOCK", "")
	if got := paneTitle("agy", strings.NewReader(`{"cwd":"/w/proj","conversation_id":"c","agent_state":"idle"}`)); got != "proj" {
		t.Errorf("outside Rex: %q", got)
	}
	if got := paneTitle("nope", strings.NewReader(`{"cwd":"/w/proj"}`)); got != "proj" {
		t.Errorf("unknown kind: %q", got)
	}
	if got := paneTitle("agy", strings.NewReader(`not json`)); got != "" {
		t.Errorf("unreadable input: %q", got)
	}
}

// `eval ""` succeeds, so a failing `agent env --sh` must print a failing
// command: agents run `eval "$(rex-amq-adapter agent env --sh)" && …` and
// must not carry on without an identity. It answers on the shell protocol
// only: no envelope follows the `false`.
func TestAFailingEnvStopsTheEvalChain(t *testing.T) {
	isolate(t)
	r := invoke(t, "", "agent", "env", "--sh")
	if r.code == 0 || r.stdout != "false\n" || !strings.HasPrefix(r.stderr, "rex-amq-adapter: ") {
		t.Errorf("agent env --sh outside Rex: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}
