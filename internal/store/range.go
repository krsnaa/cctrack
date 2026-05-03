package store

import (
	"fmt"
	"time"
)

// TimeBounds describes a [Start, End) interval. A zero Start means "no lower
// bound" — used to express the all-time range without sentinel-date games.
type TimeBounds struct {
	Start time.Time
	End   time.Time
}

// ParseRange resolves a range token into concrete bounds in the host's local
// time zone. The dashboard uses these tokens for the per-card time-range
// dropdowns; treating them as a small enum here keeps the wire format stable
// even if the UI labels change.
//
// Empty string defaults to "30d" — the most useful momentum window when no
// preference has been expressed.
func ParseRange(r string) (TimeBounds, error) {
	now := time.Now()
	switch r {
	case "", "30d":
		return TimeBounds{now.AddDate(0, 0, -30), now}, nil
	case "7d":
		return TimeBounds{now.AddDate(0, 0, -7), now}, nil
	case "mtd":
		s := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return TimeBounds{s, now}, nil
	case "last_month":
		s := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location())
		e := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return TimeBounds{s, e}, nil
	case "all":
		return TimeBounds{time.Time{}, now}, nil
	}
	return TimeBounds{}, fmt.Errorf("unknown range %q", r)
}
