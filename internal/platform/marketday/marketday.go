// Package marketday answers "what date is it" in a market's own time zone.
//
// A person recording a rent receipt or a repair cost "today" means today
// where they are: in Harare (UTC+2) the 22nd begins two hours before it does
// in UTC. Date checks such as "may not be in the future" therefore use the
// market's calendar date, not the UTC one.
package marketday

import (
	"context"
	"sync"
	"time"
)

// Zone returns the IANA time zone of a market code ("" when unknown).
type Zone func(ctx context.Context, marketCode string) string

var locations sync.Map // zone name -> *time.Location

// location returns the zone, or UTC when the name is empty or unknown.
func location(name string) *time.Location {
	if name == "" || name == "Local" {
		return time.UTC
	}
	if l, ok := locations.Load(name); ok {
		return l.(*time.Location)
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	locations.Store(name, l)
	return l
}

// Day is now's calendar date (YYYY-MM-DD) in the IANA zone; UTC when the zone
// is empty or unknown.
func Day(now time.Time, zone string) string {
	return now.In(location(zone)).Format(time.DateOnly)
}

// Today is now's calendar date in the market's zone, looked up with zone; UTC
// when zone is nil, the code is empty or the market has no known zone.
func Today(ctx context.Context, zone Zone, marketCode string, now time.Time) string {
	if zone == nil || marketCode == "" {
		return Day(now, "")
	}
	return Day(now, zone(ctx, marketCode))
}
