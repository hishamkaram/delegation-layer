package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Synthetic records use the real validators and read-only source. There are no
// providers, executable copies, production credentials or per-record fsyncs.
func syntheticSource(b *testing.B, count int) sourceFixture {
	b.Helper()
	f := newSource(b)
	require(b, os.Mkdir(filepath.Join(f.store.Root, "tasks"), 0o700))
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	brief := []byte("not-indexed")
	for i := range count {
		id := fmt.Sprintf("%032x", i+1)
		request := task.TaskRecord{
			SchemaVersion: task.SchemaVersion, RootID: f.store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: f.cwd,
			RequestedConfig: config, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
		}
		data, err := task.MarshalCanonical(request)
		require(b, err)
		meta := task.MetaRecord{
			SchemaVersion: task.SchemaVersion, RootID: f.store.RootID, TaskID: id, SpecSHA256: task.ComputeSHA256(data), RequestedConfig: config,
			Predicate: task.FixturePredicateRef(), EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("effective"))}, Containment: "fixture-only", Approval: "never",
			ProviderExecutable: filepath.Join(f.cwd, "provider"), ProviderVersion: "fixture-v1", PublisherBuild: "test", PublisherVersion: "test",
			SupervisorConfig: task.SupervisorRef{ConfigPath: filepath.Join(f.cwd, "pueue.yml"), ConfigDigest: task.ComputeSHA256([]byte("pueue")), Endpoint: filepath.Join(f.cwd, "socket"), ObservedVersion: "fixture-v1"}, CreatedAt: "2026-01-01T00:00:00Z",
		}
		require(b, task.ValidateTaskRecord(&request))
		require(b, task.ValidateMetaRecord(&meta))
		metaBytes, err := task.MarshalCanonical(meta)
		require(b, err)
		directory := filepath.Join(f.store.Root, "tasks", id)
		require(b, os.Mkdir(directory, 0o700))
		require(b, os.WriteFile(filepath.Join(directory, "task.json"), data, 0o600))
		require(b, os.WriteFile(filepath.Join(directory, "meta.json"), metaBytes, 0o600))
	}
	return f
}

func BenchmarkCatalogColdWarm(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			f := syntheticSource(b, count)
			c := openCatalog(b, f.options)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				start := time.Now()
				progress, err := c.Scan(b.Context())
				require(b, err)
				if progress.Indexed != int64(count) || progress.Incomplete {
					b.Fatalf("incomplete history: %+v", progress)
				}
				b.ReportMetric(float64(time.Since(start).Microseconds()), "scan-us")
				start = time.Now()
				page, err := c.Query(b.Context(), Query{Search: "fixture:test"})
				require(b, err)
				if len(page.Rows) != DefaultPageSize || page.Next == "" {
					b.Fatalf("warm query lost bounded page: %d", len(page.Rows))
				}
				b.ReportMetric(float64(time.Since(start).Microseconds()), "query-us")
			}
		})
	}
}
