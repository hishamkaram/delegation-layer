package recurrence

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-co-op/gocron/v2"
)

type cronFactory func() gocron.Cron

func defaultCron() gocron.Cron { return gocron.NewDefaultCron(false) }

// lookupBetween uses overlapping four-calendar-year probes to bridge the
// backend's five-year horizon. A negative result covers only (after, through].
func lookupBetween(expression string, location *time.Location, after, through time.Time, factory cronFactory) (time.Time, bool, error) {
	for probe := after; probe.Before(through); {
		cron := factory()
		if cron == nil {
			return time.Time{}, false, fmt.Errorf("%w: nil cron implementation", ErrLookup)
		}
		err := cron.IsValid(expression, location, probe)
		if err == nil {
			candidate := cron.Next(probe)
			if !candidate.After(probe) || candidate.Second() != 0 || candidate.Nanosecond() != 0 {
				return time.Time{}, false, fmt.Errorf("%w: backend candidate does not progress at minute precision", ErrLookup)
			}
			return candidate, !candidate.After(through), nil
		}
		if errors.Is(err, gocron.ErrCronJobParse) {
			return time.Time{}, false, fmt.Errorf("%w: %w", ErrInvalidExpression, err)
		}
		// Only the backend's no-candidate result permits another probe.
		// A joined concrete fault must not disappear behind this marker.
		if !onlyNoCandidate(err) {
			return time.Time{}, false, fmt.Errorf("%w: %w", ErrLookup, err)
		}
		next := probe.In(location).AddDate(4, 0, 0)
		if !next.After(probe) {
			return time.Time{}, false, fmt.Errorf("%w: calendar probe did not progress", ErrLookup)
		}
		// The failed five-year probe already covers this remaining tail. Do
		// not jump to through and inadvertently omit an unchecked interval.
		probe = next
	}
	return time.Time{}, false, nil
}

func onlyNoCandidate(err error) bool {
	if !errors.Is(err, gocron.ErrCronJobInvalid) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if !onlyNoCandidate(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return onlyNoCandidate(cause)
	}
	return true
}
