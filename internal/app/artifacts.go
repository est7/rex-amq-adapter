package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A design Epic's drafts travel byte for byte: the lead's files are
// snapshot (SnapshotArtifacts) and published with its hand-back
// (RecordHandBackWith); the user's personal agent, or the user, lists them
// (Artifacts) and writes them into a directory of theirs (Materialize).
// Nobody else reads them through the adapter: the Epic's own agents work
// from their files, and another Hub's agents are not linked.

// ArtifactSpec is one --artifact REL=@SRC: its name, and the file whose
// bytes it carries.
type ArtifactSpec struct {
	Name, Src string
}

// ParseArtifact splits REL=@SRC.
func ParseArtifact(v string) (ArtifactSpec, error) {
	rel, src, ok := strings.Cut(v, "=@")
	if !ok || rel == "" || src == "" {
		return ArtifactSpec{}, fault.New(fault.InvalidInput, "--artifact %q is not REL=@SRC", v)
	}
	return ArtifactSpec{Name: rel, Src: src}, nil
}

// SnapshotArtifacts reads each spec's source once, whole, before anything
// is recorded: a regular file opened without following a symbolic link
// (O_NOFOLLOW, and an O_NONBLOCK open so a FIFO never blocks it), its bytes
// taken as they are. Names are checked again here, the registry's rule
// (cli.CheckArtifactName) plus no two names that differ only in case. A
// symbolic link, a directory, a device, a FIFO, more than
// store.ArtifactMaxFiles files, a file over store.ArtifactMaxBytes, or more
// than store.ArtifactMaxTotal bytes in all is invalid_input, and nothing is
// recorded.
func SnapshotArtifacts(specs []ArtifactSpec) ([]store.ArtifactBlob, error) {
	if len(specs) > store.ArtifactMaxFiles {
		return nil, fault.New(fault.InvalidInput, "a hand-back carries at most %d artifacts, and this one names %d", store.ArtifactMaxFiles, len(specs))
	}
	seen := map[string]string{}
	var out []store.ArtifactBlob
	total := 0
	for _, s := range specs {
		if err := validArtifactName(s.Name); err != nil {
			return nil, err
		}
		folded := strings.ToLower(s.Name)
		if prev, dup := seen[folded]; dup {
			return nil, fault.New(fault.InvalidInput, "artifact names %q and %q are the same name", prev, s.Name)
		}
		seen[folded] = s.Name
		b, err := snapshotFile(s.Src)
		if err != nil {
			return nil, err
		}
		if total += len(b); total > store.ArtifactMaxTotal {
			return nil, fault.New(fault.InvalidInput, "the artifacts come to more than %d bytes in all", store.ArtifactMaxTotal)
		}
		out = append(out, store.ArtifactBlob{Name: s.Name, Data: b})
	}
	return out, nil
}

// validArtifactName is the registry's rule for an artifact's name, kept
// here too since the app is called without the registry: one or two
// segments of [a-z0-9][a-z0-9._-]*, ending in .md, never "..".
func validArtifactName(rel string) error {
	segs := strings.Split(rel, "/")
	ok := strings.HasSuffix(rel, ".md") && !strings.HasPrefix(rel, "/") && len(segs) <= 2
	for _, s := range segs {
		ok = ok && s != "" && !strings.Contains(s, "..") && artifactSegment(s)
	}
	if !ok {
		return fault.New(fault.InvalidInput, "artifact name %q is not one or two segments of [a-z0-9][a-z0-9._-]* ending in .md", rel)
	}
	return nil
}

func artifactSegment(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// snapshotFile reads path, a regular file reached without following a
// symbolic link at its last component, whole and once, at most
// store.ArtifactMaxBytes.
func snapshotFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fault.New(fault.InvalidInput, "artifact source %s is a symbolic link; name the file itself", path)
		}
		return nil, fault.New(fault.InvalidInput, "artifact source %s cannot be read: %v", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput, "artifact source %s is not a regular file (%s)", path, st.Mode().Type())
	}
	if st.Size() > store.ArtifactMaxBytes {
		return nil, fault.New(fault.InvalidInput, "artifact source %s is %d bytes, over the %d one artifact may be", path, st.Size(), store.ArtifactMaxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, store.ArtifactMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > store.ArtifactMaxBytes {
		return nil, fault.New(fault.InvalidInput, "artifact source %s grew past %d bytes while it was read", path, store.ArtifactMaxBytes)
	}
	return b, nil
}

// artifactReader authorizes a read or materialization of session's
// artifacts: from an agent's pane or harness session, only a personal agent
// linked to the Epic (core.LinkMatchesPersonal); from no agent's (Caller
// finds none), only the human (HumanGate), however the environment was
// trimmed.
func (e Env) artifactReader(ctx context.Context, getenv func(string) string, session string) error {
	loc, err := e.Caller(ctx, getenv)
	switch {
	case errors.Is(err, ErrNotInRex):
		return e.HumanGate(getenv)
	case err != nil:
		return err
	}
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return err
	}
	if !ok || !rec.Live() {
		return e.HumanGate(getenv)
	}
	if rec.Role != core.PersonalRole {
		return fmt.Errorf("%w: an Epic's artifacts are for the personal agent of a Hub it is linked to, or the user; %s is %s", ErrRefused, rec.Handle, whoIs(rec))
	}
	if linked, err := e.personalLinked(rec, session); err != nil || linked {
		return err
	}
	return refused(fmt.Sprintf("%s reading the artifacts of %s", rec.Handle, session), core.Decision{Reason: core.ReasonNotLinked})
}

// Artifacts returns the manifests of session's artifacts, for its linked
// personal agent or the user (artifactReader): every revision, or with seq
// only that one. Nothing is written.
func (e Env) Artifacts(ctx context.Context, getenv func(string) string, session string, seq int) ([]store.Manifest, error) {
	if err := e.artifactReader(ctx, getenv, session); err != nil {
		return nil, err
	}
	if seq > 0 {
		m, ok, err := e.Store.Manifest(session, seq)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fault.New(fault.NotFound, "Epic session %s has no artifacts %d", session, seq).With("kind", "artifact")
		}
		return []store.Manifest{m}, nil
	}
	ms, err := e.Store.Manifests(session)
	if err != nil {
		return nil, err
	}
	if ms == nil {
		ms = []store.Manifest{}
	}
	return ms, nil
}

// MaterializeRequest is `epic materialize`: write revision Seq (0: the
// latest) of Session's artifacts into Into. Replace overwrites files that
// changed since they were last written there; DryRun reports the plan.
type MaterializeRequest struct {
	Session, Into string
	Seq           int
	Replace       bool
	DryRun        bool
}

// Materialized statuses of one file.
const (
	MatCreated   = "created"   // a new file
	MatUnchanged = "unchanged" // the file already holds these bytes
	MatReplaced  = "replaced"  // it held what was materialized last, now updated
	MatOverwrite = "overwritten"
	MatConflict  = "conflict" // it changed since; not written
	MatFailed    = "failed"
)

// MaterializedItem is what happened to one artifact.
type MaterializedItem struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// MaterializeResult is a materialization: where, which revision, and each
// file. Atomic says the directory was created whole (it did not exist);
// Complete is false when a batch into an existing directory stopped part
// way.
type MaterializeResult struct {
	SessionID string             `json:"session_id"`
	Seq       int                `json:"seq"`
	Into      string             `json:"into"`
	Changed   bool               `json:"changed"`
	Atomic    bool               `json:"atomic"`
	Complete  bool               `json:"complete"`
	DryRun    bool               `json:"dry_run,omitempty"`
	Items     []MaterializedItem `json:"items"`
}

// Materialize writes revision r.Seq of r.Session's artifacts into r.Into,
// for its linked personal agent or the user (artifactReader). Every stored
// artifact is checked against its manifest's sha256 first (a mismatch is
// failed_precondition, and nothing is written). r.Into, and every directory
// above it, may not be a symbolic link an agent could have planted (only a
// root-owned system link such as /var or /tmp passes), and nothing at or
// under it is followed.
//
// A directory that does not exist is written whole: every file goes into a
// temporary directory beside it on the same filesystem, is checked again,
// and the directory is renamed into place. Into one that exists, every
// target is checked before any is written: a file already holding the
// bytes is unchanged; one holding what this command last wrote there
// (sessions/<id>/materialized.json) is replaced; anything else is a
// conflict, and with any conflict nothing is written, unless r.Replace.
// Then each file is written atomically and checked again; this batch is not
// atomic as a whole, so one that stops part way reports Complete false.
// Files are written 0644, never the store's 0444.
func (e Env) Materialize(ctx context.Context, getenv func(string) string, r MaterializeRequest) (MaterializeResult, error) {
	res := MaterializeResult{SessionID: r.Session, DryRun: r.DryRun, Items: []MaterializedItem{}}
	if err := e.artifactReader(ctx, getenv, r.Session); err != nil {
		return res, err
	}
	into, err := filepath.Abs(r.Into)
	if err != nil {
		return res, fault.New(fault.InvalidInput, "--into %s: %v", r.Into, err)
	}
	res.Into = filepath.Clean(into)
	m, err := e.manifestFor(r.Session, r.Seq)
	if err != nil {
		return res, err
	}
	res.Seq = m.Seq
	blobs := make([][]byte, len(m.Artifacts))
	for i, a := range m.Artifacts {
		if blobs[i], err = e.Store.ArtifactBytes(r.Session, m.Seq, a); err != nil {
			var bad *store.CorruptArtifact
			if errors.As(err, &bad) {
				return res, fault.New(fault.FailedPrecondition, "%v; nothing was written", err).With("reason", "artifact_corrupt")
			}
			return res, fault.New(fault.FailedPrecondition, "%v; nothing was written", err).With("reason", "artifact_missing")
		}
	}
	if err := noPlantedLink(res.Into); err != nil {
		return res, err
	}
	unlock, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	receipt, err := e.Store.Materialized(r.Session)
	if err != nil {
		return res, err
	}
	st, err := os.Lstat(res.Into)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.Atomic = true
		err = e.materializeNew(&res, m, blobs, receipt)
		return res, err
	case err != nil:
		return res, err
	case !st.IsDir():
		return res, fault.New(fault.InvalidInput, "--into %s is not a directory", res.Into)
	}
	err = e.materializeInto(&res, m, blobs, receipt, r.Replace)
	return res, err
}

// manifestFor is revision seq of session's artifacts, the latest for 0.
func (e Env) manifestFor(session string, seq int) (store.Manifest, error) {
	if seq > 0 {
		m, ok, err := e.Store.Manifest(session, seq)
		if err != nil {
			return m, err
		}
		if !ok {
			return m, fault.New(fault.NotFound, "Epic session %s has no artifacts %d", session, seq).With("kind", "artifact")
		}
		return m, nil
	}
	ms, err := e.Store.Manifests(session)
	if err != nil {
		return store.Manifest{}, err
	}
	if len(ms) == 0 {
		return store.Manifest{}, fault.New(fault.NotFound, "Epic session %s has no artifacts to materialize", session).With("kind", "artifact")
	}
	return ms[len(ms)-1], nil
}

// noPlantedLink refuses dir when it, or a directory above it, is a symbolic
// link not owned by root: one an agent could have planted to send the
// files elsewhere. Root-owned links are the system's (/var, /tmp, /etc on
// macOS).
func noPlantedLink(dir string) error {
	for p := dir; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err == nil && st.Mode()&fs.ModeSymlink != 0 {
			if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != 0 {
				return fault.New(fault.InvalidInput, "--into %s goes through the symbolic link %s; name the real directory", dir, p)
			}
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// materializeNew writes every artifact into a temporary directory beside
// res.Into, checks them, and renames it into place.
func (e Env) materializeNew(res *MaterializeResult, m store.Manifest, blobs [][]byte, receipt store.Materialized) error {
	for _, a := range m.Artifacts {
		res.Items = append(res.Items, MaterializedItem{Name: a.Name, SHA256: a.SHA256, Bytes: a.Bytes, Status: MatCreated})
	}
	res.Changed, res.Complete = true, true
	if res.DryRun {
		return nil
	}
	parent := filepath.Dir(res.Into)
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(res.Into)+".tmp-")
	if err != nil {
		return fmt.Errorf("make a directory beside %s: %w", res.Into, err)
	}
	defer os.RemoveAll(tmp)
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer root.Close()
	for i, a := range m.Artifacts {
		if err := writeIn(root, a.Name, blobs[i]); err != nil {
			return err
		}
	}
	if err := store.VerifyRevision(tmp, m.Artifacts); err != nil {
		return err
	}
	if err := os.Rename(tmp, res.Into); err != nil {
		return fmt.Errorf("move the artifacts into %s: %w", res.Into, err)
	}
	return e.recordMaterialized(receipt, res.Into, m)
}

// materializeInto writes into the existing directory res.Into, after a
// check of every target.
func (e Env) materializeInto(res *MaterializeResult, m store.Manifest, blobs [][]byte, receipt store.Materialized, replace bool) error {
	root, err := os.OpenRoot(res.Into)
	if err != nil {
		return err
	}
	defer root.Close()
	last := receipt.Dirs[res.Into]
	var conflicts []string
	for _, a := range m.Artifacts {
		item := MaterializedItem{Name: a.Name, SHA256: a.SHA256, Bytes: a.Bytes}
		cur, exists, err := readIn(root, a.Name)
		switch {
		case err != nil:
			return err
		case !exists:
			item.Status = MatCreated
		case store.RawSHA256(cur) == a.SHA256:
			item.Status = MatUnchanged
		case last.Files[a.Name] != "" && store.RawSHA256(cur) == last.Files[a.Name]:
			item.Status = MatReplaced
		case replace:
			item.Status = MatOverwrite
		default:
			item.Status = MatConflict
			conflicts = append(conflicts, a.Name)
		}
		res.Items = append(res.Items, item)
	}
	res.Complete = true
	for _, it := range res.Items {
		res.Changed = res.Changed || it.Status != MatUnchanged
	}
	if len(conflicts) > 0 {
		res.Changed = false
		return fault.New(fault.Conflict, "%s in %s changed since the artifacts were last written there; nothing was written. Pass --replace to overwrite them", strings.Join(conflicts, ", "), res.Into).
			With("reason", "changed_locally")
	}
	if res.DryRun || !res.Changed {
		return nil
	}
	for i := range res.Items {
		it := &res.Items[i]
		if it.Status == MatUnchanged {
			continue
		}
		j := slices.IndexFunc(m.Artifacts, func(a store.ArtifactEntry) bool { return a.Name == it.Name })
		err := writeIn(root, it.Name, blobs[j])
		if err == nil {
			var got []byte
			if got, _, err = readIn(root, it.Name); err == nil && store.RawSHA256(got) != it.SHA256 {
				err = fmt.Errorf("%s was not written as read", it.Name)
			}
		}
		if err != nil {
			it.Status, it.Error, res.Complete = MatFailed, err.Error(), false
		}
	}
	if err := e.recordMaterialized(receipt, res.Into, m); err != nil {
		return err
	}
	if !res.Complete {
		return fault.New(fault.Internal, "some artifacts could not be written into %s; see each item", res.Into)
	}
	return nil
}

// recordMaterialized keeps what was written into dir: m's files and their
// sha256.
func (e Env) recordMaterialized(receipt store.Materialized, dir string, m store.Manifest) error {
	files := map[string]string{}
	for _, a := range m.Artifacts {
		files[a.Name] = a.SHA256
	}
	receipt.Dirs[dir] = store.MaterializedBatch{Seq: m.Seq, At: e.Now().UTC(), Files: files}
	return e.Store.PutMaterialized(receipt)
}

// readIn reads name under root, never following a symbolic link: os.Root
// refuses one that leaves it, and a link at the target or at its directory
// is refused here.
func readIn(root *os.Root, name string) ([]byte, bool, error) {
	if dir := filepath.Dir(filepath.FromSlash(name)); dir != "." {
		st, err := root.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, false, nil
		case err != nil:
			return nil, false, fault.New(fault.InvalidInput, "%s: %v", dir, err)
		case !st.IsDir():
			return nil, false, fault.New(fault.InvalidInput, "%s in the target directory is not a directory (%s); nothing was written", dir, st.Mode().Type())
		}
	}
	st, err := root.Lstat(filepath.FromSlash(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fault.New(fault.InvalidInput, "%s: %v", name, err)
	}
	if !st.Mode().IsRegular() {
		return nil, false, fault.New(fault.InvalidInput, "%s in the target directory is not a regular file (%s); nothing was written", name, st.Mode().Type())
	}
	b, err := root.ReadFile(filepath.FromSlash(name))
	return b, true, err
}

// writeIn replaces name under root with data, atomically, 0644.
func writeIn(root *os.Root, name string, data []byte) error {
	rel := filepath.FromSlash(name)
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(rel), ".tmp-"+token+"-"+filepath.Base(rel))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Chmod(tmp, 0o644); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
