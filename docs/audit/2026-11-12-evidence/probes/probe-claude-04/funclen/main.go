package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fn struct {
	name string
	pos  string
	n    int
}

func main() {
	root := os.Args[1]
	var fns []fn
	fset := token.NewFileSet()
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if strings.Contains(p, "/.goal/") || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			s := fset.Position(fd.Pos())
			e := fset.Position(fd.End())
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				name = fmt.Sprintf("(%s).%s", exprStr(fd.Recv.List[0].Type), name)
			}
			rel, _ := filepath.Rel(root, p)
			fns = append(fns, fn{name, fmt.Sprintf("%s:%d", rel, s.Line), e.Line - s.Line + 1})
		}
		return nil
	})
	sort.Slice(fns, func(i, j int) bool { return fns[i].n > fns[j].n })
	over := map[int]int{}
	for _, f := range fns {
		for _, t := range []int{60, 100, 150} {
			if f.n > t {
				over[t]++
			}
		}
	}
	fmt.Printf("funcs=%d >60=%d >100=%d >150=%d\n", len(fns), over[60], over[100], over[150])
	for i := 0; i < 25 && i < len(fns); i++ {
		fmt.Printf("%4d %s %s\n", fns[i].n, fns[i].pos, fns[i].name)
	}
}

func exprStr(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + exprStr(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
