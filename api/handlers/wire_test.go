package handlers

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// update rewrites the wire documents instead of checking them.
var update = flag.Bool("update", false, "rewrite the wire documents in testdata/wire")

// The wire documents are what the server actually answers, written to disk so
// the CLI's own suite can decode them.
//
// Both modules otherwise assert against JSON each wrote by hand, which agrees
// with itself and with nothing else: renaming a field on a response struct here
// leaves both suites green and breaks every command in the client. Neither
// module may import the other — that is the point of the client carrying its own
// shapes — so a file one writes and the other reads is what crosses the seam.
func TestTheWireDocumentsAreWhatTheServerAnswers(t *testing.T) {
	f := newFixture(t)
	f.withLibrary(t)
	// A play, so the plays document carries a row rather than an empty page.
	// Its id is fixed, since the document is compared byte for byte.
	played := f.do(http.MethodPost, "/api/v1/plays",
		`{"id": "01920000-0000-7000-8000-000000000001", "video_id": "a", "played_ts": "2026-09-01T10:00:00Z"}`)
	if played.Code != http.StatusCreated {
		t.Fatalf("storing a play answered %d: %s", played.Code, played.Body)
	}

	for _, doc := range []struct {
		name   string
		target string
		// sorted orders the rows by id before writing. A draw reshuffles among
		// videos last played at the same moment and every never-played video
		// ties, so its order is deliberately unstable. The field names are what
		// crosses the seam and sorting leaves those alone.
		sorted bool
	}{
		{name: "playlists", target: "/api/v1/playlists"},
		{name: "playlist", target: "/api/v1/playlists/PLA"},
		{name: "playlist-items", target: "/api/v1/playlists/PLA/items"},
		{name: "videos", target: "/api/v1/videos"},
		{name: "video", target: "/api/v1/videos/a"},
		{name: "plays", target: "/api/v1/plays"},
		{name: "suggestions", target: "/api/v1/suggestions?limit=4", sorted: true},
		{name: "sync-runs", target: "/api/v1/sync/runs"},
		{name: "status", target: "/api/v1/status"},
	} {
		rec := f.get(doc.target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", doc.target, rec.Code, rec.Body)
		}
		body := rec.Body.Bytes()
		if doc.sorted {
			body = sortedByID(t, body)
		}
		pinned(t, doc.name, doc.target, body)
	}

	// The slug a playlist is reached by. The CLI offers these on Tab and has to
	// derive the same one from a title the server does, hyphens included, or it
	// offers a reference the server answers with a 404. The titles are the
	// shapes the derivation can disagree on: punctuation runs, edges, case,
	// letters outside ASCII, and a title with nothing left.
	type slugged struct {
		Title string `json:"title"`
		Slug  string `json:"slug"`
	}
	var slugs []slugged
	for _, title := range []string{
		"Sunday Morning", "002 - Audio - Tech", "Björk: Live!", "  Deep  House  ", "WSC-1",
		"DEEP", "Café del Mar", "日本の夜", "Mix #3 (2024)", "a__b", "!!!",
	} {
		slugs = append(slugs, slugged{Title: title, Slug: slug(title)})
	}
	body, err := json.Marshal(slugs)
	if err != nil {
		t.Fatalf("encode the slugs: %v", err)
	}
	pinned(t, "slugs", "the title slugs", body)
}

// pinned writes body as the wire document name under -update, and otherwise
// requires the stored one to hold exactly it. what names where body came from.
func pinned(t *testing.T, name, what string, body []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(body), "", "  "); err != nil {
		t.Fatalf("%s is not JSON: %v", what, err)
	}
	// Exactly one, since the answer's own encoder already ends with one and
	// the end-of-file hook would strip the second back out from under this.
	pretty.WriteByte('\n')

	path := filepath.Join("testdata", "wire", name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make testdata/wire: %v", err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s — run `go test ./handlers -update` to write it: %v", path, err)
	}
	if !bytes.Equal(stored, pretty.Bytes()) {
		t.Errorf("%s no longer answers what %s holds; run `go test ./handlers -update` and check what moved", what, path)
	}
}

// sortedByID is a JSON array ordered by each member's id, for a document whose
// own order is a draw.
func sortedByID(t *testing.T, body []byte) []byte {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("sorting a document that is not an array: %v", err)
	}
	slices.SortFunc(rows, func(a, b map[string]any) int {
		return cmp.Compare(fmt.Sprint(a["id"]), fmt.Sprint(b["id"]))
	})
	sorted, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("re-encode the sorted document: %v", err)
	}
	return sorted
}
