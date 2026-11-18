package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// codex-08's Slice 3 Round-1 reproducers (review-codex-08-r1.txt S1-S3),
// kept as regression tests.

// S3: the same report under a second op id binds that op id too, so the
// second op id with another report is a conflict (exit 5), not a new seq;
// the first op id still answers its own retry.
func TestReviewerOpIDAfterIdenticalHandback(t *testing.T) {
	e, _ := artifactWorld(t)
	loc := Location{Block: "block:claude-10"}
	for i, id := range []string{"op-a", "op-b"} {
		h, changed, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "report one", OpID: id})
		if err != nil || h.Seq != 1 || changed != (i == 0) {
			t.Fatalf("%s: seq=%d changed=%v err=%v", id, h.Seq, changed, err)
		}
	}
	for _, id := range []string{"op-b", "op-a"} {
		h, changed, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "report TWO", OpID: id})
		if f, ok := fault.As(err); !ok || f.Code != fault.Conflict || fault.ExitCode(f.Code) != 5 {
			t.Fatalf("same %s with incompatible report: seq=%d changed=%v err=%v; want conflict", id, h.Seq, changed, err)
		}
	}
	for _, id := range []string{"op-a", "op-b"} {
		if h, changed, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "report one", OpID: id}); err != nil || h.Seq != 1 || changed {
			t.Errorf("retry of %s: seq=%d changed=%v err=%v", id, h.Seq, changed, err)
		}
	}
	// A new report under a new op id is seq 2; the earlier op ids stay
	// bound to seq 1 (an op done, since replaced), never to seq 2.
	if h, changed, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "report TWO", OpID: "op-c"}); err != nil || h.Seq != 2 || !changed {
		t.Fatalf("op-c: seq=%d changed=%v err=%v", h.Seq, changed, err)
	}
	if h, changed, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "report one", OpID: "op-b"}); err != nil || h.Seq != 1 || changed {
		t.Errorf("op-b after seq 2: seq=%d changed=%v err=%v", h.Seq, changed, err)
	}
	lc, _, err := e.Store.EpicLifecycle("session:d")
	if err != nil || len(lc.HandBacks) != 2 {
		t.Fatalf("lifecycle %+v %v", lc, err)
	}
	for _, id := range []string{"op-a", "op-b"} {
		if !lc.HandBacks[0].Accepts(store.ContractDigest([]byte(id))) {
			t.Errorf("seq 1 lost %s: %+v", id, lc.HandBacks[0])
		}
	}
}

type reviewerWaitContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *reviewerWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

// S1: an ancestor of --into swapped for a link to another directory while
// Materialize waits for the lifecycle lock sends nothing there, whether
// --into exists or not.
func TestReviewerMaterializeAncestorSwap(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-dir", true: "existing-dir"}[existing], func(t *testing.T) {
			e, _ := artifactWorld(t)
			if _, _, err := handBack(t, e, "claude-10", "report", ArtifactSpec{Name: "spec.md", Src: writeSrc(t, "s.md", []byte("ARTIFACT"))}); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			parent := filepath.Join(base, "requested-parent")
			outside := filepath.Join(base, "outside")
			for _, p := range []string{parent, outside} {
				if err := os.Mkdir(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			into := filepath.Join(parent, "design")
			if existing {
				for _, p := range []string{into, filepath.Join(outside, "design")} {
					if err := os.Mkdir(p, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			release, err := e.Store.LockEpicLifecycle(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			wait := &reviewerWaitContext{Context: ctx, entered: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				_, err := e.Materialize(wait, in("claude-01"), MaterializeRequest{Session: "session:d", Into: into})
				done <- err
			}()
			select {
			case <-wait.entered:
			case <-ctx.Done():
				release()
				t.Fatal("did not reach lifecycle lock")
			}
			if err := os.Rename(parent, parent+"-original"); err != nil {
				release()
				t.Fatal(err)
			}
			if err := os.Symlink(outside, parent); err != nil {
				release()
				t.Fatal(err)
			}
			release()
			err = <-done
			if got, readErr := os.ReadFile(filepath.Join(outside, "design", "spec.md")); readErr == nil {
				t.Fatalf("wrote outside requested directory via swapped ancestor: contents=%q materializeErr=%v", got, err)
			}
			if f, ok := fault.As(err); !ok || f.Code != fault.InvalidInput {
				t.Errorf("materialize through the swapped ancestor: %v, want invalid_input", err)
			}
		})
	}
}

// S1, past the walk: once --into is pinned, renaming an ancestor and
// planting a link in its place changes nothing; the write lands in the
// directory that was pinned, now under its new name.
func TestPinDirSurvivesAncestorSwap(t *testing.T) {
	base := t.TempDir()
	parent, outside := filepath.Join(base, "requested-parent"), filepath.Join(base, "outside")
	into := filepath.Join(parent, "design")
	for _, p := range []string{into, filepath.Join(outside, "design")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pin, err := pinDir(into)
	if err != nil || pin.dir == nil {
		t.Fatalf("pin %s: %+v %v", into, pin, err)
	}
	defer pin.Close()
	if err := os.Rename(parent, parent+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(pin.dir, "goals/a.md", []byte("PINNED")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(parent+"-original", "design", "goals", "a.md")); err != nil || string(b) != "PINNED" {
		t.Errorf("the pinned directory: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "design")); len(entries) != 0 {
		t.Errorf("wrote through the planted link: %v", entries)
	}
	// A fresh walk sees the planted link and refuses it.
	if _, err := pinDir(into); err == nil {
		t.Error("pinned a path through a planted link")
	}
}

// S2: a batch into an existing directory that stopped part way records
// only what it wrote; a file it failed to write keeps its earlier sha256,
// so once the directory is writable again a plain retry replaces it.
func TestReviewerMaterializePartialReceipt(t *testing.T) {
	e, _ := artifactWorld(t)
	loc := Location{Block: "block:claude-10"}
	record := func(v string) {
		t.Helper()
		_, _, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: v, Artifacts: []store.ArtifactBlob{{Name: "a.md", Data: []byte(v)}, {Name: "goals/b.md", Data: []byte(v)}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	record("v1")
	into := t.TempDir()
	req := MaterializeRequest{Session: "session:d", Into: into}
	if _, err := e.Materialize(t.Context(), in("claude-01"), req); err != nil {
		t.Fatal(err)
	}
	record("v2")
	nested := filepath.Join(into, "goals")
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o755) })
	res, err := e.Materialize(t.Context(), in("claude-01"), req)
	t.Logf("partial result: complete=%v items=%+v err=%v", res.Complete, res.Items, err)
	if err == nil || res.Complete {
		t.Fatal("failure injection did not work")
	}
	r, err := e.Store.Materialized("session:d")
	if err != nil || r.Dirs[into].Files["a.md"] != sha([]byte("v2")) || r.Dirs[into].Files["goals/b.md"] != sha([]byte("v1")) {
		t.Fatalf("receipt after the partial batch: %+v %v; want a.md at v2 and goals/b.md still at v1", r.Dirs[into], err)
	}
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = e.Materialize(t.Context(), in("claude-01"), req)
	if err != nil {
		t.Fatalf("retry after restoring writable directory: items=%+v err=%v; want successful replacement of unchanged v1 file", res.Items, err)
	}
	if res.Items[0].Status != MatUnchanged || res.Items[1].Status != MatReplaced || !res.Complete {
		t.Errorf("retry %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(nested, "b.md")); string(b) != "v2" {
		t.Errorf("goals/b.md after the retry: %q", b)
	}
}

// S2: a file never materialized before that fails to be written is left
// out of the receipt, so the retry creates it.
func TestMaterializePartialReceiptNewFile(t *testing.T) {
	e, _ := artifactWorld(t)
	loc := Location{Block: "block:claude-10"}
	if _, _, err := e.RecordHandBackWith(t.Context(), loc, HandBackRequest{Body: "v1", Artifacts: []store.ArtifactBlob{{Name: "a.md", Data: []byte("v1")}, {Name: "goals/b.md", Data: []byte("v1")}}}); err != nil {
		t.Fatal(err)
	}
	into := t.TempDir()
	nested := filepath.Join(into, "goals")
	if err := os.Mkdir(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o755) })
	req := MaterializeRequest{Session: "session:d", Into: into}
	if res, err := e.Materialize(t.Context(), in("claude-01"), req); err == nil || res.Complete || res.Items[1].Status != MatFailed {
		t.Fatalf("failure injection: %+v %v", res, err)
	}
	r, _ := e.Store.Materialized("session:d")
	if _, ok := r.Dirs[into].Files["goals/b.md"]; ok || r.Dirs[into].Files["a.md"] != sha([]byte("v1")) {
		t.Fatalf("receipt %+v; want a.md only", r.Dirs[into])
	}
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := e.Materialize(t.Context(), in("claude-01"), req)
	if err != nil || res.Items[0].Status != MatUnchanged || res.Items[1].Status != MatCreated {
		t.Fatalf("retry %+v %v", res, err)
	}
}
