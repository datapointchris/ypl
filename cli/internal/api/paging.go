package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// PageSize is the most rows the server puts on one page of a paged collection.
//
// It is a copy of the server's own maximum rather than a number read from it:
// the server states that maximum only inside the sentence it refuses with, and
// nothing publishes it where a client could ask. So the copy can fall out of
// step, and it does so in a direction nothing here would report — lowering the
// server's maximum turns every larger ask into a refusal this client presents
// as a failure. The API module's own test is what holds the two level.
const PageSize = 100

// page is one page of a paged collection, as the server sends it.
type page[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// Page is rows read from a paged collection, and whether the server had more it
// was not asked for.
//
// More is carried rather than dropped because a read that stopped at its limit
// and a read that reached the end are the same rows on screen. The server owns
// that fact and hands it over; losing it here is what makes a truncated answer
// indistinguishable from a complete one.
type Page[T any] struct {
	Rows []T
	More bool
}

// collect reads pages of path, asked with pairs, until it holds limit rows or
// the server says none follow. cursor names the row a page starts after, taken
// from the last row of the page before it.
//
// The rows come back as a list at every size, including none, so a caller
// filtering the JSON writes one filter rather than a filter and a null guard.
func collect[T any](ctx context.Context, c *Client, path string, pairs [][2]string, limit int, cursor func(T) string) (Page[T], error) {
	read := Page[T]{Rows: []T{}}
	after := ""
	// A limit of nothing is a request a caller can mean, and it needs no
	// request to answer.
	for len(read.Rows) < limit {
		want := min(limit-len(read.Rows), PageSize)
		var got page[T]
		target := query(path, slices.Concat(pairs, [][2]string{{"limit", strconv.Itoa(want)}, {"starting_after", after}})...)
		if err := c.Get(ctx, target, &got); err != nil {
			return Page[T]{}, err
		}
		read.Rows = append(read.Rows, got.Data...)
		read.More = got.HasMore
		if !got.HasMore || len(got.Data) == 0 {
			break
		}
		after = cursor(got.Data[len(got.Data)-1])
	}
	return read, nil
}

// all reads every row of the collection at path, asked with pairs, whether the
// server answers it whole, as a JSON array, or a page at a time. A page is
// followed with cursor, which names the row the next starts after.
//
// Both answers are in service at once, because a client and a server are
// released apart, and a client that read only one would fail against the other
// until somebody updated it.
func all[T any](ctx context.Context, c *Client, path string, pairs [][2]string, cursor func(T) string) ([]T, error) {
	var first json.RawMessage
	if err := c.Get(ctx, query(path, pairs...), &first); err != nil {
		return nil, err
	}
	rows := []T{}
	if trimmed := bytes.TrimSpace(first); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(first, &rows); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return rows, nil
	}
	var got page[T]
	if err := json.Unmarshal(first, &got); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	for {
		rows = append(rows, got.Data...)
		if !got.HasMore || len(got.Data) == 0 {
			return rows, nil
		}
		next := slices.Concat(pairs, [][2]string{{"starting_after", cursor(got.Data[len(got.Data)-1])}})
		got = page[T]{}
		if err := c.Get(ctx, query(path, next...), &got); err != nil {
			return nil, err
		}
	}
}
