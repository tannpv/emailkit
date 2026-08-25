package emailkit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestChokepoint_DeliverIsSoleSender parses this package and fails if any
// function other than deliver calls Sender.Send. Every send must pass the
// suppression check in deliver; this asserts no second path exists.
func TestChokepoint_DeliverIsSoleSender(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var offenders []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Send" {
						return true
					}
					// s.client.Send(...) — the provider call we are guarding.
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "client" {
						return true
					}
					if fn.Name.Name != "deliver" {
						offenders = append(offenders,
							fn.Name.Name+" in "+path)
					}
					return true
				})
				return false
			})
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("only deliver() may call the Sender — found: %v.\n"+
			"Every send must pass the suppression check. If you need a new send "+
			"path, route it through deliver rather than adding a second caller.",
			offenders)
	}
}
