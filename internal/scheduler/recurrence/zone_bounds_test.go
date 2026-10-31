package recurrence

import (
	"errors"
	"testing"
	"time"
)

func TestIntervalUpperLeapYearPOSIXFooter(t *testing.T) {
	end := instant(t, "2096-12-31T00:00:00Z")
	nextYear := instant(t, "2097-01-01T00:00:00Z")
	for _, southern := range []bool{false, true} {
		location := posixFooterFixture(t, southern)
		for _, utc := range []string{"2096-12-31T00:00:00Z", "2096-12-31T12:00:00Z"} {
			probe := instant(t, utc).In(location)
			for _, through := range []time.Time{nextYear.Add(time.Hour), probe.Add(10 * time.Minute)} {
				want := nextYear.Add(-time.Nanosecond)
				if through.Before(want) {
					want = through
				}
				upper, err := intervalUpper(probe, end, through)
				if err != nil || !upper.Equal(want) {
					t.Errorf("southern=%v probe=%s through=%s: upper=%s err=%v; want %s", southern, probe, through, upper, err, want)
				}
			}
		}
	}
}

func TestIntervalUpperRejectsUnrecognizedBounds(t *testing.T) {
	location := posixFooterFixture(t, false)
	for _, test := range []struct {
		name, probe, end string
	}{
		{"ordinary non-leap year", "2097-12-31T12:00:00Z", "2097-12-31T00:00:00Z"},
		{"wrong leap-year day", "2096-12-31T12:00:00Z", "2096-12-30T00:00:00Z"},
		{"one nanosecond early", "2096-12-31T12:00:00Z", "2096-12-30T23:59:59.999999999Z"},
		{"one nanosecond late", "2096-12-31T12:00:00Z", "2096-12-31T00:00:00.000000001Z"},
		{"UTC year differs from local leap year", "2097-01-01T00:00:00Z", "2096-12-31T00:00:00Z"},
		{"arbitrary older end", "2096-12-31T12:00:00Z", "2095-12-31T00:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := instant(t, test.probe).In(location)
			_, err := intervalUpper(probe, instant(t, test.end), probe.Add(48*time.Hour))
			if !errors.Is(err, ErrLookup) {
				t.Fatalf("malformed bound was accepted: %v", err)
			}
		})
	}
}

func TestIntervalUpperPreservesFutureAndUnboundedEnds(t *testing.T) {
	probe := instant(t, "2096-12-30T23:59:00Z")
	end := instant(t, "2096-12-31T00:00:00Z")
	through := instant(t, "2097-01-01T01:00:00Z")
	upper, err := intervalUpper(probe, end, through)
	if err != nil || !upper.Equal(end.Add(-time.Nanosecond)) {
		t.Fatalf("a future 365-day boundary was extended: upper=%s err=%v", upper, err)
	}
	upper, err = intervalUpper(probe, time.Time{}, through)
	if err != nil || !upper.Equal(through) {
		t.Fatalf("unbounded interval: upper=%s err=%v", upper, err)
	}
}

func TestNextPOSIXFooterYearEnd(t *testing.T) {
	through := instant(t, "2097-01-02T00:00:00Z")
	for _, southern := range []bool{false, true} {
		location := posixFooterFixture(t, southern)
		for _, test := range []struct{ after, want string }{
			{"2096-12-30T23:59:00Z", "2096-12-31T00:00:00Z"},
			{"2096-12-31T00:00:00Z", "2096-12-31T00:01:00Z"},
			{"2096-12-31T12:00:00Z", "2096-12-31T12:01:00Z"},
			{"2096-12-31T23:59:00Z", "2097-01-01T00:00:00Z"},
			{"2097-01-01T00:00:00Z", "2097-01-01T00:01:00Z"},
		} {
			got, err := nextInZone("* * * * *", location, instant(t, test.after), through, defaultCron)
			if err != nil || !got.Equal(instant(t, test.want)) {
				t.Errorf("southern=%v after=%s: next=%s err=%v; want %s", southern, test.after, got, err, test.want)
			}
		}
	}
}
