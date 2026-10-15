package pueue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	privateQueueMaintenanceCrashHelperRoot   = "DELEGATE_MAINTENANCE_CRASH_HELPER_ROOT"
	privateQueueMaintenanceCrashHelperMarker = "DELEGATE_MAINTENANCE_CRASH_HELPER_MARKER"
)

type privateQueueMaintenanceCrashProcess struct {
	command *exec.Cmd
	scope   *lifetime.Scope
	launch  lifetime.LaunchRef
	waited  bool
	closed  bool
}

type privateQueueMaintenanceElectionResult struct {
	generation uint64
	owned      bool
	passes     uint64
	err        error
}

func TestPrivateQueueMaintenanceElectionCrashHelper(t *testing.T) {
	stateRoot := os.Getenv(privateQueueMaintenanceCrashHelperRoot)
	if stateRoot == "" {
		return
	}
	marker := os.Getenv(privateQueueMaintenanceCrashHelperMarker)
	if marker == "" {
		t.Fatal("maintenance crash helper marker is missing")
	}
	if err := withPrivateQueueMaintenance(t.Context(), stateRoot, func() error {
		if err := os.WriteFile(marker, []byte("locked\n"), 0o600); err != nil {
			return err
		}
		select {}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateQueueMaintenanceCoalescesCallbackBurstByPass(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()

	requestPrivateQueueMaintenanceBurst(t, ctx, fixture.stateRoot, 1, 200)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     200,
	})
	target, full := beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 200, false, true)

	requestPrivateQueueMaintenanceBurst(t, ctx, fixture.stateRoot, 201, 400)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     400,
	})
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, true)
	beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 400, false, true)
}

func TestPrivateQueueMaintenanceStatePersistsGenerationProgress(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()

	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 1)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     1,
	})

	target, full := beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 1, false, true)
	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 2)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
	})

	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, true)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
		Attempted:     1,
		Completed:     1,
	})

	target, full = beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 2, false, true)
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, false)

	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
		Attempted:     2,
		Completed:     2,
	})
}

func TestPrivateQueueMaintenanceFailedPassRequiresLaterWake(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()

	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 1)
	target, full := beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 1, false, true)
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, false, false)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     1,
		Attempted:     1,
	})

	beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 1, false, false)
	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 2)
	target, full = beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 2, false, true)
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, false)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
		Attempted:     2,
		Completed:     2,
	})
}

func TestPrivateQueueMaintenanceCrashWithUnattemptedRequestIsRecoverable(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()

	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 1)
	acquired, err := tryWithPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
		// Simulate interruption after selecting the generation and before the
		// attempt is recorded. Returning releases the real per-root election lock.
		return claimPrivateQueueMaintenanceGeneration(ctx, fixture.stateRoot, 1, false)
	})
	if err != nil || !acquired {
		t.Fatalf("interrupted owner acquired=%t err=%v", acquired, err)
	}
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     1,
	})

	passes := 0
	owned, err := runElectedPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
		passes++
		return recoverPrivateQueueMaintenanceGeneration(ctx, fixture.stateRoot, 1, false)
	})
	if err != nil || !owned || passes != 1 {
		t.Fatalf("replacement owner owned=%t passes=%d err=%v", owned, passes, err)
	}
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     1,
		Attempted:     1,
		Completed:     1,
	})
}

func TestPrivateQueueMaintenancePendingCompletionWaitsForElectionRelease(t *testing.T) {
	fixture, _ := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()
	held := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- withPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance election holder did not start")
	}

	pending := &Pending{done: make(chan struct{})}
	close(pending.done)
	inFlight := &InFlightError{Pending: pending}
	waitContext, cancelWait := context.WithTimeout(ctx, 100*time.Millisecond)
	resume, err := waitPrivateQueueMaintenancePending(waitContext, fixture.stateRoot, true, inFlight)
	cancelWait()
	if resume || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending completion crossed retained election: resume=%t err=%v", resume, err)
	}

	releaseOnce()
	if err = <-ownerDone; err != nil {
		t.Fatal(err)
	}
	resume, err = waitPrivateQueueMaintenancePending(ctx, fixture.stateRoot, true, inFlight)
	if err != nil || !resume {
		t.Fatalf("pending completion did not resume after election release: resume=%t err=%v", resume, err)
	}
	acquired, err := tryWithPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("maintenance retry could not acquire released election: acquired=%t err=%v", acquired, err)
	}
}

func TestElectedPrivateQueueMaintenanceReturnsWhenElectionFailsBeforeSelectingWake(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	if _, err := requestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(base, privateQueueMaintenanceLockName)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runElectedPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
			return errors.New("maintenance unexpectedly entered")
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("pre-election failure=%v, want immediate lock error", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("pre-election failure retried instead of returning")
	}
}

func TestPrivateQueueMaintenanceRecoversAfterElectedProcessDies(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()
	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 1)
	marker := filepath.Join(base, "maintenance-crash-helper.ready")
	process := startPrivateQueueMaintenanceCrashProcess(t, fixture.stateRoot, marker)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatal("maintenance crash helper did not acquire the election")
		case <-ticker.C:
		}
	}
	if acquired, err := tryWithPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error { return nil }); err != nil || acquired {
		t.Fatalf("election while helper lives acquired=%t err=%v", acquired, err)
	}
	terminatePrivateQueueMaintenanceCrashProcess(t, process)

	passes := 0
	owned, err := runElectedPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
		passes++
		return recoverPrivateQueueMaintenanceGeneration(ctx, fixture.stateRoot, 1, false)
	})
	if err != nil || !owned || passes != 1 {
		t.Fatalf("replacement owner owned=%t passes=%d err=%v", owned, passes, err)
	}
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     1,
		Attempted:     1,
		Completed:     1,
	})
}

func TestPrivateQueueMaintenanceFullRequestDominatesOnlyItsGeneration(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()

	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 1)
	if got, requestErr := requestPrivateQueueMaintenance(ctx, fixture.stateRoot); requestErr != nil || got != 2 {
		t.Fatalf("full maintenance request generation=%d err=%v, want generation 2", got, requestErr)
	}
	target, full := beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 2, true, true)
	requestExpectedPrivateQueueMaintenance(t, ctx, fixture.stateRoot, 3)
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, true)
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     3,
		Attempted:     2,
		Completed:     2,
		FullRequested: 2,
		FullCompleted: 2,
	})

	target, full = beginExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, 3, false, true)
	finishExpectedPrivateQueueMaintenancePass(t, ctx, fixture.stateRoot, target, full, true, false)
}

func TestElectedPrivateQueueMaintenanceHandsOffLastWakeWithoutWaiting(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()
	if generation, err := requestPrivateInspectionQueueMaintenance(ctx, fixture.stateRoot); err != nil || generation != 1 {
		t.Fatalf("initial maintenance generation=%d err=%v, want generation 1", generation, err)
	}

	ownerFinishedPass := make(chan struct{})
	releaseOwner := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseOwner) })
	}
	t.Cleanup(release)

	ownerDone := startPrivateQueueMaintenanceHandoffOwner(ctx, fixture.stateRoot, ownerFinishedPass, releaseOwner)
	waitForPrivateQueueMaintenanceHandoffBarrier(t, ownerFinishedPass, ownerDone)

	contenderDone := startPrivateQueueMaintenanceHandoffContender(ctx, fixture.stateRoot)
	contenderResult := waitForPrivateQueueMaintenanceHandoffContender(t, contenderDone, ownerDone)
	if contenderResult.err != nil || contenderResult.owned || contenderResult.generation != 2 || contenderResult.passes != 0 {
		t.Fatalf("maintenance contender result=%+v, want generation 2 and non-owner return", contenderResult)
	}

	release()
	ownerResult := waitForPrivateQueueMaintenanceHandoffOwner(t, ownerDone)
	if ownerResult.err != nil || !ownerResult.owned || ownerResult.passes != 2 {
		t.Fatalf("elected maintenance owner result=%+v, want two successful passes", ownerResult)
	}
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
		Attempted:     2,
		Completed:     2,
	})
}

func startPrivateQueueMaintenanceHandoffOwner(
	ctx context.Context,
	stateRoot string,
	ownerFinishedPass chan<- struct{},
	releaseOwner <-chan struct{},
) <-chan privateQueueMaintenanceElectionResult {
	done := make(chan privateQueueMaintenanceElectionResult, 1)
	go func() {
		var passes uint64
		owned, runErr := runElectedPrivateQueueMaintenance(ctx, stateRoot, func() error {
			passes++
			if passErr := recoverPrivateQueueMaintenanceGeneration(ctx, stateRoot, passes, false); passErr != nil {
				return passErr
			}
			if passes != 1 {
				return nil
			}
			close(ownerFinishedPass)
			select {
			case <-releaseOwner:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- privateQueueMaintenanceElectionResult{owned: owned, passes: passes, err: runErr}
	}()
	return done
}

func startPrivateQueueMaintenanceHandoffContender(ctx context.Context, stateRoot string) <-chan privateQueueMaintenanceElectionResult {
	done := make(chan privateQueueMaintenanceElectionResult, 1)
	go func() {
		generation, requestErr := requestPrivateInspectionQueueMaintenance(ctx, stateRoot)
		if requestErr != nil {
			done <- privateQueueMaintenanceElectionResult{generation: generation, err: requestErr}
			return
		}
		var passes uint64
		owned, runErr := runElectedPrivateQueueMaintenance(ctx, stateRoot, func() error {
			passes++
			return errors.New("maintenance contender unexpectedly acquired the election")
		})
		done <- privateQueueMaintenanceElectionResult{generation: generation, owned: owned, passes: passes, err: runErr}
	}()
	return done
}

func waitForPrivateQueueMaintenanceHandoffBarrier(
	t *testing.T,
	ownerFinishedPass <-chan struct{},
	ownerDone <-chan privateQueueMaintenanceElectionResult,
) {
	t.Helper()
	waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	select {
	case <-ownerFinishedPass:
	case ownerResult := <-ownerDone:
		t.Fatalf("elected owner returned before the handoff barrier: %+v", ownerResult)
	case <-waitContext.Done():
		t.Fatalf("elected owner did not reach the handoff barrier: %v", waitContext.Err())
	}
}

func waitForPrivateQueueMaintenanceHandoffContender(
	t *testing.T,
	contenderDone, ownerDone <-chan privateQueueMaintenanceElectionResult,
) privateQueueMaintenanceElectionResult {
	t.Helper()
	waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	select {
	case contenderResult := <-contenderDone:
		return contenderResult
	case ownerResult := <-ownerDone:
		t.Fatalf("elected owner returned before its release: %+v", ownerResult)
	case <-waitContext.Done():
		t.Fatalf("maintenance contender waited behind the elected owner: %v", waitContext.Err())
	}
	return privateQueueMaintenanceElectionResult{}
}

func waitForPrivateQueueMaintenanceHandoffOwner(
	t *testing.T,
	ownerDone <-chan privateQueueMaintenanceElectionResult,
) privateQueueMaintenanceElectionResult {
	t.Helper()
	waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	select {
	case ownerResult := <-ownerDone:
		return ownerResult
	case <-waitContext.Done():
		t.Fatalf("elected owner did not drain the handed-off wake: %v", waitContext.Err())
	}
	return privateQueueMaintenanceElectionResult{}
}

func TestElectedPrivateQueueMaintenanceDrainsNewerWakeAfterFailedPass(t *testing.T) {
	fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
	ctx := t.Context()
	if _, err := requestPrivateInspectionQueueMaintenance(ctx, fixture.stateRoot); err != nil {
		t.Fatal(err)
	}

	firstFailure := errors.New("injected first maintenance failure")
	passes := 0
	owned, err := runElectedPrivateQueueMaintenance(ctx, fixture.stateRoot, func() error {
		passes++
		target, full, run, passErr := beginPrivateQueueMaintenancePass(ctx, fixture.stateRoot, false)
		if passErr != nil || !run {
			return errors.Join(passErr, errors.New("elected owner did not observe pending work"))
		}
		if passes == 1 {
			if _, passErr = requestPrivateInspectionQueueMaintenance(ctx, fixture.stateRoot); passErr != nil {
				return passErr
			}
			_, finishErr := finishPrivateQueueMaintenancePass(ctx, fixture.stateRoot, target, full, false)
			return errors.Join(firstFailure, finishErr)
		}
		pending, finishErr := finishPrivateQueueMaintenancePass(ctx, fixture.stateRoot, target, full, true)
		if finishErr == nil && pending {
			return errors.New("settling pass left another pending generation")
		}
		return finishErr
	})
	if !owned || !errors.Is(err, firstFailure) {
		t.Fatalf("elected maintenance owned=%t err=%v, want retained first failure", owned, err)
	}
	if passes != 2 {
		t.Fatalf("maintenance passes=%d, want failed pass plus one newer wake", passes)
	}
	assertPrivateQueueMaintenanceTestState(t, base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     2,
		Attempted:     2,
		Completed:     2,
	})
}

func TestPrivateQueueMaintenanceRejectsMalformedGenerations(t *testing.T) {
	cases := []struct {
		name   string
		record privateQueueMaintenanceRecord
	}{
		{
			name: "attempted_after_requested",
			record: privateQueueMaintenanceRecord{
				SchemaVersion: task.SchemaVersion,
				Attempted:     1,
			},
		},
		{
			name: "completed_after_attempted",
			record: privateQueueMaintenanceRecord{
				SchemaVersion: task.SchemaVersion,
				Requested:     1,
				Attempted:     0,
				Completed:     1,
			},
		},
		{
			name: "completed_after_requested",
			record: privateQueueMaintenanceRecord{
				SchemaVersion: task.SchemaVersion,
				Requested:     1,
				Attempted:     1,
				Completed:     2,
			},
		},
		{
			name: "full_completed_after_full_requested",
			record: privateQueueMaintenanceRecord{
				SchemaVersion: task.SchemaVersion,
				Requested:     1,
				FullCompleted: 1,
			},
		},
		{
			name: "full_requested_after_requested",
			record: privateQueueMaintenanceRecord{
				SchemaVersion: task.SchemaVersion,
				Requested:     1,
				FullRequested: 2,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, base := newPrivateQueueMaintenanceStateTestFixture(t)
			data, err := task.MarshalCanonical(testCase.record)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, privateQueueMaintenanceRecordName)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := readPrivateQueueMaintenanceRecord(base); !errors.Is(err, ErrBinding) {
				t.Fatalf("read malformed generation error=%v, want ErrBinding", err)
			}
			if _, err := requestPrivateInspectionQueueMaintenance(t.Context(), fixture.stateRoot); !errors.Is(err, ErrBinding) {
				t.Fatalf("request over malformed generation error=%v, want ErrBinding", err)
			}
			if err := writePrivateQueueMaintenanceRecord(base, testCase.record); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("write malformed generation error=%v, want ErrConfiguration", err)
			}
		})
	}
}

func requestPrivateQueueMaintenanceBurst(t *testing.T, ctx context.Context, stateRoot string, first, last uint64) {
	t.Helper()
	const requestCount = 200
	type result struct {
		generation uint64
		err        error
	}
	results := make(chan result, requestCount)
	var callers sync.WaitGroup
	for range requestCount {
		callers.Add(1)
		go func() {
			defer callers.Done()
			generation, err := requestPrivateInspectionQueueMaintenance(ctx, stateRoot)
			results <- result{generation: generation, err: err}
		}()
	}
	callers.Wait()
	close(results)
	seen := make(map[uint64]struct{}, requestCount)
	for item := range results {
		if item.err != nil || item.generation < first || item.generation > last {
			t.Fatalf("burst generation=%d err=%v, want each generation in [%d,%d] once", item.generation, item.err, first, last)
		}
		if _, duplicate := seen[item.generation]; duplicate {
			t.Fatalf("burst generation=%d was returned more than once", item.generation)
		}
		seen[item.generation] = struct{}{}
	}
	if len(seen) != requestCount {
		t.Fatalf("burst generations=%d, want %d", len(seen), requestCount)
	}
}

func requestExpectedPrivateQueueMaintenance(t *testing.T, ctx context.Context, stateRoot string, want uint64) {
	t.Helper()
	got, err := requestPrivateInspectionQueueMaintenance(ctx, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("maintenance request generation=%d, want %d", got, want)
	}
}

func beginExpectedPrivateQueueMaintenancePass(t *testing.T, ctx context.Context, stateRoot string, wantTarget uint64, wantFull, wantRun bool) (uint64, bool) {
	t.Helper()
	target, full, run, err := beginPrivateQueueMaintenancePass(ctx, stateRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget || full != wantFull || run != wantRun {
		t.Fatalf("maintenance pass target=%d full=%t run=%t, want target=%d full=%t run=%t", target, full, run, wantTarget, wantFull, wantRun)
	}
	return target, full
}

func finishExpectedPrivateQueueMaintenancePass(t *testing.T, ctx context.Context, stateRoot string, target uint64, full, success, wantPending bool) {
	t.Helper()
	pending, err := finishPrivateQueueMaintenancePass(ctx, stateRoot, target, full, success)
	if err != nil {
		t.Fatal(err)
	}
	if pending != wantPending {
		t.Fatalf("maintenance pass pending=%t, want %t", pending, wantPending)
	}
}

func claimPrivateQueueMaintenanceGeneration(ctx context.Context, stateRoot string, wantTarget uint64, wantFull bool) error {
	target, full, run, err := beginPrivateQueueMaintenancePass(ctx, stateRoot, false)
	if err != nil {
		return err
	}
	if target != wantTarget || full != wantFull || !run {
		return errors.New("elected owner did not claim the requested maintenance generation")
	}
	return nil
}

func recoverPrivateQueueMaintenanceGeneration(ctx context.Context, stateRoot string, wantTarget uint64, wantFull bool) error {
	target, full, run, err := beginPrivateQueueMaintenancePass(ctx, stateRoot, false)
	if err != nil {
		return err
	}
	if target != wantTarget || full != wantFull || !run {
		return errors.New("replacement owner did not recover the requested maintenance generation")
	}
	pending, err := finishPrivateQueueMaintenancePass(ctx, stateRoot, target, full, true)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("recovered maintenance pass left a pending generation")
	}
	return nil
}

func startPrivateQueueMaintenanceCrashProcess(t *testing.T, stateRoot, marker string) *privateQueueMaintenanceCrashProcess {
	t.Helper()
	plan, err := lifetime.Prepare(stateRoot, "private-queue-maintenance-crash-helper", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := lifetime.CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	process := &privateQueueMaintenanceCrashProcess{scope: scope}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		defer cancelCleanup()
		cleanupPrivateQueueMaintenanceCrashProcess(t, cleanupContext, process)
	})
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestPrivateQueueMaintenanceElectionCrashHelper$")
	command.Env = append(
		os.Environ(),
		privateQueueMaintenanceCrashHelperRoot+"="+stateRoot,
		privateQueueMaintenanceCrashHelperMarker+"="+marker,
	)
	process.command = command
	if err = scope.Configure(command); err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	launch, err := scope.ObserveStarted(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	process.launch = launch
	return process
}

func terminatePrivateQueueMaintenanceCrashProcess(t *testing.T, process *privateQueueMaintenanceCrashProcess) {
	t.Helper()
	terminateContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := process.scope.Terminate(terminateContext, process.launchRefs()); err != nil {
		t.Fatal(err)
	}
	if err := process.command.Wait(); err == nil {
		t.Fatal("maintenance crash helper exited without the injected death")
	}
	process.waited = true
	if err := process.scope.DestroyEmpty(); err != nil {
		t.Fatal(err)
	}
	if err := process.scope.Release(); err != nil {
		t.Fatal(err)
	}
	process.closed = true
}

func cleanupPrivateQueueMaintenanceCrashProcess(t *testing.T, ctx context.Context, process *privateQueueMaintenanceCrashProcess) {
	t.Helper()
	if process == nil || process.closed {
		return
	}
	if process.command != nil && process.command.Process != nil && !process.waited {
		if err := process.scope.Terminate(ctx, process.launchRefs()); err != nil {
			t.Errorf("terminate maintenance crash helper during cleanup: %v", err)
		}
		if waitErr := process.command.Wait(); waitErr == nil {
			t.Errorf("maintenance crash helper exited cleanly during cleanup")
		}
		process.waited = true
	}
	if err := process.scope.DestroyEmpty(); err != nil {
		t.Errorf("destroy maintenance crash helper scope: %v", err)
	}
	if err := process.scope.Release(); err != nil {
		t.Errorf("release maintenance crash helper scope: %v", err)
	}
	process.closed = true
}

func (process *privateQueueMaintenanceCrashProcess) launchRefs() []lifetime.LaunchRef {
	if process.launch.PID() <= 0 {
		return nil
	}
	return []lifetime.LaunchRef{process.launch}
}

func newPrivateQueueMaintenanceStateTestFixture(t *testing.T) (privateSupervisorTestFixture, string) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, base)
	return fixture, base
}

func assertPrivateQueueMaintenanceTestState(t *testing.T, base string, want privateQueueMaintenanceRecord) {
	t.Helper()
	got, err := readPrivateQueueMaintenanceRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("maintenance state=%+v, want %+v", got, want)
	}
	if got.Completed > got.Attempted || got.Attempted > got.Requested ||
		got.FullCompleted > got.Completed || got.FullCompleted > got.FullRequested || got.FullRequested > got.Requested {
		t.Fatalf("maintenance generation invariant failed: %+v", got)
	}
}
