package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type lifetimeTerminationRecorderProbe struct {
	ctx   context.Context
	calls int
}

type lifetimeTestContextKey struct{}

func (p *lifetimeTerminationRecorderProbe) RecordLifetimeTerminationContext(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
	p.ctx = ctx
	p.calls++
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &task.LifetimeTerminationRecord{Cause: cause, ScopeEmpty: true}, nil
}

func TestRecordTaskLifetimeTerminationContextPreservesCallerAndNilGuard(t *testing.T) {
	probe := &lifetimeTerminationRecorderProbe{}
	var nilContext context.Context
	if _, err := recordTaskLifetimeTerminationContext(nilContext, probe, task.LifetimeTerminationRecovery); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil context error = %v, want context canceled", err)
	}
	if probe.calls != 0 {
		t.Fatalf("nil context invoked recorder %d times, want 0", probe.calls)
	}

	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), lifetimeTestContextKey{}, marker)
	if _, err := recordTaskLifetimeTerminationContext(ctx, probe, task.LifetimeTerminationRecovery); err != nil {
		t.Fatalf("record with caller context: %v", err)
	}
	if probe.ctx != ctx {
		t.Fatal("record did not receive the exact caller context")
	}
	if probe.ctx.Value(lifetimeTestContextKey{}) != marker {
		t.Fatal("record lost caller context value")
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := recordTaskLifetimeTerminationContext(canceled, probe, task.LifetimeTerminationRecovery); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want context canceled", err)
	}
	if probe.ctx != canceled {
		t.Fatal("canceled caller context was replaced")
	}
}

func TestRecoverTaskLifetimeRefusesCooperativeScopeWithoutLaunchIdentity(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, currentBootID(t))

	if err := recoverTaskLifetime(t.Context(), td); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recovery error = %v, want unverifiable launch", err)
	}
	if _, err := td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cooperative scope received false termination evidence: %v", err)
	}
}

func TestRecoverTaskLifetimeDoesNotIgnorePresentRecordsWithMissingPreparedInput(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, currentBootID(t))
	if err := os.Remove(filepath.Join(td.Dir, "brief.md")); err != nil {
		t.Fatal(err)
	}

	err := recoverTaskLifetime(t.Context(), td)
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestRecoverLifetimeScopeAcceptsConcurrentCleanupOnlyAfterEmptyRecheck(t *testing.T) {
	scope := &lifetimeGuardianTestScope{
		timeline:     &lifetimeGuardianTestTimeline{},
		emptyResults: []bool{false, true},
		terminateErr: lifetime.ErrUnverifiableLaunch,
	}
	if err := recoverLifetimeScope(t.Context(), scope, nil, task.LifetimeAssuranceExact); err != nil {
		t.Fatalf("recover after concurrent cleanup: %v", err)
	}
	empty, terminate, _ := scope.counts()
	if empty != 2 || terminate != 1 {
		t.Fatalf("scope calls empty=%d terminate=%d, want 2/1", empty, terminate)
	}
}

func TestLifetimeFinalizationRequiresDurableWorkerWait(t *testing.T) {
	if err := requireDurableLifetimeWorkerWait(nil); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing worker Wait = %v, want evidence fault", err)
	}
	if err := requireDurableLifetimeWorkerWait(&task.LifetimeWorkerWaitRecord{}); err != nil {
		t.Fatalf("present validated worker Wait was rejected: %v", err)
	}
}

func TestDisposeUnregisteredCooperativeScopeUsesCommitOrder(t *testing.T) {
	scope := &lifetimeGuardianTestScope{}

	if err := disposeUnregisteredLifetimeScope(t.Context(), scope, lifetime.AssuranceCooperative); err != nil {
		t.Fatalf("dispose unregistered cooperative scope: %v", err)
	}
	empty, _, _ := scope.counts()
	destroy, _ := scope.disposalCounts()
	if empty != 0 {
		t.Fatalf("cooperative intent-only cleanup called EmptyContext %d times, want 0", empty)
	}
	if destroy != 1 {
		t.Fatalf("cooperative intent-only cleanup destroyed %d times, want 1", destroy)
	}
}

func TestDisposeUnregisteredExactScopeProvesBeforeDestroy(t *testing.T) {
	scope := &lifetimeGuardianTestScope{emptyResults: []bool{true}}

	if err := disposeUnregisteredLifetimeScope(t.Context(), scope, lifetime.AssuranceExact); err != nil {
		t.Fatalf("dispose unregistered exact scope: %v", err)
	}
	empty, _, _ := scope.counts()
	destroy, _ := scope.disposalCounts()
	if empty != 1 || destroy != 1 {
		t.Fatalf("exact intent-only cleanup calls empty=%d destroy=%d, want 1/1", empty, destroy)
	}
}

func TestCooperativeLifetimeRecoveryPreservesJoinedCleanupErrors(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	scope := &lifetimeGuardianTestScope{
		terminateErr: errors.Join(lifetime.ErrUnverifiableLaunch, cleanupErr),
	}
	launchRef, err := lifetime.NewLaunchRef(1, "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = recoverRegisteredLifetime(t.Context(), scope, []lifetime.LaunchRef{launchRef}, task.LifetimeAssuranceCooperative, true, nil)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recovery error = %v, want ErrUnverifiableLaunch", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("recovery error = %v, lost cleanup error", err)
	}
	empty, terminate, _ := scope.counts()
	if terminate != 1 || empty != 0 {
		t.Fatalf("cooperative cleanup calls empty=%d terminate=%d, want 0/1", empty, terminate)
	}
}

func TestRecoverLifetimeScopeCooperativeCleanupTerminatesAndPreservesErrors(t *testing.T) {
	cleanupErr := errors.New("terminate failed")
	scope := &lifetimeGuardianTestScope{
		terminateErr: errors.Join(lifetime.ErrUnverifiableLaunch, cleanupErr),
	}
	launchRef, err := lifetime.NewLaunchRef(1, "token", 1)
	if err != nil {
		t.Fatal(err)
	}

	err = recoverLifetimeScope(t.Context(), scope, []lifetime.LaunchRef{launchRef}, task.LifetimeAssuranceCooperative)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("cooperative recovery error = %v, want ErrUnverifiableLaunch", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cooperative recovery error = %v, lost terminate error", err)
	}
	empty, terminate, _ := scope.counts()
	if terminate != 1 || empty != 0 {
		t.Fatalf("cooperative cleanup calls empty=%d terminate=%d, want 0/1", empty, terminate)
	}
}

func TestRecoverTaskLifetimeLeavesActiveOwnerUntouched(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, "different-boot")
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	if err = recoverTaskLifetime(t.Context(), td); !errors.Is(err, errLifetimeRecoveryPending) {
		t.Fatalf("recovery error = %v, want pending", err)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active owner was overwritten: %v", err)
	}
}

func TestUpdateStatusKeepsIntentOnlyLifetimeRunningWhileOwnerBusy(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	_, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("d", 32),
		BootID:    currentBootID(t),
		Deadline:  time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running while intent owner is active", response.Liveness)
	}
	if response.Pending == nil || response.Pending.Kind != pendingLifetimeFinalization {
		t.Fatalf("pending = %+v, want lifetime finalization", response.Pending)
	}
}

func TestRecoverTaskLifetimeAcceptsChangedBootAsEmptyProof(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, "different-boot")

	if err := recoverTaskLifetime(t.Context(), td); err != nil {
		t.Fatal(err)
	}
	terminated, err := td.HasTerminatedLifetime(task.LifetimeTerminationPreviousBoot)
	if err != nil || !terminated {
		t.Fatalf("changed boot recovery = (%t, %v)", terminated, err)
	}
}

func TestRecoverTaskLifetimeCleansUnregisteredIntentWithoutTerminationEvidence(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	deadline := time.Now().Add(time.Minute)
	_, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("b", 32),
		BootID:    currentBootID(t),
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = recoverTaskLifetime(t.Context(), td); err != nil {
		t.Fatalf("recover unregistered intent: %v", err)
	}
	if _, err = td.ReadLifetimeRegistration(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated registration: %v", err)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated termination: %v", err)
	}
}

func TestRecoverTaskLifetimeBeforeWorkerAcknowledgementRefusesWithoutTerminalProof(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	deadline := time.Now().Add(time.Minute)
	bootID := currentBootID(t)
	plan := "pg:" + strings.Repeat("c", 32)
	if _, err := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: plan,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   plan,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if err := recoverTaskLifetime(t.Context(), td); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recover pre-ack registration = %v, want ErrUnverifiableLaunch", err)
	}
	if _, err := td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-ack recovery wrote terminal evidence: %v", err)
	}
}

func TestUpdateStatusKeepsRegisteredLifetimeRunningWhileOwnerBusy(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, "different-boot")
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running while lifetime owner is active", response.Liveness)
	}
	if response.Pending == nil || response.Pending.Kind != pendingLifetimeFinalization {
		t.Fatalf("pending = %+v, want lifetime finalization", response.Pending)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy lifetime received false termination evidence: %v", err)
	}
}

func TestUpdateStatusKeepsUnprovenRegisteredLifetimeRunning(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	recordRecoverableLifetime(t, td, currentBootID(t))

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	err := updateStatusStopsContext(t.Context(), td, &response, observation)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("status recovery error = %v, want unverifiable launch", err)
	}
	if response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("liveness = %q, want running without lifetime termination proof", response.Liveness)
	}
}

func TestUpdateStatusKeepsHistoricalEndedCompatibility(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err := updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	if response.Liveness != task.LivenessEnded.String() {
		t.Fatalf("historical liveness = %q, want ended", response.Liveness)
	}
}

func TestUpdateStatusRecoversLifetimeBeforePublishingStop(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	prepareAppBudgetRecords(t, td)
	recordRecoverableLifetime(t, td, "different-boot")
	deadline := time.Now().Add(time.Minute)
	permit, err := td.PrepareStop("budget", "budget", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStopReply("budget", task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true}); err != nil {
		t.Fatal(err)
	}
	response := newResponse("status")
	observation := pueue.Observation{Matched: true, State: pueue.StateEnded, NumericTaskID: 7}
	if err = updateStatusStopsContext(t.Context(), td, &response, observation); err != nil {
		t.Fatal(err)
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Observation == nil || !records[0].Observation.Terminated {
		t.Fatalf("stop was not published after lifetime recovery: %+v", records)
	}
	if response.Liveness != task.LivenessEnded.String() {
		t.Fatalf("liveness = %q, want ended after lifetime recovery", response.Liveness)
	}
}

func recordRecoverableLifetime(t *testing.T, td *taskdir.TaskDir, bootID string) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	deadline := time.Now().Add(time.Minute)
	_, err = td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
		PID:          1 << 30,
		ProcessStart: "test-worker-start",
		BootID:       bootID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func currentBootID(t *testing.T) string {
	t.Helper()
	value, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
