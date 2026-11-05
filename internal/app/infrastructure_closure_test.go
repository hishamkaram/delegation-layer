package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestRootTaskClosureEmptyRootIsReadOnly(t *testing.T) {
	store := newBareInspectionStore(t)
	requireRootClosureCensus(t, store, false)
}

func TestRootTaskClosureOrdinaryEvidence(t *testing.T) {
	for _, state := range []string{"prepared", "prepared owner released", "fresh refusal", "settled refusal", "native closed", "native active", "owner busy", "provider started"} {
		t.Run(state, func(t *testing.T) {
			store, td := newRootClosureOrdinaryFixture(t, state)
			requireRootClosureCensus(t, store, state != "settled refusal" && state != "native closed")
			if state == "settled refusal" {
				owner, err := td.TryAcquireLifetimeOwnerContext(t.Context())
				if err != nil {
					t.Fatalf("census retained lifetime ownership: %v", err)
				}
				if err = owner.Release(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func newRootClosureOrdinaryFixture(t *testing.T, state string) (*taskdir.Store, *taskdir.TaskDir) {
	t.Helper()
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	switch state {
	case "fresh refusal":
		recordRootClosureRefusal(t, td)
	case "settled refusal", "provider started":
		settleRootClosureRefusal(t, td)
	case "native closed":
		recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	case "native active":
		recordCustodyLifetime(t, td, nil, custodyLifetimeOpen)
	case "owner busy":
		recordRootClosureRefusal(t, td)
		holdRootClosureLifetimeOwner(t, td)
	case "prepared owner released":
		owner := holdRootClosureLifetimeOwner(t, td)
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Fatal(releaseErr)
		}
	}
	if state == "provider started" {
		// An orphaned native start marker cannot become proof of absence.
		writeAppTestFile(t, filepath.Join(td.Dir, "provider.started.json"), []byte("{}"))
	}
	return store, td
}

func holdRootClosureLifetimeOwner(t *testing.T, td *taskdir.TaskDir) *taskdir.LifetimeOwnerLease {
	t.Helper()
	owner, acquireErr := td.AcquireLifetimeOwner(t.Context())
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	return owner
}

func recordRootClosureRefusal(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	permit, err := td.PrepareStop(strings.Repeat("d", 32), "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
}

func settleRootClosureRefusal(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	recordRootClosureRefusal(t, td)
	// Model an earlier explicit stop/settlement, which owns lock
	// initialization. The later census must only read that state.
	proven, _, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
	if err != nil || !proven {
		t.Fatalf("explicit refusal settlement = %t, %v", proven, err)
	}
}

func TestRootTaskClosureInspectionEvidence(t *testing.T) {
	for _, state := range []string{"expired", "unlaunched", "native closed", "native active"} {
		t.Run(state, func(t *testing.T) {
			store := newBareInspectionStore(t)
			operation := newRootClosureInspection(t, store)
			switch state {
			case "unlaunched":
				if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "native closed":
				recordCustodyLifetimeUntil(t, operation, nil, task.LifetimeTerminationCompleted, operation.Deadline())
			case "native active":
				recordCustodyLifetimeUntil(t, operation, nil, custodyLifetimeOpen, operation.Deadline())
			}
			// No ordinary task or supervisor row exists for this inspection.
			requireRootClosureCensus(t, store, state == "expired" || state == "native active")
		})
	}
}

func newRootClosureInspection(t *testing.T, store *taskdir.Store) *inspection.Operation {
	t.Helper()
	digest := strings.Repeat("a", 64)
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/fixture/helper", HelperSHA256: digest,
		WorkerExecutable: "/fixture/worker", WorkerSHA256: digest,
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
	}
	operation, err := inspection.OpenOperationContext(t.Context(), store, newBareInspectionRequest(t, store), binding,
		time.Now().Add(-inspection.AdmissionTimeout-time.Second).UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	})
	return operation
}

func TestRootTaskClosureWorkspaceEvidence(t *testing.T) {
	for _, state := range []string{"expired", "unlaunched", "native closed", "native active"} {
		t.Run(state, func(t *testing.T) {
			store, operation := workspacefixture.NewOperationDeadline(t, time.Now().Add(-time.Second))
			switch state {
			case "unlaunched":
				if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "native closed":
				recordCustodyLifetimeUntil(t, operation, nil, task.LifetimeTerminationCompleted, operation.Deadline())
			case "native active":
				recordCustodyLifetimeUntil(t, operation, nil, custodyLifetimeOpen, operation.Deadline())
			}
			// The operation is intentionally orphaned from a workspace resource.
			requireRootClosureCensus(t, store, state == "expired" || state == "native active")
		})
	}
}

func TestRootTaskClosureIncludesEveryJournalFamily(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	operation := newRootClosureInspection(t, store)
	if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: strings.Repeat("e", 32),
		WorkspaceID: strings.Repeat("f", 32), Kind: workspace.OperationCapture,
		Subject:          workspace.Subject{Kind: workspace.SubjectTask, ID: td.TaskID, SHA256: digest},
		Deadline:         time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/fixture/worker", WorkerSHA256: digest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
	}
	workspaceOperation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := workspaceOperation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	requireRootClosureCensus(t, store, true)
	if _, err = workspaceOperation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireRootClosureCensus(t, store, false)
}

func TestRootTaskClosurePartialCandidatesAndCorruptRequestsRefuse(t *testing.T) {
	for _, parent := range []string{"tasks", "inspections", "workspace-operations"} {
		t.Run(parent, func(t *testing.T) {
			store := newBareInspectionStore(t)
			directory := filepath.Join(store.Root, parent, strings.Repeat("e", 32))
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			requireRootClosureCensus(t, store, true)
		})
	}
	t.Run("corrupt task", func(t *testing.T) {
		store, td, _ := newAppTestTask(t, false)
		t.Cleanup(func() { closeAppTestTask(t, store, td) })
		settleRootClosureRefusal(t, td)
		writeAppTestFile(t, filepath.Join(td.Dir, "meta.json"), []byte("{"))
		requireRootClosureSyntaxFault(t, store)
	})
	t.Run("corrupt inspection", func(t *testing.T) {
		store := newBareInspectionStore(t)
		operation := newRootClosureInspection(t, store)
		writeAppTestFile(t, filepath.Join(store.Root, "inspections", operation.Request().IdentityID(), "request.json"), []byte("{"))
		requireRootClosureSyntaxFault(t, store)
	})
	t.Run("corrupt workspace", func(t *testing.T) {
		store, operation := workspacefixture.NewOperation(t)
		writeAppTestFile(t, filepath.Join(store.Root, "workspace-operations", operation.Request().OperationID, "request.json"), []byte("{"))
		requireRootClosureSyntaxFault(t, store)
	})
}

func TestRootTaskClosureChecksEveryOrdinaryCandidate(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	second, _ := newAppTaskInStore(t, store, strings.Repeat("e", 32), false, nil)
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	requireRootClosureCensus(t, store, true)
	settleRootClosureRefusal(t, second)
	requireRootClosureCensus(t, store, false)
}

func TestRootTaskClosurePreservesJoinedProofFaultAndReleasesOwner(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	settleRootClosureRefusal(t, td)
	fault := errors.New("run lock storage fault")
	store.SetFaultInjector(&rootClosureLockFault{err: errors.Join(guardian.ErrProofPending, fault)})
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	before := rootClosureSnapshot(t, store.Root)
	err := checkRootTaskClosure(t.Context(), store)
	if !errors.Is(err, fault) || !errors.Is(err, guardian.ErrProofPending) || errors.Is(err, errRootTaskClosureBusy) {
		t.Fatalf("joined proof fault was hidden or classified as busy: %v", err)
	}
	if after := rootClosureSnapshot(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("faulting proof changed durable storage")
	}
	store.SetFaultInjector(nil)
	// The rejected acquisition and the census handle must release ownership,
	// allowing the same production records to be verified on the next call.
	requireRootClosureCensus(t, store, false)
}

func TestRootTaskClosureBusyRunReleasesLifetimeOwner(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	settleRootClosureRefusal(t, td)
	lock, err := taskdir.OpenLockFile(filepath.Join(td.Dir, ".run.lock"), taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lock.LockEXNonblockingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireRootClosureCensus(t, store, true)
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	requireRootClosureCensus(t, store, false)
}

type rootClosureLockFault struct {
	taskdir.BaseFaultInjector
	err error
}

func (*rootClosureLockFault) OnLockFileBarrier(string) error      { return nil }
func (*rootClosureLockFault) OnLockParentDirBarrier(string) error { return nil }

func (f *rootClosureLockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".run.lock" && operation&unix.LOCK_EX != 0 {
		return f.err
	}
	return nil
}

func requireRootClosureSyntaxFault(t *testing.T, store *taskdir.Store) {
	t.Helper()
	before := rootClosureSnapshot(t, store.Root)
	err := checkRootTaskClosure(t.Context(), store)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || errors.Is(err, errRootTaskClosureBusy) {
		t.Fatalf("census discarded or misclassified record fault: %v", err)
	}
	if after := rootClosureSnapshot(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("faulting census changed durable storage")
	}
}

func TestRootTaskClosureCancellationAndClosedStore(t *testing.T) {
	store := newBareInspectionStore(t)
	before := rootClosureSnapshot(t, store.Root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkRootTaskClosure(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("early cancellation = %v, want context cancellation", err)
	}
	if after := rootClosureSnapshot(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("cancelled census changed storage")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkRootTaskClosure(t.Context(), store); err == nil || errors.Is(err, errRootTaskClosureBusy) {
		t.Fatalf("closed store became successful or busy: %v", err)
	}
}

func TestRootTaskClosureWholeErrorClassification(t *testing.T) {
	fault := errors.New("storage fault")
	for _, test := range []struct {
		name string
		err  error
		busy bool
	}{
		{name: "pending", err: guardian.ErrProofPending, busy: true},
		{name: "missing", err: fs.ErrNotExist, busy: true},
		{name: "lock busy", err: task.ErrLockBusy, busy: true},
		{name: "joined pending", err: errors.Join(guardian.ErrProofPending, task.ErrLockBusy), busy: true},
		{name: "joined fault", err: errors.Join(guardian.ErrProofPending, fault)},
		{name: "missing and fault", err: errors.Join(fs.ErrNotExist, fault)},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := rootClosureError("task", "candidate", test.err)
			if errors.Is(err, errRootTaskClosureBusy) != test.busy || !errors.Is(err, test.err) {
				t.Fatalf("census classification = %v, want busy %t and original cause", err, test.busy)
			}
		})
	}
}

func requireRootClosureCensus(t *testing.T, store *taskdir.Store, busy bool) {
	t.Helper()
	before := rootClosureSnapshot(t, store.Root)
	err := checkRootTaskClosure(t.Context(), store)
	if busy {
		if !errors.Is(err, errRootTaskClosureBusy) {
			t.Fatalf("census = %v, want unproven closure", err)
		}
	} else if err != nil {
		t.Fatalf("closed census = %v", err)
	}
	if after := rootClosureSnapshot(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only census changed durable storage")
	}
}

type rootClosureEntry struct {
	mode     os.FileMode
	modified time.Time
	content  string
}

func rootClosureSnapshot(t *testing.T, root string) map[string]rootClosureEntry {
	t.Helper()
	entries := make(map[string]rootClosureEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := rootClosureEntry{mode: info.Mode(), modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value.content = string(data)
		}
		entries[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
