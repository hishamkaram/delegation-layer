package guardian

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifetimeGuardianOwnerBindingFailurePreventsWorkerStart(t *testing.T) {
	wantErr := errors.New("subreaper setup failed")
	f := newLifetimeGuardianTestFixtureWithScopeSetup(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) { scope.bindErr = wantErr },
	)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want owner binding failure", err)
	}
	start, wait := f.worker.counts()
	if start != 0 || wait != 0 {
		t.Fatalf("worker calls after owner binding failure = start %d wait %d", start, wait)
	}
	if len(f.records.registrations) != 0 || len(f.records.workers) != 0 {
		t.Fatalf("owner binding failure published registration=%d worker=%d", len(f.records.registrations), len(f.records.workers))
	}
	destroy, release := f.scope.disposalCounts()
	if destroy != 1 || release != 1 {
		t.Fatalf("owner binding failure disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "scope-bind-owner", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianScopeCollisionUsesOwnedRecoveryBeforeRetry(t *testing.T) {
	var scopeCalls int
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			create := options.ScopeFactory
			options.ScopeFactory = func(plan lifetime.Plan) (Scope, error) {
				scopeCalls++
				if scopeCalls == 1 {
					return nil, lifetime.ErrScopeExists
				}
				return create(plan)
			}
			options.ReconcileOwned = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				lifetimeGuardianTestRecordsFromOptions(t, options).timeline.add("scope-recovery")
				return nil
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run after scope recovery: %v", err)
	}
	if scopeCalls != 2 {
		t.Fatalf("scope factory calls = %d, want collision plus one retry", scopeCalls)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "scope-recovery", "scope", "registration", "worker-start", "release")
}

func TestLifetimeGuardianScopeCollisionRecoveryFailureDoesNotRetry(t *testing.T) {
	wantErr := errors.New("owned recovery failed")
	var scopeCalls int
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			options.ScopeFactory = func(lifetime.Plan) (Scope, error) {
				scopeCalls++
				return nil, lifetime.ErrScopeExists
			}
			options.ReconcileOwned = func(context.Context) error { return wantErr }
		},
	)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want recovery failure", err)
	}
	if scopeCalls != 1 {
		t.Fatalf("scope factory calls = %d, want no retry after recovery failure", scopeCalls)
	}
	if start, wait := f.worker.counts(); start != 0 || wait != 0 {
		t.Fatalf("worker calls after recovery failure = start %d wait %d", start, wait)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "release")
}

func TestLifetimeGuardianScopeCollisionWithRegisteredScopeDoesNotRelaunch(t *testing.T) {
	var scopeCalls int
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			options.ScopeFactory = func(lifetime.Plan) (Scope, error) {
				scopeCalls++
				return nil, lifetime.ErrScopeExists
			}
			options.ReconcileOwned = func(context.Context) error {
				records.existingTermination = &task.LifetimeTerminationRecord{ScopeEmpty: true}
				return nil
			}
		},
	)
	if err := f.waitRun(t); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("Run error = %v, want terminal task after registered recovery", err)
	}
	if scopeCalls != 1 {
		t.Fatalf("scope factory calls = %d, want no relaunch after terminal recovery", scopeCalls)
	}
	if start, wait := f.worker.counts(); start != 0 || wait != 0 {
		t.Fatalf("worker calls after terminal recovery = start %d wait %d", start, wait)
	}
}

func TestLifetimeGuardianRegistrationFailureDisposesUnregisteredScope(t *testing.T) {
	wantErr := errors.New("registration write failed")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			lifetimeGuardianTestRecordsFromOptions(t, options).registrationErr = wantErr
		},
	)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want registration failure", err)
	}
	if start, wait := f.worker.counts(); start != 0 || wait != 0 {
		t.Fatalf("worker calls after registration failure = start %d wait %d", start, wait)
	}
	if got := f.records.terminationCauses(); len(got) != 0 {
		t.Fatalf("unregistered failure published termination: %v", got)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("unregistered failure disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "registration", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianRegistrationFailureFinalizesPublishedRegistration(t *testing.T) {
	wantErr := errors.New("registration acknowledgement failed")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErrors = []error{wantErr, nil}
			records.existingRegistration = matchingLifetimeGuardianTestRegistration(options, "test-scope")
		},
	)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want registration acknowledgement failure", err)
	}
	if got := f.records.terminationCauses(); len(got) != 1 || got[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("published registration termination = %v, want recovery", got)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("published registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "registration", "termination-record", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianBindFailureFinalizesExistingRegistration(t *testing.T) {
	wantErr := errors.New("owner binding failed")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) { scope.bindErr = wantErr },
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.existingRegistration = matchingLifetimeGuardianTestRegistration(options, "test-scope")
		},
	)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want binding failure", err)
	}
	if got := f.records.terminationCauses(); len(got) != 1 || got[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("existing registration termination = %v, want recovery", got)
	}
	if start, wait := f.worker.counts(); start != 0 || wait != 0 {
		t.Fatalf("worker calls after binding failure = start %d wait %d", start, wait)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "scope-bind-owner", "registration", "termination-record", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianRegistrationFailureDoesNotDestroyWithoutTerminationRecord(t *testing.T) {
	wantErr := errors.New("registration acknowledgement failed")
	wantTerminationErr := errors.New("termination write failed")
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErrors = []error{wantErr, nil}
			records.existingRegistration = matchingLifetimeGuardianTestRegistration(options, "test-scope")
			records.terminationErr = wantTerminationErr
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					records.mu.Lock()
					records.terminationErr = nil
					records.mu.Unlock()
					return nil
				}
			}
		},
	)
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian returned before termination publication became durable: %v", err)
	case <-retryEntered:
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("failed termination disposal = destroy %d release %d, want 0/0", destroy, release)
	}
	if events := f.timeline.snapshot(); timelineIndex(events, "scope-release") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("failed termination released authority: %v", events)
	}
	close(retryRelease)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want registration failure after resident retry", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("successful termination disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianRegistrationReadFailureRetainsScope(t *testing.T) {
	wantErr := errors.New("registration write failed")
	wantReadErr := errors.New("registration read failed")
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErr = wantErr
			records.registrationReadErr = wantReadErr
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					records.mu.Lock()
					records.registrationReadErr = nil
					records.registrationErr = nil
					records.mu.Unlock()
					return nil
				}
			}
		},
	)
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian returned while registration state was unreadable: %v", err)
	case <-retryEntered:
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("unreadable registration disposal = destroy %d release %d, want 0/0", destroy, release)
	}
	if events := f.timeline.snapshot(); timelineIndex(events, "scope-release") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("unreadable registration released authority: %v", events)
	}
	close(retryRelease)
	err := f.waitRun(t)
	if !errors.Is(err, wantErr) || !errors.Is(err, wantReadErr) {
		t.Fatalf("Run error = %v, want write and read failures", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("resolved unreadable registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianMarkedRegistrationAbsenceRetainsScope(t *testing.T) {
	wantErr := errors.New("registration write failed")
	wantReadErr := errors.Join(task.ErrIdentityMismatch, os.ErrNotExist)
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErr = wantErr
			records.registrationReadErr = wantReadErr
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					records.mu.Lock()
					records.registrationReadErr = nil
					records.registrationErr = nil
					records.mu.Unlock()
					return nil
				}
			}
		},
	)
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian disposed scope while registration was unverifiable: %v", err)
	case <-retryEntered:
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("marked missing registration disposal = destroy %d release %d, want 0/0", destroy, release)
	}
	close(retryRelease)
	err := f.waitRun(t)
	if !errors.Is(err, wantErr) || !errors.Is(err, wantReadErr) {
		t.Fatalf("Run error = %v, want write and marked read failures", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("resolved marked registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianConflictingRegistrationDisposesOnlyNewScope(t *testing.T) {
	wantErr := errors.New("registration write failed")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErr = wantErr
			records.existingRegistration = matchingLifetimeGuardianTestRegistration(options, "different-scope")
		},
	)
	err := f.waitRun(t)
	if !errors.Is(err, wantErr) || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("Run error = %v, want write failure and identity mismatch", err)
	}
	if got := f.records.terminationCauses(); len(got) != 0 {
		t.Fatalf("conflicting registration published termination: %v", got)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("conflicting registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "registration", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianRecordsExactWorkerWaitBeforeResourceCleanup(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].Result != task.LifetimeWorkerWaitSuccess || waits[0].ExitCode == nil || *waits[0].ExitCode != 0 || waits[0].WorkerSHA256 == "" {
		t.Fatalf("worker Wait records = %+v, want one qualified success", waits)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "termination-record", "worker-exited", "worker-wait-record", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianRecordsExactWorkerWaitFailure(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	wantErr := errors.New("inspection worker failed")
	f.worker.setWaitError(wantErr)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want worker failure", err)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].Result != task.LifetimeWorkerWaitError || waits[0].ExitCode != nil {
		t.Fatalf("worker Wait records = %+v, want one exact wait-error", waits)
	}
}

func TestLifetimeGuardianWorkerWaitRetriesLaterScopeReleaseFailure(t *testing.T) {
	wantErr := errors.New("scope release failed")
	f := newLifetimeGuardianTestFixtureWithScopeSetup(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.releaseErrors = []error{wantErr, wantErr, wantErr}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run error = %v, want resident scope release retry to succeed", err)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].Result != task.LifetimeWorkerWaitSuccess {
		t.Fatalf("cleanup rewrote exact worker Wait: %+v", waits)
	}
	if _, release := f.scope.disposalCounts(); release != 4 {
		t.Fatalf("scope release calls = %d, want three failed attempts plus a resident retry", release)
	}
}

func TestLifetimeGuardianWorkerWaitRecordWaitsForExactWorkerExit(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.guardian.options.CleanupTimeout = 100 * time.Millisecond
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian released ownership before exact Wait: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if waits := f.records.recordedWorkerWaits(); len(waits) != 0 {
		t.Fatalf("worker Wait record published before exact exit: %+v", waits)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run after worker exit: %v", err)
	}
	if waits := f.records.recordedWorkerWaits(); len(waits) != 1 {
		t.Fatalf("worker Wait records = %d, want 1 after exact exit", len(waits))
	}
}

func TestLifetimeGuardianOwnsWaitBeforeWorkerRecordPersistence(t *testing.T) {
	recordEntered := make(chan struct{})
	recordRelease := make(chan struct{})
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.workerRecordEntered = recordEntered
			records.workerRecordRelease = recordRelease
		},
	)
	select {
	case <-recordEntered:
	case <-time.After(time.Second):
		t.Fatal("worker record persistence did not start")
	}
	select {
	case <-f.worker.waitEntered:
	case <-time.After(time.Second):
		t.Fatal("guardian did not own Wait before worker persistence")
	}
	f.worker.release()
	close(recordRelease)
	if err := f.waitRun(t); !errors.Is(err, errLifetimeGuardianWorkerLost) {
		t.Fatalf("early worker exit error = %v, want worker-lost recovery", err)
	}
	startCalls, waitCalls := f.worker.counts()
	if startCalls != 1 || waitCalls != 1 {
		t.Fatalf("worker ownership = start %d wait %d, want exactly 1/1", startCalls, waitCalls)
	}
	if waits := f.records.recordedWorkerWaits(); len(waits) != 1 || waits[0].WorkerSHA256 == "" {
		t.Fatalf("exact worker Wait was not durably qualified: %+v", waits)
	}
}

func TestLifetimeGuardianFixtureCleanupReleasesBlockedWorkerRecord(t *testing.T) {
	entered := make(chan struct{})
	blocked := make(chan struct{})
	if ok := t.Run("blocked-worker-record", func(t *testing.T) {
		newLifetimeGuardianTestFixtureWithOptions(
			t,
			nil,
			lifetime.KindProcessGroup,
			lifetime.AssuranceCooperative,
			nil,
			nil,
			func(options *Options) {
				records := lifetimeGuardianTestRecordsFromOptions(t, options)
				records.workerRecordEntered = entered
				records.workerRecordRelease = blocked
			},
		)
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker record persistence did not start")
		}
	}); !ok {
		t.Fatal("fixture cleanup did not release its blocked worker record")
	}
}

func TestLifetimeGuardianRetriesAmbiguousWorkerIdentityBeforeServing(t *testing.T) {
	wantErr := errors.Join(task.ErrUncertainDurability, errors.New("worker record barrier failed"))
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.workerErrors = []error{wantErr}
			records.workerCommitOnError = true
			options.WaitRecordRetry = func(context.Context) error { return nil }
			options.FinalizationBackoff = func(context.Context, time.Duration) error { return nil }
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run after ambiguous worker identity publication: %v", err)
	}
	f.records.mu.Lock()
	workerAttempts := len(f.records.workers)
	f.records.mu.Unlock()
	if workerAttempts != 2 {
		t.Fatalf("worker identity attempts = %d, want ambiguous write plus exact replay", workerAttempts)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].WorkerSHA256 == "" || waits[0].ChildPID != 303 {
		t.Fatalf("worker Wait = %+v, want identity-qualified exact child", waits)
	}
}

func TestLifetimeGuardianWaitPublicationRetriesBeforeReleasingAuthority(t *testing.T) {
	wantErr := errors.New("worker Wait publication failed")
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			lifetimeGuardianTestRecordsFromOptions(t, options).workerWaitErrors = []error{wantErr, nil}
			options.WaitRecordRetry = func(ctx context.Context) error {
				close(retryEntered)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					return nil
				}
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	select {
	case <-retryEntered:
	case <-time.After(time.Second):
		t.Fatal("guardian did not retry Wait publication")
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("unpublished Wait disposal = destroy %d release %d, want 0/0", destroy, release)
	}
	if events := f.timeline.snapshot(); timelineIndex(events, "release") >= 0 {
		t.Fatalf("unpublished Wait released lifetime owner: %v", events)
	}
	close(retryRelease)
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run error after durable Wait retry = %v", err)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 2 || waits[0] != waits[1] {
		t.Fatalf("Wait retry changed its frozen candidate: %+v", waits)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("durable Wait disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "worker-wait-record", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianDeadlineRetriesTerminationBeforeWorkerWait(t *testing.T) {
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.emptyResults = []bool{false, false, false, false, false, true}
			scope.terminateErrors = []error{
				lifetime.ErrUnverifiableLaunch,
				lifetime.ErrUnverifiableLaunch,
				nil,
			}
		},
		func(options *Options) {
			options.Deadline = time.Now().Add(500 * time.Millisecond)
			options.Now = time.Now
			options.CleanupTimeout = 100 * time.Millisecond
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}

	terminationDeadline := time.Now().Add(2 * time.Second)
	for len(f.records.terminationCauses()) == 0 && time.Now().Before(terminationDeadline) {
		time.Sleep(time.Millisecond)
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes before worker release = %v, want [%s]", causes, task.LifetimeTerminationBudget)
	}
	if waits := f.records.recordedWorkerWaits(); len(waits) != 0 {
		t.Fatalf("worker Wait was recorded before worker release: %+v", waits)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "worker-exited") >= 0 || timelineIndex(events, "scope-destroy") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("guardian released worker or scope before exact Wait: %v", events)
	}
	empty, terminate, _ := f.scope.counts()
	if empty != 6 || terminate != 3 {
		t.Fatalf("deadline finalization calls empty=%d terminate=%d, want 6/3", empty, terminate)
	}

	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("Run error = %v, want deadline", err)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "termination-record", "worker-exited", "worker-wait-record", "scope-destroy", "scope-release", "release")
}

func TestReleaseLifetimeScopeRetriesTransientFailure(t *testing.T) {
	wantErr := errors.New("transient release failure")
	scope := &lifetimeGuardianTestScope{releaseErrors: []error{wantErr, nil}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := releaseLifetimeScope(ctx, scope); err != nil {
		t.Fatalf("release retry: %v", err)
	}
	_, _, closeCalls := scope.counts()
	if closeCalls != 2 {
		t.Fatalf("release calls = %d, want 2", closeCalls)
	}
}

func TestLifetimeFinalizationBackoffGrowsAndCaps(t *testing.T) {
	delay := defaultLifetimeFinalizationBackoff
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 5 * time.Second, 5 * time.Second}
	for index, expected := range want {
		if delay != expected {
			t.Fatalf("backoff[%d] = %s, want %s", index, delay, expected)
		}
		delay = nextLifetimeFinalizationBackoff(delay)
	}
}
