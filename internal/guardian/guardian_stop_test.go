package guardian

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifetimeGuardianDurableUserStopOwnsNativeTermination(t *testing.T) {
	requested := make(chan struct{})
	observed := make(chan struct{})
	var observedOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			options.StopPoll = time.Millisecond
			options.UserStop = func() (bool, error) {
				select {
				case <-requested:
					observedOnce.Do(func() { close(observed) })
					return true, nil
				default:
					return false, nil
				}
			}
		},
	)
	f.scope.emptyResults = []bool{false, true}
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 101))
	close(requested)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("guardian did not observe the durable user stop request")
	}
	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianStopRequested) {
		t.Fatalf("run error = %v, want durable user stop", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRequested {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRequested)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("native scope terminate calls = %d, want 1", terminate)
	}
}

func TestLifetimeGuardianStopPollCadencePreservesBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		interval   time.Duration
		wantChecks int
	}{
		{"default interval", 0, 0},
		{"explicit interval", 200 * time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checks := 0
				f := newLifetimeGuardianTestFixtureWithOptions(
					t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil,
					func(scope *lifetimeGuardianTestScope) { scope.emptyResults = []bool{false, true} },
					func(options *Options) {
						options.Now = time.Now
						options.Deadline = time.Now().Add(350 * time.Millisecond)
						options.StopPoll = tc.interval
						options.UserStop = func() (bool, error) {
							checks++
							return false, nil
						}
					},
				)
				f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
				f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
				f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 101))
				synctest.Wait()
				// Advance fake time past the budget while the worker remains
				// active; release and join it only after budget recovery runs.
				time.Sleep(450 * time.Millisecond)
				synctest.Wait()
				f.worker.release()
				err := f.waitRun(t)
				<-f.runExited
				if !errors.Is(err, errLifetimeGuardianDeadline) {
					t.Fatalf("run error = %v, want budget deadline", err)
				}
				if causes := f.records.terminationCauses(); len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
					t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationBudget)
				}
				if checks != tc.wantChecks {
					t.Fatalf("user-stop checks = %d, want %d before the budget deadline", checks, tc.wantChecks)
				}
			})
		})
	}
}

func (f *lifetimeGuardianTestFixture) setNow(now time.Time) {
	f.nowMu.Lock()
	f.now = now
	f.nowMu.Unlock()
}

func (f *lifetimeGuardianTestFixture) message(kind string, sequence uint64, pid int) lifetimeMessage {
	return lifetimeMessage{
		Version:  lifetimeProtocolVersion,
		Type:     kind,
		TaskID:   lifetimeGuardianTestTaskID,
		Sequence: sequence,
		PID:      pid,
	}
}

func (f *lifetimeGuardianTestFixture) call(t *testing.T, message lifetimeMessage) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return f.client.callContext(ctx, message)
}

func (f *lifetimeGuardianTestFixture) callMust(t *testing.T, label string, message lifetimeMessage) {
	t.Helper()
	if err := f.call(t, message); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func (f *lifetimeGuardianTestFixture) waitRun(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case err := <-f.runDone:
		return err
	case <-ctx.Done():
		t.Fatalf("guardian did not finish: %v", ctx.Err())
		return ctx.Err()
	}
}

func timelineIndex(events []string, event string) int {
	for index, candidate := range events {
		if candidate == event {
			return index
		}
	}
	return -1
}

func assertTimelineContains(t *testing.T, events []string, required ...string) {
	t.Helper()
	for _, event := range required {
		if timelineIndex(events, event) < 0 {
			t.Fatalf("missing lifecycle event %q: %v", event, events)
		}
	}
}

func assertTimelineOrder(t *testing.T, events []string, ordered ...string) {
	t.Helper()
	for index := 0; index+1 < len(ordered); index++ {
		if timelineIndex(events, ordered[index]) >= timelineIndex(events, ordered[index+1]) {
			t.Fatalf("lifecycle order = %v", events)
		}
	}
}

func TestLifetimeGuardianNormalClose(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 101))
	f.callMust(t, "finish", f.message(lifetimeMessageFinish, 1, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	launch := f.records.launchFacts()
	if launch.Sequence != 1 || launch.PID != 101 {
		t.Fatalf("launch = %+v", launch)
	}
	empty, terminate, closeCalls := f.scope.counts()
	if empty != 2 || terminate != 0 || closeCalls != 1 {
		t.Fatalf("scope calls = empty %d terminate %d close %d", empty, terminate, closeCalls)
	}
	start, wait := f.worker.counts()
	if start != 1 || wait != 1 {
		t.Fatalf("worker calls = start %d wait %d, want 1/1", start, wait)
	}
	events := f.timeline.snapshot()
	assertTimelineContains(t, events, "acquire", "intent", "scope", "scope-bind-owner", "registration", "worker-start", "scope-track-worker", "worker-wait", "worker-record", "termination-record", "worker-wait-record", "scope-destroy", "scope-release", "release")
	assertTimelineOrder(t, events, "acquire", "intent", "scope", "scope-bind-owner", "registration", "worker-start", "scope-track-worker", "worker-record", "termination-record", "worker-wait-record", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianRecordBoundariesPreserveSelectedContext(t *testing.T) {
	marker := &struct{}{}
	runContext := context.WithValue(t.Context(), lifetimeTestContextKey{}, marker)
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t,
		runContext,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		nil,
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 101))
	f.callMust(t, "finish", f.message(lifetimeMessageFinish, 1, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	intentContext, registrationContext, workerContext, workerWaitContext := f.records.boundaryWriteContexts()
	for name, ctx := range map[string]context.Context{
		"intent":       intentContext,
		"registration": registrationContext,
		"worker":       workerContext,
		"worker Wait":  workerWaitContext,
	} {
		if ctx == nil {
			t.Fatalf("%s record received nil context", name)
		}
		if got := ctx.Value(lifetimeTestContextKey{}); got != marker {
			t.Fatalf("%s record lost caller context value: %v", name, got)
		}
	}
	if intentContext != runContext || registrationContext != runContext {
		t.Fatal("initial lifetime records did not receive the state-machine context")
	}
	if workerContext.Done() != nil {
		t.Fatal("worker identity record did not retain its non-cancelable ownership context")
	}
}

func TestLifetimeGuardianTerminationUsesSameContextForAmbiguousReplay(t *testing.T) {
	marker := &struct{}{}
	runContext := context.WithValue(t.Context(), lifetimeTestContextKey{}, marker)
	wantErr := errors.New("ambiguous termination write")
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t,
		runContext,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.terminationErrors = []error{wantErr}
			records.terminationCommitOnError = true
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run after ambiguous termination write: %v", err)
	}

	recordContext, readContext := f.records.terminationContexts()
	if recordContext == nil || readContext == nil {
		t.Fatalf("termination contexts = record %v, read %v; want both present", recordContext, readContext)
	}
	if recordContext != readContext {
		t.Fatal("ambiguity reread did not use the exact state-machine termination context")
	}
	if recordContext.Value(lifetimeTestContextKey{}) != marker {
		t.Fatal("termination context lost the caller context value")
	}
}

func TestLifetimeGuardianRecordTerminationHandlesNilAndCanceledContext(t *testing.T) {
	records := &lifetimeGuardianTestRecords{timeline: &lifetimeGuardianTestTimeline{}}
	guardian := &Guardian{options: Options{Records: records}}
	var nilContext context.Context
	if err := guardian.recordTermination(nilContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil termination context error = %v, want context canceled", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := guardian.recordTermination(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled termination context error = %v, want context canceled", err)
	}
	if got := len(records.terminations); got != 0 {
		t.Fatalf("canceled termination published %d records, want 0", got)
	}
}

func TestLifetimeGuardianExpiredRecoveryReadsUseCallerContext(t *testing.T) {
	marker := &struct{}{}
	runContext := context.WithValue(t.Context(), lifetimeTestContextKey{}, marker)
	records := &lifetimeGuardianTestRecords{timeline: &lifetimeGuardianTestTimeline{}}
	guardian := &Guardian{options: Options{
		Records: records,
		ReconcileOwned: func(context.Context) error {
			return nil
		},
	}}
	if err := guardian.recoverExpiredStart(runContext); !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("expired recovery error = %v, want deadline", err)
	}

	intentContext, registrationContext := records.boundaryReadContexts()
	if intentContext == nil || registrationContext == nil {
		t.Fatalf("expired recovery read contexts = intent %v, registration %v; want both present", intentContext, registrationContext)
	}
	if intentContext != runContext || registrationContext != runContext {
		t.Fatal("expired recovery reads did not receive the state-machine context")
	}
	if intentContext.Value(lifetimeTestContextKey{}) != marker || registrationContext.Value(lifetimeTestContextKey{}) != marker {
		t.Fatal("expired recovery reads lost the caller context value")
	}
}

func TestLifetimeGuardianCancellationStillFinalizesResidentScope(t *testing.T) {
	runContext, cancel := context.WithCancel(t.Context())
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t,
		runContext,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		nil,
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	cancel()
	f.worker.release()
	if err := f.waitRun(t); err == nil {
		t.Fatal("Run returned nil after canceled serve context")
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want recovery termination", causes)
	}
	if waits := f.records.recordedWorkerWaits(); len(waits) != 1 {
		t.Fatalf("worker Wait records = %d, want exact Wait before release", len(waits))
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("canceled cleanup disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "termination-record", "worker-wait-record", "scope-destroy", "scope-release", "release")
}
