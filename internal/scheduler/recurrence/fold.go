package recurrence

import (
	"fmt"
	"time"
)

func wallPoint(instant time.Time, location *time.Location) time.Time {
	local := instant.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
}

// firstRealization checks the candidate independently of the query and of
// time.Date's unspecified choice in a fold. ZoneBounds supplies actual historical
// intervals; no fixed fold duration or maximum offset is assumed.
func firstRealization(candidate time.Time, location *time.Location) (bool, error) {
	wall := wallPoint(candidate, location)
	seen := make(map[int]struct{})
	for probe := candidate.In(location); ; {
		_, offset := probe.Zone()
		if _, checked := seen[offset]; !checked {
			seen[offset] = struct{}{}
			alternative := time.Unix(wall.Unix()-int64(offset), int64(wall.Nanosecond())).UTC()
			if alternative.Before(candidate) && wallPoint(alternative, location).Equal(wall) {
				return false, nil
			}
		}
		start, _ := probe.ZoneBounds()
		if start.IsZero() {
			return true, nil
		}
		previous := start.Add(-time.Nanosecond)
		if !previous.Before(probe) {
			return false, fmt.Errorf("%w: native zone interval did not progress backwards", ErrLookup)
		}
		probe = previous.In(location)
	}
}
