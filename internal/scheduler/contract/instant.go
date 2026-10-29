package contract

import (
	"fmt"
	"math"
	"time"
)

// ValidateInstant accepts only a caller-supplied canonical domain instant: UTC
// and inside the int64 Unix-nanosecond range the store persists. Requiring the
// UTC location also excludes a monotonic clock reading, because only time.Now
// creates one and converting to UTC strips it. Outside that range
// time.Time.UnixNano is undefined, so the range is checked before conversion.
func ValidateInstant(name string, instant time.Time) error {
	if instant.Location() != time.UTC {
		return fmt.Errorf("%s must be a UTC instant", name)
	}
	if instant.Before(minimumInstant()) || instant.After(maximumInstant()) {
		return fmt.Errorf("%s is outside the int64 Unix nanosecond range", name)
	}
	return nil
}

// UnixNanos converts a validated instant to its exact stored representation.
func UnixNanos(name string, instant time.Time) (int64, error) {
	if err := ValidateInstant(name, instant); err != nil {
		return 0, err
	}
	return instant.UnixNano(), nil
}

// InstantFromUnixNanos restores the canonical instant for a stored value. Every
// int64 maps to a valid instant, so the round trip loses no precision.
func InstantFromUnixNanos(nanos int64) time.Time {
	return time.Unix(0, nanos).UTC()
}

func minimumInstant() time.Time { return InstantFromUnixNanos(math.MinInt64) }

func maximumInstant() time.Time { return InstantFromUnixNanos(math.MaxInt64) }
