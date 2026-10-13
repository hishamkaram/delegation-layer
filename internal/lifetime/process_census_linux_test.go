//go:build linux

package lifetime

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinuxProcessCensusBrokerCoalescesTwoHundredConcurrentObservers(t *testing.T) {
	const observers = 200
	started := make(chan struct{})
	release := make(chan struct{})
	var scans atomic.Int32
	broker := newLinuxProcessCensusBroker(func(ctx context.Context) ([]linuxProcessCensusEntry, error) {
		if scans.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []linuxProcessCensusEntry{{snapshot: processScopeSnapshot{pid: 42}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	begin := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(observers)
	results := make(chan error, observers)
	for range observers {
		go func() {
			ready.Done()
			<-begin
			entries, err := broker.observe(t.Context())
			if err == nil && (len(entries) != 1 || entries[0].snapshot.pid != 42) {
				err = errors.New("shared census result changed")
			}
			results <- err
		}()
	}
	ready.Wait()
	close(begin)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("process census did not start")
	}
	waitForLinuxCensusWaiters(t, broker, observers)
	close(release)
	for range observers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := scans.Load(); got != 1 {
		t.Fatalf("concurrent process census scans = %d, want 1", got)
	}
	if _, err := broker.observe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := scans.Load(); got != 2 {
		t.Fatalf("completed process census was cached: scans=%d", got)
	}
}

func TestLinuxProcessCensusBrokerCancelsWhenLastObserverLeaves(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	broker := newLinuxProcessCensusBroker(func(ctx context.Context) ([]linuxProcessCensusEntry, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := broker.observe(ctx)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observer error = %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("orphaned process census did not stop")
	}
}

func TestLinuxProcessCensusBrokerStartsFreshScanAfterLastObserverCancels(t *testing.T) {
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	firstFinished := make(chan struct{})
	secondStarted := make(chan struct{})
	allowFirstExit := make(chan struct{})
	var scans atomic.Int32
	broker := newLinuxProcessCensusBroker(func(ctx context.Context) ([]linuxProcessCensusEntry, error) {
		switch scans.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			<-allowFirstExit
			close(firstFinished)
			return nil, ctx.Err()
		case 2:
			close(secondStarted)
			return []linuxProcessCensusEntry{{snapshot: processScopeSnapshot{pid: 43}}}, nil
		default:
			return nil, errors.New("unexpected extra process census scan")
		}
	})

	firstContext, cancelFirst := context.WithCancel(t.Context())
	firstResult := make(chan error, 1)
	go func() {
		_, err := broker.observe(firstContext)
		firstResult <- err
	}()
	<-firstStarted
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observer error = %v", err)
	}
	select {
	case <-firstCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("census scan did not observe cancellation")
	}

	broker.mu.Lock()
	active := broker.active
	broker.mu.Unlock()
	if active != nil {
		t.Fatal("canceled census remained joinable after its last observer left")
	}

	type censusResult struct {
		entries []linuxProcessCensusEntry
		err     error
	}
	secondResult := make(chan censusResult, 1)
	go func() {
		entries, err := broker.observe(t.Context())
		secondResult <- censusResult{entries: entries, err: err}
	}()
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		close(allowFirstExit)
		<-firstFinished
		<-secondResult
		t.Fatal("new observer did not start a fresh census")
	}
	result := <-secondResult
	if result.err != nil {
		t.Fatalf("fresh observer error = %v", result.err)
	}
	if len(result.entries) != 1 || result.entries[0].snapshot.pid != 43 {
		t.Fatalf("fresh observer entries = %#v", result.entries)
	}

	close(allowFirstExit)
	select {
	case <-firstFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled census did not finish")
	}
	if got := scans.Load(); got != 2 {
		t.Fatalf("process census scans = %d, want 2", got)
	}
}

func TestLinuxProcessTopologyEntryReadsOnlyStat(t *testing.T) {
	var paths []string
	entry, present, err := readLinuxProcessTopologyEntryWith(t.Context(), 42, func(path string) ([]byte, error) {
		paths = append(paths, path)
		return linuxProcessStatFixture(42, 10, 42, 42, "12345"), nil
	})
	if err != nil || !present {
		t.Fatalf("topology entry = present:%t err:%v", present, err)
	}
	if len(paths) != 1 || paths[0] != "/proc/42/stat" {
		t.Fatalf("topology reads = %#v, want only /proc/42/stat", paths)
	}
	if entry.snapshot.pid != 42 || entry.snapshot.parentPID != 10 || entry.snapshot.startToken != "linux:12345" {
		t.Fatalf("topology snapshot = %#v", entry.snapshot)
	}
}

func TestLinuxProcessDirectoryChecksContextBetweenBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batches := 0
	_, err := scanLinuxProcessDirectory(ctx, func(context.Context, int) (linuxProcessCensusEntry, bool, error) {
		cancel()
		return linuxProcessCensusEntry{}, false, nil
	}, func() ([]string, error) {
		batches++
		if batches == 1 {
			return []string{"42"}, nil
		}
		return nil, io.EOF
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("directory scan error = %v, want context.Canceled", err)
	}
	if batches != 1 {
		t.Fatalf("directory batches = %d, want 1", batches)
	}
}

func TestLinuxReapedZombieSnapshotRemainsOwnershipBridge(t *testing.T) {
	owner, err := ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	zombiePID := 42001
	childPID := 42002
	entries := []linuxProcessCensusEntry{
		{snapshot: processScopeSnapshot{
			pid: owner.PID(), parentPID: 1, pgid: 42000, sessionID: 42000, startToken: owner.StartToken(),
		}},
		{snapshot: processScopeSnapshot{
			pid: zombiePID, parentPID: owner.PID(), pgid: zombiePID, sessionID: zombiePID, startToken: "linux:zombie", zombie: true,
		}},
		{snapshot: processScopeSnapshot{
			pid: childPID, parentPID: zombiePID, pgid: childPID, sessionID: childPID, startToken: "linux:child",
		}},
	}
	kept, err := reapOwnedLinuxZombiesWith(&owner, nil, entries, func(pid int) (int, error) {
		if pid != zombiePID {
			t.Fatalf("waited PID = %d, want %d", pid, zombiePID)
		}
		return pid, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := make([]processScopeSnapshot, 0, len(kept))
	for _, entry := range kept {
		snapshots = append(snapshots, entry.snapshot)
	}
	refs, err := discoverOwnedProcessLaunchesWithOwner(nil, nil, &owner, nil, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].PID() != childPID {
		t.Fatalf("ownership bridge refs = %#v, want child PID %d", refs, childPID)
	}
}

func linuxProcessStatFixture(pid, parentPID, pgid, sessionID int, startToken string) []byte {
	fields := []string{"S", strconv.Itoa(parentPID), strconv.Itoa(pgid), strconv.Itoa(sessionID)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, startToken)
	return []byte(strconv.Itoa(pid) + " (worker) " + strings.Join(fields, " "))
}

func waitForLinuxCensusWaiters(t *testing.T, broker *linuxProcessCensusBroker, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		broker.mu.Lock()
		call := broker.active
		got := 0
		if call != nil {
			got = call.waiters
		}
		broker.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process census waiters did not reach %d", want)
}
