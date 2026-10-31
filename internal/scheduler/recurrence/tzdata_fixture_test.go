package recurrence

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

type fixtureZone struct {
	offset int32
	dst    bool
	abbrev byte
}

type fixtureTransition struct {
	at   int64
	zone byte
}

// The TZif v2 fixture contains both required data blocks and a POSIX footer.
// All authored transitions fit the first block's signed 32-bit timestamps.
// Public LoadLocationFromTZData and independent clock fixtures check the data.
func tzdataFixture(t *testing.T, name string, zones []fixtureZone, transitions []fixtureTransition, footer string) *time.Location {
	t.Helper()
	for i, transition := range transitions {
		if transition.at < math.MinInt32 || transition.at > math.MaxInt32 || int(transition.zone) >= len(zones) {
			t.Fatal("invalid authored TZif transition")
		}
		if i > 0 && transition.at <= transitions[i-1].at {
			t.Fatal("authored TZif transitions are not strictly ordered")
		}
	}
	data := append(tzdataBlock(zones, transitions, false), tzdataBlock(zones, transitions, true)...)
	data = append(data, []byte("\n"+footer+"\n")...)
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func tzdataBlock(zones []fixtureZone, transitions []fixtureTransition, wide bool) []byte {
	data := append([]byte("TZif2"), make([]byte, 15)...)
	for _, count := range []uint32{0, 0, 0, uint32(len(transitions)), uint32(len(zones)), 8} {
		data = binary.BigEndian.AppendUint32(data, count)
	}
	for _, transition := range transitions {
		if wide {
			data = binary.BigEndian.AppendUint64(data, uint64(transition.at))
		} else {
			data = binary.BigEndian.AppendUint32(data, uint32(transition.at))
		}
	}
	for _, transition := range transitions {
		data = append(data, transition.zone)
	}
	for _, zone := range zones {
		// TZif stores the signed UTC offset in a big-endian two's-complement word.
		data = binary.BigEndian.AppendUint32(data, uint32(zone.offset))
		var dst byte
		if zone.dst {
			dst = 1
		}
		data = append(data, dst, zone.abbrev)
	}
	return append(data, []byte("STD\x00DST\x00")...)
}

func posixFooterFixture(t *testing.T, southern bool) *time.Location {
	t.Helper()
	zones := []fixtureZone{{offset: -5 * 3600}, {offset: -4 * 3600, dst: true, abbrev: 4}}
	footer := "STD5DST4,M3.2.0/2,M11.1.0/2"
	var initial byte
	if southern {
		zones = []fixtureZone{{offset: 10 * 3600}, {offset: 11 * 3600, dst: true, abbrev: 4}}
		footer = "STD-10DST-11,M10.1.0/2,M4.1.0/3"
		initial = 1
	}
	transitions := []fixtureTransition{{at: instant(t, "2000-01-01T00:00:00Z").Unix(), zone: initial}}
	return tzdataFixture(t, "fixture/POSIX", zones, transitions, footer)
}

func gapBackwardFixture(t *testing.T) *time.Location {
	t.Helper()
	zones := []fixtureZone{{}, {offset: 3600, dst: true, abbrev: 4}}
	transitions := []fixtureTransition{
		{at: instant(t, "2000-01-01T00:00:00Z").Unix(), zone: 1},
		{at: instant(t, "2000-01-01T00:30:00Z").Unix()},
	}
	return tzdataFixture(t, "fixture/GapThenBackward", zones, transitions, "")
}

type fixtureClock struct {
	utc, local string
	offset     int
}

func assertFixtureClocks(t *testing.T, location *time.Location, clocks []fixtureClock) {
	t.Helper()
	for _, clock := range clocks {
		utc, local := instant(t, clock.utc), instant(t, clock.local)
		if !local.Equal(utc) {
			t.Fatalf("authored local clock %s does not round trip to %s", clock.local, clock.utc)
		}
		actual := utc.In(location)
		_, offset := actual.Zone()
		if actual.Format(time.RFC3339) != clock.local || offset != clock.offset {
			t.Fatalf("fixture %s at %s: local=%s offset=%d; want %s offset=%d", location, clock.utc, actual.Format(time.RFC3339), offset, clock.local, clock.offset)
		}
	}
}

func TestPOSIXFooterFixtureClocks(t *testing.T) {
	north := []fixtureClock{
		{"2096-01-01T00:00:00Z", "2095-12-31T19:00:00-05:00", -5 * 3600},
		{"2096-07-01T00:00:00Z", "2096-06-30T20:00:00-04:00", -4 * 3600},
		{"2096-12-31T00:00:00Z", "2096-12-30T19:00:00-05:00", -5 * 3600},
		{"2096-12-31T12:00:00Z", "2096-12-31T07:00:00-05:00", -5 * 3600},
		{"2097-01-01T00:00:00Z", "2096-12-31T19:00:00-05:00", -5 * 3600},
	}
	south := []fixtureClock{
		{"2096-01-01T00:00:00Z", "2096-01-01T11:00:00+11:00", 11 * 3600},
		{"2096-07-01T00:00:00Z", "2096-07-01T10:00:00+10:00", 10 * 3600},
		{"2096-12-31T00:00:00Z", "2096-12-31T11:00:00+11:00", 11 * 3600},
		{"2096-12-31T12:00:00Z", "2096-12-31T23:00:00+11:00", 11 * 3600},
		{"2097-01-01T00:00:00Z", "2097-01-01T11:00:00+11:00", 11 * 3600},
	}
	assertFixtureClocks(t, posixFooterFixture(t, false), north)
	assertFixtureClocks(t, posixFooterFixture(t, true), south)
}

func TestPOSIXFooterFixtureBounds(t *testing.T) {
	legacyEnd := instant(t, "2096-12-31T00:00:00Z")
	for _, southern := range []bool{false, true} {
		location := posixFooterFixture(t, southern)
		for _, utc := range []string{"2096-12-31T00:00:00Z", "2096-12-31T12:00:00Z", "2097-01-01T00:00:00Z"} {
			probe := instant(t, utc).In(location)
			start, end := probe.ZoneBounds()
			if start.After(probe) || end.IsZero() {
				t.Fatalf("unexpected fixture interval at %s: (%s,%s)", utc, start, end)
			}
			if !end.After(probe) {
				if probe.UTC().Year() != 2096 || !end.Equal(legacyEnd) {
					t.Fatalf("unrecognized fixture bound at %s: %s", utc, end)
				}
			}
			// A corrected Go may already return a proper forward bound. Do not
			// require the native bug to remain for the regression fixture to work.
			t.Logf("southern=%v UTC=%s end==probe:%v end<probe:%v", southern, utc, end.Equal(probe), end.Before(probe))
		}
	}
}
