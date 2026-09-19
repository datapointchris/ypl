package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// answering is a client talking to a server that answers every request with
// status and body.
func answering(t *testing.T, status int, body string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(server.URL, server.Client())
}

// This is what the package exists for: a CLI older than the server it is
// talking to keeps working, so the two can move at their own pace.
func TestAFieldTheServerAddsIsIgnoredRatherThanRefused(t *testing.T) {
	body := `[{"id": "PLA", "title": "Alpha", "description": "", "privacy": "private",
		"item_count": 1, "unavailable_count": 0, "enriched_count": 0,
		"something_added_later": {"nested": [1, 2, 3]}}]`
	client := answering(t, http.StatusOK, body)

	playlists, err := client.ListPlaylists(context.Background())
	if err != nil {
		t.Fatalf("list playlists: %v", err)
	}
	if len(playlists) != 1 || playlists[0].ID != "PLA" {
		t.Fatalf("playlists = %+v", playlists)
	}
}

// The code is what a caller branches on, since the sentence is for a person and
// may be reworded.
func TestARefusalCarriesTheServersCodeAndSentence(t *testing.T) {
	client := answering(t, http.StatusNotFound, `{"error": "playlist nope not found", "code": "not_found"}`)

	_, err := client.GetPlaylist(context.Background(), "nope")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a Refusal", err)
	}
	switch {
	case refusal.Status != http.StatusNotFound:
		t.Errorf("status = %d, want 404", refusal.Status)
	case refusal.Code != "not_found":
		t.Errorf("code = %q, want not_found", refusal.Code)
	case refusal.Error() != "playlist nope not found":
		t.Errorf("sentence = %q, want the server's own", refusal.Error())
	}
}

// A refusal about particular videos names them, so a caller can say which of
// the ones it sent were the problem.
func TestARefusalAboutVideosNamesThem(t *testing.T) {
	body := `{"error": "YouTube has no public video for a, b", "code": "video_unavailable", "video_ids": ["a", "b"]}`
	client := answering(t, http.StatusUnprocessableEntity, body)

	_, err := client.GetVideo(context.Background(), "a")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a Refusal", err)
	}
	if len(refusal.VideoIDs) != 2 || refusal.VideoIDs[0] != "a" {
		t.Fatalf("video ids = %v, want a and b", refusal.VideoIDs)
	}
}

// A proxy's error page or an identity provider's redirect is not the server's
// envelope, and the status is still something a caller can act on.
func TestAnAnswerThatIsNotAnEnvelopeStillCarriesItsStatus(t *testing.T) {
	client := answering(t, http.StatusBadGateway, "<html>502 Bad Gateway</html>")

	_, err := client.ListPlaylists(context.Background())
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a Refusal", err)
	}
	switch {
	case refusal.Status != http.StatusBadGateway:
		t.Errorf("status = %d, want 502", refusal.Status)
	case refusal.Code != "":
		t.Errorf("code = %q, want none for an answer the server did not write", refusal.Code)
	case refusal.Error() == "":
		t.Error("a refusal with no envelope produced no sentence at all")
	}
}

// A token the server will not take is answered by logging in again, never by a
// different request, so it is the one status a caller has to tell apart.
func TestARefusalKnowsWhetherItWasTheToken(t *testing.T) {
	refused := answering(t, http.StatusUnauthorized, `{"error": "no token", "code": "missing_token"}`)
	_, err := refused.ListPlaylists(context.Background())
	var refusal *Refusal
	if !errors.As(err, &refusal) || !refusal.Unauthorized() {
		t.Fatalf("err = %v, want a refusal that knows it was the token", err)
	}

	other := answering(t, http.StatusNotFound, `{"error": "nothing there", "code": "not_found"}`)
	_, err = other.ListPlaylists(context.Background())
	if errors.As(err, &refusal) && refusal.Unauthorized() {
		t.Error("a 404 reported itself as a rejected token")
	}
}

// A parameter with no value is left off rather than sent empty, since the
// server reads an empty one as absent and an absent one is what was meant.
func TestQueryLeavesOffWhatWasNotAskedFor(t *testing.T) {
	for _, c := range []struct {
		pairs []([2]string)
		want  string
	}{
		{nil, "/v"},
		{[][2]string{{"a", ""}}, "/v"},
		{[][2]string{{"a", "1"}, {"b", ""}}, "/v?a=1"},
		{[][2]string{{"a", "1"}, {"b", "2"}}, "/v?a=1&b=2"},
		{[][2]string{{"a", "one two"}}, "/v?a=one+two"},
	} {
		if got := query("/v", c.pairs...); got != c.want {
			t.Errorf("query(%v) = %q, want %q", c.pairs, got, c.want)
		}
	}
}

// A stored value that never reaches the server cannot be read back, and the
// only sign is a request that quietly asks for something else.
func TestTheFilterBecomesTheServersOwnParameters(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)

	bound := int64(60)
	filter := VideoFilter{Playlist: "alpha", Artist: "moby", MinSeconds: &bound, Sort: "longest"}
	if _, err := New(server.URL, server.Client()).ListVideos(context.Background(), filter); err != nil {
		t.Fatalf("list videos: %v", err)
	}
	want := "artist=moby&min_seconds=60&playlist=alpha&sort=longest"
	if asked != want {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}
