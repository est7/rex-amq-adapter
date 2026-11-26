package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
// under internal/app: every other readiness, health, blocked, close, observe,
// watchdog, and startup check goes through assessActivity.
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

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	activityCalls := 0
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(".", name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		hits := programStatusCalls(fset, f)
		if name == "activity.go" {
			activityCalls += len(hits)
			continue
		}
		for _, pos := range hits {
			t.Errorf("%s: calls .ProgramStatus( outside internal/app/activity.go; route through assessActivity", pos)
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files in internal/app", scanned)
	}
	if activityCalls == 0 {
		t.Fatal("internal/app/activity.go has 0 .ProgramStatus( calls; guard may not be matching")
	}
}
