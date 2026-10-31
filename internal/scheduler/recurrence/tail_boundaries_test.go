package recurrence

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

type fixtureWallClock struct{ utc, wall string }

func assertFixedOffsetClocks(t *testing.T, location *time.Location, offset int32, clocks []fixtureWallClock) {
	t.Helper()
	const layout = "2006-01-02T15:04:05.999999999"
	for _, clock := range clocks {
		utc := instant(t, clock.utc)
		wall, err := time.Parse(layout, clock.wall)
		if err != nil {
			t.Fatal(err)
		}
		local := utc.In(location)
		_, actualOffset := local.Zone()
		if actualOffset != int(offset) || local.Format(layout) != clock.wall {
			t.Fatalf("fixture at %s: local=%s offset=%d; want %s offset=%d", utc, local.Format(layout), actualOffset, clock.wall, offset)
		}
		roundTrip := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), location)
		if !roundTrip.Equal(utc) {
			t.Fatalf("fixture wall clock %s round trips to %s; want %s", clock.wall, roundTrip, utc)
		}
		start, end := local.ZoneBounds()
		if !start.IsZero() || !end.IsZero() {
			t.Fatalf("fixed fixture has a transition: (%s,%s)", start, end)
		}
	}
}

func TestNextSecondOffsetStoreTails(t *testing.T) {
	minimum := contract.InstantFromUnixNanos(math.MinInt64)
	maximum := contract.InstantFromUnixNanos(math.MaxInt64)
	for _, test := range []struct {
		name        string
		offset      int32
		first, last string
		clocks      []fixtureWallClock
	}{
		{
			name: "positive 30 seconds", offset: 30, first: "1677-09-21T00:13:30Z", last: "2262-04-11T23:46:30Z",
			clocks: []fixtureWallClock{
				{"1677-09-21T00:12:43.145224192Z", "1677-09-21T00:13:13.145224192"},
				{"1677-09-21T00:13:30Z", "1677-09-21T00:14:00"},
				{"2262-04-11T23:46:30Z", "2262-04-11T23:47:00"},
				{"2262-04-11T23:47:16.854775807Z", "2262-04-11T23:47:46.854775807"},
			},
		},
		{
			name: "negative 45 seconds", offset: -45, first: "1677-09-21T00:12:45Z", last: "2262-04-11T23:46:45Z",
			clocks: []fixtureWallClock{
				{"1677-09-21T00:12:43.145224192Z", "1677-09-21T00:11:58.145224192"},
				{"1677-09-21T00:12:45Z", "1677-09-21T00:12:00"},
				{"2262-04-11T23:46:45Z", "2262-04-11T23:46:00"},
				{"2262-04-11T23:47:16.854775807Z", "2262-04-11T23:46:31.854775807"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			location := tzdataFixture(t, "fixture/FixedSeconds", []fixtureZone{{offset: test.offset}}, nil, "")
			assertFixedOffsetClocks(t, location, test.offset, test.clocks)
			last := instant(t, test.last)
			for _, query := range []struct{ after, want time.Time }{
				{minimum, instant(t, test.first)}, {last.Add(-time.Nanosecond), last},
			} {
				got, err := nextInZone("* * * * *", location, query.after, maximum, defaultCron)
				if err != nil || !got.Equal(query.want) || got.Location() != time.UTC {
					t.Fatalf("after %s: next=%s err=%v; want %s", query.after, got, err, query.want)
				}
			}
			for _, after := range []time.Time{last, last.Add(time.Nanosecond), maximum} {
				if _, err := nextInZone("* * * * *", location, after, maximum, defaultCron); !errors.Is(err, ErrRangeExhausted) {
					t.Fatalf("remaining tail after %s: %v", after, err)
				}
			}
		})
	}
}

func TestSyntheticTransitionExactBounds(t *testing.T) {
	location := gapBackwardFixture(t)
	probe := instant(t, "2000-01-01T00:29:00Z").In(location)
	_, end := probe.ZoneBounds()
	if !end.Equal(instant(t, "2000-01-01T00:30:00Z")) {
		t.Fatalf("fixture transition is not the authored UTC boundary: %s", end)
	}
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		upper, err := intervalUpper(probe, end, end.Add(delta))
		if err != nil || !upper.Equal(end.Add(-time.Nanosecond)) {
			t.Fatalf("through=end%+s: upper=%s err=%v", delta, upper, err)
		}
	}
	for _, test := range []struct {
		name, after, through, want string
	}{
		{"through before end", "2000-01-01T00:29:00Z", "2000-01-01T00:29:59.999999999Z", ""},
		{"through equals end", "2000-01-01T00:29:00Z", "2000-01-01T00:30:00Z", "2000-01-01T00:30:00Z"},
		{"through after end", "2000-01-01T00:29:00Z", "2000-01-01T00:30:00.000000001Z", "2000-01-01T00:30:00Z"},
		{"after before end", "2000-01-01T00:29:59.999999999Z", "2000-01-01T00:31:00Z", "2000-01-01T00:30:00Z"},
		{"after equals end", "2000-01-01T00:30:00Z", "2000-01-01T00:31:00Z", "2000-01-01T00:31:00Z"},
		{"after past end", "2000-01-01T00:30:00.000000001Z", "2000-01-01T00:31:00Z", "2000-01-01T00:31:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := nextInZone("* * * * *", location, instant(t, test.after), instant(t, test.through), defaultCron)
			if test.want == "" {
				if !errors.Is(err, ErrRangeExhausted) {
					t.Fatalf("out-of-bound transition accepted: next=%s err=%v", got, err)
				}
				return
			}
			if err != nil || !got.Equal(instant(t, test.want)) {
				t.Fatalf("transition lookup: next=%s err=%v; want %s", got, err, test.want)
			}
		})
	}
}

func TestNextNamedZoneTailAndExhaustion(t *testing.T) {
	if _, err := Next("0 0 31 2 *", "America/New_York", instant(t, "2026-01-01T00:00:00Z")); !errors.Is(err, ErrRangeExhausted) {
		t.Fatalf("bounded calendar exhaustion lost its category: %v", err)
	}
	got, err := Next("0 0 * * *", "America/New_York", instant(t, "2040-12-30T06:00:00Z"))
	if err != nil || !got.Equal(instant(t, "2040-12-31T05:00:00Z")) {
		t.Fatalf("daily leap tail: next=%s err=%v; want 2040-12-31T05:00:00Z", got, err)
	}
}
