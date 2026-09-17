package youtube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// requestSelectors are the methods of a generated Data API call that send a
// request: Do sends one, and a list call's Pages sends one per page.
var requestSelectors = []string{"Do", "Pages"}

// Every request this package makes is handed to send, so it is counted, retried
// where its method allows and named in one place. A request made anywhere else
// would reach YouTube with none of that.
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
		inside, outside := requestsBySend(file)
		checked += len(inside) + len(outside)
		for _, selector := range outside {
			t.Errorf("%s: %s is called outside send", fset.Position(selector.Pos()), selector.Sel.Name)
		}
	}
	if checked == 0 {
		t.Fatal("found no request to check, so the walk is not reading the package")
	}
}

// The walk is proved on source holding one request of each kind outside send,
// since every request in the package itself goes through send.
func TestTheRequestWalkFindsEachKindOutsideSend(t *testing.T) {
	const planted = `package youtube

func planted(c *Channel) {
	_, _ = send(ctx, c, playlistsList, c.service.Playlists.List(nil).Mine(true).Do)
	_, _ = c.service.PlaylistItems.List(nil).Do()
	_ = c.service.Playlists.List(nil).Pages(ctx, nil)
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	inside, outside := requestsBySend(file)
	var names []string
	for _, selector := range outside {
		names = append(names, selector.Sel.Name)
	}
	if len(inside) != 1 || !slices.Equal(names, []string{"Do", "Pages"}) {
		t.Fatalf("walk found %d inside send and %v outside, want 1 inside and Do and Pages outside", len(inside), names)
	}
}

// requestsBySend is each request selector in file, split by whether it sits
// inside a call to send.
func requestsBySend(file *ast.File) (inside, outside []*ast.SelectorExpr) {
	// withinSend holds, for each node on the path from the file to the one being
	// visited, whether it sits inside a call to send.
	var withinSend []bool
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			withinSend = withinSend[:len(withinSend)-1]
			return true
		}
		within := len(withinSend) > 0 && withinSend[len(withinSend)-1]
		if call, ok := node.(*ast.CallExpr); ok && callsSend(call) {
			within = true
		}
		withinSend = append(withinSend, within)
		if selector, ok := node.(*ast.SelectorExpr); ok && slices.Contains(requestSelectors, selector.Sel.Name) {
			if within {
				inside = append(inside, selector)
			} else {
				outside = append(outside, selector)
			}
		}
		return true
	})
	return inside, outside
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
