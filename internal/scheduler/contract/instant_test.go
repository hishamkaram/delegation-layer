package contract

import (
	"math"
	"testing"
	"time"
)

func TestValidateInstantAcceptsOnlyCanonicalUTCInStoredRange(t *testing.T) {
	cases := []struct {
		name    string
		instant time.Time
		valid   bool
	}{
		{name: "utc", instant: at(1_700_000_000), valid: true},
		{name: "nanosecond precision", instant: time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC), valid: true},
		{name: "minimum", instant: InstantFromUnixNanos(math.MinInt64), valid: true},
		{name: "maximum", instant: InstantFromUnixNanos(math.MaxInt64), valid: true},
		{name: "before minimum", instant: InstantFromUnixNanos(math.MinInt64).Add(-time.Nanosecond)},
		{name: "after maximum", instant: InstantFromUnixNanos(math.MaxInt64).Add(time.Nanosecond)},
		{name: "zero", instant: time.Time{}},
		{name: "far future", instant: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "local", instant: time.Unix(1_700_000_000, 0)},
		{name: "fixed zone named UTC", instant: time.Unix(1_700_000_000, 0).In(time.FixedZone("UTC", 0))},
		{name: "monotonic local reading", instant: time.Now()},
		{name: "reading converted to UTC", instant: time.Now().UTC(), valid: true},
	}
	for _, testCase := range cases {
		err := ValidateInstant("instant", testCase.instant)
		if (err == nil) != testCase.valid {
			t.Errorf("%s: err=%v, want valid=%v", testCase.name, err, testCase.valid)
		}
	}
}

func TestUnixNanosRoundTripsWithoutPrecisionLoss(t *testing.T) {
	for _, nanos := range []int64{math.MinInt64, -1, 0, 1, 1_700_000_000_123_456_789, math.MaxInt64} {
		instant := InstantFromUnixNanos(nanos)
		got, err := UnixNanos("instant", instant)
		if err != nil || got != nanos {
			t.Fatalf("round trip %d: got %d err=%v", nanos, got, err)
		}
	}
	overflow := InstantFromUnixNanos(math.MaxInt64).Add(time.Nanosecond)
	if got, err := UnixNanos("instant", overflow); err == nil {
		t.Fatalf("overflowing instant converted to %d", got)
	}
}
