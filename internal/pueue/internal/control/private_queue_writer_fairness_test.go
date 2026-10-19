package control

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// queueReaderFairness is the synchronized record of one writer-fairness
// test. Every fact takes the next value of one sequence under mu: each
// reader's latest wait on active writer intent, its callback entry and its
// return, and the writer's callback entry and release. Observers and
// callbacks only record and signal notify without blocking; the test
// goroutine alone asserts.
type queueReaderFairness struct {
	mu            sync.Mutex
	sequence      uint64
	lastWait      []uint64
	readerEntries []uint64
	returned      []bool
	writerEntry   uint64
	writerRelease uint64
	notify        chan struct{}
}

func newQueueReaderFairness(readers int) *queueReaderFairness {
	return &queueReaderFairness{
		lastWait:      make([]uint64, readers),
		readerEntries: make([]uint64, readers),
		returned:      make([]bool, readers),
		notify:        make(chan struct{}, 1),
	}
}

func (f *queueReaderFairness) record(update func(sequence uint64)) {
	f.mu.Lock()
	f.sequence++
	update(f.sequence)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// phase returns a boundary every later fact follows.
func (f *queueReaderFairness) phase() uint64 {
	var boundary uint64
	f.record(func(sequence uint64) { boundary = sequence })
	return boundary
}

// startReader runs reader index on its own client, built from the binding
// with that reader's wait observer; no client state is shared or copied.
func (f *queueReaderFairness) startReader(t *testing.T, scope *testCallScope, bound *Client, index int) *testCall[lockCallResult] {
	t.Helper()
	options := bound.options
	options.queueWriterWaitObserver = func() {
		f.record(func(sequence uint64) { f.lastWait[index] = sequence })
	}
	client, err := NewClient(bound.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := scope.context()
	return startTestCall(scope, func() lockCallResult {
		err := client.withPrivateQueueIdentityLease(ctx, func(context.Context) error {
			f.record(func(sequence uint64) { f.readerEntries[index] = sequence })
			return nil
		})
		f.record(func(uint64) { f.returned[index] = true })
		return lockCallResult{err: err}
	})
}

// awaitWaits requires every reader to report a wait after boundary while no
// reader has entered. An entered reader fails at once, as does a reader that
// returned without entering, with its exact result; missing readers are
// named at the operation cutoff.
func (f *queueReaderFairness) awaitWaits(scope *testCallScope, readers []*testCall[lockCallResult], boundary uint64, phase string) {
	scope.t.Helper()
	timer := time.NewTimer(time.Until(scope.cutoff))
	defer timer.Stop()
	for {
		f.requireNoReaderEntry(scope.t, phase)
		missing, returned := f.missingWaits(boundary)
		if returned >= 0 {
			<-readers[returned].done
			scope.t.Fatalf("%s: reader %d returned without entering: err=%v; readers without a wait on writer intent: %v", phase, returned, readers[returned].result.err, missing)
		}
		if len(missing) == 0 {
			return
		}
		select {
		case <-f.notify:
		case <-timer.C:
			scope.t.Fatalf("%s: readers %v reported no wait on writer intent before the operation cutoff", phase, missing)
		}
	}
}

// missingWaits lists readers without a wait after boundary and the first
// reader that already returned, or -1.
func (f *queueReaderFairness) missingWaits(boundary uint64) ([]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var missing []int
	returned := -1
	for index, sequence := range f.lastWait {
		if f.returned[index] && returned < 0 {
			returned = index
		}
		if sequence <= boundary {
			missing = append(missing, index)
		}
	}
	return missing, returned
}

func (f *queueReaderFairness) requireNoReaderEntry(t *testing.T, phase string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index, sequence := range f.readerEntries {
		if sequence != 0 {
			t.Fatalf("%s: reader %d entered while writer intent was active", phase, index)
		}
	}
}

// requireReadersFollowWriter requires the writer's callback to have entered
// and been released before every reader callback entered.
func (f *queueReaderFairness) requireReadersFollowWriter(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writerEntry == 0 || f.writerRelease <= f.writerEntry {
		t.Fatalf("writer callback order was not recorded: entry=%d release=%d", f.writerEntry, f.writerRelease)
	}
	for index, sequence := range f.readerEntries {
		if sequence <= f.writerRelease {
			t.Fatalf("reader %d entered at %d, not after writer release at %d", index, sequence, f.writerRelease)
		}
	}
}

// holdPrivateQueueGateShared holds the gate shared until scope cleanup or
// the returned release, so the writer waits before its exclusive gate and
// that gate cannot mask readers that ignore writer intent.
func holdPrivateQueueGateShared(t *testing.T, scope *testCallScope, stateRoot string) func() {
	t.Helper()
	path := filepath.Join(stateRoot, privateSupervisorDirectory, privateQueueIdentityGateLockName)
	gate, err := acquireExistingSupervisorLock(scope.context(), path, unix.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() {
		if closeErr := closeBootstrapLock(gate); closeErr != nil {
			t.Errorf("release fixture gate: %v", closeErr)
		}
	})
	scope.onRelease(release)
	return release
}

func TestPrivateQueueWriterIntentBlocksNewReaders(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	scope := newTestCallScope(t, fake.client.options)
	initialEntered := make(chan struct{})
	initialReleased, releaseInitial := scope.barrier()
	initial := startQueueIdentityLease(scope, fake.client, func(context.Context) error {
		close(initialEntered)
		<-initialReleased
		return nil
	})
	awaitEntry(scope, initialEntered, initial, "initial queue identity reader did not enter")
	releaseGate := holdPrivateQueueGateShared(t, scope, store.Root)

	const readerCount = 32
	fairness := newQueueReaderFairness(readerCount)
	writerEntered := make(chan struct{})
	writerReleased, releaseWriter := scope.barrier()
	writer := startQueueIdentityMutation(scope, fake.client, func(context.Context) error {
		fairness.record(func(sequence uint64) { fairness.writerEntry = sequence })
		close(writerEntered)
		<-writerReleased
		fairness.record(func(sequence uint64) { fairness.writerRelease = sequence })
		return nil
	})
	waitForPrivateQueueWriterIntent(t, store.Root, writer.done, scope.cutoff)

	readers := make([]*testCall[lockCallResult], readerCount)
	for index := range readers {
		readers[index] = fairness.startReader(t, scope, fake.client, index)
	}
	fairness.awaitWaits(scope, readers, fairness.phase(), "waiting writer")
	releaseGate()
	releaseInitial()
	awaitEntry(scope, writerEntered, writer, "writer did not enter after the existing reader drained")
	fairness.awaitWaits(scope, readers, fairness.phase(), "active writer")
	releaseWriter()
	awaitLockCall(scope, writer, "queue identity writer")
	awaitLockCall(scope, initial, "initial queue identity reader")
	for index, reader := range readers {
		awaitLockCall(scope, reader, fmt.Sprintf("queue identity reader %d", index))
	}
	fairness.requireReadersFollowWriter(t)
}
