package recurrence

import (
	"testing"
	"time"
)

func partialMinuteFixture(t *testing.T) *time.Location {
	t.Helper()
	zones := []fixtureZone{{offset: 30}, {offset: 80, dst: true, abbrev: 4}, {}}
	transitions := []fixtureTransition{
		{at: instant(t, "2026-01-01T00:00:10Z").Unix(), zone: 1},
		{at: instant(t, "2026-01-01T00:00:50Z").Unix(), zone: 2},
	}
	return tzdataFixture(t, "fixture/PartialMinute", zones, transitions, "")
}

func assertPartialMinuteFixture(t *testing.T, location *time.Location) {
	t.Helper()
	for _, clock := range []struct {
		utc, wall string
		offset    int
	}{
		{"2026-01-01T00:00:09Z", "2026-01-01T00:00:39", 30},
		{"2026-01-01T00:00:10Z", "2026-01-01T00:01:30", 80},
		{"2026-01-01T00:00:30Z", "2026-01-01T00:01:50", 80},
		{"2026-01-01T00:00:49.999999999Z", "2026-01-01T00:02:09.999999999", 80},
		{"2026-01-01T00:00:50Z", "2026-01-01T00:00:50", 0},
		{"2026-01-01T00:00:55Z", "2026-01-01T00:00:55", 0},
		{"2026-01-01T00:01:00Z", "2026-01-01T00:01:00", 0},
	} {
		local := instant(t, clock.utc).In(location)
		_, offset := local.Zone()
		if offset != clock.offset || local.Format("2006-01-02T15:04:05.999999999") != clock.wall {
			t.Fatalf("fixture at %s: local=%s offset=%d; want %s offset=%d", clock.utc, local, offset, clock.wall, clock.offset)
		}
	}
}

func TestNextPreservesFirstExactWallPoint(t *testing.T) {
	location := partialMinuteFixture(t)
	assertPartialMinuteFixture(t, location)
	after := instant(t, "2026-01-01T00:00:55Z")
	want := instant(t, "2026-01-01T00:01:00Z")
	through := instant(t, "2026-01-02T00:02:00Z")
	got, err := nextInZone("1 0 * * *", location, after, through, defaultCron)
	if err != nil || !got.Equal(want) {
		t.Fatalf("partial local minute suppressed the first exact HH:MM:00: next=%s err=%v; want %s", got, err, want)
	}
}

func TestWallPointRetainsSecondsAndNanoseconds(t *testing.T) {
	location := partialMinuteFixture(t)
	for _, test := range []struct{ utc, wall string }{
		{"2026-01-01T00:00:30Z", "2026-01-01T00:01:50Z"},
		{"2026-01-01T00:00:30.000000001Z", "2026-01-01T00:01:50.000000001Z"},
		{"2026-01-01T00:01:00Z", "2026-01-01T00:01:00Z"},
	} {
		if got := wallPoint(instant(t, test.utc), location); !got.Equal(instant(t, test.wall)) {
			t.Fatalf("exact wall point at %s = %s; want %s", test.utc, got, test.wall)
		}
	}
}
