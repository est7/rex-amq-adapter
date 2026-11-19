package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// artifactWorld is a design Epic (session:d) led by architect claude-10,
// with a council architect it summoned, codex-11, and a narrator; its
// lifecycle and Hub link belong to the Hub of personal agents claude-01
// (linked) and claude-09 (another Hub). Each agent's pane is its block.
func artifactWorld(t *testing.T) (Env, string) {
	t.Helper()
	e, hubDir := hubCockpitWorld(t)
	now := time.Now()
	p := epicPolicy("session:d", fakeServer, designCrew, "claude-01", now)
	p.LeadHandle = "claude-10"
	if err := e.Store.PutSessionPolicy(p); err != nil {
		t.Fatal(err)
	}
	put(t, e,
		seed{Handle: "claude-10", Kind: core.Claude, Role: architectRole, SessionID: "session:d", AdoptedAt: now},
		seed{Handle: "codex-11", Kind: core.Codex, Role: architectRole, SessionID: "session:d", Parent: "claude-10", Depth: 1, AdoptedAt: now},
		seed{Handle: "agy-12", Kind: core.Agy, Role: core.NarratorRole, SessionID: "session:d", AdoptedAt: now},
	)
	if err := e.Store.PutLink(core.Link{Hub: core.NewHubKey(fakeServer, hubDir), Personal: core.SessionKey{Server: fakeServer, SessionID: "session:p1"},
		PreferredPersonal: "claude-01", Epic: core.SessionKey{Server: fakeServer, SessionID: "session:d"}}); err != nil {
		t.Fatal(err)
	}
	return e, hubDir
}

// writeSrc writes data to a fresh file named name and returns its path.
func writeSrc(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// in is the environment of a command run in h's pane.
func in(h core.Handle) func(string) string {
	return func(k string) string {
		if k == "REX_BLOCK" {
			return "block:" + string(h)
		}
		return ""
	}
}

// outsideRex is the environment of a command the user runs outside Rex.
func outsideRex(string) string { return "" }

func handBack(t *testing.T, e Env, by core.Handle, body string, specs ...ArtifactSpec) (store.HandBack, bool, error) {
	t.Helper()
	blobs, err := SnapshotArtifacts(specs)
	if err != nil {
		return store.HandBack{}, false, err
	}
	return e.RecordHandBackWith(context.Background(), Location{Block: "block:" + string(by)}, HandBackRequest{Body: body, Artifacts: blobs})
}

// A design Epic's lead hands back its drafts byte for byte: CRLF and
// non-ASCII bytes, a nested name, each published read-only under the
// hand-back's seq with a manifest of raw-byte SHA-256s, folded into the
// hand-back and the narrator's pulse. The same report and bytes again is
// changed:false with the same seq and pulse; artifact bytes alone changing
// is a new seq that wakes the narrator; an --op-id reused for another
// hand-back is a conflict (exit 5). A council architect it summoned is
// refused. Bad names and bad sources are invalid_input before anything is
// recorded.
func TestHandbackArtifacts(t *testing.T) {
	e, _ := artifactWorld(t)
	// One instant throughout: the pulse tells revisions apart by their
	// digest, never by their time.
	frozen := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return frozen }
	ctx := context.Background()
	spec := []byte("# Spec\r\nCaché ünïcödé — 设计\r\n\r\nno trailing newline")
	goal := []byte("GOAL: one\n")
	specSrc, goalSrc := writeSrc(t, "spec.md", spec), writeSrc(t, "g.md", goal)
	arts := []ArtifactSpec{{Name: "spec.md", Src: specSrc}, {Name: "goals/01-cache.md", Src: goalSrc}}

	if _, _, err := handBack(t, e, "codex-11", "## Report", arts...); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "summoned by claude-10") {
		t.Fatalf("a council architect's hand-back: %v", err)
	}
	if _, ok, _ := e.Store.HandBack("session:d"); ok {
		t.Fatal("a refused hand-back was stored")
	}

	h, changed, err := handBack(t, e, "claude-10", "## Report\nspec drafted", arts...)
	if err != nil || !changed || h.Seq != 1 || len(h.Artifacts) != 2 {
		t.Fatalf("first hand-back %+v %v %v", h, changed, err)
	}
	m, ok, err := e.Store.Manifest("session:d", 1)
	if err != nil || !ok || m.By != "claude-10" || m.ReportDigest != store.HandBackDigest("## Report\nspec drafted") {
		t.Fatalf("manifest %+v %v %v", m, ok, err)
	}
	want := map[string][]byte{"spec.md": spec, "goals/01-cache.md": goal}
	for _, a := range m.Artifacts {
		b := want[a.Name]
		if a.SHA256 != sha(b) || a.Bytes != len(b) {
			t.Errorf("manifest entry %+v, want sha256 %s bytes %d (raw, no newline change)", a, sha(b), len(b))
		}
		path := filepath.Join(e.Paths.ArtifactDir("session:d", 1), filepath.FromSlash(a.Name))
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(b) {
			t.Errorf("stored %s: %q %v", a.Name, got, err)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o444 {
			t.Errorf("%s is %v, want read-only", a.Name, st.Mode().Perm())
		}
	}
	if stored, _, _ := e.Store.HandBack("session:d"); len(stored.Artifacts) != 2 || stored.Digest != h.Digest {
		t.Fatalf("the hand-back record does not carry the manifest: %+v", stored)
	}
	key := core.SessionKey{Server: fakeServer, SessionID: "session:d"}
	p1, _ := e.sessionPulse(key)

	// The same report and bytes: nothing changes, the narrator is not woken.
	again, changed, err := handBack(t, e, "claude-10", "## Report\nspec drafted", arts...)
	if err != nil || changed || again.Seq != 1 || again.Digest != h.Digest {
		t.Fatalf("identical retry %+v %v %v", again, changed, err)
	}
	if p, _ := e.sessionPulse(key); p.Digest() != p1.Digest() {
		t.Fatal("an identical retry changed the pulse")
	}

	// One artifact's bytes change, the report does not: a new seq.
	spec2 := append([]byte(nil), spec...)
	spec2[len(spec2)-1] = '!'
	if err := os.WriteFile(specSrc, spec2, 0o600); err != nil {
		t.Fatal(err)
	}
	rev, changed, err := handBack(t, e, "claude-10", "## Report\nspec drafted", arts...)
	if err != nil || !changed || rev.Seq != 2 || rev.Digest == h.Digest {
		t.Fatalf("artifact-only revision %+v %v %v", rev, changed, err)
	}
	if p, _ := e.sessionPulse(key); p.Digest() == p1.Digest() {
		t.Fatal("an artifact-only revision left the pulse unchanged: the narrator would not wake")
	}
	if m1, _, _ := e.Store.Manifest("session:d", 1); m1.Artifacts[1].SHA256 != sha(spec) {
		t.Fatal("revision 1 changed")
	}

	// An op id names one hand-back: its retry is changed:false, and another
	// hand-back under it is a conflict.
	lead := Location{Block: "block:claude-10"}
	blobs, _ := SnapshotArtifacts(arts)
	op := HandBackRequest{Body: "## Report\nround 3", Artifacts: blobs, OpID: "design:rev-3"}
	h3, changed, err := e.RecordHandBackWith(ctx, lead, op)
	if err != nil || !changed || h3.Seq != 3 {
		t.Fatalf("op hand-back %+v %v %v", h3, changed, err)
	}
	if r, changed, err := e.RecordHandBackWith(ctx, lead, op); err != nil || changed || r.Seq != 3 {
		t.Fatalf("op retry %+v %v %v", r, changed, err)
	}
	op.Body = "## Report\nsomething else"
	_, _, err = e.RecordHandBackWith(ctx, lead, op)
	if f, ok := fault.As(err); !ok || f.Code != fault.Conflict || fault.ExitCode(f.Code) != 5 {
		t.Fatalf("another hand-back under the op id: %v", err)
	}
	if latest, _, _ := e.Store.HandBack("session:d"); latest.Seq != 3 {
		t.Fatalf("the conflict changed the hand-back: %+v", latest)
	}

	// Names and sources refused before anything is recorded.
	dir := t.TempDir()
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(specSrc, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	big := writeSrc(t, "big.md", make([]byte, store.ArtifactMaxBytes+1))
	quarter := writeSrc(t, "q.md", make([]byte, store.ArtifactMaxBytes))
	many := make([]ArtifactSpec, store.ArtifactMaxFiles+1)
	for i := range many {
		many[i] = ArtifactSpec{Name: fmt.Sprintf("a%02d.md", i), Src: goalSrc}
	}
	var overTotal []ArtifactSpec
	for i := 0; i < 5; i++ {
		overTotal = append(overTotal, ArtifactSpec{Name: fmt.Sprintf("q%d.md", i), Src: quarter})
	}
	for name, specs := range map[string][]ArtifactSpec{
		"dot-dot":       {{Name: "../spec.md", Src: goalSrc}},
		"absolute":      {{Name: "/spec.md", Src: goalSrc}},
		"three deep":    {{Name: "a/b/spec.md", Src: goalSrc}},
		"not markdown":  {{Name: "spec.txt", Src: goalSrc}},
		"upper case":    {{Name: "Spec.md", Src: goalSrc}},
		"case dup":      {{Name: "spec.md", Src: goalSrc}, {Name: "spec.md", Src: specSrc}},
		"symlink":       {{Name: "spec.md", Src: link}},
		"directory":     {{Name: "spec.md", Src: dir}},
		"fifo":          {{Name: "spec.md", Src: fifo}},
		"device":        {{Name: "spec.md", Src: "/dev/null"}},
		"over 256 KiB":  {{Name: "spec.md", Src: big}},
		"over 16 files": many,
		"over 1 MiB":    overTotal,
	} {
		_, err := SnapshotArtifacts(specs)
		if f, ok := fault.As(err); !ok || f.Code != fault.InvalidInput {
			t.Errorf("%s: %v, want invalid_input", name, err)
		}
	}
	if ms, _ := e.Store.Manifests("session:d"); len(ms) != 3 {
		t.Fatalf("revisions %d, want 3", len(ms))
	}

	// epic artifacts: the linked personal agent and the user read every
	// revision or one; the Epic's own agents and another Hub's are refused.
	for _, c := range []struct {
		name   string
		getenv func(string) string
		ok     bool
	}{
		{"linked personal", in("claude-01"), true},
		{"the user", outsideRex, true},
		{"the lead", in("claude-10"), false},
		{"a council architect", in("codex-11"), false},
		{"the narrator", in("agy-12"), false},
		{"another Hub's personal", in("claude-09"), false},
	} {
		ms, err := e.Artifacts(ctx, c.getenv, "session:d", 0)
		if c.ok != (err == nil) || c.ok && len(ms) != 3 {
			t.Errorf("%s: %d manifests, %v", c.name, len(ms), err)
		}
		if !c.ok && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want a refusal", c.name, err)
		}
	}
	if ms, err := e.Artifacts(ctx, outsideRex, "session:d", 2); err != nil || len(ms) != 1 || ms[0].Seq != 2 {
		t.Errorf("--seq 2: %+v %v", ms, err)
	}
	if _, err := e.Artifacts(ctx, outsideRex, "session:d", 9); err == nil {
		t.Error("--seq 9 found a revision")
	}
}

// Materialize writes a revision byte for byte: CRLF and non-ASCII bytes as
// stored, nested names, ordinary user-writable files, into a directory it
// creates whole; the receipt is kept in the session's state, not in the
// directory. Stored bytes that no longer match the manifest are
// failed_precondition and nothing is written; a symbolic link in the
// target's path is refused; an agent of the Epic, and another Hub's
// personal agent, are refused.
func TestMaterializeBytes(t *testing.T) {
	e, _ := artifactWorld(t)
	ctx := context.Background()
	spec := []byte("# Spec\r\nCaché — 设计\r\n")
	goal := []byte("GOAL: one\n\n")
	if _, _, err := handBack(t, e, "claude-10", "## Report", ArtifactSpec{Name: "spec.md", Src: writeSrc(t, "s.md", spec)}, ArtifactSpec{Name: "goals/01-x.md", Src: writeSrc(t, "g.md", goal)}); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	into := filepath.Join(base, "design")

	plan, err := e.Materialize(ctx, in("claude-01"), MaterializeRequest{Session: "session:d", Into: into, DryRun: true})
	if err != nil || !plan.DryRun || len(plan.Items) != 2 || plan.Items[0].Status != MatCreated {
		t.Fatalf("dry run %+v %v", plan, err)
	}
	if _, err := os.Stat(into); !os.IsNotExist(err) {
		t.Fatal("the dry run created the directory")
	}

	res, err := e.Materialize(ctx, in("claude-01"), MaterializeRequest{Session: "session:d", Into: into})
	if err != nil || !res.Changed || !res.Atomic || !res.Complete || res.Seq != 1 {
		t.Fatalf("materialize %+v %v", res, err)
	}
	for name, b := range map[string][]byte{"spec.md": spec, "goals/01-x.md": goal} {
		p := filepath.Join(into, filepath.FromSlash(name))
		got, err := os.ReadFile(p)
		if err != nil || sha(got) != sha(b) || string(got) != string(b) {
			t.Errorf("%s: %q %v", name, got, err)
		}
		st, _ := os.Lstat(p)
		if !st.Mode().IsRegular() || st.Mode().Perm()&0o200 == 0 {
			t.Errorf("%s is %v, want a user-writable regular file", name, st.Mode())
		}
	}
	entries, _ := os.ReadDir(into)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") || en.Name() == "materialized.json" {
			t.Errorf("left %s in the target", en.Name())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(base, ".design.tmp-*")); len(left) != 0 {
		t.Errorf("temporary directories left: %v", left)
	}
	if r, err := e.Store.Materialized("session:d"); err != nil || r.Dirs[into].Files["spec.md"] != sha(spec) {
		t.Fatalf("receipt %+v %v", r, err)
	}

	// The user may materialize too; the Epic's own agents and another Hub may not.
	if _, err := e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: filepath.Join(base, "mine")}); err != nil {
		t.Errorf("the user: %v", err)
	}
	for _, h := range []core.Handle{"claude-10", "codex-11", "agy-12", "claude-09"} {
		dst := filepath.Join(base, "by-"+string(h))
		if _, err := e.Materialize(ctx, in(h), MaterializeRequest{Session: "session:d", Into: dst}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want a refusal", h, err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("%s's refused materialize wrote %s", h, dst)
		}
	}

	// A symbolic link an agent planted in the path is refused.
	real := t.TempDir()
	link := filepath.Join(base, "via-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{link, filepath.Join(link, "design")} {
		_, err := e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: dst})
		if f, ok := fault.As(err); !ok || f.Code != fault.InvalidInput {
			t.Errorf("--into %s: %v, want invalid_input", dst, err)
		}
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Errorf("wrote through the link: %v", entries)
	}
	// A symbolic link at a target inside the directory is never followed.
	trap := t.TempDir()
	withLink := filepath.Join(base, "with-link")
	if err := os.MkdirAll(withLink, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(trap, "spec.md"), filepath.Join(withLink, "spec.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: withLink, Replace: true}); err == nil {
		t.Error("materialized over a symbolic link")
	}
	if entries, _ := os.ReadDir(trap); len(entries) != 0 {
		t.Errorf("wrote through a target link: %v", entries)
	}

	// Stored bytes that no longer match the manifest: nothing is written.
	stored := filepath.Join(e.Paths.ArtifactDir("session:d", 1), "spec.md")
	if err := os.Chmod(stored, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(base, "fresh")
	_, err = e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: fresh})
	if f, ok := fault.As(err); !ok || f.Code != fault.FailedPrecondition || fault.ExitCode(f.Code) != 5 {
		t.Fatalf("a corrupt store: %v, want failed_precondition", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Error("a corrupt store wrote the directory")
	}
}

// Into a directory that exists, every target is checked before any is
// written: bytes already there are unchanged (changed:false); a file that
// holds what was materialized last is replaced by a newer revision; a file
// the user changed is a conflict (exit 5) and nothing at all is written,
// unless --replace. The receipt follows each write.
func TestMaterializeConflict(t *testing.T) {
	e, _ := artifactWorld(t)
	ctx := context.Background()
	specSrc, notesSrc := writeSrc(t, "s.md", []byte("v1 spec\n")), writeSrc(t, "n.md", []byte("v1 notes\n"))
	arts := []ArtifactSpec{{Name: "spec.md", Src: specSrc}, {Name: "notes.md", Src: notesSrc}}
	if _, _, err := handBack(t, e, "claude-10", "r1", arts...); err != nil {
		t.Fatal(err)
	}
	into := t.TempDir() // exists already
	req := MaterializeRequest{Session: "session:d", Into: into}
	res, err := e.Materialize(ctx, outsideRex, req)
	if err != nil || !res.Changed || res.Atomic || !res.Complete || res.Items[0].Status != MatCreated {
		t.Fatalf("into an existing directory %+v %v", res, err)
	}
	// Again: nothing changes.
	if res, err := e.Materialize(ctx, outsideRex, req); err != nil || res.Changed || res.Items[0].Status != MatUnchanged || res.Items[1].Status != MatUnchanged {
		t.Fatalf("unchanged %+v %v", res, err)
	}

	// Revision 2: both files still hold revision 1 as written, so both are
	// replaced.
	if err := os.WriteFile(specSrc, []byte("v2 spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notesSrc, []byte("v2 notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handBack(t, e, "claude-10", "r2", arts...); err != nil {
		t.Fatal(err)
	}
	res, err = e.Materialize(ctx, outsideRex, req)
	if err != nil || !res.Changed || res.Seq != 2 || res.Items[0].Status != MatReplaced || res.Items[1].Status != MatReplaced {
		t.Fatalf("revision 2 %+v %v", res, err)
	}

	// The user edits spec.md; revision 3 changes both files. The edit is a
	// conflict, and notes.md, which would be replaced, is not written either.
	edited := []byte("my local edit\n")
	if err := os.WriteFile(filepath.Join(into, "spec.md"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specSrc, []byte("v3 spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notesSrc, []byte("v3 notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := handBack(t, e, "claude-10", "r3", arts...); err != nil {
		t.Fatal(err)
	}
	res, err = e.Materialize(ctx, outsideRex, req)
	if f, ok := fault.As(err); !ok || f.Code != fault.Conflict || fault.ExitCode(f.Code) != 5 || !strings.Contains(f.Message, "spec.md") {
		t.Fatalf("a local edit: %v", err)
	}
	if res.Changed || res.Items[1].Status != MatConflict {
		t.Fatalf("conflict result %+v", res)
	}
	for name, want := range map[string]string{"spec.md": string(edited), "notes.md": "v2 notes\n"} {
		if got, _ := os.ReadFile(filepath.Join(into, name)); string(got) != want {
			t.Errorf("%s after a conflict: %q, want %q", name, got, want)
		}
	}
	// An older revision stays available, and --replace takes revision 3.
	res, err = e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: into, Replace: true})
	if err != nil || !res.Changed || !res.Complete || res.Items[1].Status != MatOverwrite || res.Items[0].Status != MatReplaced {
		t.Fatalf("--replace %+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(into, "spec.md")); string(got) != "v3 spec\n" {
		t.Errorf("spec.md after --replace: %q", got)
	}
	// Materializing revision 1 now: both files hold revision 3 as written,
	// so the older revision replaces them.
	res, err = e.Materialize(ctx, outsideRex, MaterializeRequest{Session: "session:d", Into: into, Seq: 1})
	if err != nil || res.Seq != 1 || res.Items[1].Status != MatReplaced {
		t.Fatalf("--seq 1 %+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(into, "spec.md")); string(got) != "v1 spec\n" {
		t.Errorf("spec.md at --seq 1: %q", got)
	}
}

// epic grant: the Epic's lead and its linked personal agent read the
// notarized grant, the user outside Rex too; a council architect and
// another Hub's personal agent are refused. contract_sha256_ok is checked
// against the contract's bytes now: true while they are as notarized,
// false once they change, and false for a grant that names no contract.
func TestEpicGrantChecksTheContract(t *testing.T) {
	e, _ := artifactWorld(t)
	contract := writeSrc(t, "goal.md", []byte("GOAL: design the cache\n"))
	if err := e.notarize("session:d", "claude-01", epicGrant{asked: "design it", goal: contract, text: "GOAL: design the cache\n"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who core.Handle
		ok  bool
	}{{"claude-10", true}, {"claude-01", true}, {"codex-11", false}, {"claude-09", false}} {
		g, err := e.ReadGrant(Location{Block: "block:" + string(c.who)}, "session:d")
		if c.ok != (err == nil) || c.ok && !g.ContractSHA256OK {
			t.Errorf("%s: %+v %v", c.who, g, err)
		}
	}
	if g, err := e.ReadGrantAsHuman(outsideRex, "session:d"); err != nil || !g.ContractSHA256OK || g.Asked != "design it" {
		t.Fatalf("the user: %+v %v", g, err)
	}
	if _, err := e.ReadGrantAsHuman(func(k string) string {
		if k == "ME_UNATTENDED" {
			return "1"
		}
		return ""
	}, "session:d"); err == nil {
		t.Error("an unattended caller read the grant as the user")
	}
	if err := os.WriteFile(contract, []byte("GOAL: something wider\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g, err := e.ReadGrant(Location{Block: "block:claude-10"}, ""); err != nil || g.ContractSHA256OK || !strings.Contains(g.ContractNote, "changed") {
		t.Errorf("a changed contract: %+v %v", g, err)
	}
	if err := e.notarize("session:w", "claude-01", epicGrant{asked: "just words"}); err != nil {
		t.Fatal(err)
	}
	if g, err := e.ReadGrantAsHuman(outsideRex, "session:w"); err != nil || g.ContractSHA256OK {
		t.Errorf("a grant with no contract: %+v %v", g, err)
	}
}
