package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// viewWorld is Epic session:a (an orchestrator, an implementer, a reviewer,
// and its narrator), a personal agent linked to it in session:p, and an
// unlinked one in session:q, on a real amq.
func viewWorld(t *testing.T) Env {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{sessionLabel: "auth-refresh"})
	put(t, e,
		seed{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:a"},
		seed{Handle: "claude-02", Kind: core.Claude, Role: "implementer", SessionID: "session:a", Task: "token refresh"},
		seed{Handle: "codex-03", Kind: core.Codex, Role: "reviewer", SessionID: "session:a"},
		seed{Handle: "pi-04", Kind: core.Pi, Role: core.NarratorRole, SessionID: "session:a"},
		seed{Handle: "pi-05", Kind: core.Pi, Role: core.PersonalRole, SessionID: "session:p"},
		seed{Handle: "pi-06", Kind: core.Pi, Role: core.PersonalRole, SessionID: "session:q"},
	)
	for _, h := range []string{"claude-01", "claude-02", "codex-03", "pi-04", "pi-05", "pi-06"} {
		if err := e.AMQ.EnsureMailbox(context.Background(), e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:a"}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func sendID(t *testing.T, e Env, as core.Handle, args ...string) string {
	t.Helper()
	out, _, err := run(t, e, context.Background(), as, "send", append(args, "--json")...)
	if err != nil {
		t.Fatalf("%s send: %v %s", as, err, out)
	}
	var s amq.Sent
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.ID == "" {
		t.Fatalf("send output %q", out)
	}
	return s.ID
}

// mailTree lists every mailbox file under the AMQ root with its size: what a
// read that moved, drained, or acknowledged mail would change.
func mailTree(t *testing.T, e Env) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(filepath.Join(e.Paths.Root(), "agents"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".wake") {
			info, _ := d.Info()
			fmt.Fprintf(&b, "%s %d\n", strings.TrimPrefix(p, e.Paths.Root()), info.Size())
		}
		return nil
	})
	return b.String()
}

func gapRefs(gs []CoverageGap, kind string) []string {
	for _, g := range gs {
		if g.Kind == kind {
			return g.Refs
		}
	}
	return nil
}

func refs(ms []ViewMessage) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Ref)
	}
	return out
}

// The narrator's refresh reads envelopes and open obligations (tier 1) and
// the orchestrator's control-plane bodies by id (tier 2) into view.json
// under the extension area, moving no mail and writing no receipt. Peer mail
// that bypassed the orchestrator and mail without the adapter's provenance
// are coverage gaps, never read.
func TestSessionViewTwoTierRefresh(t *testing.T) {
	e := viewWorld(t)
	needsAmq(t, e, "amq's open-obligation scan")
	ctx := context.Background()
	report := sendID(t, e, "claude-02", "--to", "claude-01", "--kind", "status", "--subject", "done", "--body", "CONTROL: tests pass on token refresh")
	review := sendID(t, e, "claude-01", "--to", "codex-03", "--kind", "review_request", "--subject", "review token refresh", "--body", "CONTROL: please review")
	peer := sendID(t, e, "claude-02", "--to", "codex-03", "--subject", "handoff", "--body", "PEER: the diff is on the branch")
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-03", "claude-02", "raw", "RAW: sent without the adapter"); err != nil {
		t.Fatal(err)
	}
	raw := unread(t, e, "claude-02")[0]
	before := mailTree(t, e)

	if _, err := e.RefreshSessionView(ctx, Location{Block: "block:claude-02"}, ""); !errors.Is(err, ErrRefused) {
		t.Fatalf("a worker refreshed the view: %v", err)
	}
	if _, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, "implementer done; review pending on codex-03"); err != nil {
		t.Fatal(err)
	}
	if after := mailTree(t, e); after != before {
		t.Fatalf("observing moved or wrote mail:\nbefore\n%s\nafter\n%s", before, after)
	}
	for _, h := range []string{"claude-01", "claude-02", "codex-03"} {
		noReceipts(t, e, h)
	}
	path := e.Paths.ViewPath("session:a")
	if !strings.HasPrefix(path, filepath.Join(e.Paths.Root(), "extensions", "rex-amq-adapter", "sessions")+string(filepath.Separator)) || filepath.Base(path) != "view.json" {
		t.Fatalf("view path %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v SessionView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Schema != ViewSchema || v.Session.Epic != "auth-refresh" || v.Narrator != "pi-04" || v.AsOf.Pulse == "" || v.AsOf.LastMessage == "" || v.AsOf.At.IsZero() {
		t.Fatalf("header: %+v %+v", v.Session, v.AsOf)
	}
	if v.Narrative == nil || v.Narrative.Text != "implementer done; review pending on codex-03" {
		t.Fatalf("narrative %+v", v.Narrative)
	}
	var members []core.Handle
	for _, m := range v.Members {
		members = append(members, m.Handle)
	}
	if !slices.Equal(members, []core.Handle{"claude-01", "claude-02", "codex-03"}) {
		t.Fatalf("members %v", members)
	}
	var owed bool
	for _, o := range v.Obligations {
		owed = owed || o.Ref == review && o.OwedBy == "codex-03" && o.OwedTo == "claude-01" && o.Provenance == "adapter"
	}
	if !owed {
		t.Fatalf("the open review is missing: %+v", v.Obligations)
	}
	if got := refs(v.ControlPlane); !slices.Equal(got, []string{review, report}) {
		t.Fatalf("control-plane bodies %v, want %v", got, []string{review, report})
	}
	for _, m := range v.ControlPlane {
		if !strings.HasPrefix(m.Body, "CONTROL:") {
			t.Fatalf("control-plane body %q", m.Body)
		}
	}
	if strings.Contains(string(b), "PEER: the diff") || strings.Contains(string(b), "RAW:") {
		t.Fatalf("a body outside the control plane reached the view")
	}
	if !slices.Contains(gapRefs(v.CoverageGaps, GapPeerBody), peer) || !slices.Contains(gapRefs(v.CoverageGaps, GapUnverified), raw) {
		t.Fatalf("coverage gaps %+v", v.CoverageGaps)
	}
	if len(refs(v.Activity)) != 4 || v.Limits.RecentMessages != viewMessages || v.Limits.ExcerptChars != viewExcerpt {
		t.Fatalf("activity %v limits %+v", refs(v.Activity), v.Limits)
	}

	// A refresh without a summary keeps the narrator's last one.
	if v2, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, ""); err != nil || v2.Narrative == nil || v2.Narrative.Text != v.Narrative.Text {
		t.Fatalf("narrative not kept: %+v %v", v2.Narrative, err)
	}
}

// The view is the narrator's, its orchestrator's, and a linked personal
// agent's to read; a worker and an unlinked personal agent are refused.
func TestSessionViewReadAccess(t *testing.T) {
	e := viewWorld(t)
	noOpenObligations(e) // access is the point here, not obligations
	ctx := context.Background()
	if _, err := e.ReadSessionView(ctx, Location{Block: "block:pi-05"}, ""); !errors.Is(err, ErrNoView) {
		t.Fatalf("before any refresh: %v", err)
	}
	if _, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, "quiet"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who     core.Handle
		session string
		ok      bool
	}{
		{"pi-04", "", true},
		{"claude-01", "", true},
		{"pi-05", "", true},
		{"pi-05", "session:a", true},
		{"pi-05", "session:q", false},
		{"pi-06", "session:a", false},
		{"pi-06", "", false},
		{"claude-02", "", false},
		{"pi-04", "session:q", false},
	} {
		b, err := e.ReadSessionView(ctx, Location{Block: "block:" + string(c.who)}, c.session)
		if c.ok != (err == nil) {
			t.Errorf("%s reading %q: %v", c.who, c.session, err)
		}
		if c.ok && !strings.Contains(string(b), `"session_id":"session:a"`) {
			t.Errorf("%s read %s", c.who, b)
		}
	}
	if _, err := e.Store.DeleteLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadSessionView(ctx, Location{Block: "block:pi-05"}, "session:a"); !errors.Is(err, ErrRefused) {
		t.Fatalf("an unlinked personal agent read the view: %v", err)
	}
}

// The narrator reads the body of a message of its session by id, peer mail
// included, without a receipt; mail without the adapter's provenance, from
// another session, or that names a path outside a mailbox is a gap, unread.
func TestSessionViewNarratorReadsByID(t *testing.T) {
	e := viewWorld(t)
	ctx := context.Background()
	peer := sendID(t, e, "claude-02", "--to", "codex-03", "--subject", "handoff", "--body", "PEER: the diff is on the branch")
	if err := amq.Send(ctx, e.AMQ, e.Paths.Root(), "codex-03", "claude-02", "raw", "RAW: sent without the adapter"); err != nil {
		t.Fatal(err)
	}
	raw := unread(t, e, "claude-02")[0]
	before := mailTree(t, e)
	if _, err := e.ReadSessionMessages(ctx, Location{Block: "block:claude-01"}, []string{peer}); !errors.Is(err, ErrRefused) {
		t.Fatalf("the orchestrator read by id as a narrator: %v", err)
	}
	r, err := e.ReadSessionMessages(ctx, Location{Block: "block:pi-04"}, []string{peer, raw, "../../config", "2026-01-01T00-00-00.000Z_pid1_00000000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 || r.Messages[0].Ref != peer || !strings.HasPrefix(r.Messages[0].Body, "PEER:") {
		t.Fatalf("messages %+v", r.Messages)
	}
	if !slices.Contains(gapRefs(r.CoverageGaps, GapUnverified), raw) || len(gapRefs(r.CoverageGaps, GapNotFound)) != 2 {
		t.Fatalf("gaps %+v", r.CoverageGaps)
	}
	if after := mailTree(t, e); after != before {
		t.Fatalf("reading by id moved or wrote mail")
	}
	for _, h := range []string{"claude-02", "codex-03"} {
		noReceipts(t, e, h)
	}
}

// The view carries the orchestrator's recorded hand-back whole, past the
// excerpt limit, for the narrator, the orchestrator, and a linked personal
// agent to read; a refresh after a new one shows the new one.
func TestHandBackInTheViewWhole(t *testing.T) {
	e := viewWorld(t)
	noOpenObligations(e)
	ctx := context.Background()
	body := "## Review queue\n- https://github.com/est7/rex-amq-adapter/pull/18\n## Blocked on me\n- merge #18\n" + strings.Repeat("Found: 一行说明。", viewExcerpt)
	if _, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, "before"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:claude-01"}, body); err != nil {
		t.Fatal(err)
	}
	written, _, _ := e.Store.ReadView("session:a")
	// Read at once, before the narrator refreshes, then after.
	for i, who := range []string{"pi-05", "pi-04", "claude-01", "pi-05"} {
		if i == 3 {
			if _, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, ""); err != nil {
				t.Fatal(err)
			}
		} else if after, _, _ := e.Store.ReadView("session:a"); string(after) != string(written) {
			t.Fatal("reading the view wrote it")
		}
		b, err := e.ReadSessionView(ctx, Location{Block: "block:" + who}, "")
		if err != nil {
			t.Fatal(err)
		}
		var v SessionView
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if v.HandBack == nil || v.HandBack.By != "claude-01" || v.HandBack.Body != body || v.HandBack.At.IsZero() {
			t.Fatalf("%s read the hand-back %+v", who, v.HandBack)
		}
	}
	e.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := e.RecordHandBack(ctx, Location{Block: "block:claude-01"}, "Goal 2"); err != nil {
		t.Fatal(err)
	}
	v, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, "")
	if err != nil || v.HandBack == nil || v.HandBack.Body != "Goal 2" {
		t.Fatalf("the next hand-back: %+v %v", v.HandBack, err)
	}
}

// cockpitWorld is viewWorld with the personal agent pi-05 working in a hub
// directory and linked to a second Epic, session:b, whose narrator has not
// written a view yet but whose lifecycle is recorded; pi-06 is linked to
// session:c. The workers of session:a run as agent processes 701 to 704.
func cockpitWorld(t *testing.T) (Env, string) {
	t.Helper()
	e := viewWorld(t)
	noOpenObligations(e)
	hub := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hub, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	for h, pid := range map[core.Handle]int{"claude-01": 701, "claude-02": 702, "codex-03": 703, "pi-04": 704} {
		r := recordOf(t, e, h)
		rSeed := seedOf(r)
		rSeed.AgentPID, rSeed.AgentSessionID = pid, "harness-"+string(h)
		put(t, e, rSeed)
	}
	r := recordOf(t, e, "pi-05")
	rSeed := seedOf(r)
	rSeed.Cwd, rSeed.AgentPID, rSeed.AgentSessionID = filepath.Join(hub, "work"), 705, "harness-pi-05"
	put(t, e, rSeed)
	put(t, e, seed{Handle: "claude-07", Kind: core.Claude, SessionID: "session:a", AgentPID: 707, AgentSessionID: "harness-claude-07"})
	for _, l := range []core.Link{
		{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:b"}},
		{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:q"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:c"}},
	} {
		if err := e.Store.PutLink(l); err != nil {
			t.Fatal(err)
		}
	}
	for _, lc := range []store.EpicLifecycle{
		{SessionID: "session:a", Server: fakeServer, Name: "auth-refresh", State: store.EpicOpened, PersonalHandle: "pi-05"},
		{SessionID: "session:b", Server: fakeServer, Name: "billing", Dir: hub, State: store.EpicPartialStart, PersonalHandle: "pi-05", OpenOpID: "op-b"},
		{SessionID: "session:c", Server: fakeServer, Name: "secret", State: store.EpicOpened, PersonalHandle: "pi-06"},
	} {
		if err := e.Store.PutEpicLifecycle(lc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.RefreshSessionView(context.Background(), Location{Block: "block:pi-04"}, "quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RecordHandBack(context.Background(), Location{Block: "block:claude-01"}, "## Review queue\nPR #3"); err != nil {
		t.Fatal(err)
	}
	// The test process stands for whatever runs the command: by default no
	// agent's.
	e.Ancestors = func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 9000}}, nil }
	return e, hub
}

func cockpitEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// A worker, the orchestrator, the narrator, a roleless agent, a harness
// session no hook adopted, an unattended shell, a child an agent started
// with its environment stripped, and a shell that names no personal agent
// are all refused the cockpit, with and without --session or a scope that
// would otherwise match; nothing is read for them.
func TestCockpitViewRefusesEveryoneButThePersonalScope(t *testing.T) {
	e, hub := cockpitWorld(t)
	ctx := context.Background()
	scoped := []CockpitViewRequest{{}, {Session: "session:a"}, {Session: "session:c"}, {Personal: "pi-05"}, {Personal: "pi-05", Session: "session:a"}, {Hub: hub}}
	for _, c := range []struct {
		name      string
		env       map[string]string
		ancestors []proc
		reqs      []CockpitViewRequest
		want      error
	}{
		{"an implementer's pane", map[string]string{"REX_BLOCK": "block:claude-02"}, nil, scoped, ErrRefused},
		{"a reviewer's tool shell", map[string]string{"CODEX_THREAD_ID": "harness-codex-03"}, nil, scoped, ErrRefused},
		{"a reviewer's tool shell in the personal pane", map[string]string{"CODEX_THREAD_ID": "harness-codex-03", "REX_BLOCK": "block:pi-05"}, nil, scoped, ErrRefused},
		{"a roleless agent", map[string]string{"REX_BLOCK": "block:claude-07"}, nil, scoped, ErrRefused},
		{"the orchestrator", map[string]string{"REX_BLOCK": "block:claude-01"}, nil, scoped, ErrRefused},
		{"the narrator", map[string]string{"PI_SESSION_ID": "harness-pi-04"}, nil, scoped, ErrRefused},
		{"an unadopted claude -p", map[string]string{"CLAUDE_CODE_SESSION_ID": "nobody", "REX_BLOCK": "block:pi-05"}, nil, scoped, ErrRefused},
		{"a reviewer's session beside the personal agent's", map[string]string{"PI_SESSION_ID": "harness-pi-05", "CODEX_THREAD_ID": "harness-codex-03"}, []proc{{PID: 705}}, scoped, ErrRefused},
		{"an unattended shell", map[string]string{"ME_UNATTENDED": "1"}, nil, scoped[3:], ErrRefused},
		{"env -i under an implementer", nil, []proc{{PID: 9001}, {PID: 702}, {PID: 1}}, scoped[3:], ErrRefused},
		{"env -i under a roleless agent", nil, []proc{{PID: 707}}, scoped[3:], ErrRefused},
		{"a shell naming nobody", nil, nil, scoped[:3], ErrUsage},
		{"a shell naming an unknown agent", nil, nil, []CockpitViewRequest{{Personal: "pi-99"}}, ErrRefused},
	} {
		e := e
		if c.ancestors != nil {
			e.Ancestors = func() ([]proc, error) { return c.ancestors, nil }
		}
		for _, r := range c.reqs {
			b, err := e.ReadCockpitView(ctx, cockpitEnv(c.env), r)
			if !errors.Is(err, c.want) || b != nil {
				t.Errorf("%s %+v: %v, read %s; want %v", c.name, r, err, b, c.want)
			}
		}
	}
	// The personal agent's own scope is its own: another hub, and an Epic
	// linked to another personal agent, are refused; --personal is only a
	// filter inside that scope (§4.2(8)), so another's handle reads the
	// empty subset, never that agent's Epics.
	for _, r := range []CockpitViewRequest{{Hub: t.TempDir()}, {Session: "session:c"}, {Session: "session:q"}, {Personal: "pi-06", Session: "session:c"}} {
		if _, err := e.ReadCockpitView(ctx, cockpitEnv(map[string]string{"REX_BLOCK": "block:pi-05"}), r); !errors.Is(err, ErrRefused) {
			t.Errorf("pi-05 reading %+v: %v", r, err)
		}
	}
	if b, err := e.ReadCockpitView(ctx, cockpitEnv(map[string]string{"REX_BLOCK": "block:pi-05"}), CockpitViewRequest{Personal: "pi-06"}); err != nil || !strings.Contains(string(b), `"epics": []`) {
		t.Errorf("pi-05 --personal pi-06: %s %v; want the empty subset", b, err)
	}
	// A shell with no agent names one even when only one personal agent is
	// live: being outside every agent is no grant by itself.
	gone := recordOf(t, e, "pi-06")
	put(t, e, departed(seedOf(gone), time.Now()))
	if _, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{}); !errors.Is(err, ErrUsage) {
		t.Errorf("a bare shell with one personal agent live: %v", err)
	}
	put(t, e, seedOf(gone))
	// Gate 3 against the real process table: this test process is an
	// implementer's agent.
	real := e
	real.Ancestors = nil
	put(t, real, seed{Handle: "claude-08", Kind: core.Claude, Role: "implementer", SessionID: "session:a", AgentPID: os.Getpid()})
	if _, err := real.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Personal: "pi-05"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "claude-08") {
		t.Errorf("a command under an implementer, by the real process table: %v", err)
	}
	r8 := recordOf(t, e, "claude-08")
	put(t, e, departed(seedOf(r8), time.Now()))
	// A Hub is read whole however many of its personal agents are live
	// (C26): none, and two, are no refusal.
	if b, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: t.TempDir()}); err != nil || !strings.Contains(string(b), `"epics": []`) {
		t.Errorf("a Hub with no live personal agent: %s %v", b, err)
	}
	r := recordOf(t, e, "pi-06")
	r.Cwd = hub
	put(t, e, seedOf(r))
	b, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: hub})
	var v CockpitView
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	if err != nil || len(v.Epics) != 3 || v.Personal != "" {
		t.Errorf("a Hub with two live personal agents: %+v %v", v, err)
	}
}

// The personal agent (even carrying an inherited ME_UNATTENDED=1), and the
// human outside Rex naming it by handle or by hub, read the summary of its
// linked Epics only, and one of them whole with --session: a view with its
// stored hand-back and lifecycle, or for an Epic whose narrator has not
// written one, its lifecycle alone. Reading moves no mail.
func TestCockpitViewReadsOnlyThePersonalAgentsLinkedEpics(t *testing.T) {
	e, hub := cockpitWorld(t)
	ctx := context.Background()
	before := mailTree(t, e)
	for name, c := range map[string]struct {
		env map[string]string
		req CockpitViewRequest
	}{
		"its pane, unattended":   {map[string]string{"REX_BLOCK": "block:pi-05", "ME_UNATTENDED": "1"}, CockpitViewRequest{}},
		"its tool shell":         {map[string]string{"PI_SESSION_ID": "harness-pi-05"}, CockpitViewRequest{Personal: "pi-05", Hub: hub}},
		"outside Rex, by handle": {nil, CockpitViewRequest{Personal: "pi-05"}},
		"outside Rex, by hub":    {nil, CockpitViewRequest{Hub: hub}},
		"a plain Rex pane":       {map[string]string{"REX_BLOCK": "block:shell"}, CockpitViewRequest{Hub: hub}},
	} {
		on := e
		if c.env["PI_SESSION_ID"] != "" {
			// Its tool shell runs under its harness's process (pid 705).
			on.Ancestors = func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 705, Name: "pi"}, {PID: 9000}}, nil }
		}
		b, err := on.ReadCockpitView(ctx, cockpitEnv(c.env), c.req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var v CockpitView
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, ep := range v.Epics {
			ids = append(ids, ep.SessionID)
		}
		slices.Sort(ids)
		if v.Personal != "pi-05" || v.SessionID != "session:p" || !slices.Equal(ids, []string{"session:a", "session:b"}) {
			t.Fatalf("%s: %+v", name, v)
		}
		for _, ep := range v.Epics {
			switch ep.SessionID {
			case "session:a":
				if !ep.HasView || ep.Name != "auth-refresh" || ep.HandBack == nil || ep.HandBack.Seq != 1 || ep.Lifecycle == nil || ep.Lifecycle.State != store.EpicHandBack {
					t.Errorf("%s: session:a %+v", name, ep)
				}
			case "session:b":
				if ep.HasView || ep.Name != "billing" || ep.Lifecycle == nil || ep.Lifecycle.State != store.EpicPartialStart || ep.HandBack != nil {
					t.Errorf("%s: session:b %+v", name, ep)
				}
			}
		}
	}
	b, err := e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Personal: "pi-05", Session: "session:a"})
	if err != nil {
		t.Fatal(err)
	}
	var v SessionView
	if err := json.Unmarshal(b, &v); err != nil || v.Session.SessionID != "session:a" || v.HandBack == nil || v.HandBack.Body != "## Review queue\nPR #3" || v.Lifecycle == nil || v.Lifecycle.State != store.EpicHandBack || len(v.Members) == 0 {
		t.Fatalf("session:a whole: %+v %v", v, err)
	}
	b, err = e.ReadCockpitView(ctx, cockpitEnv(nil), CockpitViewRequest{Hub: hub, Session: "session:b"})
	if err != nil {
		t.Fatal(err)
	}
	v = SessionView{}
	if err := json.Unmarshal(b, &v); err != nil || v.Session.Epic != "billing" || v.Lifecycle == nil || v.Lifecycle.OpenOpID != "op-b" || v.Members == nil || gapRefs(v.CoverageGaps, GapNoView) != nil || len(v.CoverageGaps) != 1 || v.CoverageGaps[0].Kind != GapNoView {
		t.Fatalf("session:b before its view: %s %v", b, err)
	}
	if after := mailTree(t, e); after != before {
		t.Fatal("reading the cockpit moved or wrote mail")
	}
	// The plain view carries the lifecycle too, for those it already lets read.
	b, err = e.ReadSessionView(ctx, Location{Block: "block:pi-05"}, "session:a")
	if err != nil || !strings.Contains(string(b), `"lifecycle":{"session_id":"session:a"`) {
		t.Fatalf("the view without --cockpit: %s %v", b, err)
	}
	if _, err := e.ReadSessionView(ctx, Location{Block: "block:pi-05"}, "session:b"); !errors.Is(err, ErrNoView) {
		t.Errorf("the plain view of an Epic with no view yet: %v", err)
	}
}

// The plain view keeps refusing an Epic's workers, and a roleless agent,
// another Epic's view and their own: the cockpit opened no way around it.
func TestSessionViewStillRefusesWorkersAcrossEpics(t *testing.T) {
	e, _ := cockpitWorld(t)
	ctx := context.Background()
	for _, h := range []string{"claude-02", "codex-03", "claude-07"} {
		for _, session := range []string{"", "session:a", "session:b", "session:c"} {
			if b, err := e.ReadSessionView(ctx, Location{Block: "block:" + h}, session); !errors.Is(err, ErrRefused) || b != nil {
				t.Errorf("%s reading %q: %v %s", h, session, err, b)
			}
		}
	}
}

// In a pair Epic the implementer lead is the control plane: the narrator's
// directive to it and its review traffic reach view.control_plane, while
// mail between two workers it summoned stays a coverage gap, unread.
func TestSessionViewPairEpicLeadIsTheControlPlane(t *testing.T) {
	e := viewWorld(t)
	noOpenObligations(e)
	ctx := context.Background()
	if err := unplant(e.Store, "claude-01"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutSessionCrew("session:a", store.SessionCrew{Crew: pairCrew, ClaimedBy: "pi-05", ClaimedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	put(t, e, seed{Handle: "claude-07", Kind: core.Claude, Role: "implementer", SessionID: "session:a", Parent: "claude-02", Depth: 1})
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), "claude-07"); err != nil {
		t.Fatal(err)
	}
	directive := sendID(t, e, "pi-04", "--to", "claude-02", "--control", "req-1", "--subject", "directive", "--body", "CONTROL: the user: also cover X")
	review := sendID(t, e, "claude-02", "--to", "codex-03", "--kind", "review_request", "--subject", "review", "--body", "CONTROL: please review")
	peer := sendID(t, e, "codex-03", "--to", "claude-07", "--subject", "aside", "--body", "PEER: between workers")
	v, err := e.RefreshSessionView(ctx, Location{Block: "block:pi-04"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := refs(v.ControlPlane); !slices.Contains(got, directive) || !slices.Contains(got, review) || slices.Contains(got, peer) {
		t.Fatalf("control plane %v: want the directive %s and the review %s, not the peer mail %s", got, directive, review, peer)
	}
	if !slices.Contains(gapRefs(v.CoverageGaps, GapPeerBody), peer) {
		t.Fatalf("worker-to-worker mail is not a gap: %+v", v.CoverageGaps)
	}
}

// epic messages caps and spills bodies itself (16 KiB, 0600 blob), so the
// narrator's read by id returns each authorized body whole: a 20 KiB one
// is not cut to the view's excerpt first.
func TestSessionMessagesAreWhole(t *testing.T) {
	e := viewWorld(t)
	ctx := context.Background()
	long := "BIG:" + strings.Repeat("长文本 ", 20<<10/10)
	id := sendID(t, e, "claude-02", "--to", "codex-03", "--subject", "big", "--body", long)
	r, err := e.ReadSessionMessages(ctx, Location{Block: "block:pi-04"}, []string{id})
	if err != nil || len(r.Messages) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if m := r.Messages[0]; m.Truncated || strings.TrimRight(m.Body, "\n") != long {
		t.Fatalf("read %d of %d bytes, truncated=%v", len(m.Body), len(long), m.Truncated)
	}
	if got := excerpt(amq.ThreadMessage{Body: long}); !got.Truncated || len([]rune(got.Body)) != viewExcerpt {
		t.Errorf("the stored view's excerpt is no longer bounded: %d runes", len([]rune(got.Body)))
	}
}
