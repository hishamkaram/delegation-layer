package taskdir_test

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// briefSizes are the fixture brief sizes both brief measurements cover.
var briefSizes = []struct {
	name  string
	bytes int
}{
	{"1KiB", 1 << 10}, {"64KiB", 64 << 10}, {"1MiB", 1 << 20}, {"8MiB", 8 << 20},
}

// openBriefCollector returns a collector TaskDir over a held-lease fixture
// task with a brief of size bytes. Setup stays outside the measured loop.
func openBriefCollector(tb testing.TB, size int) *taskdir.TaskDir {
	tb.Helper()
	cohort := testsupport.NewHeldCollectionCohort(tb, size, 1)
	return cohort.OpenCollector(tb, 0).TaskDir()
}

// BenchmarkPreparedRecordsM1 measures the public PreparedRecords call, which
// validates the whole prepared set including the full brief.
func BenchmarkPreparedRecordsM1(b *testing.B) {
	for _, size := range briefSizes {
		b.Run("brief="+size.name, func(b *testing.B) {
			td := openBriefCollector(b, size.bytes)
			b.ReportAllocs()
			for b.Loop() {
				req, meta, err := td.PreparedRecords()
				if err != nil || req == nil || meta == nil || req.BriefLength != int64(size.bytes) {
					b.Fatalf("PreparedRecords = %v, %v, %v", req, meta, err)
				}
			}
		})
	}
}

// BenchmarkReadBriefM1 measures the public ReadBrief call, the only consumer
// of the validated brief bytes.
func BenchmarkReadBriefM1(b *testing.B) {
	for _, size := range briefSizes {
		b.Run("brief="+size.name, func(b *testing.B) {
			td := openBriefCollector(b, size.bytes)
			b.ReportAllocs()
			for b.Loop() {
				brief, err := td.ReadBrief()
				if err != nil || len(brief) != size.bytes {
					b.Fatalf("ReadBrief = %d bytes, %v", len(brief), err)
				}
			}
		})
	}
}
