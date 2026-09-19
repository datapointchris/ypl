package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// collect reads the collection at path, asked with pairs, until it holds limit
// rows or the server says none follow. A page is followed with cursor, which
// names the row the next starts after.
//
// A server that answers the collection whole, as a JSON array, is read too and
// cut at limit. A client and a server are released apart, so a client that read
// only one shape would fail against the other until somebody updated it.
//
// The rows come back as a list at every size, including none, so a caller
// filtering the JSON writes one filter rather than a filter and a null guard.
func collect[T any](ctx context.Context, c *Client, path string, pairs [][2]string, limit int, cursor func(T) string) (Page[T], error) {
	read := Page[T]{Rows: []T{}}
	after := ""
	// A limit of nothing is a request a caller can mean, and it needs no
	// request to answer.
	for len(read.Rows) < limit {
		asked := slices.Concat(pairs, [][2]string{{"starting_after", after}})
		if limit != every {
			asked = append(asked, [2]string{"limit", strconv.Itoa(min(limit-len(read.Rows), PageSize))})
		}
		var raw json.RawMessage
		target := query(path, asked...)
		if err := c.Get(ctx, target, &raw); err != nil {
			return Page[T]{}, err
		}
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
			var rows []T
			if err := json.Unmarshal(raw, &rows); err != nil {
				return Page[T]{}, fmt.Errorf("decode %s: %w", path, err)
			}
			kept := min(len(rows), limit-len(read.Rows))
			read.Rows = append(read.Rows, rows[:kept]...)
			read.More = len(rows) > kept
			return read, nil
		}
		var got page[T]
		if err := json.Unmarshal(raw, &got); err != nil {
			return Page[T]{}, fmt.Errorf("decode %s: %w", path, err)
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

// every is the limit that reads every row. Its requests carry no limit, so the
// server answers with its own page size, and a whole read never rests on
// PageSize matching the server's maximum.
const every = math.MaxInt

// all reads every row of the collection at path, asked with pairs.
func all[T any](ctx context.Context, c *Client, path string, pairs [][2]string, cursor func(T) string) ([]T, error) {
	read, err := collect(ctx, c, path, pairs, every, cursor)
	return read.Rows, err
}
