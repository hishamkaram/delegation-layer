package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func operatorDispositionFixture(t *testing.T) (*Store, *TaskDir, task.OperatorDispositionQueue) {
	t.Helper()
	store := testStore(t)
	td := supervisorTask(t, store)
	supervisorSubmit(t, td)
	receipt := supervisorReceipt(t, td)
	boot, err := lifetime.BootID()
	must(t, err)
	self, err := lifetime.ObserveProcess(os.Getpid())
	must(t, err)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	deadline := time.Now().UTC().Add(time.Hour)
	_, err = td.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "settlement-plan", BootID: boot, Deadline: deadline})
	must(t, err)
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "settlement-scope", BootID: boot, Deadline: deadline})
	must(t, err)
	_, err = td.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{PID: 1 << 30, ProcessStart: self.StartToken(), BootID: boot})
	must(t, err)
	_, err = td.RecordLifetimeLaunchContext(t.Context(), LifetimeLaunchFacts{Sequence: 1, PID: 1<<30 + 1, ProcessStart: self.StartToken(), PGID: 1<<30 + 1})
	must(t, err)
	must(t, owner.Release())
	return store, td, task.OperatorDispositionQueue{NumericTaskID: receipt.NumericTaskID, Label: receipt.Label, Ended: true}
}

func TestOperatorDispositionImmutableRetryAndNativeClaimsRemainUnknown(t *testing.T) {
	store, td, queue := operatorDispositionFixture(t)
	before, err := os.ReadDir(td.Dir)
	must(t, err)
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	must(t, err)
	after, err := os.ReadDir(td.Dir)
	must(t, err)
	if len(before) != len(after) {
		t.Fatal("prepare changed task entries")
	}
	winner, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	must(t, err)
	encoded, err := os.ReadFile(filepath.Join(td.Dir, operatorDispositionName))
	must(t, err)
	again, err := td.ConfirmOperatorDispositionContext(t.Context(), task.OperatorDispositionQueue{}, challenge, lifetime.NativeObservation{})
	must(t, err)
	if !reflect.DeepEqual(winner, again) {
		t.Fatal("retry changed immutable winner")
	}
	for range 2 {
		allowed, readErr := td.ReadSupervisorMaintenancePermissionContext(t.Context(), queue, lifetime.NativeObservation{})
		must(t, readErr)
		if !allowed {
			t.Fatal("immutable permission disappeared between maintenance assessments")
		}
	}
	current, err := os.ReadFile(filepath.Join(td.Dir, operatorDispositionName))
	must(t, err)
	if !bytes.Equal(encoded, current) {
		t.Fatal("maintenance rewrote disposition")
	}
	evidence, err := LoadLifetimeEvidenceContext(t.Context(), td)
	must(t, err)
	closure, err := task.ResolveLifetimeClosure(evidence)
	must(t, err)
	if closure.Closed() || evidence.Termination != nil || evidence.WorkerWait != nil {
		t.Fatal("administrative permission fabricated native closure")
	}
	observed, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	must(t, err)
	if observed.Proof != nil {
		t.Fatal("administrative permission released generic claims")
	}
	for _, name := range []string{"outcome.json", "provider.exit", "lifetime/terminated.json", "lifetime/worker-wait.json"} {
		if _, err = os.Stat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fabricated %s: %v", name, err)
		}
	}
}

func TestOperatorDispositionRefusesExactQueueFaultsAndStaleChallenge(t *testing.T) {
	_, td, queue := operatorDispositionFixture(t)
	for _, bad := range []task.OperatorDispositionQueue{{}, {NumericTaskID: queue.NumericTaskID + 1, Label: queue.Label, Ended: true}, {NumericTaskID: queue.NumericTaskID, Label: queue.Label + "x", Ended: true}} {
		if _, _, err := td.PrepareOperatorDispositionContext(t.Context(), bad, lifetime.NativeObservation{}); err == nil {
			t.Fatal("accepted unknown or mismatched queue")
		}
	}
	_, _, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	must(t, err)
	if _, err = td.ConfirmOperatorDispositionContext(t.Context(), queue, strings.Repeat("a", 64), lifetime.NativeObservation{}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("stale challenge: %v", err)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, operatorDispositionName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale challenge persisted permission")
	}
}

func TestOperatorDispositionRefusesLiveAndUnknownNativeIdentities(t *testing.T) {
	for _, kind := range []string{"worker", "launch", "unknown-worker", "unknown-launch", "unknown-boot"} {
		t.Run(kind, func(t *testing.T) {
			store, td, queue := operatorDispositionFixture(t)
			mutateDispositionNativeFixture(t, td, kind)
			if _, _, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{}); err == nil {
				t.Fatal("unproven native identity authorized settlement")
			}
			if _, err := store.TaskEntryExists(td.TaskID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOperatorDispositionPrepareNeverCreatesMissingLocks(t *testing.T) {
	for _, name := range []string{".admission.lock", ".run.lock", "lifetime/owner.lock"} {
		t.Run(name, func(t *testing.T) {
			store, td, queue := operatorDispositionFixture(t)
			must(t, td.Close())
			must(t, os.Remove(filepath.Join(td.Dir, name)))
			metadata, err := OpenExistingStoreMetadataContext(t.Context(), store.Root, store.predicates)
			must(t, err)
			defer closeTestStore(t, metadata)
			opened, err := metadata.OpenTask(td.TaskID)
			if err == nil {
				_, _, err = opened.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
				must(t, opened.Close())
			}
			if err == nil {
				t.Fatal("missing lock accepted")
			}
			if _, err = os.Stat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing lock created: %v", err)
			}
		})
	}
}

func TestOperatorDispositionRetainsBusyOwner(t *testing.T) {
	store, td, queue := operatorDispositionFixture(t)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	other, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	_, _, err = other.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("active lifetime owner: %v", err)
	}
	must(t, owner.Release())
}

func TestOperatorDispositionAmbiguousWritePreservesWinnerAndError(t *testing.T) {
	store, td, queue := operatorDispositionFixture(t)
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	must(t, err)
	fault := errors.New("lost disposition acknowledgement")
	hook := &completionLinkFault{target: filepath.Join(td.Dir, operatorDispositionName), injected: fault}
	store.SetFaultInjector(hook)
	winner, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	store.SetFaultInjector(nil)
	if winner == nil || !errors.Is(err, fault) || !hook.fired {
		t.Fatalf("lost ack winner=%+v error=%v", winner, err)
	}
	again, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	must(t, err)
	if !reflect.DeepEqual(winner, again) {
		t.Fatal("lost ack retry changed winner")
	}
}

func TestOperatorDispositionConcurrentConfirmsKeepOneWinner(t *testing.T) {
	store, td, queue := operatorDispositionFixture(t)
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	must(t, err)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			opened, openErr := OpenExistingStoreMetadataContext(t.Context(), store.Root, store.predicates)
			if openErr != nil {
				outcomes <- openErr
				return
			}
			current, openErr := opened.OpenTask(td.TaskID)
			if openErr != nil {
				outcomes <- errors.Join(openErr, opened.Close())
				return
			}
			_, confirmErr := current.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
			outcomes <- errors.Join(confirmErr, current.Close(), opened.Close())
		})
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil && !errors.Is(err, task.ErrLockBusy) {
			t.Fatal(err)
		}
	}
	winner, err := td.ConfirmOperatorDispositionContext(context.Background(), queue, challenge, lifetime.NativeObservation{})
	must(t, err)
	if winner.Challenge != challenge {
		t.Fatal("concurrent confirmations changed challenge")
	}
}

func TestOperatorDispositionRejectsRootSubstitutionAndAdvancedLaunchSet(t *testing.T) {
	for _, mode := range []string{"root", "launch"} {
		t.Run(mode, func(t *testing.T) {
			store, td, queue := operatorDispositionFixture(t)
			_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
			must(t, err)
			if mode == "root" {
				must(t, os.Rename(store.Root, store.Root+".original"))
				replacement, initErr := InitStore(store.Root)
				must(t, initErr)
				t.Cleanup(func() { must(t, replacement.Close()) })
			} else {
				self, observeErr := lifetime.ObserveProcess(os.Getpid())
				must(t, observeErr)
				owner, ownerErr := td.AcquireLifetimeOwner(t.Context())
				must(t, ownerErr)
				_, err = td.RecordLifetimeLaunchContext(t.Context(), LifetimeLaunchFacts{Sequence: 2, PID: 1<<30 + 2, ProcessStart: self.StartToken(), PGID: 1<<30 + 2})
				must(t, err)
				must(t, owner.Release())
			}
			if _, err = td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{}); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("changed %s subject accepted: %v", mode, err)
			}
		})
	}
}

type dispositionWriteFault struct {
	BaseFaultInjector
	target, mode string
	cause        error
	fired        bool
}

func (f *dispositionWriteFault) OnStageWrite(_ string, data []byte) (int, error) {
	if f.mode == "absent" && !f.fired {
		f.fired = true
		return 0, f.cause
	}
	return len(data), nil
}

func (f *dispositionWriteFault) OnAfterLinkDirBarrier(path string) error {
	if path != f.target || f.fired {
		return nil
	}
	f.fired = true
	if f.mode == "unreadable" {
		return errors.Join(f.cause, os.WriteFile(path, []byte("{broken"), 0o600))
	}
	if f.mode == "conflicting" {
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.Join(f.cause, err)
		}
		var record task.OperatorDispositionRecord
		if err = task.DecodeStrict(data, &record); err != nil {
			return errors.Join(f.cause, err)
		}
		record.ConfirmedAt = "2026-11-15T00:00:00Z"
		data, err = task.MarshalCanonical(record)
		if err != nil {
			return errors.Join(f.cause, err)
		}
		return errors.Join(f.cause, os.WriteFile(path, data, 0o600))
	}
	return f.cause
}

func TestOperatorDispositionAmbiguousAbsentConflictingAndUnreadable(t *testing.T) {
	for _, mode := range []string{"absent", "conflicting", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			store, td, queue := operatorDispositionFixture(t)
			_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
			must(t, err)
			injected := errors.New("ambiguous disposition write")
			hook := &dispositionWriteFault{target: filepath.Join(td.Dir, operatorDispositionName), mode: mode, cause: injected}
			store.SetFaultInjector(hook)
			winner, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
			store.SetFaultInjector(nil)
			if !hook.fired || !errors.Is(err, injected) {
				t.Fatalf("write cause disappeared: winner=%+v err=%v", winner, err)
			}
			if mode == "absent" && winner == nil {
				t.Fatal("authoritative absence did not replay the original candidate")
			}
			if mode == "conflicting" && (winner == nil || !errors.Is(err, task.ErrInvariantFault)) {
				t.Fatalf("conflicting winner not preserved: %+v %v", winner, err)
			}
			if mode == "unreadable" && winner != nil {
				t.Fatal("unreadable winner was replaced")
			}
		})
	}
}

type dispositionBootObservation struct {
	lifetime.NativeObservation
	boot string
}

func (n dispositionBootObservation) CurrentBootID() (string, error) { return n.boot, nil }

func TestOperatorDispositionRefusesUnknownOrPreviousBootAndAllActiveOwners(t *testing.T) {
	for _, boot := range []string{"unknown", "linux:00000000-0000-0000-0000-000000000001", "darwin:boot-session:00000000-0000-0000-0000-000000000001"} {
		t.Run(boot, func(t *testing.T) {
			_, td, queue := operatorDispositionFixture(t)
			if _, _, err := td.PrepareOperatorDispositionContext(t.Context(), queue, dispositionBootObservation{boot: boot}); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("unknown/previous boot authorized settlement: %v", err)
			}
		})
	}
	for _, name := range []string{".admission.lock", ".run.lock", "lifetime/owner.lock"} {
		t.Run(name, func(t *testing.T) {
			store, td, queue := operatorDispositionFixture(t)
			held, err := store.openExistingLock(filepath.Join(td.Dir, name), LockLevelRun)
			must(t, err)
			must(t, held.LockEX(t.Context()))
			t.Cleanup(func() { must(t, held.Close()) })
			other, err := store.OpenTask(td.TaskID)
			must(t, err)
			t.Cleanup(func() { must(t, other.Close()) })
			_, _, err = other.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
			must(t, held.Unlock())
			if !errors.Is(err, task.ErrLockBusy) {
				t.Fatalf("active %s authorized settlement: %v", name, err)
			}
		})
	}
}

func mutateDispositionNativeFixture(t *testing.T, td *TaskDir, kind string) {
	t.Helper()
	self, err := lifetime.ObserveProcess(os.Getpid())
	must(t, err)
	if strings.Contains(kind, "launch") {
		records, readErr := td.ReadLifetimeLaunchesContext(t.Context())
		must(t, readErr)
		record := records[0]
		if kind == "launch" {
			record.PID = os.Getpid()
			record.ProcessStart = self.StartToken()
		} else {
			record.ProcessStart = "unknown"
		}
		data, encodeErr := task.MarshalCanonical(record)
		must(t, encodeErr)
		must(t, os.WriteFile(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(1)), data, 0o600))
	} else if kind == "unknown-boot" {
		registration, readErr := td.ReadLifetimeRegistrationContext(t.Context())
		must(t, readErr)
		registration.BootID = "unknown"
		data, encodeErr := task.MarshalCanonical(registration)
		must(t, encodeErr)
		must(t, os.WriteFile(filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName), data, 0o600))
	} else {
		record, readErr := td.ReadLifetimeWorkerContext(t.Context())
		must(t, readErr)
		if kind == "worker" {
			record.PID = os.Getpid()
			record.ProcessStart = self.StartToken()
		} else {
			record.ProcessStart = "unknown"
		}
		data, encodeErr := task.MarshalCanonical(record)
		must(t, encodeErr)
		must(t, os.WriteFile(filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName), data, 0o600))
	}
}

type dispositionCancelAfterCommit struct {
	BaseFaultInjector
	target string
	cancel context.CancelFunc
}

func (f dispositionCancelAfterCommit) OnAfterLinkDirBarrier(path string) error {
	if path == f.target {
		f.cancel()
	}
	return nil
}

func TestOperatorDispositionCancellationAfterCommitKeepsImmutableWinner(t *testing.T) {
	store, td, queue := operatorDispositionFixture(t)
	_, challenge, err := td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.SetFaultInjector(&dispositionCancelAfterCommit{target: filepath.Join(td.Dir, operatorDispositionName), cancel: cancel})
	_, err = td.ConfirmOperatorDispositionContext(ctx, queue, challenge, lifetime.NativeObservation{})
	store.SetFaultInjector(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commit cancellation was lost: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(td.Dir, operatorDispositionName))
	must(t, err)
	winner, err := td.ConfirmOperatorDispositionContext(t.Context(), queue, challenge, lifetime.NativeObservation{})
	must(t, err)
	after, err := os.ReadFile(filepath.Join(td.Dir, operatorDispositionName))
	must(t, err)
	if winner.Challenge != challenge || !bytes.Equal(before, after) {
		t.Fatal("cancelled acknowledgement rewrote durable winner")
	}
}
