package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
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
	return New([]string{server.URL}, server.Client())
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
	client := answering(t, http.StatusNotFound, `{"message": "playlist nope not found", "code": "not_found"}`)

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
	body := `{"message": "YouTube has no public video for a, b", "code": "video_unavailable", "video_ids": ["a", "b"]}`
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
	refused := answering(t, http.StatusUnauthorized, `{"message": "no token", "code": "missing_token"}`)
	_, err := refused.ListPlaylists(context.Background())
	var refusal *Refusal
	if !errors.As(err, &refusal) || !refusal.Unauthorized() {
		t.Fatalf("err = %v, want a refusal that knows it was the token", err)
	}

	other := answering(t, http.StatusNotFound, `{"message": "nothing there", "code": "not_found"}`)
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
// The filter reaches the server on every page, whether the server answers the
// library whole or a page at a time, and a paged answer is followed to its end.
func TestTheFilterBecomesTheServersOwnParametersOnEveryPage(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("starting_after") == "" {
			_, _ = w.Write([]byte(`{"data": [{"id": "a"}], "has_more": true}`))
			return
		}
		_, _ = w.Write([]byte(`{"data": [{"id": "b"}], "has_more": false}`))
	}))
	t.Cleanup(server.Close)

	bound := int64(60)
	filter := VideoFilter{Playlist: "alpha", Artist: "moby", MinSeconds: &bound, Sort: "longest"}
	videos, err := New([]string{server.URL}, server.Client()).ListVideos(context.Background(), filter)
	if err != nil || len(videos) != 2 || videos[0].ID != "a" || videos[1].ID != "b" {
		t.Fatalf("videos = %+v, %v, want a then b, the second page followed", videos, err)
	}
	want := []string{
		"artist=moby&min_seconds=60&playlist=alpha&sort=longest",
		"artist=moby&min_seconds=60&playlist=alpha&sort=longest&starting_after=a",
	}
	if !slices.Equal(asked, want) {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}

// An address that takes no connection is passed over for the next, and every
// later request starts at the one that answered. An answer of any kind, a
// refusal included, is the server's, so the address that gave it is kept.
func TestAnAddressThatTakesNoConnectionIsPassedOverForTheNext(t *testing.T) {
	var served []string
	answer := func(name string, status int) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served = append(served, name+" "+r.Method)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"version": "v1"}`))
		}))
		t.Cleanup(server.Close)
		return server
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	refusing, public := answer("refusing", http.StatusServiceUnavailable), answer("public", http.StatusOK)
	transport := &http.Client{Transport: MarkConnectFailures(http.DefaultTransport)}

	client := New([]string{closed.URL, public.URL}, transport)
	for range 2 {
		if err := client.Get(context.Background(), "/api/v1/version", &Version{}); err != nil {
			t.Fatalf("a request past the closed address failed: %v", err)
		}
	}
	if !slices.Equal(served, []string{"public GET", "public GET"}) || client.reached.Load() != 1 {
		t.Fatalf("served %v from address %d, want both requests from the public address", served, client.reached.Load())
	}

	served = nil
	if err := New([]string{refusing.URL, public.URL}, transport).Get(context.Background(), "/api/v1/version", &Version{}); !errors.As(err, new(*Refusal)) {
		t.Fatalf("a refusal from the first address answered %v, want that refusal", err)
	}
	if !slices.Equal(served, []string{"refusing GET"}) {
		t.Fatalf("served %v, want the refusal alone", served)
	}
}

// api_tried_first lists a LAN address, and on this network its packets are
// dropped rather than refused. The request reaches the next address within a
// few seconds rather than after the dial's own 30.
func TestAnAddressThatDropsPacketsIsPassedOverWithinASecond(t *testing.T) {
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version": "v1"}`))
	}))
	t.Cleanup(public.Close)
	// 10.255.255.1 is a private address that no network here routes to.
	client := New([]string{"http://10.255.255.1", public.URL}, &http.Client{Transport: ServerTransport()})

	began := time.Now()
	if err := client.Get(context.Background(), "/api/v1/version", &Version{}); err != nil {
		t.Fatalf("a request past the address that drops packets failed: %v", err)
	}
	if took := time.Since(began); took > 5*triedFirstDial || client.reached.Load() != 1 {
		t.Fatalf("the request took %v and reached address %d, want the second address within %v", took, client.reached.Load(), 5*triedFirstDial)
	}
}
