package recurrence

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestNextCalendarAndDOMWeekdaySemantics(t *testing.T) {
	for _, test := range []struct {
		expression, after, want string
	}{
		{"* * * * *", "2026-12-31T23:59:00Z", "2027-01-01T00:00:00Z"},
		{"5/20 * * * *", "2026-01-01T00:06:00Z", "2026-01-01T00:25:00Z"},
		{"0 0 1 JAN *", "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"0 0 * * WED", "2026-06-01T00:00:00Z", "2026-06-03T00:00:00Z"},
		{"0 0 * * MON", "2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"},
		{"0 0 */1 * MON", "2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"},
		{"0 0 */2 * MON", "2026-06-01T00:00:00Z", "2026-06-03T00:00:00Z"},
		{"0 0 1-31 * MON", "2026-06-01T00:00:00Z", "2026-06-02T00:00:00Z"},
		{"0 0 15 * MON", "2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"},
		{"0 0 *,15 * MON", "2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"},
		{"0 0 29 2 *", "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"0 0 29 2 *", "2027-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
	} {
		got, err := Next(test.expression, "UTC", instant(t, test.after))
		if err != nil || !got.Equal(instant(t, test.want)) || got.Location() != time.UTC {
			t.Fatalf("Next(%q, %s) = %s, %v; want %s", test.expression, test.after, got, err, test.want)
		}
	}
}

func TestNextRejectsWholeExpressionAndTimezone(t *testing.T) {
	after := instant(t, "2026-01-01T00:00:00Z")
	for _, expression := range []string{"61 0 * * *", "0 25 * * *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 7", "0 0 * * BAD", "*/0 * * * *", "0 0 31 FEB BAD", "0 0 ? * MON", "0,,1 * * * *"} {
		if _, err := Next(expression, "UTC", after); !errors.Is(err, ErrInvalidExpression) {
			t.Fatalf("invalid expression %q: %v", expression, err)
		}
	}
	for _, zone := range []string{"", "Local", "No/Such_Zone"} {
		if _, err := Next("* * * * *", zone, after); !errors.Is(err, ErrInvalidTimezone) {
			t.Fatalf("invalid zone %q: %v", zone, err)
		}
	}
}

func TestNextStorageBoundsAndExhaustion(t *testing.T) {
	minimum := contract.InstantFromUnixNanos(math.MinInt64)
	maximum := contract.InstantFromUnixNanos(math.MaxInt64)
	for _, after := range []time.Time{minimum.Add(-time.Nanosecond), maximum, maximum.Add(time.Nanosecond)} {
		if _, err := Next("* * * * *", "UTC", after); !errors.Is(err, ErrRangeExhausted) {
			t.Fatalf("range boundary %s: %v", after, err)
		}
	}
	for _, test := range []struct{ after, want string }{
		{"1677-09-21T00:12:43.145224192Z", "1677-09-21T00:13:00Z"},
		{"2262-04-11T23:46:59Z", "2262-04-11T23:47:00Z"},
	} {
		got, err := Next("* * * * *", "UTC", instant(t, test.after))
		if err != nil || !got.Equal(instant(t, test.want)) {
			t.Fatalf("boundary lookup = %s, %v; want %s", got, err, test.want)
		}
	}
	for _, expression := range []string{"* * * * *", "0 0 31 2 *"} {
		if _, err := Next(expression, "UTC", instant(t, "2262-04-11T23:47:00Z")); !errors.Is(err, ErrRangeExhausted) {
			t.Fatalf("remaining supported tail %q: %v", expression, err)
		}
	}
}

func TestNextPreservesResolvedZoneAcrossNativeChanges(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	zone, err := ResolveTimezone("")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "Asia/Tokyo")
	got, err := Next("0 9 * * *", zone, instant(t, "2026-01-01T00:00:00Z"))
	if err != nil || !got.Equal(instant(t, "2026-01-01T14:00:00Z")) {
		t.Fatalf("saved zone changed: %s, %v", got, err)
	}
}
