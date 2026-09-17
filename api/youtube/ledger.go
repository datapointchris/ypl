// Package youtube reads a channel's playlists through the YouTube Data API, and
// charges every call to a daily quota ledger before making it.
package youtube

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // the Pacific zone, on a host with no zoneinfo

	"github.com/datapointchris/ypl/api/store/generated"
)

// DailyQuota is the Data API's default daily allocation for the methods this
// service calls, in units.
const DailyQuota = 10_000

// The Data API methods this service calls, as the quota ledger names them.
const (
	MethodPlaylistsList     = "playlists.list"
	MethodPlaylistItemsList = "playlistItems.list"
)

// ErrQuotaSpent is the refusal for a call the day's quota cannot cover. The
// call is not made.
var ErrQuotaSpent = errors.New("the day's YouTube quota is spent")

// pacific is the zone YouTube resets the daily quota in.
var pacific = func() *time.Location {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(fmt.Sprintf("load the Pacific time zone: %v", err))
	}
	return location
}()

// QuotaDate is the Pacific date a call made at t counts against.
func QuotaDate(t time.Time) string {
	return t.In(pacific).Format(time.DateOnly)
}

// Ledger charges Data API calls against the Pacific day they are made in, up
// to a daily limit.
type Ledger struct {
	queries *generated.Queries
	limit   int64
	now     func() time.Time
}

// NewLedger charges calls through queries, up to limit units a day.
func NewLedger(queries *generated.Queries, limit int64) *Ledger {
	return &Ledger{queries: queries, limit: limit, now: time.Now}
}

// Spend records one call of method before it is made. It records nothing and
// returns ErrQuotaSpent when the call would take the day past the limit.
func (l *Ledger) Spend(ctx context.Context, method string) error {
	now := l.now()
	date := QuotaDate(now)
	charged, err := l.queries.SpendQuota(ctx, generated.SpendQuotaParams{
		QuotaDate:  date,
		SpentTs:    now.UTC().Format(time.RFC3339),
		Method:     method,
		DailyLimit: l.limit,
	})
	if err != nil {
		return fmt.Errorf("charge %s: %w", method, err)
	}
	if charged == 1 {
		return nil
	}

	// No row: either the day is spent, or the method was never seeded, which
	// is a programming error rather than a quota fact.
	if _, err := l.queries.QuotaMethodUnits(ctx, method); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("charge %s: no quota cost is recorded for this method", method)
	} else if err != nil {
		return fmt.Errorf("charge %s: %w", method, err)
	}
	return fmt.Errorf("%w on %s: %s would exceed %d units", ErrQuotaSpent, date, method, l.limit)
}

// SpentSince is how many units calls charged at or after since have spent,
// across however many Pacific days that covers.
func (l *Ledger) SpentSince(ctx context.Context, since time.Time) (int64, error) {
	units, err := l.queries.QuotaSpentSince(ctx, since.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("read the quota spent since %s: %w", since.Format(time.RFC3339), err)
	}
	return units, nil
}

// Spent is how many units the current Pacific day has spent.
func (l *Ledger) Spent(ctx context.Context) (int64, error) {
	units, err := l.queries.QuotaSpent(ctx, QuotaDate(l.now()))
	if err != nil {
		return 0, fmt.Errorf("read the day's quota spend: %w", err)
	}
	return units, nil
}
