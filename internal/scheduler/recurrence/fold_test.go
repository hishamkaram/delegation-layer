package recurrence

import (
	"testing"
	"time"
)

func TestFirstRealizationUsesNativeIntervals(t *testing.T) {
	for _, test := range []struct {
		zone, first, second, wall string
	}{
		{"America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", "2026-11-01T01:30:00Z"},
		{"Australia/Lord_Howe", "2026-04-04T14:45:00Z", "2026-04-04T15:15:00Z", "2026-04-05T01:45:00Z"},
		// tzdb 2026f canceled Cuba's projected 2026 rollback; use its historical midnight fold.
		{"America/Havana", "2024-11-03T04:30:00Z", "2024-11-03T05:30:00Z", "2024-11-03T00:30:00Z"},
		{"Pacific/Kwajalein", "1969-09-30T01:00:00Z", "1969-10-01T00:00:00Z", "1969-09-30T12:00:00Z"},
	} {
		t.Run(test.zone, func(t *testing.T) {
			location, err := time.LoadLocation(test.zone)
			if err != nil {
				t.Fatal(err)
			}
			first, second, wall := instant(t, test.first), instant(t, test.second), instant(t, test.wall)
			if !wallPoint(first, location).Equal(wall) || !wallPoint(second, location).Equal(wall) {
				t.Fatal("fixture instants do not name the documented repeated local minute")
			}
			for _, candidate := range []time.Time{first, second} {
				got, err := firstRealization(candidate, location)
				if err != nil || got != candidate.Equal(first) {
					t.Fatalf("candidate %s first=%v err=%v", candidate, got, err)
				}
			}
		})
	}
}
