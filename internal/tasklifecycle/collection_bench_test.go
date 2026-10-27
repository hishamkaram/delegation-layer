package tasklifecycle_test

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// These benchmarks record measurements, not assertions. Every task holds a
// real runner lease and is never sealed, so each Collect call ends as a
// pending publication without any supervisor, catalog, or provider.
//
// One benchmark operation is one cohort operation: every collector in the
// cohort makes one public Collect call concurrently, each over its own
// Store and TaskDir handles, and all are joined before the operation ends.
// Only calls that end as a pending publication with no error count as
// completed; any other result fails the benchmark.

type collectionBriefSize struct {
	name  string
	bytes int
}

var collectionBriefSizes = []collectionBriefSize{
	{"1KiB", 1 << 10}, {"64KiB", 64 << 10}, {"1MiB", 1 << 20}, {"8MiB", 8 << 20},
}

// collectionCohortShape fixes collectors and tasks. Same-task collectors
// share one task; independent collectors each own one.
type collectionCohortShape struct {
	name       string
	collectors int
	tasks      int
}

var collectionCohortShapes = []collectionCohortShape{
	{"Single", 1, 1}, {"SameTask8", 8, 1}, {"IndependentTasks8", 8, 8},
}

// collectionUnits names the logical unit one Collect call completes. M2
// completes exactly one pass per call; M3 completes one watch of unknown
// pass count. The per-unit ns and B figures divide the cohort's elapsed time
// and allocation by completed calls: they are amortized throughput, not the
// latency of any single call. ns/op stays the cohort's elapsed time. M3 totals
// cover different amounts of work, so M3 is realism reporting only; M2's known
// passes and the allocation guard are authoritative.
type collectionUnits struct {
	perOp  string
	perNS  string
	perMem string
}

var (
	passUnits  = collectionUnits{"passes/op", "ns/pass", "B/pass"}
	watchUnits = collectionUnits{"watches/op", "ns/watch", "B/watch"}
)

// BenchmarkCollectM2WatchZero measures Collect with Watch 0: one full pass
// per completed call, including the initial PreparedRecords read.
func BenchmarkCollectM2WatchZero(b *testing.B) {
	runCollectionMatrix(b, 0, passUnits, false)
}

// BenchmarkCollectM3Watch300ms measures Collect with a 300ms watch. A watch
// holds an unknown number of passes, so results are per completed watch and
// are never normalized by an inferred pass count.
func BenchmarkCollectM3Watch300ms(b *testing.B) {
	runCollectionMatrix(b, 300*time.Millisecond, watchUnits, false)
}

// BenchmarkCollectM2HeapDiagnostic repeats M2 under a 10ms heap sampler. Its
// peak-heap-B and retained-heap-B are diagnostics for the same window as its
// timed operations; its ns/op carries the sampler's overhead and is not a
// timing measurement.
func BenchmarkCollectM2HeapDiagnostic(b *testing.B) {
	runCollectionMatrix(b, 0, passUnits, true)
}

func runCollectionMatrix(b *testing.B, watch time.Duration, units collectionUnits, sampleHeap bool) {
	for _, size := range collectionBriefSizes {
		for _, shape := range collectionCohortShapes {
			b.Run(fmt.Sprintf("brief=%s/cohort=%s", size.name, shape.name), func(b *testing.B) {
				benchmarkCollectCohort(b, size, shape, watch, units, sampleHeap)
			})
		}
	}
}

func benchmarkCollectCohort(b *testing.B, size collectionBriefSize, shape collectionCohortShape, watch time.Duration, units collectionUnits, sampleHeap bool) {
	cohort := testsupport.NewHeldCollectionCohort(b, size.bytes, shape.tasks)
	collectors := make([]*testsupport.CollectionHandle, shape.collectors)
	for index := range collectors {
		collectors[index] = cohort.OpenCollector(b, index%shape.tasks)
	}
	options := tasklifecycle.CollectionOptions{Watch: watch}
	completed := 0
	var sampler *heapSampler
	var baseline runtime.MemStats
	if sampleHeap {
		runtime.GC()
		runtime.ReadMemStats(&baseline)
		sampler = startHeapSampler(10 * time.Millisecond)
		defer sampler.stop()
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		calls, err := collectCohortOnce(cohort.Root(), collectors, options)
		if err != nil {
			b.Fatal(err)
		}
		completed += calls
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)
	var peak uint64
	if sampleHeap {
		peak = sampler.stop()
	}
	total := float64(completed)
	if total != float64(b.N*shape.collectors) {
		b.Fatalf("completed %v calls, want %d", total, b.N*shape.collectors)
	}
	perOp := total / float64(b.N)
	b.ReportMetric(perOp, units.perOp)
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/total, units.perNS)
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/total, units.perMem)
	if sampleHeap {
		runtime.GC()
		var retained runtime.MemStats
		runtime.ReadMemStats(&retained)
		b.ReportMetric(float64(peak), "peak-heap-B")
		b.ReportMetric(float64(int64(retained.HeapAlloc)-int64(baseline.HeapAlloc)), "retained-heap-B")
	}
}

// collectCohortOnce runs one concurrent Collect call per collector, joins
// every worker before returning, and reports how many ended as the expected
// pending publication.
func collectCohortOnce(root string, collectors []*testsupport.CollectionHandle, options tasklifecycle.CollectionOptions) (int, error) {
	errs := runCohort(len(collectors), func(index int) error {
		result, err := tasklifecycle.Collect(root, collectors[index].TaskDir(), options)
		return pendingPublicationError(result, err)
	})
	completed := 0
	for _, err := range errs {
		if err == nil {
			completed++
		}
	}
	return completed, errors.Join(errs...)
}

// runCohort runs work once per index on its own goroutine. Every worker
// announces readiness and waits; the caller releases all of them together once
// the last is ready, so no worker's work starts before every worker exists.
// It returns each worker's error by index after all workers have finished.
func runCohort(workers int, work func(index int) error) []error {
	errs := make([]error, workers)
	ready := make(chan struct{}, workers)
	start := make(chan struct{})
	finished := make(chan struct{}, workers)
	for index := range workers {
		go func() {
			ready <- struct{}{}
			<-start
			errs[index] = work(index)
			finished <- struct{}{}
		}()
	}
	for range workers {
		<-ready
	}
	close(start)
	for range workers {
		<-finished
	}
	return errs
}

// pendingPublicationError reports why a Collect result is not the expected
// pending publication with no concrete error.
func pendingPublicationError(result tasklifecycle.CollectionResult, err error) error {
	if err != nil {
		return fmt.Errorf("collect error: %w", err)
	}
	if result.Pending != tasklifecycle.PendingPublication || !result.Waiting || result.Completion != nil {
		return fmt.Errorf("collect result pending=%q waiting=%t completion=%v, want a pending publication", result.Pending, result.Waiting, result.Completion)
	}
	return nil
}

// heapSampler records the maximum runtime.MemStats.HeapAlloc seen at a fixed
// interval. It is a diagnostic only and is joined by stop.
type heapSampler struct {
	quit chan struct{}
	done chan struct{}
	peak uint64
}

func startHeapSampler(interval time.Duration) *heapSampler {
	sampler := &heapSampler{quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sampler.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var stats runtime.MemStats
		for {
			select {
			case <-sampler.quit:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&stats)
				sampler.peak = max(sampler.peak, stats.HeapAlloc)
			}
		}
	}()
	return sampler
}

// stop ends sampling, joins the sampler goroutine, and returns the peak. The
// quit channel is never replaced because the worker selects on it, so a repeat
// call sees it already closed. It is idempotent for its one calling goroutine,
// so a deferred call covers early failure.
func (s *heapSampler) stop() uint64 {
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	<-s.done
	return s.peak
}
