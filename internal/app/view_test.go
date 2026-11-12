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
		store.Record{Handle: "claude-01", Kind: core.Claude, Role: core.OrchestratorRole, SessionID: "session:a"},
		store.Record{Handle: "claude-02", Kind: core.Claude, Role: "implementer", SessionID: "session:a", Task: "token refresh"},
		store.Record{Handle: "codex-03", Kind: core.Codex, Role: "reviewer", SessionID: "session:a"},
		store.Record{Handle: "pi-04", Kind: core.Pi, Role: core.NarratorRole, SessionID: "session:a"},
		store.Record{Handle: "pi-05", Kind: core.Pi, Role: core.PersonalRole, SessionID: "session:p"},
		store.Record{Handle: "pi-06", Kind: core.Pi, Role: core.PersonalRole, SessionID: "session:q"},
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
