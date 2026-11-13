package taskdir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func enableConnectorFixture(t *testing.T, store *Store) string {
	t.Helper()
	must(t, store.EnableConnectorContext(t.Context(), true))
	root, err := store.RootTokenContext(t.Context())
	must(t, err)
	return root
}

type connectorInventoryCreationGate struct {
	BaseFaultInjector
	directory        string
	beforeTaskRecord bool
	failure          error
	entered          chan struct{}
	release          chan struct{}
	enteredOnce      sync.Once
	releaseOnce      sync.Once
}

func (g *connectorInventoryCreationGate) pause() error {
	g.enteredOnce.Do(func() { close(g.entered) })
	<-g.release
	return g.failure
}

func (g *connectorInventoryCreationGate) OnDirBarrier(path string) error {
	if !g.beforeTaskRecord && path == g.directory {
		return g.pause()
	}
	return nil
}

func (g *connectorInventoryCreationGate) OnBeforeStageCreate(_, destination string) error {
	if g.beforeTaskRecord && destination == filepath.Join(g.directory, "task.json") {
		return g.pause()
	}
	return nil
}

func (g *connectorInventoryCreationGate) requireBoundary(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(g.directory); err != nil {
		t.Fatal("creator has not published its task directory:", err)
	}
	if _, err := os.Stat(filepath.Join(g.directory, "task.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creator already published its task record: %v", err)
	}
	_, admissionErr := os.Stat(filepath.Join(g.directory, ".admission.lock"))
	if g.beforeTaskRecord {
		if admissionErr != nil {
			t.Fatal("prepared-record fixture has no admission lock:", admissionErr)
		}
		return
	}
	if !errors.Is(admissionErr, os.ErrNotExist) {
		t.Fatalf("directory fixture already has an admission lock: %v", admissionErr)
	}
}

func startConnectorInventoryCreation(t *testing.T, store *Store, req *task.TaskRecord, meta *task.MetaRecord, brief []byte, gate *connectorInventoryCreationGate) func() error {
	t.Helper()
	store.SetFaultInjector(gate)
	done := make(chan initializationCreateResult, 1)
	go func() {
		directory, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
		done <- initializationCreateResult{taskDir: directory, err: err}
	}()
	var once sync.Once
	var creationErr error
	join := func() error {
		once.Do(func() {
			gate.releaseOnce.Do(func() { close(gate.release) })
			created := <-done
			creationErr = created.err
			if created.taskDir != nil {
				if err := created.taskDir.Close(); err != nil {
					t.Errorf("close inventory fixture task: %v", err)
				}
			}
		})
		return creationErr
	}
	t.Cleanup(func() {
		if err := join(); err != nil && !errors.Is(err, gate.failure) {
			t.Errorf("inventory fixture creator: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("creator did not reach the inventory fixture boundary:", ctx.Err())
	}
	return join
}

func TestConnectorInventoryRetriesActiveTaskCreation(t *testing.T) {
	for _, phase := range []struct {
		name             string
		beforeTaskRecord bool
	}{{"before_admission_lock", false}, {"before_task_record", true}} {
		t.Run(phase.name, func(t *testing.T) {
			store := testStore(t)
			root := enableConnectorFixture(t, store)
			_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
			must(t, err)
			reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
			must(t, err)
			t.Cleanup(func() { must(t, reader.Close()) })
			req, meta, brief := preparedInput(t, store)
			gate := &connectorInventoryCreationGate{directory: filepath.Join(store.Root, "tasks", req.TaskID), beforeTaskRecord: phase.beforeTaskRecord, entered: make(chan struct{}), release: make(chan struct{})}
			join := startConnectorInventoryCreation(t, store, req, meta, brief, gate)
			gate.requireBoundary(t)
			requireConnectorOriginBusy(t, store, root)
			inventory, err := reader.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
			if !errors.Is(err, task.ErrLockBusy) {
				t.Errorf("active creator inventory error = %v, want retryable lock contention", err)
			}
			if inventory.Complete || inventory.CompletenessReceipt != "" || len(inventory.Entries) != 0 {
				t.Errorf("active creator yielded inventory completeness: %+v", inventory)
			}
			must(t, join())
			inventory, err = reader.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
			must(t, err)
			if !inventory.Complete || inventory.CompletenessReceipt == "" || len(inventory.Entries) != 0 {
				t.Fatalf("completed ordinary creation did not permit inventory: %+v", inventory)
			}
		})
	}
}

func TestConnectorInventoryRejectsOwnerlessFailedTaskCreation(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	req, meta, brief := preparedInput(t, store)
	failure := errors.New("fixture task creation interrupted before its immutable request")
	gate := &connectorInventoryCreationGate{directory: filepath.Join(store.Root, "tasks", req.TaskID), beforeTaskRecord: true, failure: failure, entered: make(chan struct{}), release: make(chan struct{})}
	join := startConnectorInventoryCreation(t, store, req, meta, brief, gate)
	if err = join(); !errors.Is(err, failure) {
		t.Fatalf("creator did not retain the injected interruption: %v", err)
	}
	inventory, err := reader.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	if !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("ownerless creation inventory error = %v, want actionable evidence fault", err)
	}
	if inventory.Complete || inventory.CompletenessReceipt != "" || len(inventory.Entries) != 0 {
		t.Fatalf("ownerless partial yielded inventory completeness: %+v", inventory)
	}
}

func connectorInventoryAfterCreation(t *testing.T, transition func(*Store, string, context.CancelFunc)) (OriginInventory, error) {
	t.Helper()
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	req, meta, brief := preparedInput(t, store)
	creatorGate := &connectorInventoryCreationGate{directory: filepath.Join(store.Root, "tasks", req.TaskID), beforeTaskRecord: true, entered: make(chan struct{}), release: make(chan struct{})}
	joinCreator := startConnectorInventoryCreation(t, store, req, meta, brief, creatorGate)
	readerGate := newInitializationLockBarrier(req.TaskID + ".lock")
	reader.store.SetFaultInjector(readerGate)
	type inventoryResult struct {
		inventory OriginInventory
		err       error
	}
	done := make(chan inventoryResult, 1)
	joined := false
	t.Cleanup(func() {
		readerGate.unblock()
		creationErr := joinCreator()
		if !joined {
			<-done
		}
		if creationErr != nil {
			t.Error("inventory fixture creator:", creationErr)
		}
	})
	inventoryContext, cancelInventory := context.WithCancel(t.Context())
	defer cancelInventory()
	go func() {
		inventory, inventoryErr := reader.OriginInventoryContext(inventoryContext, "gateway", "generation", "", 10, root)
		done <- inventoryResult{inventory: inventory, err: inventoryErr}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), taskInitializationTestTimeout)
	defer cancel()
	select {
	case <-readerGate.entered:
	case early := <-done:
		joined = true
		must(t, joinCreator())
		t.Fatalf("inventory returned before reconciling the creation owner: %+v %v", early.inventory, early.err)
	case <-ctx.Done():
		t.Fatal("inventory did not borrow the existing namespace:", ctx.Err())
	}
	must(t, joinCreator())
	if transition != nil {
		transition(store, req.TaskID, cancelInventory)
	}
	readerGate.unblock()
	observed := <-done
	joined = true
	return observed.inventory, observed.err
}

func TestConnectorInventoryRereadsTaskAfterCreatorReleasesNamespace(t *testing.T) {
	inventory, err := connectorInventoryAfterCreation(t, nil)
	must(t, err)
	if !inventory.Complete || inventory.CompletenessReceipt == "" || len(inventory.Entries) != 0 {
		t.Fatalf("completed ordinary creator did not permit a fresh inventory: %+v", inventory)
	}
}

func TestConnectorInventoryRereadsRefusalBeforeBorrow(t *testing.T) {
	inventory, err := connectorInventoryAfterCreation(t, func(store *Store, id string, _ context.CancelFunc) {
		directory := filepath.Join(store.Root, "tasks", id)
		must(t, os.Remove(filepath.Join(directory, "task.json")))
		must(t, os.Remove(filepath.Join(directory, "meta.json")))
		refused, fenceErr := store.FencePartialTaskContext(t.Context(), id, PartialFenceSelection{OperatorUnattributed: true})
		must(t, fenceErr)
		if refused.Resolution != "permanently_refused" {
			t.Fatalf("fixture failed to publish refusal winner: %+v", refused)
		}
	})
	must(t, err)
	if !inventory.Complete || inventory.CompletenessReceipt == "" || len(inventory.Entries) != 1 || inventory.Entries[0].Resolution != "permanently_refused" || inventory.Entries[0].Receipt != nil {
		t.Fatalf("refusal winner was lost or self-contended: %+v", inventory)
	}
}

func TestConnectorInventoryCancellationBeforeBorrowHasNoCompleteness(t *testing.T) {
	inventory, err := connectorInventoryAfterCreation(t, func(_ *Store, _ string, cancel context.CancelFunc) { cancel() })
	if !errors.Is(err, context.Canceled) || inventory.Complete || inventory.CompletenessReceipt != "" || len(inventory.Entries) != 0 {
		t.Fatalf("cancelled namespace observation yielded completeness: %+v %v", inventory, err)
	}
}

func TestConnectorInventoryAcceptsCompleteLegacyTaskWithoutNamespace(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	directory, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	must(t, err)
	must(t, directory.Close())
	root := enableConnectorFixture(t, store)
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	namespace := filepath.Join(store.Root, "task-namespace", req.TaskID+".lock")
	if _, err = os.Stat(namespace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy task unexpectedly has a namespace owner: %v", err)
	}
	inventory, err := reader.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	if !inventory.Complete || inventory.CompletenessReceipt == "" || len(inventory.Entries) != 0 {
		t.Fatalf("complete legacy ordinary task blocked inventory: %+v", inventory)
	}
	if _, err = os.Stat(namespace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive inventory initialized a legacy namespace: %v", err)
	}
}

func TestConnectorNamespaceRefusalExcludesOrdinaryCreation(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	root := enableConnectorFixture(t, store)
	before, err := store.RootTokenContext(t.Context())
	must(t, err)
	if root != before {
		t.Fatal("upgrade changed physical root identity")
	}
	result, err := store.FenceTaskNamespaceContext(t.Context(), req.TaskID, nil)
	must(t, err)
	if result.Resolution != "permanently_refused" {
		t.Fatalf("fence = %+v", result)
	}
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	if td != nil {
		must(t, td.Close())
	}
	if !errors.Is(err, ErrNamespaceRefused) {
		t.Fatalf("ordinary producer bypassed refusal: %v", err)
	}
	exists, err := store.TaskEntryExists(req.TaskID)
	must(t, err)
	if exists {
		t.Fatal("refused creation wrote task fragments")
	}
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	refusal, err := reader.ReadNamespaceRefusalContext(t.Context(), req.TaskID)
	must(t, err)
	if refusal == nil {
		t.Fatal("passive reader lost permanent refusal")
	}
}

func TestConnectorOriginBarrierRejectsPreparedOldGenerationBeforeMkdir(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	first, err := store.OriginBarrierContext(t.Context(), "gateway", "restore-1", "", "generation-1", root)
	must(t, err)
	if first.AdmissionOpen {
		t.Fatal("barrier opened admissions")
	}
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation-1", "", 10, root)
	must(t, err)
	if !inventory.Complete || len(inventory.Entries) != 0 || inventory.CompletenessReceipt == "" {
		t.Fatalf("empty inventory = %+v", inventory)
	}
	_, err = store.OriginReopenContext(t.Context(), "gateway", "restore-1", "generation-1", inventory.CompletenessReceipt, root)
	must(t, err)
	req, meta, brief := preparedInput(t, store)
	project, err := ObserveProjectContext(t.Context(), req.CanonicalCwd)
	must(t, err)
	req.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
	req.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation-1", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "restore-2", "generation-1", "generation-2", root)
	must(t, err)
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	if td != nil {
		must(t, td.Close())
	}
	if !errors.Is(err, ErrOriginClosed) {
		t.Fatalf("late preparation bypassed barrier: %v", err)
	}
	exists, err := store.TaskEntryExists(req.TaskID)
	must(t, err)
	if exists {
		t.Fatal("stale generation created ordinary fragments")
	}
}

func TestConnectorReopenRejectsStaleMembership(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	origin := &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("b", 64)}
	_, err = store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("e", 32), origin)
	must(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	if !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("stale inventory reopened admission: %v", err)
	}
	refreshed, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	if len(refreshed.Entries) != 1 || refreshed.Entries[0].Resolution != "permanently_refused" || refreshed.Entries[0].Receipt != nil {
		t.Fatalf("refusal absent from canonical inventory: %+v", refreshed)
	}
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", refreshed.CompletenessReceipt, root)
	must(t, err)
}

func TestConnectorUnattributedPartialRequiresExplicitProofAndRetainsEvidence(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	req, meta, brief := preparedInput(t, store)
	directory := filepath.Join(store.Root, "tasks", req.TaskID)
	must(t, store.mkdir(directory, nil))
	must(t, os.WriteFile(filepath.Join(directory, "brief.md"), brief, 0o600))
	if _, err := store.FencePartialTaskContext(t.Context(), req.TaskID, PartialFenceSelection{}); err == nil {
		t.Fatal("automatic unattributed fence accepted")
	}
	result, err := store.FencePartialTaskContext(t.Context(), req.TaskID, PartialFenceSelection{OperatorUnattributed: true})
	must(t, err)
	if result.Resolution != "permanently_refused" || !result.Refusal.Unattributed {
		t.Fatalf("partial fence = %+v", result)
	}
	kept, err := os.ReadFile(filepath.Join(directory, "brief.md"))
	must(t, err)
	if string(kept) != string(brief) {
		t.Fatal("proof deleted or changed preparation fragment")
	}
	td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	if td != nil {
		must(t, td.Close())
	}
	if !errors.Is(err, ErrNamespaceRefused) {
		t.Fatalf("partial refusal allowed late creator: %v", err)
	}
}

func TestConnectorPartialFenceRechecksCompletedPreparation(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	req, meta, brief := preparedInput(t, store)
	directory := filepath.Join(store.Root, "tasks", req.TaskID)
	initial, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	must(t, err)
	must(t, initial.Close())
	must(t, os.Remove(filepath.Join(directory, "meta.json")))
	// The caller's selection predates completion. The owner must not treat that
	// earlier partial observation as permission to refuse a now-complete task.
	frozen, err := store.RecordedTaskIdentityContext(t.Context(), req.TaskID)
	must(t, err)
	if _, err = os.Stat(filepath.Join(directory, "meta.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture was not partial: %v", err)
	}
	created, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, created.Close()) })
	for _, selection := range []struct {
		name     string
		request  *RecordedTaskIdentity
		operator bool
	}{{"frozen", frozen, false}, {"operator", nil, true}} {
		t.Run(selection.name, func(t *testing.T) {
			result, fenceErr := store.FencePartialTaskContext(t.Context(), req.TaskID, PartialFenceSelection{Frozen: selection.request, OperatorUnattributed: selection.operator})
			must(t, fenceErr)
			if result.Resolution != "existing" {
				t.Fatalf("complete preparation was refused: %+v", result)
			}
			refusal, readErr := store.ReadNamespaceRefusalContext(t.Context(), req.TaskID)
			must(t, readErr)
			if refusal != nil {
				t.Fatalf("complete preparation acquired refusal: %+v", refusal)
			}
		})
	}
}

type connectorCreationLinkGate struct {
	BaseFaultInjector
	taskID  string
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (g *connectorCreationLinkGate) OnLink(_, destination string) error {
	if filepath.Base(destination) == "meta.json" && filepath.Base(filepath.Dir(destination)) == g.taskID {
		g.once.Do(func() { close(g.started) })
		<-g.release
	}
	return nil
}

func TestConnectorCreationCommitExcludesNamespaceMutations(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	req, meta, brief := preparedInput(t, store)
	release := make(chan struct{})
	gate := &connectorCreationLinkGate{taskID: req.TaskID, started: make(chan struct{}), release: release}
	store.SetFaultInjector(gate)
	type creation struct {
		directory *TaskDir
		err       error
	}
	done := make(chan creation, 1)
	go func() {
		td, err := store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
		done <- creation{directory: td, err: err}
	}()
	joined := false
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if !joined {
			created := <-done
			if created.directory != nil {
				must(t, created.directory.Close())
			}
		}
	}()
	select {
	case <-gate.started:
	case created := <-done:
		joined = true
		if created.directory != nil {
			must(t, created.directory.Close())
		}
		t.Fatalf("creation ended before the commit gate: %v", created.err)
	}
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("barrier crossed active create commit: %v", err)
	}
	if err = store.EnableConnectorContext(t.Context(), true); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("enablement crossed active create commit: %v", err)
	}
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", strings.Repeat("a", 64), root)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("reopen crossed active create commit: %v", err)
	}
	_, err = store.FenceTaskNamespaceContext(t.Context(), req.TaskID, nil)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("fence crossed active create commit: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	created := <-done
	joined = true
	if created.directory != nil {
		t.Cleanup(func() { must(t, created.directory.Close()) })
	}
	must(t, created.err)
	if created.directory == nil {
		t.Fatal("successful creation returned no task owner")
	}
	_, _, err = created.directory.PreparedRecords()
	must(t, err)
	refusal, err := store.ReadNamespaceRefusalContext(t.Context(), req.TaskID)
	must(t, err)
	if refusal != nil {
		t.Fatal("creation and namespace refusal both committed")
	}
}

func TestConnectorCancelledFenceCreatesNothing(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.FenceTaskNamespaceContext(ctx, strings.Repeat("f", 32), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fence = %v", err)
	}
	refusal, err := store.ReadNamespaceRefusalContext(t.Context(), strings.Repeat("f", 32))
	must(t, err)
	if refusal != nil {
		t.Fatal("cancelled control committed refusal")
	}
}

type connectorVerificationFailure struct {
	BaseFaultInjector
	failure error
	path    string
}

func (f *connectorVerificationFailure) OnConnectorVerificationBarrier(path string) error {
	if f.path == "" || f.path == path {
		return f.failure
	}
	return nil
}

func TestConnectorPassiveRefusalRequiresPersistenceVerification(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	id := strings.Repeat("a", 32)
	_, err := store.FenceTaskNamespaceContext(t.Context(), id, nil)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	before := snapshotTree(t, store.Root)
	failure := errors.New("fixture existing-evidence barrier failure")
	reader.store.SetFaultInjector(&connectorVerificationFailure{failure: failure})
	refusal, err := reader.ReadDurableNamespaceRefusalContext(t.Context(), id)
	if refusal != nil || !errors.Is(err, failure) {
		t.Fatalf("unverified refusal escaped: %+v %v", refusal, err)
	}
	reader.store.SetFaultInjector(nil)
	refusal, err = reader.ReadDurableNamespaceRefusalContext(t.Context(), id)
	must(t, err)
	if refusal == nil || refusal.TaskID != id {
		t.Fatalf("verified refusal missing: %+v", refusal)
	}
	after := snapshotTree(t, store.Root)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("passive verification changed logical state or created files")
	}
}

func TestConnectorBusyNamespaceCannotPublishDurableRefusal(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	id := strings.Repeat("b", 32)
	_, err := store.FenceTaskNamespaceContext(t.Context(), id, nil)
	must(t, err)
	held, err := store.openExistingLock(filepath.Join(store.Root, "task-namespace", id+".lock"), LockLevelNone)
	must(t, err)
	t.Cleanup(func() { must(t, held.Close()) })
	must(t, held.LockEXNonblocking())
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	refusal, err := reader.ReadDurableNamespaceRefusalContext(t.Context(), id)
	if refusal != nil || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("busy namespace published refusal: %+v %v", refusal, err)
	}
	must(t, held.Unlock())
	refusal, err = reader.ReadDurableNamespaceRefusalContext(t.Context(), id)
	must(t, err)
	if refusal == nil {
		t.Fatal("released namespace did not permit verification")
	}
}

type connectorAbsentWriteFailure struct {
	BaseFaultInjector
	target string
	links  int
}

func (f *connectorAbsentWriteFailure) OnLink(_, destination string) error {
	if destination != f.target {
		return nil
	}
	f.links++
	if f.links == 1 {
		return errors.New("fixture first publication absent")
	}
	return nil
}

func TestConnectorAbsentRefusalWriteReplaysExactCandidate(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	id := strings.Repeat("c", 32)
	injector := &connectorAbsentWriteFailure{target: filepath.Join(store.Root, "task-refusals", id+".json")}
	store.SetFaultInjector(injector)
	result, err := store.FenceTaskNamespaceContext(t.Context(), id, nil)
	store.SetFaultInjector(nil)
	must(t, err)
	if result.Resolution != "permanently_refused" || result.Refusal.TaskID != id || injector.links != 2 {
		t.Fatalf("absent replay=%+v links=%d", result, injector.links)
	}
	exists, err := store.TaskEntryExists(id)
	must(t, err)
	if exists {
		t.Fatal("refusal recovery fabricated ordinary task records")
	}
}

func TestConnectorExecutionClosureNeedsCompleteIdleLifetimeEvidence(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	before := snapshotTree(t, store.Root)
	proof, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if proof.Proof != nil {
		t.Fatal("prepared task without permanent stop claimed closure")
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("closure observation created missing lifetime authority")
	}
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	proof, err = reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if proof.Proof != nil || proof.PendingReason != ConnectorClosureOwnerBusy {
		t.Fatal("busy lifetime owner yielded complete closure or lost its pending reason")
	}
	must(t, owner.Release())
	proof, err = reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	wait, err := td.ReadLifetimeWorkerWaitContext(t.Context())
	must(t, err)
	data, err := task.MarshalCanonical(wait)
	must(t, err)
	if proof.Proof == nil || proof.Proof.Kind != ConnectorClosureRegisteredNative || proof.Proof.ReferenceSHA256 != task.ComputeSHA256(data) {
		t.Fatalf("closure did not reference canonical worker Wait: %+v", proof)
	}
	failure := errors.New("fixture closure fsync failure")
	reader.store.SetFaultInjector(&connectorVerificationFailure{failure: failure})
	proof, err = reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	if proof.Proof != nil || !errors.Is(err, failure) {
		t.Fatalf("failed closure barrier yielded proof: %+v %v", proof, err)
	}
}

func TestConnectorOpenLifetimeDoesNotProveClosure(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureOpen)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	proof, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if proof.Proof != nil {
		t.Fatalf("registration/worker without canonical completion yielded closure: %+v", proof)
	}
}

func TestConnectorClosureReferencesActualOwnerBytes(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
	original, err := os.ReadFile(path)
	must(t, err)
	var formatted bytes.Buffer
	must(t, json.Indent(&formatted, original, " ", "  "))
	raw := append(formatted.Bytes(), '\n')
	must(t, os.WriteFile(path, raw, 0o600))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	observed, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if observed.Proof == nil || observed.Proof.ReferenceSHA256 != task.ComputeSHA256(raw) || observed.Proof.ReferenceSHA256 == task.ComputeSHA256(original) {
		t.Fatalf("reference is not actual owner bytes: %+v", observed)
	}
}

func TestConnectorClosureRejectsEveryVerificationBarrierFailure(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	targets := []string{filepath.Join(td.Dir, lifetimeDirectory, lifetimeIntentName), filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName), filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName), filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName), filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName), filepath.Join(td.Dir, lifetimeDirectory), td.Dir, filepath.Dir(td.Dir), store.Root}
	failure := errors.New("fixture verification barrier failure")
	for _, target := range targets {
		reader.store.SetFaultInjector(&connectorVerificationFailure{failure: failure, path: target})
		observed, readErr := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
		if observed.Proof != nil || !errors.Is(readErr, failure) {
			t.Fatalf("barrier %s produced proof: %+v %v", target, observed, readErr)
		}
	}
	reader.store.SetFaultInjector(nil)
}

func TestConnectorMissingNamespaceLockNeverRepairsOrProvesRefusal(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	id := strings.Repeat("d", 32)
	_, err := store.FenceTaskNamespaceContext(t.Context(), id, nil)
	must(t, err)
	path := filepath.Join(store.Root, "task-namespace", id+".lock")
	must(t, os.Remove(path))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	refusal, err := reader.ReadDurableNamespaceRefusalContext(t.Context(), id)
	if refusal != nil || err == nil {
		t.Fatalf("missing namespace owner produced proof: %+v %v", refusal, err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive observation recreated missing owner: %v", err)
	}
}

func TestConnectorExecutionOptionsProjectOnlyCanonicalRequestedValues(t *testing.T) {
	request := task.TaskRecord{Provider: "codex:exec", Mode: "workspace-write", BudgetNanos: 1500000001, CanonicalCwd: "/private/canary-project", RequestedConfig: task.TaskConfig{Model: "chosen-model", Effort: "high", NativeTimeout: "private-canary-timeout"}}
	projected := connectorExecutionOptions(request)
	if projected.Provider != request.Provider || projected.Permission != request.Mode || projected.BudgetNanoseconds != 1500000001 {
		t.Fatalf("execution values changed: %+v", projected)
	}
	if projected.Model.Kind != ConnectorSelectionExplicit || projected.Model.Value != "chosen-model" || projected.Effort.Value != "high" {
		t.Fatalf("requested selections changed: %+v", projected)
	}
	data, err := json.Marshal(projected)
	must(t, err)
	if bytes.Contains(data, []byte("canary")) {
		t.Fatalf("private request fields leaked: %s", data)
	}
	var fields map[string]json.RawMessage
	must(t, json.Unmarshal(data, &fields))
	if len(fields) != 5 {
		t.Fatalf("unexpected execution metadata fields: %s", data)
	}
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "", ""
	projected = connectorExecutionOptions(request)
	if projected.Model.Kind != ConnectorSelectionNativeDefault || projected.Model.Value != "" || projected.Effort.Kind != ConnectorSelectionNativeDefault {
		t.Fatalf("omitted request claimed a resolved default: %+v", projected)
	}
}

type connectorUnlockFailure struct {
	BaseFaultInjector
	name    string
	failure error
}

var _ LockFaultInjector = (*connectorUnlockFailure)(nil)

func (*connectorUnlockFailure) OnLockFileBarrier(string) error      { return nil }
func (*connectorUnlockFailure) OnLockParentDirBarrier(string) error { return nil }

func (f *connectorUnlockFailure) OnFlock(path string, operation int) error {
	if operation == unix.LOCK_UN && filepath.Base(path) == f.name {
		return f.failure
	}
	return nil
}

func TestConnectorClosureCleanupRetainsExactTaskExclusions(t *testing.T) {
	for _, name := range []string{".run.lock", ".admission.lock", lifetimeOwnerLockName} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			td := preparedTask(t, store)
			recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
			reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
			must(t, err)
			fault := &connectorUnlockFailure{name: name, failure: errors.New("fixture task exclusion unlock failure")}
			reader.store.SetFaultInjector(fault)
			var cleanup *ConnectorTaskCleanupError
			t.Cleanup(func() {
				fault.failure = nil
				if cleanup != nil {
					must(t, cleanup.Release())
				}
				must(t, reader.Close())
			})
			observed, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
			if observed.Proof != nil || !errors.Is(err, fault.failure) || !errors.As(err, &cleanup) || !CleanupHasRetainedStore(err) {
				t.Fatalf("cleanup lost exact owner or yielded proof: %+v %v", observed, err)
			}
			if cleanup.RetainedStore() == reader.store || cleanup.RetainedStore() == store {
				t.Fatal("cleanup replaced exact borrowed store with another handle")
			}
			if closeErr := reader.Close(); closeErr == nil {
				t.Fatal("reader closed an ancestor behind pending task cleanup")
			}
			fault.failure = nil
			must(t, cleanup.Release())
			if CleanupHasRetainedStore(err) {
				t.Fatal("settled cleanup retained historical authority")
			}
			must(t, reader.Close())
			_, err = store.PinRootContext(t.Context())
			must(t, err)
		})
	}
}

func TestConnectorPartialFenceCleanupRetainsTaskUntilNamespaceSettles(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	req, _, brief := preparedInput(t, store)
	directory := filepath.Join(store.Root, "tasks", req.TaskID)
	must(t, store.mkdir(directory, nil))
	must(t, os.WriteFile(filepath.Join(directory, "brief.md"), brief, 0o600))
	fault := &connectorUnlockFailure{name: req.TaskID + ".lock", failure: errors.New("fixture nested namespace unlock failure")}
	store.SetFaultInjector(fault)
	var namespace *NamespaceCleanupError
	var cleanup *ConnectorTaskCleanupError
	t.Cleanup(func() {
		fault.failure = nil
		if namespace != nil {
			must(t, namespace.Release())
		}
		if cleanup != nil {
			must(t, cleanup.Release())
		}
	})
	resolution, err := store.FencePartialTaskContext(t.Context(), req.TaskID, PartialFenceSelection{OperatorUnattributed: true})
	if resolution.Resolution == "permanently_refused" || !errors.As(err, &namespace) || !errors.As(err, &cleanup) {
		t.Fatalf("nested cleanup owners lost: %+v %v", resolution, err)
	}
	fault.failure = nil
	must(t, namespace.Release())
	if !CleanupRetainsStore(err, store) {
		t.Fatal("namespace settlement dropped task's ancestor lease")
	}
	other, openErr := OpenStore(store.Root)
	must(t, openErr)
	t.Cleanup(func() { must(t, other.Close()) })
	if lockErr := other.maintLock.LockEXNonblocking(); !errors.Is(lockErr, task.ErrLockBusy) {
		t.Fatalf("maintenance released before task cleanup: %v", lockErr)
	}
	must(t, cleanup.Release())
	if CleanupRetainsStore(err, store) {
		t.Fatal("settled task cleanup retained ancestor")
	}
	must(t, other.maintLock.LockEXNonblocking())
	must(t, other.maintLock.Unlock())
}

type connectorOwnerBytesChange struct {
	BaseFaultInjector
	path    string
	changed bool
}

func (f *connectorOwnerBytesChange) OnConnectorVerificationBarrier(path string) error {
	if path != f.path || f.changed {
		return nil
	}
	f.changed = true
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func TestConnectorChangedOwnerBytesDuringVerificationNeverProvesClosure(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	fault := &connectorOwnerBytesChange{path: filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)}
	reader.store.SetFaultInjector(fault)
	observed, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	if !fault.changed || observed.Proof != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed owner bytes yielded closure: %+v %v", observed, err)
	}
}

func TestConnectorCanonicalDigestCancelsDuringStreaming(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(store.Root, "large-read-fixture")
	must(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reachedRead := false
	observed := &lifetimeErrHookContext{Context: ctx, onErr: func(call int) {
		if call == 3 {
			reachedRead = true
			cancel()
		}
	}}
	digest, length, err := store.fileDigestContext(observed, path)
	if !reachedRead || !errors.Is(err, context.Canceled) || digest != "" || length != 0 {
		t.Fatalf("cancelled stream yielded digest: %q %d %v", digest, length, err)
	}
}

func TestConnectorMissingTaskOwnerStaysPendingWithoutRepair(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	path := filepath.Join(td.Dir, ".run.lock")
	must(t, os.Remove(path))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	observed, err := reader.ReadConnectorExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if observed.Proof != nil || observed.PendingReason != ConnectorClosureMissingOwner {
		t.Fatalf("missing task owner yielded closure: %+v", observed)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive closure repaired missing owner: %v", err)
	}
}

func TestConnectorInventoryReceiptsPreserveFrozenProjectIdentity(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	_, err = store.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	must(t, err)
	want := make(map[string]ConnectorReceipt)
	for range 2 {
		request, meta, brief := preparedInput(t, store)
		project, observeErr := ObserveProjectContext(t.Context(), request.CanonicalCwd)
		must(t, observeErr)
		request.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: request.TaskID, PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
		request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
		td, createErr := store.CreateTaskContext(t.Context(), request.TaskID, request, brief, meta)
		must(t, createErr)
		must(t, td.Close())
		data, readErr := os.ReadFile(filepath.Join(store.Root, "tasks", request.TaskID, "task.json"))
		must(t, readErr)
		projectToken, tokenErr := ProjectToken(project)
		must(t, tokenErr)
		want[request.TaskID] = ConnectorReceipt{RootToken: root, ProjectToken: projectToken, TaskID: request.TaskID, Origin: *request.Origin, SpecSHA256: task.ComputeSHA256(data), BriefSHA256: task.ComputeSHA256(brief), ExecutionOptions: ConnectorExecutionOptions{Provider: "fixture:test", Permission: "read-only", BudgetNanoseconds: request.BudgetNanos, Model: ConnectorRequestedSelection{Kind: ConnectorSelectionNativeDefault}, Effort: ConnectorRequestedSelection{Kind: ConnectorSelectionNativeDefault}}}
		// Neither absent nor replaced paths may change recorded attribution.
		must(t, os.Remove(request.CanonicalCwd))
		if len(want) == 2 {
			must(t, os.Mkdir(request.CanonicalCwd, 0o700))
		}
	}
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery-2", "generation", "generation-2", root)
	must(t, err)
	inventory, err = store.OriginInventoryContext(t.Context(), "gateway", "generation-2", "", 10, root)
	must(t, err)
	if len(inventory.Entries) != len(want) {
		t.Fatalf("inventory has %d entries, want %d", len(inventory.Entries), len(want))
	}
	for _, entry := range inventory.Entries {
		expected, ok := want[entry.TaskID]
		if !ok || entry.Receipt == nil || !reflect.DeepEqual(*entry.Receipt, expected) {
			t.Fatalf("inventory lost immutable receipt for %s: %+v", entry.TaskID, entry.Receipt)
		}
		if entry.SpecSHA256 != expected.SpecSHA256 || entry.BriefSHA256 != expected.BriefSHA256 || !reflect.DeepEqual(*entry.Origin, expected.Origin) {
			t.Fatal("inventory correlation differs from receipt")
		}
	}
}

func TestConnectorOriginBarrierAllowsUnrelatedLifetimeOwner(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	td := preparedTask(t, store)
	control, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	state, err := control.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	if state == nil || state.AdmissionOpen || state.Generation != "generation" {
		t.Fatalf("unrelated lifetime owner blocked barrier: %+v", state)
	}
}

func TestConnectorOriginReopenAllowsUnrelatedLifetimeOwner(t *testing.T) {
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	td := preparedTask(t, store)
	control, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	_, err = control.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	inventory, err := control.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
	must(t, err)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	state, err := control.OriginReopenContext(t.Context(), "gateway", "recovery", "generation", inventory.CompletenessReceipt, root)
	must(t, err)
	if state == nil || !state.AdmissionOpen || state.Generation != "generation" {
		t.Fatalf("unrelated lifetime owner blocked reopen: %+v", state)
	}
}

func TestConnectorPartialFenceAppliesSpecToFrozenSelection(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	td := preparedTask(t, store)
	must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	frozen, err := store.RecordedTaskIdentityContext(t.Context(), td.TaskID)
	must(t, err)
	result, err := store.FencePartialTaskContext(t.Context(), td.TaskID, PartialFenceSelection{Frozen: frozen, ExpectedSpecSHA256: strings.Repeat("f", 64)})
	if !errors.Is(err, task.ErrRequestConflict) || result.Resolution != "unresolved" {
		t.Fatalf("frozen selection ignored supplied spec: %+v %v", result, err)
	}
	refusal, err := store.ReadNamespaceRefusalContext(t.Context(), td.TaskID)
	must(t, err)
	if refusal != nil {
		t.Fatal("mismatching frozen selection committed refusal")
	}
}

func TestConnectorPartialFenceRequiresAbsentEntrySpecEvidence(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	before := snapshotTree(t, store.Root)
	result, err := store.FencePartialTaskContext(t.Context(), strings.Repeat("a", 32), PartialFenceSelection{OperatorUnattributed: true, ExpectedSpecSHA256: strings.Repeat("f", 64)})
	if !errors.Is(err, task.ErrEvidenceFault) || result.Resolution != "unresolved" {
		t.Fatalf("absent task entry yielded spec proof: %+v %v", result, err)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("absent guarded entry caused filesystem mutation")
	}
}

func TestConnectorPartialFenceHashesActualRequestBytes(t *testing.T) {
	store := testStore(t)
	enableConnectorFixture(t, store)
	td := preparedTask(t, store)
	must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	path := filepath.Join(td.Dir, "task.json")
	original, err := os.ReadFile(path)
	must(t, err)
	var formatted bytes.Buffer
	must(t, json.Indent(&formatted, original, "", "  "))
	actual := append(formatted.Bytes(), '\n')
	if bytes.Equal(original, actual) {
		t.Fatal("fixture did not change request representation")
	}
	must(t, os.WriteFile(path, actual, 0o600))
	result, err := store.FencePartialTaskContext(t.Context(), td.TaskID, PartialFenceSelection{OperatorUnattributed: true, ExpectedSpecSHA256: task.ComputeSHA256(actual)})
	must(t, err)
	if result.Resolution != "permanently_refused" || result.Refusal == nil || !result.Refusal.Unattributed {
		t.Fatalf("exact request bytes failed operator spec proof: %+v", result)
	}
}

func TestConnectorInventoryDoesNotRepairMissingOrUnsafeNamespace(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		name := "missing"
		if unsafe {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			root := enableConnectorFixture(t, store)
			_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
			must(t, err)
			td := preparedTask(t, store)
			must(t, td.Close())
			must(t, os.Remove(filepath.Join(td.Dir, "task.json")))
			namespace := filepath.Join(store.Root, "task-namespace", td.TaskID+".lock")
			must(t, os.Remove(namespace))
			if unsafe {
				must(t, os.Symlink(filepath.Join(td.Dir, "brief.md"), namespace))
			}
			before := snapshotTree(t, store.Root)
			inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
			if !errors.Is(err, task.ErrEvidenceFault) || inventory.Complete || inventory.CompletenessReceipt != "" || len(inventory.Entries) != 0 {
				t.Fatalf("unsafe owner yielded completeness: %+v %v", inventory, err)
			}
			if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
				t.Fatal("inventory repaired or changed unsafe namespace")
			}
		})
	}
}
