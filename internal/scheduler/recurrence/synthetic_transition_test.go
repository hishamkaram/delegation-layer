package recurrence

import (
	"testing"
	"time"
)

func TestSyntheticGapThenBackwardFixtureClocks(t *testing.T) {
	location := gapBackwardFixture(t)
	assertFixtureClocks(t, location, []fixtureClock{
		{"1999-12-31T23:59:00Z", "1999-12-31T23:59:00Z", 0},
		{"2000-01-01T00:00:00Z", "2000-01-01T01:00:00+01:00", 3600},
		{"2000-01-01T00:29:00Z", "2000-01-01T01:29:00+01:00", 3600},
		{"2000-01-01T00:30:00Z", "2000-01-01T00:30:00Z", 0},
		{"2000-01-01T01:00:00Z", "2000-01-01T01:00:00Z", 0},
	})
	probe := instant(t, "2000-01-01T00:29:00Z").In(location)
	start, end := probe.ZoneBounds()
	if !start.Equal(instant(t, "2000-01-01T00:00:00Z")) || !end.Equal(instant(t, "2000-01-01T00:30:00Z")) {
		t.Fatalf("authored +1h interval bounds: (%s,%s)", start, end)
	}
	for _, test := range []struct {
		utc   string
		first bool
	}{{"2000-01-01T00:30:00Z", true}, {"2000-01-01T00:00:00Z", true}, {"2000-01-01T01:00:00Z", false}} {
		first, err := firstRealization(instant(t, test.utc), location)
		if err != nil || first != test.first {
			t.Fatalf("fixture %s: first=%v err=%v; want %v", test.utc, first, err, test.first)
		}
	}
}

func TestNextSyntheticGapThenBackward(t *testing.T) {
	location := gapBackwardFixture(t)
	after := instant(t, "2000-01-01T00:29:00Z")
	want := instant(t, "2000-01-01T00:30:00Z")
	for _, expression := range []string{"* * * * *", "30 0 * * *"} {
		got, err := nextInZone(expression, location, after, after.Add(24*time.Hour), defaultCron)
		if err != nil || !got.Equal(want) {
			t.Fatalf("Next(%q): %s, %v; want first-realized local 00:30 at %s", expression, got, err, want)
		}
	}
}
