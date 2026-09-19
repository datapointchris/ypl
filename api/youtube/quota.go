package youtube

import (
	"fmt"
	"time"
	_ "time/tzdata" // the quota day is Pacific, whatever zone the host is in
)

// DailyQuota is the units YouTube allows the Cloud project each Pacific day.
const DailyQuota = 10_000

// pacific is the zone YouTube's daily quota resets in.
var pacific = mustLoadLocation("America/Los_Angeles")

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("load %s from the embedded zone database: %v", name, err))
	}
	return location
}

// QuotaDate is the Pacific date whose quota a request made at t counts against.
func QuotaDate(t time.Time) string {
	return t.In(pacific).Format(time.DateOnly)
}

// QuotaReset is the next midnight Pacific after t, when the quota a request made
// at t counts against resets.
func QuotaReset(t time.Time) time.Time {
	year, month, day := t.In(pacific).Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, pacific)
}
