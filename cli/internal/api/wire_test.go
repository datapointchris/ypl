package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
		{"playlists", func() any { return &[]PlaylistSummary{} }},
		{"playlist", func() any { return &Playlist{} }},
		{"videos", func() any { return &[]LibraryVideo{} }},
		{"video", func() any { return &Video{} }},
		{"plays", func() any { return &page[Play]{} }},
		{"suggestions", func() any { return &[]Suggestion{} }},
		{"sync-runs", func() any { return &page[SyncRun]{} }},
		{"status", func() any { return &Status{} }},
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
}

// The order is read separately from the resources above because its revision is
// a header rather than a field, so the document alone cannot carry it. The field
// that is in the document is checked here against the type an edit is built
// from.
func TestThePlaylistOrderDecodesFromTheServersDocument(t *testing.T) {
	_, body := keysIn(t, "playlist-items")
	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		t.Fatalf("the order document does not decode: %v", err)
	}
	if len(order.VideoIDs) == 0 {
		t.Fatal("the order document carries no video ids, so it proves nothing about the field")
	}
	// Asserted on the send rather than the read. The document carries no
	// revision key, so decoding leaves the field empty whatever tag it has, and
	// the check would hold under the tag it exists to forbid. Sending is where
	// the tag bites: this same value is the PUT body, the server decodes it with
	// DisallowUnknownFields, and a revision in it refuses every edit with 400.
	sent, err := json.Marshal(Order{VideoIDs: []string{"a"}, Revision: "3"})
	if err != nil {
		t.Fatalf("the order does not encode: %v", err)
	}
	if bytes.Contains(sent, []byte("revision")) {
		t.Errorf("an edit sends %s, and the revision is only ever a header", sent)
	}
}
