package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResidentGrantZeroNilAndInvalidContextRefuse(t *testing.T) {
	calls := 0
	start := func() error { calls++; return nil }
	for _, grant := range []*residentGrant{nil, {}, {entry: &ownedResidentEntry{}}} {
		if err := grant.GuardDaemonStart(t.Context(), "/unused", start); err == nil {
			t.Fatal("zero grant accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid grant invoked Start")
	}
}

func TestResidentGrantNativeComponentLifecycle(t *testing.T) {
	for _, mode := range []string{"closed-grant", "malformed-context", "stale-grant", "close-race", "close-retry", "canceled-close-retry"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newComponentEntryFixture(t, "")
			runComponentEntryChild(t, fixture, mode, componentArguments(t, fixture), nil)
		})
	}
}

func exerciseResidentComponent(t *testing.T, entry *ownedResidentEntry, mode string) {
	t.Helper()
	switch mode {
	case "success":
		checkResidentStartResult(t, entry)
	case "closed-grant":
		checkResidentClosedGrant(t, entry)
	case "malformed-context":
		entry.operation.context.ControlEnvironment = nil
		requireResidentRefusal(t, entry, entry.operation.Intent().Inputs.Root)
	case "stale-grant":
		successor := entry.operation.Intent()
		successor.Sequence++
		successor.Epoch = strings.Repeat("f", 32)
		if created, err := entry.journal.putIntent(t.Context(), entry.lease, successor); err != nil || !created {
			t.Fatalf("actual journal successor fixture: %v", err)
		}
		requireResidentRefusal(t, entry, entry.operation.Intent().Inputs.Root)
	case "close-race":
		checkResidentCloseWaitsForStart(t, entry)
	case "close-retry":
		checkResidentCloseRetainsDependentResources(t, entry)
	case "canceled-close-retry":
		checkResidentCanceledCloseRetry(t, entry)
	default:
		t.Fatalf("unknown component test mode %q", mode)
	}
}

func requireResidentRefusal(t *testing.T, entry *ownedResidentEntry, root string) {
	t.Helper()
	calls := 0
	err := entry.grant.GuardDaemonStart(t.Context(), root, func() error { calls++; return nil })
	if err == nil || calls != 0 {
		t.Fatalf("invalid resident invoked native Start: calls=%d error=%v", calls, err)
	}
}

func checkResidentStartResult(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	root := entry.operation.Intent().Inputs.Root
	requireResidentRefusal(t, entry, filepath.Join(root, "foreign"))
	if err := entry.grant.GuardDaemonStart(t.Context(), root, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil callback accepted: %v", err)
	}
	var missingContext context.Context // Exercise the invalid input boundary.
	if err := entry.grant.GuardDaemonStart(missingContext, root, func() error { t.Error("nil context invoked Start"); return nil }); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil context accepted: %v", err)
	}
	for _, expected := range []*componentStartError{nil, {message: "exact native Start result"}} {
		var result error
		if expected != nil {
			result = expected
		}
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		err := entry.grant.GuardDaemonStart(ctx, root, func() error { calls++; cancel(); return result })
		cancel()
		requireExactComponentStartResult(t, err, expected)
		if calls != 1 {
			t.Fatalf("native Start calls=%d", calls)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	if err := entry.grant.GuardDaemonStart(ctx, root, func() error { calls++; return nil }); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled grant invoked Start: %v", err)
	}
}

func checkResidentClosedGrant(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	endpoint := entry.endpoint
	if err := entry.Close(); err != nil {
		t.Fatal(err)
	}
	requireEndpointListenerClosed(t, endpoint)
	requireResidentRefusal(t, entry, entry.operation.Intent().Inputs.Root)
	if err := entry.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func checkResidentCloseWaitsForStart(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan error, 1)
	go func() {
		started <- entry.grant.GuardDaemonStart(t.Context(), entry.operation.Intent().Inputs.Root, func() error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case err := <-started:
		t.Fatalf("grant refused before callback: %v", err)
	}
	if entry.mu.TryLock() {
		entry.mu.Unlock()
		unblock()
		if err := <-started; err != nil {
			t.Error(err)
		}
		t.Fatal("callback did not hold the actual entry closure guard")
	}
	attempted := make(chan struct{})
	closed := make(chan error, 1)
	go func() { close(attempted); closed <- entry.Close() }()
	<-attempted
	// Entry Close uses the same held mutex. This observed native resource remains
	// owned until callback release; no timing/sleep assertion substitutes for it.
	if _, err := os.Lstat(entry.endpoint.Identity().Path); err != nil {
		unblock()
		t.Error(err)
	}
	unblock()
	if err := <-started; err != nil {
		t.Error(err)
	}
	if err := <-closed; err != nil {
		t.Error(err)
	}
	requireResidentRefusal(t, entry, entry.operation.Intent().Inputs.Root)
}

var _ DaemonStartGuard = (*residentGrant)(nil)

func checkResidentCloseRetainsDependentResources(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	root := entry.operation.Intent().Inputs.Root
	observer := &rootSwapEndpointObserver{store: entry.store, sequence: entry.operation.Intent().Sequence}
	t.Cleanup(func() { restoreEndpointRoot(t, observer) })
	if _, err := observer.PinRootContext(t.Context()); err == nil || !observer.moved {
		t.Fatalf("fixture did not replace actual native root: %v", err)
	}
	replacement := ownerEndpointPath(root, entry.operation.Intent().Sequence)
	endpoint := entry.endpoint
	if err := entry.Close(); err == nil {
		t.Fatal("replaced root cleanup claimed complete")
	}
	requireEndpointListenerClosed(t, endpoint)
	if entry.endpoint != endpoint || entry.lease == nil || entry.journal == nil || entry.store == nil {
		t.Fatal("failed cleanup discarded dependent resources")
	}
	requireResidentRefusal(t, entry, root)
	data, err := os.ReadFile(replacement)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("cleanup modified replacement: %v", err)
	}
	restoreEndpointRoot(t, observer)
	if observer.moved {
		return
	}
	if err = entry.Close(); err != nil {
		t.Fatalf("restored cleanup retry: %v", err)
	}
	if _, err = os.Lstat(ownerEndpointPath(root, entry.operation.Intent().Sequence)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left original socket: %v", err)
	}
}

// No Is/Unwrap method: errors.Is on this concrete pointer proves exact identity.
type componentStartError struct{ message string }

func (e *componentStartError) Error() string { return e.message }
func requireExactComponentStartResult(t *testing.T, actual error, expected *componentStartError) {
	t.Helper()
	if expected == nil {
		if actual != nil {
			t.Fatalf("grant changed nil native Start result: %v", actual)
		}
		return
	}
	// Search only the known unwrapped fixture, never the actual error's chain.
	// With no Is/Unwrap on expected, success requires its exact pointer identity.
	if !errors.Is(expected, actual) {
		t.Fatalf("grant wrapped/replaced native Start result: %v", actual)
	}
}

func checkResidentCanceledCloseRetry(t *testing.T, entry *ownedResidentEntry) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	endpoint := entry.endpoint
	if err := entry.closeContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup lost context: %v", err)
	}
	requireEndpointListenerClosed(t, endpoint)
	if entry.endpoint != endpoint || entry.lease == nil || entry.store == nil {
		t.Fatal("canceled cleanup discarded dependencies")
	}
	requireResidentRefusal(t, entry, entry.operation.Intent().Inputs.Root)
	if err := entry.Close(); err != nil {
		t.Fatalf("fresh cleanup retry: %v", err)
	}
}
