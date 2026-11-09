package taskdir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputActivityRequiresActualProviderStartAndNonemptyCapture(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	permit := consumeStart(t, td)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, td.WriteRawFiles("Delegate preparation diagnostic", ""))
	activity, err := td.ReadOutputActivityContext(t.Context())
	must(t, err)
	if activity != nil {
		t.Fatal("preparation bytes were called provider output")
	}
	td = preparedTask(t, store)
	active := consumeStart(t, td)
	t.Cleanup(func() { must(t, active.Release()) })
	must(t, td.RecordStarted(0))
	must(t, td.WriteRawFiles("", ""))
	activity, err = td.ReadOutputActivityContext(t.Context())
	must(t, err)
	if activity != nil {
		t.Fatal("empty capture creation was called output")
	}
	must(t, td.WriteRawFiles("partial bytes", "stderr bytes"))
	first := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	last := first.Add(time.Second)
	must(t, os.Chtimes(filepath.Join(td.Dir, "raw", "stdout"), first, first))
	must(t, os.Chtimes(filepath.Join(td.Dir, "raw", "stderr"), last, last))
	activity, err = td.ReadOutputActivityContext(t.Context())
	must(t, err)
	if activity == nil || activity.LastProviderOutputAt != "2026-11-09T12:00:01Z" || activity.Source != "capture_metadata" {
		t.Fatalf("activity=%+v", activity)
	}
}
