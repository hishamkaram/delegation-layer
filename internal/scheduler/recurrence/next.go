package recurrence

import (
	"fmt"
	"math"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// Next returns the first eligible UTC instant strictly after after. Inputs are
// immutable; there is no scheduler, timer or implicit clock read.
//
// The backend ORs restricted day-of-month and weekday fields. A field containing
// * or */1 instead requires both to match; patterned steps such as */2 and
// restricted full ranges keep OR semantics. Sunday is 0..6 or a name (not 7),
// and N/step means N through that field's maximum, by step.
//
// ErrRangeExhausted describes only the supported int64 Unix-nanosecond range,
// never universal impossibility. Missing local minutes are skipped; repeated
// minutes are eligible only at their first chronological realization.
func Next(expression, zone string, after time.Time) (time.Time, error) {
	if err := validateGrammar(expression); err != nil {
		return time.Time{}, err
	}
	location, err := namedLocation(zone)
	if err != nil {
		return time.Time{}, err
	}
	after = after.UTC()
	if err = contract.ValidateInstant("schedule after", after); err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrRangeExhausted, err)
	}
	through := contract.InstantFromUnixNanos(math.MaxInt64)
	return nextInZone(expression, location, after, through, defaultCron)
}

// The default backend can stall when AddDate crosses an absent local day.
// Calendar lookup therefore uses nominal UTC fields within each native offset
// interval. gocron still owns all grammar and calendar search; ZoneBounds and
// round trips own the mapping to real instants. No time.Date fold choice is used.
func nextInZone(expression string, location *time.Location, after, through time.Time, factory cronFactory) (time.Time, error) {
	for cursor := after; cursor.Before(through); {
		probe := cursor.Add(time.Nanosecond).In(location)
		_, offset := probe.Zone()
		_, end := probe.ZoneBounds()
		upper, err := intervalUpper(probe, end, through)
		if err != nil {
			return time.Time{}, err
		}
		candidate, found, err := lookupBetween(expression, time.UTC, shiftSeconds(cursor, int64(offset)), shiftSeconds(upper, int64(offset)), factory)
		if err != nil {
			return time.Time{}, err
		}
		if found {
			actual := shiftSeconds(candidate, -int64(offset))
			if !actual.After(cursor) || actual.After(upper) || !wallPoint(actual, location).Equal(candidate) {
				return time.Time{}, fmt.Errorf("%w: calendar candidate failed its native interval round trip", ErrLookup)
			}
			first, foldErr := firstRealization(actual, location)
			if foldErr != nil {
				return time.Time{}, foldErr
			}
			if first {
				return actual.UTC(), nil
			}
			cursor = actual
			continue
		}
		if !upper.After(cursor) {
			return time.Time{}, fmt.Errorf("%w: native interval did not progress", ErrLookup)
		}
		cursor = upper
	}
	return time.Time{}, ErrRangeExhausted
}

func intervalUpper(probe, end, through time.Time) (time.Time, error) {
	if !end.IsZero() && !end.After(probe) {
		// Some Go POSIX-footer lookups synthesize a 365-day year-end bound,
		// leaving a leap year's final UTC day outside its reported interval.
		// Recognize only that exact tail; proper forward bounds bypass this.
		yearStart := time.Date(probe.UTC().Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		nextYear := yearStart.AddDate(1, 0, 0)
		leapYear := nextYear.Sub(yearStart) == 366*24*time.Hour
		if !leapYear || !end.Equal(yearStart.Add(365*24*time.Hour)) || !probe.Before(nextYear) {
			return time.Time{}, fmt.Errorf("%w: native interval ends before its probe", ErrLookup)
		}
		end = nextYear
	}
	if !end.IsZero() && !end.After(through) {
		return end.Add(-time.Nanosecond), nil
	}
	return through, nil
}

func shiftSeconds(value time.Time, seconds int64) time.Time {
	return time.Unix(value.Unix()+seconds, int64(value.Nanosecond())).UTC()
}
