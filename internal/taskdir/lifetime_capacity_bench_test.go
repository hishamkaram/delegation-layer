package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These benchmarks record measurements, not timing assertions: the inventory
// reads only the records that exist, so an empty or short set does not pay
// for the 4096-launch namespace. The expected-records metric is the semantic
// record count each operation must read; taskdir has no read hook, so actual
// read attempts are not instrumented.

func BenchmarkTaskLifetimeLaunchRead(b *testing.B) {
	for _, count := range []uint64{1, 32, 64} {
		b.Run(fmt.Sprintf("records=%d", count), func(b *testing.B) {
			td, digest := registeredCapacityTask(b)
			for sequence := uint64(1); sequence <= count; sequence++ {
				seedTaskLaunch(b, td, digest, sequence, "native:start")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				launches, err := td.ReadLifetimeLaunches()
				if err != nil || uint64(len(launches)) != count {
					b.Fatalf("read = %d, %v", len(launches), err)
				}
			}
			b.ReportMetric(float64(count), "expected-records/op")
		})
	}
}

func BenchmarkControlLifetimeLaunchRead(b *testing.B) {
	for _, count := range []uint64{1, 32, 64} {
		b.Run(fmt.Sprintf("records=%d", count), func(b *testing.B) {
			control, lifetime := newCapacityControlLifetime(b)
			digest := registerCapacityControlLifetime(b, lifetime)
			for sequence := uint64(1); sequence <= count; sequence++ {
				putCapacityControlLaunch(b, control, controlCapacityLaunch(lifetime, digest, sequence), sequence)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				launches, err := lifetime.ReadLifetimeLaunches()
				if err != nil || uint64(len(launches)) != count {
					b.Fatalf("read = %d, %v", len(launches), err)
				}
			}
			b.ReportMetric(float64(count), "expected-records/op")
		})
	}
}

// BenchmarkControlLifetimeEmptyInventory200Stores inventories 200 independent
// operation stores holding no launches, as the orphan fence does.
func BenchmarkControlLifetimeEmptyInventory200Stores(b *testing.B) {
	const stores = 200
	store, err := InitStore(b.TempDir() + "/state")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			b.Errorf("close store: %v", closeErr)
		}
	})
	lifetimes := make([]*ControlLifetime, 0, stores)
	for index := range stores {
		_, lifetime := openCapacityControlLifetime(b, store, fmt.Sprintf("%032x", index+1))
		lifetimes = append(lifetimes, lifetime)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, lifetime := range lifetimes {
			err = withControlLifetimeTransactionContext(context.Background(), lifetime.control, func(tx *ControlTransaction) error {
				sequences, inventoryErr := tx.lifetimeLaunchSequences()
				if inventoryErr == nil && len(sequences) != 0 {
					inventoryErr = fmt.Errorf("empty store inventoried %d launches", len(sequences))
				}
				return inventoryErr
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(0, "expected-records/op")
	b.ReportMetric(stores, "stores/op")
}

// BenchmarkControlLifetimeLaunchReadConcurrent200Stores reads populated launch
// sets from 200 independent stores at once, each through its own rooted
// control transaction, canonical Verify and the production fallback deadline.
// Setup, including seeding valid canonical records, is outside the timer.
func BenchmarkControlLifetimeLaunchReadConcurrent200Stores(b *testing.B) {
	const stores = 200
	for _, count := range []uint64{1, 32, 64} {
		b.Run(fmt.Sprintf("records=%d", count), func(b *testing.B) {
			lifetimes := make([]*ControlLifetime, 0, stores)
			for range stores {
				control, lifetime := newCapacityControlLifetime(b)
				digest := registerCapacityControlLifetime(b, lifetime)
				for sequence := uint64(1); sequence <= count; sequence++ {
					seedCapacityControlLaunch(b, control, controlCapacityLaunch(lifetime, digest, sequence))
				}
				lifetimes = append(lifetimes, lifetime)
			}
			var slowest time.Duration
			deadlineFailures := 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result := readConcurrentControlLaunches(lifetimes, count)
				if result.err != nil {
					b.Fatal(result.err)
				}
				slowest = max(slowest, result.slowest)
				deadlineFailures += result.deadlineFailures
			}
			b.ReportMetric(float64(slowest.Milliseconds()), "slowest-store-ms")
			b.ReportMetric(float64(deadlineFailures), "deadline-failures")
			b.ReportMetric(float64(stores*count), "expected-records/op")
			b.ReportMetric(stores, "stores/op")
		})
	}
}

type concurrentLaunchReadResult struct {
	slowest          time.Duration
	deadlineFailures int
	err              error
}

func readConcurrentControlLaunches(lifetimes []*ControlLifetime, count uint64) concurrentLaunchReadResult {
	var (
		mu     sync.Mutex
		result concurrentLaunchReadResult
		ready  sync.WaitGroup
		done   sync.WaitGroup
	)
	start := make(chan struct{})
	for _, lifetime := range lifetimes {
		ready.Add(1)
		done.Go(func() {
			ready.Done()
			<-start
			began := time.Now()
			launches, err := lifetime.ReadLifetimeLaunchesContext(context.Background())
			elapsed := time.Since(began)
			mu.Lock()
			defer mu.Unlock()
			result.slowest = max(result.slowest, elapsed)
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				result.deadlineFailures++
			case err != nil:
				result.err = errors.Join(result.err, err)
			case uint64(len(launches)) != count:
				result.err = errors.Join(result.err, fmt.Errorf("read %d launches, want %d", len(launches), count))
			}
		})
	}
	ready.Wait()
	close(start)
	done.Wait()
	return result
}

// seedCapacityControlLaunch writes one valid canonical record without the
// durable writer; readers still verify its exact bytes and directory barrier.
func seedCapacityControlLaunch(b *testing.B, control *ControlDir, record task.LifetimeLaunchRecord) {
	b.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		b.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(control.dir, controlLifetimeLaunchName(record.Sequence)), data, 0o600); err != nil {
		b.Fatal(err)
	}
}

// The append benchmarks measure the durable writer from a fresh owner each
// iteration: setup and cleanup are outside the timer. They use only APIs
// that predate the capacity change, so the same file measures the baseline.
func BenchmarkTaskLifetimeLaunchAppend(b *testing.B) {
	for _, count := range []uint64{1, 32, 64, 70} {
		b.Run(fmt.Sprintf("launches=%d", count), func(b *testing.B) {
			skipBeyondLaunchLimit(b, count)
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				td, _ := registeredCapacityTask(b)
				b.StartTimer()
				for sequence := uint64(1); sequence <= count; sequence++ {
					if _, err := td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: sequence, PID: 1000 + int(sequence), ProcessStart: "native:start", PGID: 1000 + int(sequence)}); err != nil {
						b.Fatalf("append %d: %v", sequence, err)
					}
				}
			}
			b.ReportMetric(float64(count), "launches/op")
		})
	}
}

func BenchmarkControlLifetimeLaunchAppend(b *testing.B) {
	for _, count := range []uint64{1, 32, 64, 70} {
		b.Run(fmt.Sprintf("launches=%d", count), func(b *testing.B) {
			skipBeyondLaunchLimit(b, count)
			b.ReportAllocs()
			barriers := 0
			for range b.N {
				b.StopTimer()
				control, lifetime := newCapacityControlLifetime(b)
				registerCapacityControlLifetime(b, lifetime)
				counter := &directoryBarrierCounter{}
				control.store.SetFaultInjector(counter)
				b.StartTimer()
				for sequence := uint64(1); sequence <= count; sequence++ {
					if _, err := lifetime.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: sequence, PID: 1000 + int(sequence), ProcessStart: "control-start", PGID: 1000 + int(sequence)}); err != nil {
						b.Fatalf("append %d: %v", sequence, err)
					}
				}
				barriers += counter.count()
			}
			b.ReportMetric(float64(barriers)/float64(b.N), "dir-barriers/op")
			b.ReportMetric(float64(count), "launches/op")
		})
	}
}

func skipBeyondLaunchLimit(b *testing.B, count uint64) {
	b.Helper()
	if count > task.MaxLifetimeLaunches {
		b.Skipf("%d launches exceed this source's %d-launch limit", count, task.MaxLifetimeLaunches)
	}
}

// directoryBarrierCounter counts actual directory barriers through the
// store's existing fault-injection hooks without changing their outcome.
type directoryBarrierCounter struct {
	BaseFaultInjector
	barriers int
}

func (c *directoryBarrierCounter) OnDirBarrier(string) error {
	c.barriers++
	return nil
}

func (c *directoryBarrierCounter) OnParentDirBarrier(string) error {
	c.barriers++
	return nil
}

func (c *directoryBarrierCounter) count() int {
	return c.barriers
}
