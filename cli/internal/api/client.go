// Package api reaches the ypl server over HTTP.
//
// Every type here is the shape of a JSON body the server sends or takes, and
// none of them is the server's own. A field the server adds is ignored rather
// than refused, so a CLI older than the server it is talking to keeps working,
// and the two are free to move at their own pace.
//
// Nothing here handles a token. The *http.Client it is given adds the
// Authorization header and refreshes what is behind it.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client reaches one ypl server.
type Client struct {
	base string
	http *http.Client
}

// New is a client for the server at base, sending every request through
// httpClient. A trailing slash on base is dropped so a path is joined to it
// once.
func New(base string, httpClient *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: httpClient}
}

// Refusal is an answer the server refused, carrying the code it refused with.
// The sentence is for a person and may change between versions; the code does
// not, so a caller branches on that.
type Refusal struct {
	Status int
	// Code is the server's own name for this refusal, and "" where the answer
	// was not one the server wrote — a proxy's error page, an identity
	// provider's redirect.
	Code string
	// Message is the server's sentence, and "" for the same reason.
	Message string
	// VideoIDs names the videos a refusal about particular videos is about.
	VideoIDs []string
}

func (r *Refusal) Error() string {
	if r.Message != "" {
		return r.Message
	}
	return fmt.Sprintf("the server answered %s", http.StatusText(r.Status))
}

// Unauthorized reports whether the server refused the token rather than the
// request, which is answered by logging in again and never by a different
// request.
func (r *Refusal) Unauthorized() bool { return r.Status == http.StatusUnauthorized }

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	_, err := c.send(ctx, http.MethodGet, path, nil, nil, out)
	return err
}

// send makes one request and decodes a 2xx body into out. A nil body sends
// none, and a nil out reads and discards the answer. header carries the fields
// a particular request needs beyond the ones every request sends. It returns
// the answer's own headers, for a caller that needs the ETag.
func (c *Client) send(ctx context.Context, method, path string, header http.Header, body, out any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		req.Header[name] = values
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the ypl server at %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, refusalFrom(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.Header, fmt.Errorf("read the server's answer: %w", err)
	}
	return resp.Header, nil
}

// maxRefusalBody bounds what is read from an answer that refused, many times
// the size of any refusal the server writes.
const maxRefusalBody = 8 << 10

// refusalFrom is resp as a Refusal, reading the server's envelope where the
// body is one. A body that is not — a proxy's HTML, an identity provider's
// redirect — still gives a Refusal carrying the status, since the status is the
// part a caller can always act on.
func refusalFrom(resp *http.Response) error {
	refusal := &Refusal{Status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	var envelope struct {
		Error    string   `json:"error"`
		Code     string   `json:"code"`
		VideoIDs []string `json:"video_ids"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		refusal.Message, refusal.Code, refusal.VideoIDs = envelope.Error, envelope.Code, envelope.VideoIDs
	}
	return refusal
}

// query is path with every named parameter that has a value, in the order
// given. A parameter with no value is left off rather than sent empty, since
// the server reads an empty one as absent anyway and an absent one is what the
// caller meant.
func query(path string, pairs ...[2]string) string {
	values := url.Values{}
	for _, pair := range pairs {
		if pair[1] != "" {
			values.Set(pair[0], pair[1])
		}
	}
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

// ref is s as one segment of a path. A playlist is named by its title as well
// as its id, and a title holds spaces and punctuation.
func ref(s string) string { return url.PathEscape(s) }
