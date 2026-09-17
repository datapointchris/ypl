package api

import (
	"context"
	"strconv"
)

// pageSize is the most rows the server puts on one page of a paged collection.
// The server publishes the number and refuses a larger limit naming it, so it
// is read from the far end rather than picked here — a number picked here that
// were too large would be clamped silently, and every read would come back
// short with nothing saying why.
const pageSize = 100

// page is one page of a paged collection, as the server sends it.
type page[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// collect reads pages of path until it holds limit rows, or until the server
// says none follow. cursor names the row a page starts after, taken from the
// last row of the page before it.
//
// The rows come back as a list at every size, including none, so a caller
// filtering the JSON writes one filter rather than a filter and a null guard.
func collect[T any](ctx context.Context, c *Client, path string, limit int, cursor func(T) string) ([]T, error) {
	held := []T{}
	after := ""
	for len(held) < limit {
		want := min(limit-len(held), pageSize)
		var got page[T]
		target := query(path, [2]string{"limit", strconv.Itoa(want)}, [2]string{"starting_after", after})
		if err := c.Get(ctx, target, &got); err != nil {
			return nil, err
		}
		held = append(held, got.Data...)
		if !got.HasMore || len(got.Data) == 0 {
			break
		}
		after = cursor(got.Data[len(got.Data)-1])
	}
	return held, nil
}
