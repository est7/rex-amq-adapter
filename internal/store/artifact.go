package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A design Epic's lead hands back drafts byte for byte beside its report
// (`epic handback --artifact REL=@SRC`). Each hand-back that carries them
// publishes one revision, read-only and never rewritten:
//
//   - sessions/<session_id>/artifacts/<seq>/<rel>: each artifact's bytes as
//     the lead's file held them, 0444.
//   - sessions/<session_id>/artifacts/<seq>/manifest.json: the Manifest.
//
// A revision is written in a temporary directory beside the others and
// renamed into place whole, so a reader sees all of it or none of it.
// `epic materialize` writes a revision into a directory of the user's and
// keeps what it wrote in sessions/<session_id>/materialized.json.

// Artifact limits: how many files one hand-back carries, and how large each
// one and all of them together may be.
const (
	ArtifactMaxFiles = 16
	ArtifactMaxBytes = 256 << 10
	ArtifactMaxTotal = 1 << 20
)

// ArtifactEntry is one artifact of a revision: its name under the revision
// (one or two segments, ending in .md), and the SHA-256 and length of its
// raw bytes.
type ArtifactEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// Manifest is one revision of a session's artifacts: the hand-back Seq that
// published it, who did and when, the digest of the report it came with
// (HandBackDigest), and its artifacts sorted by name.
type Manifest struct {
	SessionID    string          `json:"session_id"`
	Seq          int             `json:"seq"`
	By           core.Handle     `json:"by"`
	At           time.Time       `json:"at"`
	ReportDigest string          `json:"report_digest"`
	Artifacts    []ArtifactEntry `json:"artifacts"`
}

// ArtifactBlob is an artifact's bytes, snapshot before the hand-back.
type ArtifactBlob struct {
	Name string
	Data []byte
}

// RawSHA256 is the hex SHA-256 of b as it is: no line ending is changed.
func RawSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Entries are blobs' manifest entries, sorted by name; nil for none.
func Entries(blobs []ArtifactBlob) []ArtifactEntry {
	if len(blobs) == 0 {
		return nil
	}
	out := make([]ArtifactEntry, len(blobs))
	for i, b := range blobs {
		out[i] = ArtifactEntry{Name: b.Name, SHA256: RawSHA256(b.Data), Bytes: len(b.Data)}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HandBackWithArtifacts is the digest of a hand-back that carries
// artifacts: its report's digest and every artifact's name, SHA-256, and
// length. Changing any artifact's bytes changes it, so an artifact-only
// revision is a new hand-back. A hand-back with none keeps reportDigest.
func HandBackWithArtifacts(reportDigest string, entries []ArtifactEntry) string {
	if len(entries) == 0 {
		return reportDigest
	}
	var b strings.Builder
	b.WriteString(reportDigest + "\n")
	for _, a := range entries {
		fmt.Fprintf(&b, "%s %s %d\n", a.Name, a.SHA256, a.Bytes)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p Paths) artifactsDir(id string) string { return filepath.Join(p.SessionDir(id), "artifacts") }

// ArtifactDir is where revision seq of session id's artifacts is kept.
func (p Paths) ArtifactDir(id string, seq int) string {
	return filepath.Join(p.artifactsDir(id), strconv.Itoa(seq))
}

func (p Paths) materializedPath(id string) string {
	return filepath.Join(p.SessionDir(id), "materialized.json")
}

// PutArtifacts publishes m with blobs as revision m.Seq of its session,
// whole: the files and manifest are written read-only in a temporary
// directory, checked against m, and renamed into place. A revision already
// published with the same manifest is left as it is; one with another is
// an error, never replaced.
func (s Store) PutArtifacts(m Manifest, blobs []ArtifactBlob) error {
	if m.SessionID == "" || m.Seq < 1 || len(blobs) == 0 || len(blobs) != len(m.Artifacts) {
		return errors.New("a revision of artifacts needs its session, its seq, and its files")
	}
	dst := s.P.ArtifactDir(m.SessionID, m.Seq)
	if kept, ok, err := s.Manifest(m.SessionID, m.Seq); err != nil {
		return err
	} else if ok {
		if sameEntries(kept.Artifacts, m.Artifacts) && kept.ReportDigest == m.ReportDigest {
			return nil
		}
		return fmt.Errorf("artifacts %d of %s are already published with other content, and are never replaced", m.Seq, m.SessionID)
	}
	parent := s.P.artifactsDir(m.SessionID)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = removeReadOnly(tmp) }()
	for _, b := range blobs {
		if err := writeReadOnly(filepath.Join(tmp, filepath.FromSlash(b.Name)), b.Data); err != nil {
			return err
		}
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeReadOnly(filepath.Join(tmp, "manifest.json"), append(mb, '\n')); err != nil {
		return err
	}
	// What is about to be published is what the manifest says.
	if err := VerifyRevision(tmp, m.Artifacts); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, ok, rerr := s.Manifest(m.SessionID, m.Seq); rerr == nil && ok {
			return s.PutArtifacts(m, blobs) // another writer published it
		}
		return err
	}
	return nil
}

// Manifest reads revision seq of session id's artifacts; ok is false when
// none was published.
func (s Store) Manifest(id string, seq int) (Manifest, bool, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(s.P.ArtifactDir(id, seq), "manifest.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("decode the manifest of artifacts %d of %s: %w", seq, id, err)
	}
	if m.Seq != seq || m.SessionID != id {
		return m, false, fmt.Errorf("the manifest of artifacts %d of %s names %s %d", seq, id, m.SessionID, m.Seq)
	}
	return m, true, nil
}

// Manifests reads every published revision of session id's artifacts, by
// seq. A revision whose manifest cannot be read is an error, never skipped.
func (s Store) Manifests(id string) ([]Manifest, error) {
	entries, err := os.ReadDir(s.P.artifactsDir(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		seq, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() || seq < 1 {
			continue // a temporary directory, or nothing of ours
		}
		m, ok, err := s.Manifest(id, seq)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// ArtifactBytes reads one artifact of revision seq of session id and checks
// it against its manifest entry: a file that is missing, of another length,
// or of another SHA-256 is an error.
func (s Store) ArtifactBytes(id string, seq int, a ArtifactEntry) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.P.ArtifactDir(id, seq), filepath.FromSlash(a.Name)))
	if err != nil {
		return nil, fmt.Errorf("artifact %s of revision %d: %w", a.Name, seq, err)
	}
	if len(b) != a.Bytes || RawSHA256(b) != a.SHA256 {
		return nil, &CorruptArtifact{Name: a.Name, Seq: seq}
	}
	return b, nil
}

// CorruptArtifact is a stored artifact whose bytes no longer match its
// manifest.
type CorruptArtifact struct {
	Name string
	Seq  int
}

func (c *CorruptArtifact) Error() string {
	return fmt.Sprintf("artifact %s of revision %d does not match its manifest's sha256", c.Name, c.Seq)
}

// VerifyRevision checks every artifact under dir against entries.
func VerifyRevision(dir string, entries []ArtifactEntry) error {
	for _, a := range entries {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(a.Name)))
		if err != nil {
			return err
		}
		if len(b) != a.Bytes || RawSHA256(b) != a.SHA256 {
			return fmt.Errorf("artifact %s was not written as read", a.Name)
		}
	}
	return nil
}

func sameEntries(a, b []ArtifactEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeReadOnly creates path (and its directory) holding data, 0444.
func writeReadOnly(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o444)
}

// removeReadOnly removes dir, whose files may be read-only; a directory
// already gone is not an error.
func removeReadOnly(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// Materialized is what `epic materialize` last wrote of a session's
// artifacts into each directory: by directory, each file's name and the
// SHA-256 it wrote, so a later run tells a file the user changed from one
// it wrote itself. It is kept beside the session's state, never in the
// directory.
type Materialized struct {
	SessionID string                       `json:"session_id"`
	Dirs      map[string]MaterializedBatch `json:"dirs"`
}

// MaterializedBatch is one materialization into a directory.
type MaterializedBatch struct {
	Seq   int               `json:"seq"`
	At    time.Time         `json:"at"`
	Files map[string]string `json:"files"` // name -> sha256 written
}

// Materialized reads session id's materialization receipt; one never
// written is empty.
func (s Store) Materialized(id string) (Materialized, error) {
	m := Materialized{SessionID: id, Dirs: map[string]MaterializedBatch{}}
	b, err := os.ReadFile(s.P.materializedPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("decode the materialization receipt of %s: %w", id, err)
	}
	if m.Dirs == nil {
		m.Dirs = map[string]MaterializedBatch{}
	}
	return m, nil
}

// PutMaterialized records m whole.
func (s Store) PutMaterialized(m Materialized) error {
	if m.SessionID == "" {
		return errors.New("a materialization receipt needs its session")
	}
	return writeJSON(s.P.materializedPath(m.SessionID), m)
}
