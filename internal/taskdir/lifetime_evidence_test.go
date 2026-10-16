package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type completionTestStore interface {
	LifetimeRecordReader
	AcquireLifetimeOwner(context.Context) (*LifetimeOwnerLease, error)
	RecordLifetimeIntentContext(context.Context, LifetimeIntentFacts) (*task.LifetimeIntentRecord, error)
	RecordLifetimeRegistrationContext(context.Context, LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error)
	RecordLifetimeWorkerContext(context.Context, LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error)
	RecordLifetimeWorkerWaitContext(context.Context, LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error)
	RecordLifetimeTerminationContext(context.Context, string) (*task.LifetimeTerminationRecord, error)
	RecordLifetimeWorkerAbsenceContext(context.Context, task.LifetimeAbsenceProof, string) (*task.LifetimeWorkerWaitRecord, error)
}

func newCompletionTestStore(t *testing.T, family string) (completionTestStore, string) {
	t.Helper()
	store := testStore(t)
	if family == "task" {
		td := preparedTask(t, store)
		return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
	}
	id := strings.Repeat("a", 32)
	ownerKind := task.LifetimeOwnerInspection
	var control *ControlDir
	var err error
	if family == "workspace" {
		ownerKind = task.LifetimeOwnerWorkspace
		control, err = store.OpenWorkspaceOperation(t.Context(), id, true)
	} else {
		control, err = store.OpenInspection(id, true)
	}
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	records, err := NewControlLifetime(control, store.RootID, id, ownerKind, task.ComputeSHA256([]byte("completion-test")), time.Now().UTC().Add(time.Hour))
	must(t, err)
	return records, filepath.Join(control.dir, controlLifetimeWorkerWaitName)
}

func prepareCompletionTestIntent(t *testing.T, records completionTestStore) LifetimeRegistrationFacts {
	t.Helper()
	deadline := time.Now().UTC().Add(time.Hour)
	if control, ok := records.(*ControlLifetime); ok {
		deadline = control.deadline
	}
	_, err := records.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "completion-plan", BootID: "completion-boot", Deadline: deadline})
	must(t, err)
	return LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "completion-scope", BootID: "completion-boot", Deadline: deadline}
}

func TestLifetimeEvidenceUnregisteredRecoveryFencesLateRegistration(t *testing.T) {
	for _, family := range []string{"task", "inspection", "workspace"} {
		t.Run(family, func(t *testing.T) {
			records, _ := newCompletionTestStore(t, family)
			owner, err := records.AcquireLifetimeOwner(t.Context())
			must(t, err)
			t.Cleanup(func() { must(t, owner.Release()) })
			facts := prepareCompletionTestIntent(t, records)
			completion, err := records.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID)
			must(t, err)
			if completion.RegistrationSHA256 != "" || completion.IntentSHA256 == "" || completion.ExitCode != nil {
				t.Fatalf("fabricated native facts: %+v", completion)
			}
			evidence, err := LoadLifetimeEvidenceContext(t.Context(), records)
			must(t, err)
			closure, err := task.ResolveLifetimeClosure(evidence)
			must(t, err)
			if closure.Kind != task.LifetimeClosureUnregisteredRecovered {
				t.Fatalf("closure = %+v", closure)
			}
			if _, err = records.RecordLifetimeRegistrationContext(t.Context(), facts); !errors.Is(err, task.ErrTerminalTask) {
				t.Fatalf("late registration = %v, want terminal refusal", err)
			}
			again, err := records.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID)
			must(t, err)
			if *again != *completion {
				t.Fatal("recovery retry changed the completion winner")
			}
		})
	}
}

func TestLifetimeEvidenceRecoveredCompletionCannotBeReplacedByNativeWait(t *testing.T) {
	for _, family := range []string{"task", "inspection", "workspace"} {
		t.Run(family, func(t *testing.T) {
			records, path := newCompletionTestStore(t, family)
			owner, err := records.AcquireLifetimeOwner(t.Context())
			must(t, err)
			t.Cleanup(func() { must(t, owner.Release()) })
			facts := prepareCompletionTestIntent(t, records)
			_, err = records.RecordLifetimeRegistrationContext(t.Context(), facts)
			must(t, err)
			worker, err := records.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{PID: 1234, ProcessStart: "completion-start", BootID: facts.BootID})
			must(t, err)
			termination, err := records.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted)
			must(t, err)
			completion, err := records.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceExactWorker, facts.BootID)
			must(t, err)
			before, err := os.ReadFile(path)
			must(t, err)
			workerData, err := task.MarshalCanonical(worker)
			must(t, err)
			exitCode := 0
			_, err = records.RecordLifetimeWorkerWaitContext(t.Context(), LifetimeWorkerWaitFacts{WorkerSHA256: task.ComputeSHA256(workerData), ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &exitCode})
			if err == nil {
				t.Fatal("native Wait overwrote recovered completion")
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("completion bytes changed after conflicting writer")
			}
			evidence, err := LoadLifetimeEvidenceContext(t.Context(), records)
			must(t, err)
			closure, err := task.ResolveLifetimeClosure(evidence)
			must(t, err)
			if closure.Kind != task.LifetimeClosureRegisteredRecovered || *evidence.Termination != *termination || *evidence.WorkerWait != *completion {
				t.Fatalf("recovery changed native authority: %+v", closure)
			}
		})
	}
}

type completionLinkFault struct {
	BaseFaultInjector
	target   string
	injected error
	fired    bool
}

func (f *completionLinkFault) OnAfterLinkDirBarrier(destination string) error {
	if destination == f.target && !f.fired {
		f.fired = true
		return f.injected
	}
	return nil
}

func injectCompletionLinkFault(t *testing.T, records completionTestStore, fault *completionLinkFault) {
	t.Helper()
	var store *Store
	switch recordStore := records.(type) {
	case *TaskDir:
		store = recordStore.store
	case *ControlLifetime:
		store = recordStore.control.store
	default:
		t.Fatal("completion fixture did not use a production record store")
	}
	store.faultInjector = fault
	t.Cleanup(func() { store.faultInjector = nil })
}

func TestLifetimeEvidenceAmbiguousAbsenceWriteReturnsMatchingWinner(t *testing.T) {
	for _, family := range []string{"task", "inspection", "workspace"} {
		t.Run(family, func(t *testing.T) {
			records, path := newCompletionTestStore(t, family)
			owner, err := records.AcquireLifetimeOwner(t.Context())
			must(t, err)
			t.Cleanup(func() { must(t, owner.Release()) })
			facts := prepareCompletionTestIntent(t, records)
			injected := errors.New("completion publication acknowledgement failed")
			fault := &completionLinkFault{target: path, injected: injected}
			injectCompletionLinkFault(t, records, fault)
			completion, err := records.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID)
			if !errors.Is(err, injected) || completion == nil {
				t.Fatalf("matching published winner was not classified: completion=%+v err=%v", completion, err)
			}
			evidence, err := LoadLifetimeEvidenceContext(t.Context(), records)
			must(t, err)
			closure, err := task.ResolveLifetimeClosure(evidence)
			must(t, err)
			if !closure.Closed() || evidence.WorkerWait == nil || *evidence.WorkerWait != *completion {
				t.Fatal("ambiguous acknowledgement lost or changed durable closure")
			}
		})
	}
}
