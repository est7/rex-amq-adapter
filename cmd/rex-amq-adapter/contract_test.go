package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/out"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// specExits is §3.4's table, copied from the spec by hand: the oracle the
// published map is held to, never derived from fault's own table.
var specExits = map[string]int{
	"internal": 1, "rex_unavailable": 1, "amq_unavailable": 1, "timeout": 1, "resource_locked": 1,
	"usage": 2, "invalid_input": 2, "confirmation_required": 2,
	"permission_denied": 3, "untrusted_folder": 3, "unapproved_hooks": 3, "no_identity": 3,
	"not_found": 4,
	"conflict":  5, "busy": 5, "cap_reached": 5, "failed_precondition": 5,
	"guard_denied":  6,
	"partial_start": 7, "not_adopted": 7, "seat_failed": 7,
	"interrupted": 130, "terminated": 143,
}

// The exit and error map is closed and published: `schema errors` lists
// exactly §3.4's codes with their exit statuses, every app error is mapped
// into it, and real invocations exit with the status their code names.
func TestExitCodeMap(t *testing.T) {
	isolate(t)
	stubTools(t)
	code, env := machine(t, "schema", "errors")
	if code != 0 {
		t.Fatalf("schema errors: exit %d", code)
	}
	var published struct{ Errors []fault.Info }
	if err := json.Unmarshal(env.Data, &published); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, i := range published.Errors {
		got[string(i.Code)] = i.Exit
		if i.Description == "" {
			t.Errorf("%s has no description", i.Code)
		}
	}
	if !mapsEqual(got, specExits) {
		t.Errorf("published map\n %v\nwant §3.4's\n %v", got, specExits)
	}
	for c, exit := range specExits {
		if fault.ExitCode(fault.Code(c)) != exit || !fault.Known(fault.Code(c)) {
			t.Errorf("%s exits %d, want %d", c, fault.ExitCode(fault.Code(c)), exit)
		}
	}
	if fault.Known("refused") || fault.ExitCode("refused") != 1 {
		t.Error("a code outside the set is known, or does not fall back to internal")
	}
	s := "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"
	// One real invocation per class reachable without Rex or amq.
	for _, c := range []want{
		{[]string{"version"}, 0, ""},
		{[]string{"agent", "list", "--help"}, 0, ""},
		{[]string{"hub", "list"}, 0, ""},
		{[]string{"nonesuch"}, 2, "usage"},
		{[]string{"agent", "retire", "../x"}, 2, "invalid_input"},
		{[]string{"mail", "dlq", "purge"}, 2, "confirmation_required"},
		{[]string{"skill", "show", "nonesuch"}, 4, "not_found"},
		{[]string{"agent", "retire", "codex-01"}, 5, "failed_precondition"},
	} {
		if c.args[len(c.args)-1] == "--help" {
			if r := invoke(t, "", c.args...); r.code != 0 {
				t.Errorf("%q: exit %d", c.args, r.code)
			}
			continue
		}
		check(t, []want{c})
	}
	t.Setenv("REX_AMQ_REX", filepath.Join(t.TempDir(), "no-rex"))
	check(t, []want{{[]string{"agent", "list", "--session", s}, 1, "rex_unavailable"}})
	t.Setenv("ME_UNATTENDED", "1")
	check(t, []want{{[]string{"trust", "grant", t.TempDir()}, 6, "guard_denied"}})
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// hostile are values every id refuses: traversal, smuggled query and
// fragment, double encoding, a control character, a bidi override, a
// zero-width space, and whitespace.
var hostile = []string{"../../etc/passwd", "res_123?fields=secret", "res_123#frag", "res_123%2f..", "res_123\x07", "\u202eevil", "zero\u200bwidth", "two words"}

// hostileText are values no single line of text takes: control and format
// characters, which op ids and subjects refuse while keeping punctuation.
var hostileText = []string{"res_123\x07", "\u202eevil", "zero\u200bwidth", "a\nb", "a\x1b[31mb", "a\u2066b", "a\ufeffb"}

// sample is a valid value of each kind, so an invocation fails only on
// the one value a case makes hostile.
func sample(t *testing.T, v cli.Value) string {
	switch v.Kind {
	case cli.KindHandle, cli.KindTarget, cli.KindHandles:
		return "claude-02"
	case cli.KindRole:
		return "reviewer"
	case cli.KindCrew:
		return "pair"
	case cli.KindTopic:
		return "guide"
	case cli.KindSessionID:
		return "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"
	case cli.KindBlockID:
		return "block:01a1eac4-f234-7985-8bef-2ea92c753265"
	case cli.KindMessageID, cli.KindEventID, cli.KindMessageIDs:
		return "m-1"
	case cli.KindThreadID:
		return "p2p/a__b"
	case cli.KindOpID, cli.KindSingleLine, cli.KindBody:
		return "x"
	case cli.KindDir, cli.KindOutDir, cli.KindPath:
		return t.TempDir()
	case cli.KindFile, cli.KindFileOrDash:
		return sampleFile(t)
	case cli.KindArtifact:
		return "a.md=@" + sampleFile(t)
	case cli.KindRef:
		return "inbox:1"
	case cli.KindDuration:
		return "5s"
	case cli.KindInt:
		return "1"
	case cli.KindEnum:
		return v.Enum[0]
	case cli.KindFields:
		return "handle"
	case cli.KindCommand:
		return "errors"
	}
	t.Fatalf("no sample for kind %s", v.Kind)
	return ""
}

func sampleFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// idKinds are the kinds whose values name things (ids, names, refs): each
// refuses every hostile value.
var idKinds = map[cli.Kind]bool{cli.KindHandle: true, cli.KindTarget: true, cli.KindHandles: true, cli.KindRole: true, cli.KindCrew: true,
	cli.KindTopic: true, cli.KindSessionID: true, cli.KindBlockID: true, cli.KindMessageID: true, cli.KindEventID: true,
	cli.KindMessageIDs: true, cli.KindThreadID: true, cli.KindRef: true, cli.KindCursor: true}

// textKinds refuse the control and format characters of hostileText.
var textKinds = map[cli.Kind]bool{cli.KindOpID: true, cli.KindSingleLine: true}

// invocation is the words of cmd with every required value valid, and
// value at the given arg or flag.
func invocation(t *testing.T, cmd *cli.Command, argName, flagName, value string) []string {
	words := slices.Clone(cmd.Path)
	for _, a := range cmd.Args {
		switch {
		case a.Name == argName:
			words = append(words, value)
		case a.Required:
			words = append(words, sample(t, a.Value))
		}
	}
	for _, f := range cmd.Flags {
		switch {
		case f.Name == flagName:
			words = append(words, "--"+f.Name+"="+value)
		case f.Required:
			words = append(words, "--"+f.Name+"="+sample(t, f.Value))
		}
	}
	return words
}

// Every value is checked against its kind in the registry before anything
// else: a hostile id, name, or line of text is exit 2 with no caller
// lookup, no Rex or amq call, no @file read, and no directory created, so
// the whole set runs outside Rex with the state directory absent.
func TestPreIOValidation(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	state := filepath.Join(t.TempDir(), "state-never-made")
	t.Setenv("REX_AMQ_ADAPTER_HOME", state)
	// An agent shell would be refused by the human guard instead: run as
	// the human so validation is what answers.
	cases := 0
	for _, cmd := range registry().Commands {
		type slot struct {
			arg, flag string
			kind      cli.Kind
		}
		var slots []slot
		for _, a := range cmd.Args {
			slots = append(slots, slot{arg: a.Name, kind: a.Kind})
		}
		for _, f := range cmd.Flags {
			slots = append(slots, slot{flag: f.Name, kind: f.Kind})
		}
		for _, s := range slots {
			var values []string
			switch {
			case idKinds[s.kind]:
				values = append(slices.Clone(hostile), hostileText...)
			case textKinds[s.kind]:
				values = hostileText
			case s.kind == cli.KindBody:
				values = []string{"a\x1b[31mb", "a\u202eb", "a\x07b"}
			case s.kind == cli.KindDir || s.kind == cli.KindFile:
				values = []string{"/nonexistent/x", "a\x07b"}
			default:
				continue
			}
			for _, v := range values {
				args := invocation(t, cmd, s.arg, s.flag, v)
				code, env := machine(t, args...)
				cases++
				if code != 2 || (env.code() != "invalid_input" && env.code() != "usage") {
					t.Errorf("%q: exit %d %q, want 2 invalid_input", args, code, env.code())
				}
			}
		}
	}
	if cases < 300 {
		t.Fatalf("only %d cases: the registry walk is not reaching the commands", cases)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a rejected invocation ran rex or amq")
	}
	if _, err := os.Stat(state); err == nil {
		t.Errorf("a rejected invocation created the state directory %s", state)
	}
	// -y and --yes are unknown flags everywhere: they never authorize.
	for _, cmd := range registry().Commands {
		for _, y := range []string{"-y", "--yes"} {
			args := append(invocation(t, cmd, "", "", ""), y)
			if code, env := machine(t, args...); code != 2 || env.code() != "usage" {
				t.Errorf("%q: exit %d %q, want 2 usage", args, code, env.code())
			}
		}
	}
}

// The human guard refuses (exit 6, guard_denied) every human-only command
// from an agent's harness session, an unattended run, a pane holding a live
// agent, or a process descending from one, before it asks Rex anything and
// whether or not it is a dry run. ME_UNATTENDED=1 does not stop the Epic's
// own coordination (mail send, agent summon).
func TestHumanGate(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	var gated []*cli.Command
	for _, cmd := range registry().Commands {
		if cmd.Guard == cli.GuardHuman {
			gated = append(gated, cmd)
		}
	}
	names := func() []string {
		var n []string
		for _, c := range gated {
			n = append(n, c.Name())
		}
		return n
	}()
	for _, want := range []string{"trust grant", "trust root add", "trust root remove", "hub add", "hub remove", "install setup", "install remove", "stream panel"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s is not guarded (guarded: %v)", want, names)
		}
	}
	try := func(label string, wantReason string) {
		t.Helper()
		for _, cmd := range gated {
			for _, dry := range []bool{false, true} {
				if dry && !cmd.DryRun {
					continue
				}
				args := invocation(t, cmd, "", "", "")
				if dry {
					args = append(args, "--dry-run")
				}
				code, env := machine(t, args...)
				reason, _ := map[string]any{}, ""
				if env.Error != nil {
					reason["r"] = env.Error.Details["reason"]
				}
				if code != 6 || env.code() != "guard_denied" || reason["r"] != wantReason {
					t.Errorf("%s: %q: exit %d %q reason %v, want 6 guard_denied %s", label, args, code, env.code(), reason["r"], wantReason)
				}
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: a refused command asked Rex or amq", label)
		}
	}
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
		t.Setenv(v, "an-agent-session")
		try(v, "human_only")
		t.Setenv(v, "")
	}
	t.Setenv("ME_UNATTENDED", "1")
	try("ME_UNATTENDED=1", "unattended")
	// Epic coordination runs unattended: these reach their caller lookup
	// (no Rex pane here) instead of the guard.
	check(t, []want{
		{[]string{"mail", "send", "--to", "claude-02", "--subject", "s", "--body", "b"}, 5, "failed_precondition"},
		{[]string{"agent", "summon", "reviewer", "--subject", "s", "--body", "b", "--asked", "review it"}, 5, "failed_precondition"},
	})
	t.Setenv("ME_UNATTENDED", "")
	if _, err := os.Stat(marker); err == nil {
		t.Error("a refused command asked Rex or amq")
	}
	// A pane that holds a live agent, and a process descending from one,
	// are agents however the environment was trimmed.
	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	s := store.Store{P: store.Paths{Home: home}}
	if err := putAgent(s, testAgent{Handle: "claude-07", Kind: core.Claude, Role: core.PersonalRole, Server: "unix:///t.sock", BlockID: "block:pane",
		SessionID: "session:1", WindowID: "window:1", AgentPID: 999999}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_BLOCK", "block:pane")
	try("a live agent's pane", "human_only")
	t.Setenv("REX_BLOCK", "")
	if err := putAgent(s, testAgent{Handle: "codex-08", Kind: core.Codex, Role: "implementer", Server: "unix:///t.sock", BlockID: "block:above",
		SessionID: "session:1", WindowID: "window:1", AgentPID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	try("under a live agent", "human_only")
}

// The Operator guard admits only an Epic's Operator (the user, a personal
// agent, a top-level attended standalone agent) to the Epic lifecycle and
// cockpit commands, inside Rex and out alike: everyone else is refused with
// exit 6 guard_denied before any Rex or amq call, dry run or not. An
// unadopted harness session outside Rex (a Desktop app's agent) is the
// user's; an agent of an Epic is refused from its own pane even after the
// Epic's narrator crashed, since its session's records still say Epic.
func TestOperatorGuard(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	var gated []*cli.Command
	for _, cmd := range registry().Commands {
		if cmd.Guard == cli.GuardOperator || cmd.Guard == cli.GuardOperatorOrSelf {
			gated = append(gated, cmd)
		}
	}
	var names []string
	for _, c := range gated {
		names = append(names, c.Name())
	}
	slices.Sort(names)
	if want := []string{"epic artifacts", "epic close", "epic grant", "epic list", "epic materialize", "epic open", "epic revive", "epic show", "hub inbox"}; !slices.Equal(names, want) {
		t.Fatalf("Operator commands %v, want %v", names, want)
	}
	run := func(cmd *cli.Command, dry bool) (int, envelope) {
		t.Helper()
		args := invocation(t, cmd, "", "", "")
		if dry {
			args = append(args, "--dry-run")
		}
		return machine(t, args...)
	}
	refused := func(label, wantReason string, spare func(*cli.Command) bool) {
		t.Helper()
		for _, cmd := range gated {
			if spare != nil && spare(cmd) {
				continue
			}
			for _, dry := range []bool{false, true} {
				if dry && !cmd.DryRun {
					continue
				}
				code, env := run(cmd, dry)
				var reason any
				if env.Error != nil {
					reason = env.Error.Details["reason"]
				}
				if code != 6 || env.code() != "guard_denied" || reason != wantReason {
					t.Errorf("%s: %s (dry %v): exit %d %q reason %v, want 6 guard_denied %s", label, cmd.Name(), dry, code, env.code(), reason, wantReason)
				}
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: a refused command asked Rex or amq", label)
		}
	}
	admitted := func(label string, only func(*cli.Command) bool) {
		t.Helper()
		for _, cmd := range gated {
			if only != nil && !only(cmd) {
				continue
			}
			if code, env := run(cmd, false); code == 6 || env.code() == "guard_denied" {
				t.Errorf("%s: %s refused by the guard: %+v", label, cmd.Name(), env.Error)
			}
		}
		os.Remove(marker)
	}
	for _, kv := range [][2]string{{"ME_UNATTENDED", "1"}, {"AGY_UNATTENDED", "1"}, {"OWL_DISPATCH_RUN", "run.1"}, {"OWL_DISPATCH_RUN_ID", "r-1"}} {
		t.Setenv(kv[0], kv[1])
		refused(kv[0], "unattended", nil)
		t.Setenv(kv[0], "")
	}
	// A Desktop app's agent outside Rex: a harness session no hook adopted.
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
		t.Setenv(v, "a-desktop-session")
		admitted(v+" outside Rex", nil)
		t.Setenv(v, "")
	}
	admitted("the user's shell", nil)

	home := os.Getenv("REX_AMQ_ADAPTER_HOME")
	s := store.Store{P: store.Paths{Home: home}}
	epic := "session:epic"
	if err := s.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyEpic, SessionID: epic, Server: "unix:///t.sock", LeadRole: core.OrchestratorRole, LeadHandle: "claude-02"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEpicLifecycle(store.EpicLifecycle{SessionID: epic, Server: "unix:///t.sock", State: store.EpicOpened}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []testAgent{
		{Handle: "claude-02", Kind: core.Claude, Role: core.OrchestratorRole, BlockID: "block:lead"},
		{Handle: "claude-03", Kind: core.Claude, Role: core.NarratorRole, BlockID: "block:narrator"},
		{Handle: "codex-04", Kind: core.Codex, Role: "implementer", BlockID: "block:worker"},
		{Handle: "codex-05", Kind: core.Codex, BlockID: "block:norole"},
		{Handle: "claude-06", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:personal", SessionID: "session:hub", AgentSessionID: "s-personal"},
	} {
		a.Server, a.WindowID, a.AgentPID = "unix:///t.sock", "window:1", 999999
		if a.SessionID == "" {
			a.SessionID = epic
		}
		if err := putAgent(s, a); err != nil {
			t.Fatal(err)
		}
	}
	self := func(c *cli.Command) bool { return c.Guard == cli.GuardOperatorOrSelf }
	t.Setenv("REX_BLOCK", "block:worker")
	refused("an Epic worker's pane", "operator_only", nil)
	// The narrator of this Epic crashed: its record is gone from the live
	// ones, and the session's records still make its no-role agent an Epic's.
	if _, err := s.ReleaseLease("claude-03", 1, store.Departure{Reason: store.DepartSessionEnd, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_BLOCK", "block:norole")
	refused("a no-role agent of an Epic whose narrator crashed", "operator_only", nil)
	t.Setenv("REX_BLOCK", "block:worker")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "nested-claude-p")
	refused("an unadopted session nested in a worker's pane", "operator_only", nil)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("REX_BLOCK", "block:lead")
	refused("the Epic's lead, outside its own session's reads", "operator_only", self)
	admitted("the Epic's lead reading its own session", self)
	t.Setenv("REX_BLOCK", "block:personal")
	admitted("a personal agent's pane", nil)
	t.Setenv("REX_BLOCK", "")
	// A harness session no hook adopted, nested under the personal agent
	// whose own session the environment also names.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-personal")
	t.Setenv("CODEX_THREAD_ID", "nested-codex")
	refused("an unadopted session nested under the personal agent", "operator_only", nil)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	if err := putAgent(s, testAgent{Handle: "codex-08", Kind: core.Codex, Role: "implementer", Server: "unix:///t.sock", BlockID: "block:above",
		SessionID: epic, WindowID: "window:1", AgentPID: os.Getppid()}); err != nil {
		t.Fatal(err)
	}
	refused("under a live agent", "operator_only", nil)
}

// inject is amq wake's injector: exactly three positional arguments after
// the verb (the third amq's payload), its progress on stderr as one
// AMQ_INJECT_PROGRESS line, nothing on stdout, and exit 0 or 1; the
// waker the adapter registers passes exactly that argv. There is no
// five-argument form.
func TestInjectArgvUnchanged(t *testing.T) {
	isolate(t)
	stubTools(t)
	w := amq.Waker{InjectVia: "/state/bin/inject", Handle: "codex-01", Root: "/state/amq-root"}
	if tgt := w.Target(); tgt.InjectVia != "/state/bin/inject" || !slices.Equal(tgt.InjectArgs, []string{"inject", "codex-01", "/state/amq-root"}) || tgt.RetryUntil != "injected" {
		t.Fatalf("waker target %+v", tgt)
	}
	progress := regexp.MustCompile(`^AMQ_INJECT_PROGRESS=[a-z_]+\n`)
	root := filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "amq-root")
	for _, c := range []struct {
		args []string
		want string // the progress line
	}{
		{[]string{"inject"}, "AMQ_INJECT_PROGRESS=failed\n"},
		{[]string{"inject", "codex-01", root}, "AMQ_INJECT_PROGRESS=failed\n"},
		{[]string{"inject", "codex-01", root, "payload", "block:x", "unix:///s"}, "AMQ_INJECT_PROGRESS=failed\n"},
		{[]string{"inject", "Bad Handle", root, "payload"}, "AMQ_INJECT_PROGRESS=failed\n"},
		{[]string{"inject", "codex-01", root, "payload"}, ""},
	} {
		r := invoke(t, "", c.args...)
		if r.stdout != "" || (r.code != 0 && r.code != 1) || !progress.MatchString(r.stderr) || c.want != "" && !strings.HasPrefix(r.stderr, c.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", c.args, r.code, r.stdout, r.stderr)
		}
	}
	// The wrong argc answers with v1's exact bytes.
	if r := invoke(t, "", "inject", "a", "b"); r.code != 1 || r.stdout != "" || r.stderr != "AMQ_INJECT_PROGRESS=failed\nrex-amq-adapter: inject: want <handle> <root> <payload>\n" {
		t.Errorf("wrong argc: %d %q", r.code, r.stderr)
	}
}

// Every legacy top-level verb is gone: each is a usage error naming no
// alias, and the root help lists nouns only.
func TestLegacyVerbsAreUsage(t *testing.T) {
	isolate(t)
	stubTools(t)
	for _, v := range []string{"who", "env", "title", "join", "crews", "summon", "await", "retire", "collect", "send", "reply", "read", "drain",
		"thread", "trace", "dlq", "list", "link", "unlink", "view", "hand-back", "notify-personal", "events", "panel", "setup", "status", "remove"} {
		check(t, []want{{[]string{v}, 2, "usage"}})
	}
	r := invoke(t, "", "--help", "--text")
	if r.code != 0 {
		t.Fatalf("--help: exit %d", r.code)
	}
	for _, noun := range []string{"agent", "mail", "epic", "hub", "crew", "trust", "stream", "install", "skill", "schema", "version"} {
		if !strings.Contains(r.stdout, "\n  "+noun+" ") {
			t.Errorf("root help lacks %s:\n%s", noun, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "\n  who ") || strings.Contains(r.stdout, "\n  hook ") || strings.Contains(r.stdout, "\n  inject ") || strings.Contains(r.stdout, "__internal") {
		t.Errorf("root help lists a legacy or internal command:\n%s", r.stdout)
	}
	code, env := machine(t, "schema")
	var sch struct{ Commands []struct{ Name string } }
	if err := json.Unmarshal(env.Data, &sch); code != 0 || err != nil {
		t.Fatal(err)
	}
	for _, c := range sch.Commands {
		if strings.HasPrefix(c.Name, "hook") || strings.HasPrefix(c.Name, "inject") || strings.HasPrefix(c.Name, "__internal") {
			t.Errorf("schema lists the internal callback %s", c.Name)
		}
	}
}

// A panic in a command answers as a failure in the caller's mode: in
// machine mode one internal envelope (exit 1) and nothing on stderr, with a
// redacted crash dump (0600) under <state>/logs.
func TestPanicAnswersInternal(t *testing.T) {
	isolate(t)
	var o, e bytes.Buffer
	c := &cli.Ctx{Getenv: os.Getenv, Out: out.Printer{Streams: out.Streams{Stdout: &o, Stderr: &e}, Mode: out.Machine, Verb: "agent list"}}
	code := protect(c, func() int { panic(errors.New("boom token=s3cret")) })
	if code != 1 || e.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, e.String())
	}
	env := decode(t, nil, result{code: code, stdout: o.String()})
	if env.code() != "internal" || env.Meta.Verb != "agent list" {
		t.Fatalf("envelope %+v", env)
	}
	dump, _ := env.Error.Details["crash_dump"].(string)
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("crash dump %q: %v", dump, err)
	}
	if st, _ := os.Stat(dump); st.Mode().Perm() != 0o600 {
		t.Errorf("crash dump mode %v", st.Mode().Perm())
	}
	if strings.Contains(string(b), "s3cret") || !strings.Contains(string(b), "token=<redacted>") {
		t.Errorf("crash dump is not redacted:\n%s", b)
	}
}

// Machine mode is one envelope line on stdout and nothing on stderr for
// usage errors too; --text puts the data on stdout and the diagnostics on
// stderr; meta carries the trace id from the environment.
func TestOutputModes(t *testing.T) {
	isolate(t)
	t.Setenv("AGENT_TRACE_ID", "trace-a")
	_, env := machine(t, "nonesuch", "--json")
	if env.Meta.TraceID != "trace-a" {
		t.Errorf("trace id %q", env.Meta.TraceID)
	}
	t.Setenv("REX_AMQ_ADAPTER_TRACE_ID", "trace-b")
	if _, env := machine(t, "version"); env.Meta.TraceID != "trace-b" {
		t.Errorf("REX_AMQ_ADAPTER_TRACE_ID does not win: %q", env.Meta.TraceID)
	}
	r := invoke(t, "", "nonesuch", "--text")
	if r.code != 2 || r.stdout != "" || !strings.HasPrefix(r.stderr, "rex-amq-adapter: unknown command") || strings.Contains(r.stderr, "\x1b") {
		t.Errorf("--text failure: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := invoke(t, "", "version", "--json", "--text"); r.code != 2 {
		t.Errorf("--json --text: exit %d", r.code)
	}
	r = invoke(t, "", "skill", "show", "peers", "--text")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "#") {
		t.Errorf("skill show --text is not raw markdown: %q", r.stdout[:min(80, len(r.stdout))])
	}
}

// hub inbox reads a Hub's inbox from the records alone: with Rex and amq
// both failing (and marking any call), the user outside Rex reads it,
// pages it, and narrows it, and nothing asks Rex or amq.
func TestHubInboxReadsWithRexDown(t *testing.T) {
	home := isolate(t)
	marker := filepath.Join(t.TempDir(), "touched")
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REX_AMQ_REX", stub)
	t.Setenv("REX_AMQ_AMQ", stub)
	hub, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := store.Store{P: store.Paths{Home: filepath.Join(home, "state")}}
	key := core.NewHubKey("unix:///t.sock", hub)
	if err := s.PutLink(core.Link{Hub: key, Epic: core.SessionKey{Server: "unix:///t.sock", SessionID: "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3"}}); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{store.InboxHandBack, store.InboxNarrator} {
		if _, _, err := s.AppendHubInbox(context.Background(), hub, store.HubInboxEntry{Epic: "session:01a1eac4-f222-7d79-9291-9a8a86d39ec3", HubKey: key, Kind: kind, By: "agy-01", DedupeKey: fmt.Sprint("k", i)}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	code, env := machine(t, "hub", "inbox", "--hub", hub)
	var page struct {
		Hub   string `json:"hub"`
		Items []struct {
			Seq  int64  `json:"seq"`
			Kind string `json:"kind"`
		} `json:"items"`
		NextSince int64 `json:"next_since"`
	}
	if code != 0 || json.Unmarshal(env.Data, &page) != nil || page.Hub != hub || len(page.Items) != 2 || page.NextSince != 2 {
		t.Fatalf("hub inbox: exit %d %s %+v", code, env.Data, env.Error)
	}
	if code, env := machine(t, "hub", "inbox", "--hub", hub, "--since", "1", "--kind", "narrator"); code != 0 || !strings.Contains(string(env.Data), `"seq":2`) || strings.Contains(string(env.Data), `"seq":1,`) {
		t.Errorf("--since 1 --kind narrator: exit %d %s", code, env.Data)
	}
	check(t, []want{
		{[]string{"hub", "inbox", "--hub", hub, "--epic", "session:01a1eac4-f222-7d79-9291-9a8a86d39ec4"}, 3, "permission_denied"},
		{[]string{"hub", "inbox", "--hub", hub, "--kind", "gossip"}, 2, "invalid_input"},
	})
	if _, err := os.Stat(marker); err == nil {
		t.Error("hub inbox asked Rex or amq")
	}
}

// mail send declares --op-id (a narrator's mail to personal): checked as
// an op id before anything runs, and carried to the caller's lookup.
func TestMailSendTakesAnOpID(t *testing.T) {
	isolate(t)
	stubTools(t)
	check(t, []want{
		{[]string{"mail", "send", "--to", "personal", "--subject", "s", "--body", "b", "--op-id", "a\tb"}, 2, "invalid_input"},
		{[]string{"mail", "send", "--to", "personal", "--subject", "s", "--body", "b", "--op-id", "hb-1"}, 5, "failed_precondition"},
	})
	if spec := registry().Find("mail", "send"); spec == nil || !slices.ContainsFunc(spec.Flags, func(f cli.Flag) bool { return f.Name == "op-id" && f.Kind == cli.KindOpID }) {
		t.Errorf("mail send does not declare --op-id")
	}
}
