package out

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stdoutAllowed is where this package may name the process's stdout: OS,
// the one place the CLI's streams are built. The four protocol writers
// (HookReply, PaneTitle, InjectProgress, WatchFailed) write through the
// Streams they are given, so they need no exception.
var stdoutAllowed = map[string]bool{"OS": true}

// violation is one way a file writes the process's stdout directly.
type violation struct {
	pos  token.Position
	what string
}

// stdoutWriters lists every direct stdout write in one parsed file:
// fmt.Print*, the print builtins, os.Stdout (encoders and exec.Cmd.Stdout
// bound to it included), syscall.Stdout, and os.NewFile on fd 1. A function
// of this package named in allowed may name os.Stdout.
func stdoutWriters(fset *token.FileSet, f *ast.File, allowed map[string]bool) []violation {
	var out []violation
	add := func(n ast.Node, what string) { out = append(out, violation{fset.Position(n.Pos()), what}) }
	for _, decl := range f.Decls {
		fn, _ := decl.(*ast.FuncDecl)
		exempt := fn != nil && fn.Recv == nil && allowed[fn.Name.Name]
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				pkg, ok := x.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case pkg.Name == "fmt" && (x.Sel.Name == "Print" || x.Sel.Name == "Printf" || x.Sel.Name == "Println"):
					add(x, "fmt."+x.Sel.Name)
				case pkg.Name == "os" && x.Sel.Name == "Stdout" && !exempt:
					add(x, "os.Stdout")
				case pkg.Name == "syscall" && x.Sel.Name == "Stdout":
					add(x, "syscall.Stdout")
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
					add(x, id.Name)
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) > 0 {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && sel.Sel.Name == "NewFile" {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Value == "1" {
							add(x, "os.NewFile(1)")
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// Only this package writes the process's stdout (§3.3): every other file
// of cmd/ and internal/ gets its streams from out.OS() through a Printer.
// The check is proven on a planted fixture that holds one of each kind of
// violation, so a checker that finds nothing fails here first.
func TestStdoutSingleWriter(t *testing.T) {
	fset := token.NewFileSet()
	planted, err := parser.ParseFile(fset, "testdata/violation.go.txt", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, v := range stdoutWriters(fset, planted, nil) {
		found[v.what]++
	}
	for what, n := range map[string]int{"fmt.Println": 1, "fmt.Printf": 1, "fmt.Print": 1, "println": 1, "print": 1, "os.Stdout": 3, "os.NewFile(1)": 1, "syscall.Stdout": 1} {
		if found[what] != n {
			t.Errorf("planted fixture: found %d of %s, want %d (all: %v)", found[what], what, n, found)
		}
	}

	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
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
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			scanned++
			var allowed map[string]bool
			if filepath.Dir(path) == filepath.Join(repo, "internal", "out") {
				allowed = stdoutAllowed
			}
			for _, v := range stdoutWriters(fset, f, allowed) {
				t.Errorf("%s: %s writes stdout outside internal/out; print through the command's out.Printer", v.pos, v.what)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned %d files: the walk is not reaching the source", scanned)
	}
}
