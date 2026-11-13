package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func writeConnectorRootFeatures(t *testing.T, store *Store, features []string) {
	t.Helper()
	record, err := readStoreRootRecord(t.Context(), store)
	must(t, err)
	record.RequiredFeatures = features
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
}

func TestConnectorMutationFeatureOfflineUpgrade(t *testing.T) {
	store := testStore(t)
	ordinary := preparedTask(t, store)
	writeConnectorRootFeatures(t, store, []string{task.NamespaceAdmissionFeature})
	if err := store.RequireConnectorEnabled(t.Context()); !errors.Is(err, ErrConnectorDisabled) {
		t.Fatalf("v1-only root enabled the new connector contract: %v", err)
	}
	before := snapshotTree(t, store.Root)
	if err := store.EnableConnectorContext(t.Context(), false); err == nil {
		t.Fatal("upgrade omitted explicit offline attestation")
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("unattested upgrade changed root")
	}
	must(t, store.EnableConnectorContext(t.Context(), true))
	features, err := store.ConnectorFeaturesContext(t.Context())
	must(t, err)
	if !slices.Contains(features, task.NamespaceAdmissionFeature) || !slices.Contains(features, task.NamespaceMutationFeature) {
		t.Fatalf("upgrade did not publish both required features: %v", features)
	}
	must(t, store.RequireConnectorEnabled(t.Context()))
	_, _, err = ordinary.PreparedRecords()
	must(t, err)
	before = snapshotTree(t, store.Root)
	must(t, store.EnableConnectorContext(t.Context(), true))
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("repeated enable changed the initialized root")
	}
}

func TestConnectorMutationFeaturePublishWaitsForLockDurability(t *testing.T) {
	store := testStore(t)
	writeConnectorRootFeatures(t, store, []string{task.NamespaceAdmissionFeature})
	failure := errors.New("fixture mutation inode barrier failure")
	gate := newInitializationLockBarrier(namespaceMutationLockName)
	gate.err = failure
	gate.unblock()
	store.SetFaultInjector(gate)
	if err := store.EnableConnectorContext(t.Context(), true); !errors.Is(err, failure) {
		t.Fatalf("cutover ignored lock durability failure: %v", err)
	}
	features, err := store.ConnectorFeaturesContext(t.Context())
	must(t, err)
	if slices.Contains(features, task.NamespaceMutationFeature) {
		t.Fatal("root published feature before mutation lock was durable")
	}
	store.SetFaultInjector(nil)
	must(t, store.EnableConnectorContext(t.Context(), true))
	must(t, store.RequireConnectorEnabled(t.Context()))
}

func TestConnectorMutationEnabledLockNeverRecreated(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		name := "missing"
		if unsafe {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) { testConnectorMutationMissingAuthority(t, unsafe) })
	}
}

func testConnectorMutationMissingAuthority(t *testing.T, unsafe bool) {
	t.Helper()
	store := testStore(t)
	enableConnectorFixture(t, store)
	path := filepath.Join(store.Root, namespaceMutationLockName)
	must(t, os.Remove(path))
	if unsafe {
		must(t, os.Symlink(filepath.Join(store.Root, "root.json"), path))
	}
	before := snapshotTree(t, store.Root)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	observation, inspectErr := reader.InspectConnectorContext(t.Context(), "")
	must(t, reader.Close())
	if !errors.Is(inspectErr, task.ErrEvidenceFault) || observation.RootToken != "" || observation.ProjectToken != "" {
		t.Fatalf("broken mutation authority yielded usable inspection: %+v %v", observation, inspectErr)
	}
	if err = store.RequireConnectorEnabled(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("enabled root accepted missing/unsafe lock: %v", err)
	}
	if err = store.EnableConnectorContext(t.Context(), true); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("enable repaired missing/unsafe enabled lock: %v", err)
	}
	req, meta, brief := preparedInput(t, store)
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	if td != nil {
		must(t, td.Close())
	}
	if !errors.Is(err, task.ErrEvidenceFault) || !CreationNotAttempted(err) {
		t.Fatalf("creation ignored missing mutation authority: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("operation repaired enabled mutation authority")
	}
}

func TestConnectorMutationOrdinaryRootsDoNotCreateLease(t *testing.T) {
	store := testStore(t)
	preparedTask(t, store)
	if _, err := os.Stat(filepath.Join(store.Root, namespaceMutationLockName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary task initialized connector mutation authority: %v", err)
	}
}

func requireConnectorMutationBusy(t *testing.T, operation func() error, unblock func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- operation() }()
	joined := false
	t.Cleanup(func() {
		if !joined {
			unblock()
			<-done
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case err := <-done:
		joined = true
		if !errors.Is(err, task.ErrLockBusy) || CleanupFault(err) {
			t.Fatalf("mutation contention was not pure busy: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("background mutation blocked instead of returning busy:", ctx.Err())
	}
}

func TestConnectorMutationLeaseExcludesAllWriters(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	partial := preparedTask(t, store)
	must(t, os.Remove(filepath.Join(partial.Dir, "meta.json")))
	other, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	blocker, err := store.beginConnectorMutationContext(t.Context(), true)
	must(t, err)
	unblock := func() { must(t, blocker.release()) }
	t.Cleanup(unblock)
	for _, writer := range []struct {
		name  string
		store *Store
	}{{"same_store", store}, {"independent_store", other}} {
		t.Run(writer.name, func(t *testing.T) {
			req, meta, brief := preparedInput(t, writer.store)
			requireConnectorMutationBusy(t, func() error {
				td, createErr := writer.store.CreateTaskContext(context.Background(), req.TaskID, req, brief, meta)
				if td != nil {
					createErr = errors.Join(createErr, td.Close())
				}
				return createErr
			}, unblock)
			requireConnectorMutationBusy(t, func() error {
				_, fenceErr := writer.store.FenceTaskNamespaceContext(context.Background(), strings.Repeat("a", 32), nil)
				return fenceErr
			}, unblock)
			requireConnectorMutationBusy(t, func() error {
				_, fenceErr := writer.store.FencePartialTaskContext(context.Background(), partial.TaskID, PartialFenceSelection{OperatorUnattributed: true})
				return fenceErr
			}, unblock)
		})
	}
}

type connectorRefusalCommitGate struct {
	BaseFaultInjector
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *connectorRefusalCommitGate) OnLink(_, destination string) error {
	if filepath.Base(filepath.Dir(destination)) == "task-refusals" {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return nil
}

func TestConnectorMutationRefusalWritersExcludeBarrier(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "absent"
		if partial {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) { testConnectorMutationRefusalWriter(t, partial) })
	}
}

func testConnectorMutationRefusalWriter(t *testing.T, partial bool) {
	t.Helper()
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	id := strings.Repeat("b", 32)
	if partial {
		td := preparedTask(t, store)
		id = td.TaskID
		must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	}
	gate := &connectorRefusalCommitGate{entered: make(chan struct{}), release: make(chan struct{})}
	store.SetFaultInjector(gate)
	done := make(chan error, 1)
	var releaseOnce sync.Once
	joined := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(gate.release) })
		if !joined {
			must(t, <-done)
		}
	})
	go func() {
		var fenceErr error
		if partial {
			_, fenceErr = store.FencePartialTaskContext(t.Context(), id, PartialFenceSelection{OperatorUnattributed: true})
		} else {
			_, fenceErr = store.FenceTaskNamespaceContext(t.Context(), id, nil)
		}
		done <- fenceErr
	}()
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("refusal did not reach durable commit boundary:", ctx.Err())
	}
	requireConnectorOriginBusy(t, store, root)
	releaseOnce.Do(func() { close(gate.release) })
	err := <-done
	joined = true
	must(t, err)
}

func TestConnectorMutationCreatorCleanupRetainsEveryAncestor(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	other, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	failure := errors.New("fixture creator admission unlock failure")
	fault := &connectorUnlockFailure{name: ".admission.lock", failure: failure}
	store.SetFaultInjector(fault)
	req, meta, brief := preparedInput(t, store)
	td, cause := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	if td != nil {
		must(t, td.Close())
	}
	var retained *ConnectorTaskCleanupError
	errors.As(cause, &retained)
	t.Cleanup(func() {
		fault.failure = nil
		if retained != nil {
			must(t, retained.Release())
		}
	})
	if !errors.Is(cause, failure) || retained == nil || !CleanupRetainsStore(cause, store) {
		t.Fatalf("creator discarded its admission cleanup owner: %v", cause)
	}
	unexpectedMutation, probeErr := other.beginConnectorMutationContext(t.Context(), true)
	if unexpectedMutation != nil {
		must(t, unexpectedMutation.release())
	}
	if !errors.Is(probeErr, task.ErrLockBusy) {
		t.Fatalf("creator released mutation before admission cleanup: %v", probeErr)
	}
	namespace, err := other.openExistingLock(filepath.Join(store.Root, "task-namespace", req.TaskID+".lock"), LockLevelNone)
	must(t, err)
	t.Cleanup(func() { must(t, namespace.closeIdle()) })
	if err = namespace.LockEXNonblockingContext(t.Context()); !errors.Is(err, task.ErrLockBusy) {
		if err == nil {
			must(t, namespace.Unlock())
		}
		t.Fatalf("creator released namespace before admission cleanup: %v", err)
	}
	if err = other.maintLock.LockEXNonblockingContext(t.Context()); !errors.Is(err, task.ErrLockBusy) {
		if err == nil {
			must(t, other.maintLock.Unlock())
		}
		t.Fatalf("creator released maintenance before admission cleanup: %v", err)
	}
	fault.failure = nil
	must(t, retained.Release())
	must(t, namespace.LockEXNonblockingContext(t.Context()))
	must(t, namespace.Unlock())
	mutation, err := other.beginConnectorMutationContext(t.Context(), true)
	must(t, err)
	must(t, mutation.release())
	must(t, other.maintLock.LockEXNonblockingContext(t.Context()))
	must(t, other.maintLock.Unlock())
}

func requireConnectorOriginBusy(t *testing.T, store *Store, root string) {
	t.Helper()
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("barrier crossed active membership mutation: %v", err)
	}
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", strings.Repeat("a", 64), root)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("reopen crossed active membership mutation: %v", err)
	}
}

func TestConnectorMutationCreatorExcludesBarrierBeforeTaskDirectory(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	control, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	req, meta, brief := preparedInput(t, store)
	gate := newInitializationLockBarrier(req.TaskID + ".lock")
	store.SetFaultInjector(gate)
	done := make(chan initializationCreateResult, 1)
	go func() {
		directory, createErr := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
		done <- initializationCreateResult{taskDir: directory, err: createErr}
	}()
	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			gate.unblock()
			created := <-done
			if created.taskDir != nil {
				must(t, created.taskDir.Close())
			}
			must(t, created.err)
		})
	}
	t.Cleanup(join)
	defer join()
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("creator did not reach namespace ownership:", ctx.Err())
	}
	exists, err := store.TaskEntryExists(req.TaskID)
	must(t, err)
	if exists {
		t.Fatal("fixture already created the task directory")
	}
	requireConnectorOriginBusy(t, control, root)
	join()
	_, err = control.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
}

func TestConnectorMutationInspectionIsPassiveAcrossFeatureStates(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "v1_only"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			if enabled {
				enableConnectorFixture(t, store)
			} else {
				writeConnectorRootFeatures(t, store, []string{task.NamespaceAdmissionFeature})
			}
			reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
			must(t, err)
			t.Cleanup(func() { must(t, reader.Close()) })
			gate := newInitializationLockBarrier(namespaceMutationLockName)
			gate.err = errors.New("passive inspection attempted lock synchronization")
			gate.unblock()
			reader.store.SetFaultInjector(gate)
			before := snapshotTree(t, store.Root)
			observation, err := reader.InspectConnectorContext(t.Context(), "")
			must(t, err)
			if observation.RootToken == "" || slices.Contains(observation.Features, task.NamespaceMutationFeature) != enabled {
				t.Fatalf("inspection misreported actual root features: %+v", observation)
			}
			if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
				t.Fatal("inspection changed root authority")
			}
		})
	}
}
