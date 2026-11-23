package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// hubWorld is an Env whose one Hub directory (ME_HUB) is hub, and a Rex
// with block:a in session:s, block:b in session:s too (another tab), and
// block:c in session:t. Each block's foreground works in cwd[block], the Hub
// itself unless set.
func hubWorld(t *testing.T) (Env, *fakeTerm, string) {
	t.Helper()
	hubDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	term := &fakeTerm{state: "idle",
		windows:  map[string]string{"block:a": "window:1", "block:b": "window:2", "block:c": "window:3"},
		sessions: map[string]string{"window:3": "session:t"},
		fgs:      map[string]rex.Process{},
	}
	e := realAmqEnv(t, term)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	e.Getenv = func(k string) string {
		if k == "ME_HUB" {
			return hubDir
		}
		return ""
	}
	for _, b := range []string{"block:a", "block:b", "block:c"} {
		term.fgs[b] = rex.Process{PID: os.Getpid(), Name: "pi", Cwd: hubDir}
	}
	return e, term, hubDir
}

// hookAs runs SessionStart for a Pi agent in b with pane label label.
func hookAs(t *testing.T, e Env, term *fakeTerm, b, session, label string, in HookInput) Reply {
	t.Helper()
	term.mu.Lock()
	term.label = label
	term.mu.Unlock()
	defer func() {
		term.mu.Lock()
		term.label = ""
		term.mu.Unlock()
	}()
	in.SessionID, in.AgentPID = session, os.Getpid()
	r, err := e.Hook(context.Background(), EventSessionStart, core.Pi, Location{Block: b}, in)
	if err != nil {
		t.Fatalf("session-start in %s: %v", b, err)
	}
	t.Cleanup(func() { _ = e.Depart(context.Background(), Location{Block: b}, HookInput{}) })
	return r
}

func guardReason(err error) (fault.Code, string) {
	f, ok := fault.As(err)
	if !ok {
		return "", ""
	}
	reason, _ := f.Details["reason"].(string)
	return f.Code, reason
}

// An agent adopted with no role label, no summon, and no ME_UNATTENDED, in
// a Hub directory and a session of no Epic, is the session's personal
// agent, and the session is recorded as that Hub's before its identity is;
// the policy stays when the agent leaves. A repository linked into the Hub
// (a symlink out of it) is no Hub, an unattended launch is no personal
// agent, and an Epic's policy beats the directory.
func TestHubAutoPersonal(t *testing.T) {
	e, term, hubDir := hubWorld(t)
	hookAs(t, e, term, "block:a", "s1", "", HookInput{})
	rec, ok, err := e.agentAt("block:a")
	if err != nil || !ok || rec.Role != core.PersonalRole || rec.Unattended {
		t.Fatalf("adopted in the Hub: %+v %v %v", rec, ok, err)
	}
	p, ok, err := e.Store.SessionPolicy("session:s")
	if err != nil || !ok || !p.Hub() || p.HubKey != core.NewHubKey(fakeServer, hubDir) || p.CreatedBy != rec.Handle {
		t.Fatalf("policy %+v %v %v", p, ok, err)
	}
	if err := e.Depart(context.Background(), Location{Block: "block:a"}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if p, ok, _ := e.Store.SessionPolicy("session:s"); !ok || !p.Hub() {
		t.Fatalf("the Hub policy went with its last personal agent: %+v", p)
	}

	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hubDir, "repos"), 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(hubDir, "repos", "x")
	if err := os.Symlink(repo, linked); err != nil {
		t.Fatal(err)
	}
	term.fgs["block:c"] = rex.Process{PID: os.Getpid(), Name: "pi", Cwd: linked}
	hookAs(t, e, term, "block:c", "c1", "", HookInput{})
	if r, _, _ := e.agentAt("block:c"); r.Role != "" {
		t.Errorf("an agent in a repository linked under the Hub became %q", r.Role)
	}
	if _, ok, _ := e.Store.SessionPolicy("session:t"); ok {
		t.Error("a session outside the Hub got a policy")
	}

	for name, c := range map[string]struct {
		in    HookInput
		setup func(session string)
	}{
		"unattended": {HookInput{Unattended: true}, func(string) {}},
		"epic policy": {HookInput{}, func(session string) {
			if err := e.Store.PutSessionPolicy(epicPolicy(session, fakeServer, "", "claude-09", e.Now())); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		session := "session:" + strings.ReplaceAll(name, " ", "-")
		term.mu.Lock()
		term.windows["block:"+name], term.sessions["window:"+name] = "window:"+name, session
		term.fgs["block:"+name] = rex.Process{PID: os.Getpid(), Name: "pi", Cwd: hubDir}
		term.mu.Unlock()
		c.setup(session)
		hookAs(t, e, term, "block:"+name, name, "", c.in)
		if r, _, _ := e.agentAt("block:" + name); r.Role == core.PersonalRole {
			t.Errorf("%s: became the personal agent", name)
		}
		if p, ok, _ := e.Store.SessionPolicy(session); ok && p.Hub() {
			t.Errorf("%s: the session became a Hub's", name)
		}
	}
}

// A Hub session (its policy, or a live personal agent in it) holds personal
// agents only: a launch labelled for another role, or carrying a summon
// token, is refused before anything changes (no identity, mailbox, or
// lease) and told to open an Epic; joining it as another role, or as a
// crew, is refused too, and a session already mixed reports the conflict.
func TestHubSessionPure(t *testing.T) {
	e, term, _ := hubWorld(t)
	hookAs(t, e, term, "block:a", "s1", "", HookInput{})
	before, err := amq.Agents(e.Paths.Root())
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]struct {
		label string
		in    HookInput
	}{
		"labelled implementer": {"implementer | fix", HookInput{}},
		"summoned":             {"", HookInput{SummonToken: "0123456789abcdef"}},
		"summoned as personal": {"personal | x", HookInput{SummonToken: "0123456789abcdef"}},
	} {
		r := hookAs(t, e, term, "block:b", "b-"+strings.ReplaceAll(name, " ", "-"), in.label, in.in)
		if r.Notice != core.HubRefusalNotice || !strings.Contains(r.Notice, "epic open") {
			t.Errorf("%s: notice %q", name, r.Notice)
		}
		if _, ok, _ := e.agentAt("block:b"); ok {
			t.Errorf("%s: got an identity", name)
		}
		if after, _ := amq.Agents(e.Paths.Root()); !slices.Equal(after, before) {
			t.Errorf("%s: mailboxes %v, before %v", name, after, before)
		}
		leases, _ := e.Store.Leases()
		if len(leases) != 1 {
			t.Errorf("%s: leases %+v", name, leases)
		}
	}
	// A worker already in the Hub session (mixed before admission): it may
	// not change role or claim a crew there, and nothing of it changes.
	put(t, e, seed{Handle: "codex-09", Kind: core.Codex, Role: "implementer", BlockID: "block:b", SessionID: "session:s", WindowID: "window:2", AgentPID: os.Getpid(), AdoptedAt: time.Now()})
	for _, j := range []struct{ crew, role string }{{"", "reviewer"}, {"pair", ""}} {
		_, err := e.Join(context.Background(), Location{Block: "block:b"}, j.crew, j.role)
		code, reason := guardReason(err)
		f, _ := fault.As(err)
		if code != fault.GuardDenied || reason != core.ReasonSessionPure || f.Details["policy_conflict"] != true || !strings.Contains(f.NextAction, "epic open") {
			t.Errorf("join %+v in the Hub session: %v (%s %s)", j, err, code, reason)
		}
	}
	if r := recordNamed(t, e, "codex-09"); r.Role != "implementer" || !r.Live() {
		t.Errorf("a refused join changed the worker: %+v", r)
	}
	if _, ok, _ := e.Store.SessionCrew("session:s"); ok {
		t.Error("a refused join claimed a crew")
	}
}

// A personal agent never takes another role, with or without a crew, for
// real or as a preview, and a narrator is locked likewise; a personal role
// is refused in an Epic's session. Each refusal is guard_denied (exit 6)
// with its reason, and leaves the role and the session's crew as they were.
func TestHubRoleLock(t *testing.T) {
	e, term, _ := hubWorld(t)
	hookAs(t, e, term, "block:a", "s1", "", HookInput{})
	me, _, _ := e.agentAt("block:a")
	ctx := context.Background()
	for _, role := range []string{core.OrchestratorRole, "implementer", core.NarratorRole} {
		_, err := e.Join(ctx, Location{Block: "block:a"}, "", role)
		if code, reason := guardReason(err); code != fault.GuardDenied || reason != core.ReasonRoleLocked || fault.ExitCode(code) != 6 {
			t.Errorf("personal -> %s: %v", role, err)
		}
		if _, err := e.PreviewJoin(ctx, Location{Block: "block:a"}, "", role); err == nil {
			t.Errorf("personal -> %s previewed as allowed", role)
		}
	}
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "pair", ""); err == nil {
		t.Error("a personal agent joined a crew")
	}
	if r := recordOf(t, e, me.Handle); r.Role != core.PersonalRole {
		t.Fatalf("the personal agent's role became %q", r.Role)
	}

	// An Epic's session: a narrator is locked, and nobody becomes personal.
	term.mu.Lock()
	term.fgs["block:c"] = rex.Process{PID: os.Getpid(), Name: "pi", Cwd: t.TempDir()}
	term.mu.Unlock()
	if err := e.Store.PutSessionPolicy(epicPolicy("session:t", fakeServer, "", "claude-09", e.Now())); err != nil {
		t.Fatal(err)
	}
	narr := hookAs(t, e, term, "block:c", "n1", core.NarratorRole+" | epic", HookInput{})
	_ = narr
	if _, err := e.Join(ctx, Location{Block: "block:c"}, "", "implementer"); err == nil {
		t.Error("a narrator took another role")
	} else if _, reason := guardReason(err); reason != core.ReasonRoleLocked {
		t.Errorf("narrator -> implementer: %v", err)
	}
	put(t, e, seed{Handle: "codex-08", Kind: core.Codex, Role: "implementer", BlockID: "block:w", SessionID: "session:t", AgentPID: os.Getpid(), AdoptedAt: time.Now()})
	term.mu.Lock()
	term.windows["block:w"], term.sessions["window:w"] = "window:w", "session:t"
	term.mu.Unlock()
	_, err := e.Join(ctx, Location{Block: "block:w"}, "", core.PersonalRole)
	if code, reason := guardReason(err); code != fault.GuardDenied || reason != core.ReasonNoPersonal {
		t.Errorf("worker -> personal in an Epic: %v", err)
	}
	if r := recordNamed(t, e, "codex-08"); r.Role != "implementer" {
		t.Errorf("a refused join changed the worker's role to %q", r.Role)
	}
}

// A personal agent, or any agent of a Hub session, summons nothing: the
// summon is refused before any mail, reservation, or pane, for real and as
// a preview, and the refusal is permission_denied (exit 3) whose next
// action opens an Epic.
func TestSummonRefusedInHub(t *testing.T) {
	e, term, loc := summonWorld(t)
	asked := func(r *SummonRequest) { r.Asked = "start one" }
	// A live personal agent in the caller's session makes it a Hub's.
	add(t, e, seed{Handle: "pi-07", Kind: core.Pi, Server: fakeServer, BlockID: "block:p", SessionID: "session:s", Role: core.PersonalRole, AgentPID: 4, AdoptedAt: e.Now()})
	for _, dry := range []bool{false, true} {
		res := summon(t, e, loc, "implementer", asked, func(r *SummonRequest) { r.DryRun = dry })
		if res.Action != "refused" || res.Mailed || res.Block != "" {
			t.Fatalf("dry=%v: %+v", dry, res)
		}
		f := SummonRefusal(res.Reason)
		if f.Code != fault.PermissionDenied || fault.ExitCode(f.Code) != 3 || f.Details["reason"] != "hub_session" || !strings.Contains(f.NextAction, "rex-amq-adapter epic open") {
			t.Errorf("dry=%v: refusal %+v", dry, f)
		}
	}
	me, _, _ := e.agentAt(loc.Block)
	me.Role = core.PersonalRole
	add(t, e, seedOf(me))
	res := summon(t, e, loc, "implementer", asked)
	if f := SummonRefusal(res.Reason); res.Action != "refused" || f.Code != fault.PermissionDenied || !strings.Contains(f.NextAction, "epic open") {
		t.Errorf("a personal caller: %+v %+v", res, f)
	}
	if len(term.launches) != 0 {
		t.Fatalf("launched: %+v", term.launches)
	}
	if n, _ := e.Store.PendingSummons("session:s", e.Now()); n != 0 {
		t.Fatalf("pending reservations: %d", n)
	}
}

// hubOf records session as a Hub's (key).
func putHub(t *testing.T, e Env, session string, key core.HubKey) {
	t.Helper()
	if err := e.Store.PutSessionPolicy(store.SessionPolicy{Kind: store.PolicyHub, SessionID: session, Server: fakeServer, HubKey: key, CreatedAt: e.Now()}); err != nil {
		t.Fatal(err)
	}
}

// An Epic one personal agent links is linked to its Hub: every personal
// agent of the Hub, in any Rex session, exchanges mail with its narrator,
// and none of another Hub does; a session-keyed link (one made before
// links named Hubs) still grants its own session, and nothing else.
func TestHubLinkAcrossSessions(t *testing.T) {
	term := &fakeTerm{}
	e := realAmqEnv(t, term)
	ctx := context.Background()
	key, other := core.NewHubKey(fakeServer, "/hub"), core.NewHubKey(fakeServer, "/elsewhere")
	put(t, e,
		seed{Handle: "claude-01", Role: core.PersonalRole, SessionID: "session:p1"},
		seed{Handle: "claude-02", Role: core.PersonalRole, SessionID: "session:p2"},
		seed{Handle: "claude-03", Role: core.PersonalRole, SessionID: "session:p3"},
		seed{Handle: "claude-04", Role: core.PersonalRole, SessionID: "session:p4"},
		seed{Handle: "agy-05", Role: core.NarratorRole, SessionID: "session:e"},
		seed{Handle: "agy-06", Role: core.NarratorRole, SessionID: "session:f"})
	putHub(t, e, "session:p1", key)
	putHub(t, e, "session:p2", key)
	putHub(t, e, "session:p3", other)
	for _, h := range []string{"claude-01", "claude-02", "claude-03", "claude-04", "agy-05", "agy-06"} {
		if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	l, changed, err := e.Link(ctx, Location{Block: "block:claude-01"}, "session:e", true)
	if err != nil || !changed || l.Hub != key || l.PreferredPersonal != "claude-01" {
		t.Fatalf("link: %+v %v %v", l, changed, err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.Paths.Extensions(), "links"))
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "hub_") || !strings.HasSuffix(entries[0].Name(), "session_e.json") || len(strings.SplitN(entries[0].Name(), "__", 2)[0]) != len("hub_")+16 {
		t.Fatalf("link files: %v", entries)
	}
	if _, changed, err := e.Link(ctx, Location{Block: "block:claude-02"}, "session:e", true); err != nil || changed {
		t.Fatalf("the Hub's other personal agent linking again: %v %v", changed, err)
	}
	send := func(narr, to string) error {
		var out strings.Builder
		_, err := e.ManagedMail(ctx, Location{Block: "block:" + narr}, "send", []string{"--to", to, "--kind", "status", "--subject", "s", "--body", "b"}, nil, &out, &out)
		return err
	}
	for _, to := range []string{"claude-01", "claude-02"} {
		if err := send("agy-05", to); err != nil {
			t.Errorf("the narrator to %s of its Hub: %v", to, err)
		}
		if kept, err := e.admit(context.Background(), recordNamed(t, e, core.Handle(to))); err != nil || len(kept) != 1 {
			t.Errorf("%s admitted %v %v", to, kept, err)
		}
	}
	if err := send("agy-05", "claude-03"); !errors.Is(err, ErrRefused) {
		t.Errorf("the narrator to another Hub's personal agent: %v", err)
	}
	// A session-keyed link grants its session only.
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p4"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:f"}}); err != nil {
		t.Fatal(err)
	}
	if err := send("agy-06", "claude-04"); err != nil {
		t.Errorf("a session-keyed link: %v", err)
	}
	if err := send("agy-06", "claude-01"); !errors.Is(err, ErrRefused) {
		t.Errorf("a session-keyed link reached another session: %v", err)
	}
	p1, p4 := core.Endpoint{Handle: "claude-01", Server: fakeServer, SessionID: "session:p1", Hub: key}, core.Endpoint{Handle: "claude-04", Server: fakeServer, SessionID: "session:p4"}
	if !core.LinkMatchesPersonal(l, p1) || core.LinkMatchesPersonal(l, p4) || core.LinkMatchesPersonal(core.Link{Epic: l.Epic}, core.Endpoint{}) {
		t.Error("LinkMatchesPersonal")
	}
	// Any personal agent of the Hub unlinks it for the Hub.
	if _, changed, err := e.Link(ctx, Location{Block: "block:claude-02"}, "session:e", false); err != nil || !changed {
		t.Fatalf("unlink by the Hub's other personal agent: %v %v", changed, err)
	}
	if err := send("agy-05", "claude-01"); !errors.Is(err, ErrRefused) {
		t.Errorf("after the unlink: %v", err)
	}
}

// From outside Rex, `epic list` (and `epic show`) read a Hub's Epics by the
// Hub, the same with no, one, or three of its personal agents live; and
// --personal only narrows them to the Epics that agent opened.
func TestEpicListHubScope(t *testing.T) {
	term := &fakeTerm{}
	e := realAmqEnv(t, term)
	e.Ancestors = func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 9000}}, nil }
	hubDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Getenv = func(k string) string {
		if k == "ME_HUB" {
			return hubDir
		}
		return ""
	}
	key := core.NewHubKey(fakeServer, hubDir)
	for i, epic := range []string{"session:e1", "session:e2"} {
		pref := core.Handle([]string{"claude-01", "claude-02"}[i])
		if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p1"}, PreferredPersonal: pref, Epic: core.SessionKey{Server: fakeServer, SessionID: epic}}); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: epic, Server: fakeServer, Hub: key, State: store.EpicHandBack, PersonalHandle: pref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: core.NewHubKey(fakeServer, "/other"), Epic: core.SessionKey{Server: fakeServer, SessionID: "session:x"}}); err != nil {
		t.Fatal(err)
	}
	read := func(req CockpitViewRequest) (CockpitView, error) {
		b, err := e.ReadCockpitView(context.Background(), cockpitEnv(nil), req)
		var v CockpitView
		if err == nil {
			err = json.Unmarshal(b, &v)
		}
		return v, err
	}
	ids := func(v CockpitView) []string {
		var out []string
		for _, ep := range v.Epics {
			out = append(out, ep.SessionID)
			if ep.Obligation != ObligationHandBack {
				t.Errorf("%s: obligation %q", ep.SessionID, ep.Obligation)
			}
		}
		slices.Sort(out)
		return out
	}
	for n := 0; n <= 3; n++ {
		if n > 0 {
			h := core.Handle([]string{"", "claude-01", "claude-02", "claude-03"}[n])
			session := "session:p" + string(rune('0'+n))
			put(t, e, seed{Handle: h, Role: core.PersonalRole, SessionID: session, Cwd: hubDir})
			putHub(t, e, session, key)
		}
		if n == 2 {
			continue
		}
		for _, req := range []CockpitViewRequest{{}, {Hub: hubDir}} {
			v, err := read(req)
			if err != nil || !slices.Equal(ids(v), []string{"session:e1", "session:e2"}) || v.Hub != hubDir {
				t.Errorf("%d live, %+v: %+v %v", n, req, v, err)
			}
			if b, err := e.ReadCockpitView(context.Background(), cockpitEnv(nil), CockpitViewRequest{Hub: req.Hub, Session: "session:e2"}); err != nil || !strings.Contains(string(b), `"session_id":"session:e2"`) {
				t.Errorf("%d live: epic show session:e2: %v", n, err)
			}
			if _, err := e.ReadCockpitView(context.Background(), cockpitEnv(nil), CockpitViewRequest{Hub: req.Hub, Session: "session:x"}); !errors.Is(err, ErrRefused) {
				t.Errorf("%d live: another Hub's Epic: %v", n, err)
			}
		}
	}
	v, err := read(CockpitViewRequest{Personal: "claude-02"})
	if err != nil || !slices.Equal(ids(v), []string{"session:e2"}) || v.Personal != "claude-02" {
		t.Errorf("--personal claude-02: %+v %v", v, err)
	}
	// Inside Rex, any personal agent of the Hub reads all of them.
	v2, err := e.ReadCockpitView(context.Background(), cockpitEnv(map[string]string{"REX_BLOCK": "block:claude-03"}), CockpitViewRequest{})
	if err != nil || !strings.Contains(string(v2), "session:e1") || !strings.Contains(string(v2), "session:e2") {
		t.Errorf("claude-03 inside Rex: %s %v", v2, err)
	}
}

// killCounter counts the kills Rex is asked for.
type killCounter struct {
	*fakeTerm
	kills atomic.Int32
}

func (k *killCounter) KillSession(ctx context.Context, session string) error {
	k.kills.Add(1)
	return k.fakeTerm.KillSession(ctx, session)
}

// Two personal agents of a Hub, in two Rex sessions, closing one linked
// Epic at once under distinct op ids converge on one termination: one kill,
// one changed close, and the other changed false (exit 0), with the closed
// lifecycle kept as the tombstone a later close or replay answers from.
func TestEpicCloseRace(t *testing.T) {
	e, term, loc := closeWorld(t)
	kc := &killCounter{fakeTerm: term}
	e.Rex, e.RexOn = kc, func(string) Terminal { return kc }
	key := core.NewHubKey(fakeServer, "/hub")
	putHub(t, e, "session:me", key)
	putHub(t, e, "session:me2", key)
	put(t, e, seed{Handle: "claude-02", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me2", SessionID: "session:me2", WindowID: "window:me2", AdoptedAt: time.Now()})
	links, _ := e.Store.Links()
	for _, l := range links {
		if _, err := e.Store.DeleteLink(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:me"}, PreferredPersonal: "claude-01", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:epic"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]CloseResult, 2)
	errs := make([]error, 2)
	for i, l := range []Location{loc, {Block: "block:me2"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = closeEpic(e, l, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-" + string(rune('a'+i))})
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("closes: %v / %v", errs[0], errs[1])
	}
	if results[0].Changed == results[1].Changed {
		t.Fatalf("both closes changed=%v: %+v %+v", results[0].Changed, results[0], results[1])
	}
	if n := kc.kills.Load(); n != 1 {
		t.Fatalf("%d kills, want 1", n)
	}
	if lc, ok, _ := e.Store.EpicLifecycle("session:epic"); !ok || lc.State != store.EpicClosed || lc.Hub != key {
		t.Fatalf("lifecycle %+v", lc)
	}
	// The tombstone answers a later close of the Hub, a replay, and a
	// --force; none kills again.
	for _, r := range []struct {
		loc Location
		req CloseRequest
	}{
		{Location{Block: "block:me2"}, CloseRequest{Session: "session:epic", Asked: "again"}},
		{loc, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-a"}},
		{Location{Block: "block:me2"}, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-b"}},
		{loc, CloseRequest{Session: "session:epic", Asked: "again", Force: true, OpID: "op-c"}},
	} {
		res, err := closeEpic(e, r.loc, r.req)
		if err != nil || res.Parked == nil {
			t.Errorf("%+v: %+v %v", r.req, res, err)
		}
	}
	if n := kc.kills.Load(); n != 1 {
		t.Fatalf("%d kills after the replays, want 1", n)
	}
	// op-a with --force is another request: refused.
	if _, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-a", Force: true}); err == nil {
		t.Error("an op id reused with --force was accepted")
	}
}

// A close whose kill failed (its op left closing) and that another op id has
// since seen through is answered, on its retry, by the Epic's tombstone:
// changed false, its op reconciled to closed, and no second kill, park, or
// unlink.
func TestEpicCloseReplayAfterAnotherOpClosed(t *testing.T) {
	e, term, loc := closeWorld(t)
	kc := &killCounter{fakeTerm: term}
	e.Rex, e.RexOn = kc, func(string) Terminal { return kc }
	a := CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-a"}
	term.mu.Lock()
	term.killErr = errors.New("injected kill failure")
	term.mu.Unlock()
	if _, err := closeEpic(e, loc, a); err == nil {
		t.Fatal("op-a closed despite the failed kill")
	}
	if op, ok, _ := e.Store.EpicOp(store.EpicOpClose, "op-a"); !ok || op.State != store.EpicClosing {
		t.Fatalf("op-a after its failed kill: %+v", op)
	}
	term.mu.Lock()
	term.killErr = nil
	term.mu.Unlock()
	b, err := closeEpic(e, loc, CloseRequest{Session: "session:epic", Asked: "close it", OpID: "op-b"})
	if err != nil || !b.Changed {
		t.Fatalf("op-b: %+v %v", b, err)
	}
	kills := kc.kills.Load()
	res, err := closeEpic(e, loc, a)
	if err != nil || res.Changed || !res.Replayed || res.LifecycleState != store.EpicClosed || res.Parked == nil || len(res.Parked) != 0 {
		t.Fatalf("op-a retried after op-b closed the Epic: %+v %v", res, err)
	}
	if n := kc.kills.Load(); n != kills {
		t.Fatalf("the retry killed again: %d kills, %d before it", n, kills)
	}
	if op, _, _ := e.Store.EpicOp(store.EpicOpClose, "op-a"); op.State != store.EpicClosed {
		t.Errorf("op-a not reconciled: %+v", op)
	}
	if lc, _, _ := e.Store.EpicLifecycle("session:epic"); lc.CloseOpID != "op-b" {
		t.Errorf("the tombstone was rewritten: %+v", lc)
	}
}

// hubCockpitWorld is two personal agents of one Hub (claude-01 in
// session:p1, claude-02 in session:p2) and one of another Hub (claude-09 in
// session:p9); Hub links to session:e1 (preferring claude-01) and session:e2
// (preferring claude-02), and a session-keyed link of session:p2 to
// session:legacy, made before links named Hubs.
func hubCockpitWorld(t *testing.T) (Env, string) {
	t.Helper()
	e := realAmqEnv(t, &fakeTerm{})
	e.Ancestors = func() ([]proc, error) { return []proc{{PID: 9001}, {PID: 9000}}, nil }
	hubDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Getenv = func(k string) string {
		if k == "ME_HUB" {
			return hubDir
		}
		return ""
	}
	key, other := core.NewHubKey(fakeServer, hubDir), core.NewHubKey(fakeServer, "/other")
	put(t, e,
		seed{Handle: "claude-01", Role: core.PersonalRole, SessionID: "session:p1", Cwd: hubDir},
		seed{Handle: "claude-02", Role: core.PersonalRole, SessionID: "session:p2", Cwd: hubDir},
		seed{Handle: "claude-09", Role: core.PersonalRole, SessionID: "session:p9", Cwd: "/other"})
	putHub(t, e, "session:p1", key)
	putHub(t, e, "session:p2", key)
	putHub(t, e, "session:p9", other)
	for i, epic := range []string{"session:e1", "session:e2"} {
		pref := core.Handle([]string{"claude-01", "claude-02"}[i])
		if err := e.Store.PutLink(core.Link{Hub: key, Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p" + string(rune('1'+i))}, PreferredPersonal: pref, Epic: core.SessionKey{Server: fakeServer, SessionID: epic}}); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.PutEpicLifecycle(store.EpicLifecycle{SessionID: epic, Server: fakeServer, Hub: key, State: store.EpicOpened, PersonalHandle: pref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Store.PutLink(core.Link{Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p2"}, Epic: core.SessionKey{Server: fakeServer, SessionID: "session:legacy"}}); err != nil {
		t.Fatal(err)
	}
	return e, hubDir
}

func cockpitIDs(t *testing.T, e Env, env map[string]string, req CockpitViewRequest) ([]string, error) {
	t.Helper()
	b, err := e.ReadCockpitView(context.Background(), cockpitEnv(env), req)
	if err != nil {
		return nil, err
	}
	var v CockpitView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, ep := range v.Epics {
		out = append(out, ep.SessionID)
	}
	slices.Sort(out)
	return out, nil
}

// Inside Rex a personal agent reads by its own endpoint alone
// (core.LinkMatchesPersonal): a session-keyed link of another personal
// session is not its Hub's because that session is a member of the Hub now.
// Outside Rex, --hub still reads the Hub's directory, legacy links included.
func TestCockpitPersonalScopeIsItsOwnEndpoint(t *testing.T) {
	e, hubDir := hubCockpitWorld(t)
	ids, err := cockpitIDs(t, e, map[string]string{"REX_BLOCK": "block:claude-01"}, CockpitViewRequest{})
	if err != nil || !slices.Equal(ids, []string{"session:e1", "session:e2"}) {
		t.Errorf("claude-01 inside Rex: %v %v", ids, err)
	}
	if _, err := e.ReadCockpitView(context.Background(), cockpitEnv(map[string]string{"REX_BLOCK": "block:claude-01"}), CockpitViewRequest{Session: "session:legacy"}); !errors.Is(err, ErrRefused) {
		t.Errorf("claude-01 reading another session's legacy link: %v", err)
	}
	// The legacy link's own session still reads it.
	if ids, err := cockpitIDs(t, e, map[string]string{"REX_BLOCK": "block:claude-02"}, CockpitViewRequest{}); err != nil || !slices.Equal(ids, []string{"session:e1", "session:e2", "session:legacy"}) {
		t.Errorf("claude-02 inside Rex: %v %v", ids, err)
	}
	if ids, err := cockpitIDs(t, e, nil, CockpitViewRequest{Hub: hubDir}); err != nil || !slices.Equal(ids, []string{"session:e1", "session:e2", "session:legacy"}) {
		t.Errorf("outside Rex, --hub: %v %v", ids, err)
	}
}

// Inside Rex, --personal only narrows a personal agent's own Hub scope to
// the Epics the named personal agent prefers, whether or not it is live
// (§4.2(8)): it never switches Hubs, so a personal agent of another Hub or an
// unknown handle reads the empty subset, and a member that departed still
// filters to the Epics it prefers.
func TestCockpitPersonalFilterInsideRex(t *testing.T) {
	e, _ := hubCockpitWorld(t)
	in := map[string]string{"REX_BLOCK": "block:claude-01"}
	for _, c := range []struct {
		personal string
		want     []string
	}{
		{"claude-01", []string{"session:e1"}},
		{"claude-02", []string{"session:e2"}},
	} {
		if ids, err := cockpitIDs(t, e, in, CockpitViewRequest{Personal: c.personal}); err != nil || !slices.Equal(ids, c.want) {
			t.Errorf("claude-01 --personal %s: %v %v", c.personal, ids, err)
		}
	}
	if b, err := e.ReadCockpitView(context.Background(), cockpitEnv(in), CockpitViewRequest{Personal: "claude-02", Session: "session:e2"}); err != nil || !strings.Contains(string(b), `"session_id":"session:e2"`) {
		t.Errorf("claude-01 --personal claude-02 --session session:e2: %v", err)
	}
	if _, err := e.ReadCockpitView(context.Background(), cockpitEnv(in), CockpitViewRequest{Personal: "claude-02", Session: "session:e1"}); !errors.Is(err, ErrRefused) {
		t.Errorf("an Epic the filter leaves out: %v", err)
	}
	for _, p := range []string{"claude-09", "claude-99"} {
		if ids, err := cockpitIDs(t, e, in, CockpitViewRequest{Personal: p}); err != nil || len(ids) != 0 {
			t.Errorf("claude-01 --personal %s: %v %v; want the empty subset", p, ids, err)
		}
		if _, err := e.ReadCockpitView(context.Background(), cockpitEnv(in), CockpitViewRequest{Personal: p, Session: "session:e1"}); !errors.Is(err, ErrRefused) {
			t.Errorf("claude-01 --personal %s --session session:e1: %v", p, err)
		}
	}
	// P2 departs: P1 still reads P2's Epic unfiltered, and filtered by P2.
	put(t, e, departed(seedOf(recordOf(t, e, "claude-02")), time.Now()))
	if ids, err := cockpitIDs(t, e, in, CockpitViewRequest{}); err != nil || !slices.Equal(ids, []string{"session:e1", "session:e2"}) {
		t.Errorf("claude-01 after claude-02 departed: %v %v", ids, err)
	}
	if ids, err := cockpitIDs(t, e, in, CockpitViewRequest{Personal: "claude-02"}); err != nil || !slices.Equal(ids, []string{"session:e2"}) {
		t.Errorf("claude-01 --personal claude-02 after claude-02 departed: %v %v", ids, err)
	}
	if b, err := e.ReadCockpitView(context.Background(), cockpitEnv(in), CockpitViewRequest{Personal: "claude-02", Session: "session:e2"}); err != nil || !strings.Contains(string(b), `"session_id":"session:e2"`) {
		t.Errorf("claude-01 --personal claude-02 --session session:e2 after claude-02 departed: %v", err)
	}
}

// A repeated adoption in the same session is admitted again when the role
// it would take changed: a roleless agent of an Epic's session whose
// launcher label now says personal is refused (epic_session_no_personal),
// and keeps its identity, role, and lease as they were; the Epic's session
// gets no Hub policy.
func TestHubReadoptRoleChangeIsAdmitted(t *testing.T) {
	e, term, _ := hubWorld(t)
	term.mu.Lock()
	term.fgs["block:c"] = rex.Process{PID: os.Getpid(), Name: "pi", Cwd: t.TempDir()}
	term.mu.Unlock()
	if err := e.Store.PutSessionPolicy(epicPolicy("session:t", fakeServer, "", "claude-09", e.Now())); err != nil {
		t.Fatal(err)
	}
	hookAs(t, e, term, "block:c", "r1", "", HookInput{})
	before, ok, err := e.agentAt("block:c")
	if err != nil || !ok || before.Role != "" || !before.Live() {
		t.Fatalf("adopted roleless in the Epic: %+v %v %v", before, ok, err)
	}
	leaseBefore, _, err := e.Store.LeaseOf(before.Handle)
	if err != nil {
		t.Fatal(err)
	}
	r := hookAs(t, e, term, "block:c", "r1", core.PersonalRole+" | x", HookInput{})
	if !strings.Contains(r.Notice, "personal agent lives only in a Hub session") {
		t.Errorf("notice %q", r.Notice)
	}
	after := recordNamed(t, e, before.Handle)
	if after.Role != "" || !after.Live() {
		t.Fatalf("a refused re-adoption changed the agent: %+v", after)
	}
	leaseAfter, _, err := e.Store.LeaseOf(before.Handle)
	if err != nil || leaseAfter.Epoch != leaseBefore.Epoch || !leaseAfter.AcquiredAt.Equal(leaseBefore.AcquiredAt) {
		t.Errorf("a refused re-adoption touched the lease: %+v, before %+v (%v)", leaseAfter, leaseBefore, err)
	}
	if p, _, _ := e.Store.SessionPolicy("session:t"); p.Hub() {
		t.Errorf("the Epic's session became a Hub's: %+v", p)
	}
}
