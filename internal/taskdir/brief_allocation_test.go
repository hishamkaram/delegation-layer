package taskdir_test

import (
	"runtime"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	// These are fixture budgets for the same fixed small controls, not
	// production limits. The brief size must not drive allocation: validating
	// an 8MiB brief may not allocate a brief-sized copy per call.
	allocationCalls          = 4
	maxLargeBriefAllocation  = 1 << 20
	maxBriefAllocationGrowth = 256 << 10
	smallBriefBytes          = 1 << 10
	largeBriefBytes          = 8 << 20
)

// TestPreparedRecordsAllocationDoesNotScaleWithBrief is not parallel: it
// reads process-wide runtime.MemStats.TotalAlloc deltas around public
// PreparedRecords calls and a concurrent test would pollute them.
func TestPreparedRecordsAllocationDoesNotScaleWithBrief(t *testing.T) {
	small := preparedRecordsAllocationPerCall(t, smallBriefBytes)
	large := preparedRecordsAllocationPerCall(t, largeBriefBytes)
	if large >= maxLargeBriefAllocation {
		t.Errorf("8MiB brief allocates %d bytes per PreparedRecords call, want < %d", large, maxLargeBriefAllocation)
	}
	if growth := int64(large) - int64(small); growth >= maxBriefAllocationGrowth {
		t.Errorf("allocation grows %d bytes per call from 1KiB (%d) to 8MiB (%d) briefs, want < %d", growth, small, large, maxBriefAllocationGrowth)
	}
}

// preparedRecordsAllocationPerCall returns the mean bytes allocated per
// PreparedRecords call over allocationCalls calls after one warm-up call.
func preparedRecordsAllocationPerCall(t *testing.T, briefBytes int) uint64 {
	t.Helper()
	td := openBriefCollector(t, briefBytes)
	call := func(td *taskdir.TaskDir) {
		if _, _, err := td.PreparedRecords(); err != nil {
			t.Fatal(err)
		}
	}
	call(td)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range allocationCalls {
		call(td)
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / allocationCalls
}
