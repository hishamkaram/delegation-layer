package recurrence

import "testing"

func TestNextFirstFoldIncludingQueriesBetweenRealizations(t *testing.T) {
	for _, test := range []struct {
		name, expression, zone, after, want string
	}{
		{"before first", "30 1 * * *", "America/New_York", "2026-11-01T05:29:59Z", "2026-11-01T05:30:00Z"},
		{"after first", "30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
		{"between folds", "30 1 * * *", "America/New_York", "2026-11-01T06:15:00Z", "2026-11-02T06:30:00Z"},
		{"first minute remains eligible", "* * * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-01T05:31:00Z"},
		{"every minute rejects repeated hour", "* * * * *", "America/New_York", "2026-11-01T05:59:00Z", "2026-11-01T07:00:00Z"},
		{"inside repeated hour", "* * * * *", "America/New_York", "2026-11-01T06:15:00Z", "2026-11-01T07:00:00Z"},
		{"transition boundary", "* * * * *", "America/New_York", "2026-11-01T05:59:59.999999999Z", "2026-11-01T07:00:00Z"},
		{"half hour before first", "45 1 * * *", "Australia/Lord_Howe", "2026-04-04T14:44:59Z", "2026-04-04T14:45:00Z"},
		{"half hour between", "45 1 * * *", "Australia/Lord_Howe", "2026-04-04T15:00:00Z", "2026-04-05T15:15:00Z"},
		{"half hour every minute", "* * * * *", "Australia/Lord_Howe", "2026-04-04T14:59:00Z", "2026-04-04T15:30:00Z"},
		// Historical Havana midnight fold; tzdb 2026f canceled the projected 2026 rollback.
		{"midnight first", "30 0 * * *", "America/Havana", "2024-11-03T04:29:59Z", "2024-11-03T04:30:00Z"},
		{"midnight between", "30 0 * * *", "America/Havana", "2024-11-03T05:00:00Z", "2024-11-04T05:30:00Z"},
		{"repeated date", "0 12 * * *", "Pacific/Kwajalein", "1969-09-30T13:00:00Z", "1969-10-02T00:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Next(test.expression, test.zone, instant(t, test.after))
			if err != nil || !got.Equal(instant(t, test.want)) {
				t.Fatalf("Next = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestNextSkipsAbsentMinutesAndDates(t *testing.T) {
	for _, test := range []struct {
		name, expression, zone, after, want string
	}{
		{"missing hour", "30 2 * * *", "America/New_York", "2026-03-08T06:59:00Z", "2026-03-09T06:30:00Z"},
		{"missing half hour", "15 2 * * *", "Australia/Lord_Howe", "2026-10-03T15:00:00Z", "2026-10-04T15:15:00Z"},
		{"missing midnight", "30 0 * * *", "America/Sao_Paulo", "2018-11-03T03:30:00Z", "2018-11-05T02:30:00Z"},
		{"skipped date daily", "0 0 * * *", "Pacific/Apia", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z"},
		{"skipped date restricted", "0 0 30 12 *", "Pacific/Apia", "2011-12-29T10:00:00Z", "2012-12-29T10:00:00Z"},
		{"date skip boundary", "* * * * *", "Pacific/Apia", "2011-12-30T09:59:59.999999999Z", "2011-12-30T10:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Next(test.expression, test.zone, instant(t, test.after))
			if err != nil || !got.Equal(instant(t, test.want)) {
				t.Fatalf("Next = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestNextNativeCalendarBoundaries(t *testing.T) {
	for _, test := range []struct {
		expression, after, want string
	}{
		{"0 0 29 2 *", "2096-02-29T05:00:00Z", "2104-02-29T05:00:00Z"},
		{"0 0 * * *", "2028-12-30T05:00:00Z", "2028-12-31T05:00:00Z"},
		{"0 0 * * *", "2028-12-31T05:00:00Z", "2029-01-01T05:00:00Z"},
		{"0 0 * * *", "1860-01-01T00:00:00Z", "1860-01-01T04:56:02Z"},
	} {
		got, err := Next(test.expression, "America/New_York", instant(t, test.after))
		if err != nil || !got.Equal(instant(t, test.want)) {
			t.Fatalf("Next(%q, %s) = %s, %v; want %s", test.expression, test.after, got, err, test.want)
		}
	}
}
