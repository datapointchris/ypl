package api

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// wireDir is where the API module's own suite writes what the server answers.
// The two modules cannot import each other — the client carries its own shapes
// precisely so the server's types are not its dependency — so a file one writes
// and the other reads is what crosses the seam.
const wireDir = "../../../api/handlers/testdata/wire"

// keys is every JSON key in a document, with its path, so two documents can be
// compared by what they name rather than by what they hold.
func keys(t *testing.T, value any, at string, into map[string]bool) {
	t.Helper()
	switch shaped := value.(type) {
	case map[string]any:
		for name, held := range shaped {
			here := at + "." + name
			into[here] = true
			keys(t, held, here, into)
		}
	case []any:
		for _, held := range shaped {
			keys(t, held, at+"[]", into)
		}
	}
}

// keysOf is every key a value carries once encoded.
func keysOf(t *testing.T, value any) map[string]bool {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var shaped any
	if err := json.Unmarshal(encoded, &shaped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := map[string]bool{}
	keys(t, shaped, "", found)
	return found
}

// keysIn is every key the stored document carries.
func keysIn(t *testing.T, name string) (map[string]bool, []byte) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(wireDir, name+".json"))
	if err != nil {
		t.Fatalf("read the wire document — run `go test ./handlers -update` in the api module: %v", err)
	}
	var shaped any
	if err := json.Unmarshal(body, &shaped); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
	found := map[string]bool{}
	keys(t, shaped, "", found)
	return found, body
}

// Every field these types declare has to arrive from the real server, or the
// command reading it renders an empty value and nothing reports why.
//
// Asserting against JSON written by hand cannot find that: the hand-written
// document agrees with the type because the same person wrote both. This decodes
// what the server actually answered.
func TestEveryDeclaredFieldArrivesFromTheServer(t *testing.T) {
	for _, c := range []struct {
		document string
		into     func() any
	}{
		{"playlists", func() any { return &page[PlaylistSummary]{} }},
		{"playlist", func() any { return &Playlist{} }},
		{"videos", func() any { return &page[LibraryVideo]{} }},
		{"video", func() any { return &Video{} }},
		{"plays", func() any { return &page[Play]{} }},
		{"suggestions", func() any { return &[]Suggestion{} }},
		{"sync-runs", func() any { return &page[SyncRun]{} }},
		{"status", func() any { return &Status{} }},
		{"ratings", func() any { return &page[Rating]{} }},
		{"tags", func() any { return &[]TagCount{} }},
		{"history", func() any { return &[]HistoryEntry{} }},
		{"transcript", func() any { return &Transcript{} }},
		{"version", func() any { return &Version{} }},
		{"ytdlp-lease", func() any { return &Lease{} }},
		// The order's revision is a header rather than a field, so it is marked
		// as no part of the body and this walk sees only what the document
		// carries.
		{"playlist-items", func() any { return &Order{} }},
	} {
		sent, body := keysIn(t, c.document)
		decoded := c.into()
		if err := json.Unmarshal(body, decoded); err != nil {
			t.Errorf("%s does not decode into its type: %v", c.document, err)
			continue
		}
		missing := []string{}
		for name := range keysOf(t, decoded) {
			if !sent[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: the type names %v, which the server does not send", c.document, missing)
		}
	}

	// The walk reads, and the order is also sent. Its revision is a header, so
	// the document carries no key for it and a decode passes under the tag that
	// exists to forbid one. Sending is where the tag bites: this value is the PUT
	// body, the server decodes it with DisallowUnknownFields, and a revision in
	// it refuses every edit with 400.
	sentOrder, err := json.Marshal(Order{VideoIDs: []string{"a"}, Revision: "3"})
	if err != nil {
		t.Fatalf("the order does not encode: %v", err)
	}
	if bytes.Contains(sentOrder, []byte("revision")) {
		t.Errorf("an edit sends %s, and the revision is only ever a header", sentOrder)
	}
}

// A run's answer is read entry by entry, each expected by name in the order
// the server writes them, so a renamed or moved entry fails every run.
func TestARunsEntriesAreTheServers(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(wireDir, "ytdlp-run.json"))
	if err != nil {
		t.Fatalf("read a run's entries — run `go test ./handlers -update` in the api module: %v", err)
	}
	var entries []string
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("decode a run's entries: %v", err)
	}
	if len(entries) < 5 || !slices.Equal(entries[:3], []string{runStdout, runStderr, runExitCode}) ||
		!strings.HasPrefix(entries[3], runFiles) || entries[len(entries)-1] != runEnd {
		t.Errorf("the server answers a run as %v, and the client reads %s, %s, %s, %s<path> for each file, then %s",
			entries, runStdout, runStderr, runExitCode, runFiles, runEnd)
	}
}

// Every refusal code this client branches on is one the server answers with,
// so a code the server renames fails here rather than turning a wait into a
// failure.
func TestTheRefusalCodesAreTheServers(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(wireDir, "refusal-codes.json"))
	if err != nil {
		t.Fatalf("read the server's refusal codes — run `go test ./handlers -update` in the api module: %v", err)
	}
	var served []string
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the refusal codes: %v", err)
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				literal, ok := valueAt(spec, i).(*ast.BasicLit)
				if !strings.HasPrefix(name.Name, "Code") || !ok || literal.Kind != token.STRING {
					continue
				}
				code, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("%s is %s: %v", name.Name, literal.Value, err)
				}
				held++
				if !slices.Contains(served, code) {
					t.Errorf("%s is %q, which the server does not answer with", name.Name, code)
				}
			}
			return true
		})
	}
	if held == 0 {
		t.Fatal("found no Code constant in this package")
	}
}

// valueAt is the value spec gives its ith name, and nil where it gives none.
func valueAt(spec *ast.ValueSpec, i int) ast.Expr {
	if i < len(spec.Values) {
		return spec.Values[i]
	}
	return nil
}

func TestTheLanguageShapeIsTheServers(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(wireDir, "language_pattern.json"))
	if err != nil {
		t.Fatalf("read the server's language pattern — run `go test ./handlers -update` in the api module: %v", err)
	}
	var pattern string
	if err := json.Unmarshal(body, &pattern); err != nil {
		t.Fatalf("decode the language pattern: %v", err)
	}
	if pattern != LanguagePattern {
		t.Errorf("LanguagePattern is %q, and the server checks %q", LanguagePattern, pattern)
	}
}

func TestTheRatingKindsAreTheServers(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(wireDir, "rating_kinds.json"))
	if err != nil {
		t.Fatalf("read the server's rating kinds — run `go test ./handlers -update` in the api module: %v", err)
	}
	var kinds []string
	if err := json.Unmarshal(body, &kinds); err != nil {
		t.Fatalf("decode the rating kinds: %v", err)
	}
	if !slices.Equal(kinds, RatingKinds) {
		t.Errorf("RatingKinds is %v, and the server takes %v", RatingKinds, kinds)
	}
}
