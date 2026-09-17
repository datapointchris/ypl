package youtube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every Data API call's Do is handed to send, so every request is counted,
// retried and named in one place. A Do called anywhere else would reach YouTube
// with none of that.
func TestEveryRequestGoesThroughSend(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// withinSend holds, for each node on the path from the file to the one
		// being visited, whether it sits inside a call to send.
		var withinSend []bool
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				withinSend = withinSend[:len(withinSend)-1]
				return true
			}
			inside := len(withinSend) > 0 && withinSend[len(withinSend)-1]
			if call, ok := node.(*ast.CallExpr); ok && callsSend(call) {
				inside = true
			}
			withinSend = append(withinSend, inside)
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "Do" {
				checked++
				if !inside {
					t.Errorf("%s: Do is called outside send", fset.Position(selector.Pos()))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no Do to check, so the walk is not reading the package")
	}
}

// callsSend is whether call is a call to send, with or without explicit type
// arguments.
func callsSend(call *ast.CallExpr) bool {
	fun := call.Fun
	if index, ok := fun.(*ast.IndexExpr); ok {
		fun = index.X
	}
	ident, ok := fun.(*ast.Ident)
	return ok && ident.Name == "send"
}
