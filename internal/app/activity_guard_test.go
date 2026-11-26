package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func programStatusCalls(fset *token.FileSet, f *ast.File) []token.Position {
	var hits []token.Position
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ProgramStatus" {
			hits = append(hits, fset.Position(sel.Sel.Pos()))
		}
		return true
	})
	return hits
}

// Only internal/app/activity.go may call .ProgramStatus( in production code
// across cmd/ and internal/ (internal/rex defines Client.ProgramStatus itself):
// every readiness, health, blocked, close, observe, watchdog, and startup
// check goes through assessActivity.
func TestProgramStatusConfinedToActivityBoundary(t *testing.T) {
	// Prove the guard catches a planted violation first.
	fset := token.NewFileSet()
	planted, err := parser.ParseFile(fset, "planted.go", `package app
func bad(t Terminal) { _, _, _ = t.ProgramStatus(nil, "b") }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hits := programStatusCalls(fset, planted); len(hits) != 1 {
		t.Fatalf("planted violation: found %d hits, want 1", len(hits))
	}

	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(repo, "internal", "app", "activity.go")
	activityCalls := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repo, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			hits := programStatusCalls(fset, f)
			if path == allowed {
				activityCalls += len(hits)
				return nil
			}
			for _, pos := range hits {
				t.Errorf("%s: calls .ProgramStatus( outside internal/app/activity.go; route through assessActivity", pos)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if activityCalls == 0 {
		t.Fatal("internal/app/activity.go has 0 .ProgramStatus( calls; guard may not be matching")
	}
}
