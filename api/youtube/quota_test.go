package youtube

import (
	"testing"
	"time"
)

func TestTheQuotaDayIsPacific(t *testing.T) {
	cases := []struct {
		at        string
		date      string
		resetsAt  string
		dayLength time.Duration
	}{
		// 05:00 UTC on the 18th is 22:00 PDT on the 17th.
		{"2026-09-18T05:00:00Z", "2026-09-17", "2026-09-18T07:00:00Z", 0},
		// 07:00 UTC is midnight PDT, the first instant of the 18th.
		{"2026-09-18T07:00:00Z", "2026-09-18", "2026-09-19T07:00:00Z", 24 * time.Hour},
		// Pacific time falls back on 2026-11-01, so that day is 25 hours long.
		{"2026-11-01T07:00:00Z", "2026-11-01", "2026-11-02T08:00:00Z", 25 * time.Hour},
		// It springs forward on 2027-03-14, a day of 23 hours.
		{"2027-03-14T08:00:00Z", "2027-03-14", "2027-03-15T07:00:00Z", 23 * time.Hour},
	}
	for _, c := range cases {
		at, err := time.Parse(time.RFC3339, c.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := QuotaDate(at); got != c.date {
			t.Errorf("QuotaDate(%s) = %s, want %s", c.at, got, c.date)
		}
		reset := QuotaReset(at)
		if got := reset.UTC().Format(time.RFC3339); got != c.resetsAt {
			t.Errorf("QuotaReset(%s) = %s, want %s", c.at, got, c.resetsAt)
		}
		if c.dayLength != 0 && reset.Sub(at) != c.dayLength {
			t.Errorf("the quota day from %s lasts %s, want %s", c.at, reset.Sub(at), c.dayLength)
		}
	}
}
