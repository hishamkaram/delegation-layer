package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type rootContextReplacementFault struct {
	BaseFaultInjector
	before  func(string, string) error
	after   func(string) error
	stage   error
	barrier error
	staged  func()
}

func (fault *rootContextReplacementFault) OnBeforeRootContextRename(stage, destination string) error {
	if fault.before != nil {
		return fault.before(stage, destination)
	}
	return nil
}

func (fault *rootContextReplacementFault) OnAfterRootContextRename(destination string) error {
	if fault.after != nil {
		return fault.after(destination)
	}
	return nil
}

func (fault *rootContextReplacementFault) OnRootContextBarrier(string) error { return fault.barrier }

func (fault *rootContextReplacementFault) OnStageBarrier(string) error {
	if fault.staged != nil {
		fault.staged()
	}
	return fault.stage
}

func TestRootContextReplacementClassifiesStorageFaults(t *testing.T) {
	for _, boundary := range []string{"stage", "old", "candidate", "barrier", "conflicting", "unreadable"} {
		t.Run(boundary, func(t *testing.T) { testRootContextReplacementFault(t, boundary) })
	}
}

func testRootContextReplacementFault(t *testing.T, boundary string) {
	t.Helper()
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	prepared, err := rootContextReader(t, store).PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	old, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	must(t, err)
	failure := errors.New("fixture root replacement fault")
	fault, conflict := rootContextFault(t, store, boundary, failure)
	store.SetFaultInjector(fault)
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	store.SetFaultInjector(nil)
	if boundary == "candidate" {
		must(t, err)
	} else if !errors.Is(err, failure) {
		t.Fatalf("lost %s fault: %v", boundary, err)
	}
	if boundary == "unreadable" {
		if _, readErr := os.Stat(filepath.Join(store.Root, "root.json")); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("missing root was recreated: %v", readErr)
		}
		return
	}
	winner, readErr := os.ReadFile(filepath.Join(store.Root, "root.json"))
	must(t, readErr)
	switch boundary {
	case "stage", "old":
		if !bytes.Equal(old, winner) {
			t.Fatal("failed precommit attempt changed root record")
		}
	case "conflicting":
		if !errors.Is(err, task.ErrInvariantFault) || !bytes.Equal(winner, conflict) {
			t.Fatalf("conflicting winner was not preserved: %v", err)
		}
		return
	case "candidate", "barrier":
		actual, tokenErr := store.RootTokenContext(t.Context())
		must(t, tokenErr)
		if actual != token {
			t.Fatal("matching candidate lost old token")
		}
	}
	if boundary == "barrier" && !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("barrier uncertainty was concealed: %v", err)
	}
	// A clear old record may be explicitly retried; a matching candidate may
	// finish its durability proof. Both use the same original challenge.
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	must(t, err)
}

func rootContextFault(t *testing.T, store *Store, boundary string, failure error) (*rootContextReplacementFault, []byte) {
	t.Helper()
	fault := &rootContextReplacementFault{}
	var conflict []byte
	switch boundary {
	case "stage":
		fault.stage = failure
	case "old":
		fault.before = func(string, string) error { return failure }
	case "candidate":
		fault.after = func(string) error { return failure }
	case "barrier":
		fault.barrier = failure
	case "conflicting":
		record, err := readStoreRootRecord(t.Context(), store)
		must(t, err)
		record.CreatedAt = "2026-11-19T00:00:00Z"
		conflict, err = task.MarshalCanonical(record)
		must(t, err)
		fault.before = func(_ string, destination string) error {
			return errors.Join(failure, os.WriteFile(destination, conflict, 0o600))
		}
	case "unreadable":
		fault.before = func(_ string, destination string) error { return errors.Join(failure, os.Remove(destination)) }
	}
	return fault, conflict
}

func TestRootContextReconciliationExcludesConcurrentConfirmation(t *testing.T) {
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	prepared, err := rootContextReader(t, store).PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	other, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	store.SetFaultInjector(&rootContextReplacementFault{before: func(string, string) error {
		close(entered)
		<-release
		return nil
	}})
	done := make(chan error, 1)
	go func() {
		_, confirmErr := store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
		done <- confirmErr
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case confirmErr := <-done:
			must(t, confirmErr)
		case <-time.After(10 * time.Second):
			t.Error("confirmation did not join")
		}
		store.SetFaultInjector(nil)
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first confirmation did not reach replacement")
	}
	passGate := rootContextTestGate(func(ctx context.Context, _ RootPin, operation func(context.Context) error) error {
		return operation(ctx)
	})
	if _, err = other.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, passGate); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("second confirmation bypassed maintenance EX: %v", err)
	}
	unblock()
}

func TestRootContextBindingRefusesChangedDeviceAndCopiedRoot(t *testing.T) {
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	reader := rootContextReader(t, store)
	prepared, err := reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	must(t, err)
	record, err := readStoreRootRecord(t.Context(), store)
	must(t, err)
	record.ContextBinding.Accepted.Device++ // model a saved binding before a second device change
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
	if _, err = store.RootTokenContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("new device silently adopted: %v", err)
	}
	prepared, err = reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	if prepared.AlreadyReconciled {
		t.Fatal("new physical acquisition reported already reconciled")
	}
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	must(t, err)
	record, err = readStoreRootRecord(t.Context(), store)
	must(t, err)
	if record.ContextBinding.Origin != origin {
		t.Fatal("subsequent reconciliation replaced the original token preimage")
	}
	testRootContextCopyRefusal(t, store, token, origin)
}

func testRootContextCopyRefusal(t *testing.T, store *Store, token string, origin task.RootContextCoordinates) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	must(t, err)
	copyRoot := filepath.Join(t.TempDir(), "copy")
	must(t, os.Mkdir(copyRoot, 0o700))
	must(t, os.WriteFile(filepath.Join(copyRoot, "root.json"), data, 0o600))
	copyReader, err := OpenReadOnlyStoreContext(t.Context(), copyRoot, store.predicates)
	must(t, err)
	defer func() { must(t, copyReader.Close()) }()
	if _, err = copyReader.store.RootTokenContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("copy derived the original token: %v", err)
	}
	if _, err = copyReader.PrepareRootContextReconciliation(t.Context(), token, origin); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("copy accepted reconciliation: %v", err)
	}
	retained := store.Root + "-retained"
	must(t, os.Rename(store.Root, retained))
	must(t, os.Mkdir(store.Root, 0o700))
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
	if _, err = store.RootTokenContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("held store accepted replaced physical root: %v", err)
	}
	replaced := rootContextReader(t, store)
	if _, err = replaced.store.RootTokenContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("new reader accepted same-path directory replacement: %v", err)
	}
}

func TestRootContextBindingFreshReadFailsClosed(t *testing.T) {
	store := testStore(t)
	_, err := store.RootTokenContext(t.Context())
	must(t, err)
	record, err := readStoreRootRecord(t.Context(), store)
	must(t, err)
	record.RequiredFeatures = append(record.RequiredFeatures, task.RootContextBindingFeature)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
	if token, tokenErr := store.RootTokenContext(t.Context()); tokenErr == nil || token != "" {
		t.Fatalf("missing binding returned a cached/fallback token: %q %v", token, tokenErr)
	}
	record.RequiredFeatures = nil
	record.RootID = strings.Repeat("a", 32)
	data, err = task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
	if token, tokenErr := store.RootTokenContext(t.Context()); !errors.Is(tokenErr, task.ErrIdentityMismatch) || token != "" {
		t.Fatalf("changed RootID returned a usable token: %q %v", token, tokenErr)
	}
}

func TestRootContextCancellationAfterStagingDoesNotCommit(t *testing.T) {
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	prepared, err := rootContextReader(t, store).PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.SetFaultInjector(&rootContextReplacementFault{staged: cancel})
	_, err = store.ConfirmRootContextReconciliation(ctx, token, origin, prepared.Challenge, true, rootContextGate(t, store))
	store.SetFaultInjector(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("staged cancellation was lost: %v", err)
	}
	record, err := readStoreRootRecord(t.Context(), store)
	must(t, err)
	if record.ContextBinding != nil {
		t.Fatal("canceled staged replacement committed")
	}
	// Successful retry also proves the canceled invocation cleaned its owned
	// stage and released maintenance despite its canceled caller context.
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	must(t, err)
}
