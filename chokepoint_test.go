package emailkit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestChokepoint_DeliverIsSoleSender parses this package and fails unless
// EXACTLY ONE call site reaches <expr>.client.Send, and that call site is
// inside (*Service).deliver. Every send must pass the suppression check in
// deliver; this asserts no second path exists.
//
// SYNTACTIC, NOT SEMANTIC: this walks source text patterns, not typed
// program semantics — it has no idea what a "Sender" or a "client" field
// actually is beyond the identifiers used to spell them. Evasions this does
// NOT catch:
//   - a local copy: `c := s.client; c.Send(...)` — the call site is `c.Send`,
//     not `<expr>.client.Send`, so the pattern never matches it.
//   - a method value: `f := s.client.Send; f(...)` — the call site is `f(...)`,
//     a plain *ast.Ident, not a selector at all.
//   - a method expression: `Sender.Send(s.client, ...)` — `Send` is selected
//     off the type `Sender`, not off `s.client`.
//   - a package-level `var` holding a func literal that itself calls
//     `s.client.Send` — the walk below only descends into *ast.FuncDecl
//     bodies, so a send hidden inside a var initializer's closure is never
//     inspected at all.
//
// What this IS good for: catching the realistic regression, where someone
// later adds a second, more convenient send path on *Service — a helper
// method, a "just this once" shortcut — because routing everything through
// deliver felt like friction. That is the failure mode this guard exists to
// catch, and it catches it as a build failure rather than a review comment.
// It is not a proof that no send can ever bypass deliver; a determined
// rewrite of the field/method names, or one of the evasions above, defeats
// it. Closing those would need type information (go/types or
// golang.org/x/tools/go/analysis), and the latter is a non-stdlib dependency
// this module forbids.
func TestChokepoint_DeliverIsSoleSender(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var matches []sendMatch
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				isDeliver := fn.Name.Name == "deliver" && isServicePtrReceiver(fn.Recv)
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Send" {
						return true
					}
					// <expr>.client.Send(...) — the provider call we are guarding.
					inner, ok := sel.X.(*ast.SelectorExpr)
					if !ok || inner.Sel.Name != "client" {
						return true
					}
					matches = append(matches, sendMatch{
						desc:      funcDesc(fn) + " in " + path,
						isDeliver: isDeliver,
					})
					return true
				})
				return false
			})
		}
	}

	// Zero matches is not "nothing to report" — it means the guard has lost
	// track of the very call it exists to police, most likely because the
	// `client` field or the `Send` method was renamed and this guard's
	// literal string matches ("client", "Send") were not updated to match.
	// A guard that silently matches nothing still passes green forever,
	// which is worse than no guard: it keeps advertising protection it no
	// longer provides. Whoever renamed the field/method must update this
	// guard's matching strings in the same change.
	if len(matches) == 0 {
		t.Fatalf("chokepoint guard matched zero call sites for <expr>.client.Send " +
			"(want exactly 1, in (*Service).deliver). The guard has lost track of " +
			"the send call — most likely a field or method it matches on " +
			"(\"client\" or \"Send\") was renamed without updating this test. " +
			"Whoever made that rename must update the guard's match to find the " +
			"call again; do not leave this passing on zero matches.")
	}

	var offenders []string
	for _, m := range matches {
		if !m.isDeliver {
			offenders = append(offenders, m.desc)
		}
	}
	if len(matches) != 1 || len(offenders) > 0 {
		t.Fatalf("expected exactly 1 call to <expr>.client.Send, in "+
			"(*Service).deliver; found %d total, offenders (not in "+
			"(*Service).deliver): %v.\n"+
			"Every send must pass the suppression check. If you need a new send "+
			"path, route it through deliver rather than adding a second caller.",
			len(matches), offenders)
	}
}

// sendMatch is one AST call site matched by the chokepoint guard, plus
// whether the enclosing function is (*Service).deliver.
type sendMatch struct {
	desc      string
	isDeliver bool
}

// isServicePtrReceiver reports whether recv is a single pointer receiver of
// type *Service — i.e. the receiver shape of `func (s *Service) deliver(...)`.
// Matching the receiver as well as the bare function name is what stops any
// other type from declaring its own method named deliver and sending freely:
// a `func (x *other) deliver(...)` no longer counts as the guarded chokepoint.
func isServicePtrReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) != 1 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Service"
}

// funcDesc renders a FuncDecl as "(*Service).deliver" (or "(Service).deliver"
// for a value receiver, or bare "name" with no receiver) for failure
// messages.
func funcDesc(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		switch t := fn.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if ident, ok := t.X.(*ast.Ident); ok {
				return fmt.Sprintf("(*%s).%s", ident.Name, fn.Name.Name)
			}
		case *ast.Ident:
			return fmt.Sprintf("(%s).%s", t.Name, fn.Name.Name)
		}
	}
	return fn.Name.Name
}
