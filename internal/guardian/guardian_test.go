package guardian

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	lifetimeGuardianTestRootID = "fedcba9876543210fedcba9876543210"
	lifetimeGuardianTestTaskID = "0123456789abcdef0123456789abcdef"
)

type lifetimeGuardianTestTimeline struct {
	mu       sync.Mutex
	events   []string
	backoffs []time.Duration
}

func (t *lifetimeGuardianTestTimeline) add(event string) {
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()
}

func (t *lifetimeGuardianTestTimeline) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.events...)
}

func (t *lifetimeGuardianTestTimeline) addBackoff(delay time.Duration) {
	t.mu.Lock()
	t.backoffs = append(t.backoffs, delay)
	t.mu.Unlock()
}

func (t *lifetimeGuardianTestTimeline) backoffSnapshot() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]time.Duration(nil), t.backoffs...)
}

type lifetimeGuardianTestOwner struct {
	timeline      *lifetimeGuardianTestTimeline
	mu            sync.Mutex
	releases      int
	releaseErrors []error
}

func (o *lifetimeGuardianTestOwner) Release() error {
	o.mu.Lock()
	o.releases++
	var err error
	if len(o.releaseErrors) > 0 {
		err = o.releaseErrors[0]
		o.releaseErrors = o.releaseErrors[1:]
	}
	o.mu.Unlock()
	o.timeline.add("release")
	return err
}

func (o *lifetimeGuardianTestOwner) setReleaseErrors(errs []error) {
	o.mu.Lock()
	o.releaseErrors = append([]error(nil), errs...)
	o.mu.Unlock()
}

type lifetimeGuardianTestRecords struct {
	existingCompletion *task.LifetimeWorkerWaitRecord
	timeline           *lifetimeGuardianTestTimeline

	mu                        sync.Mutex
	intents                   []taskdir.LifetimeIntentFacts
	registrations             []taskdir.LifetimeRegistrationFacts
	workers                   []taskdir.LifetimeWorkerFacts
	workerWaits               []taskdir.LifetimeWorkerWaitFacts
	launches                  []taskdir.LifetimeLaunchFacts
	terminations              []string
	intentErr                 error
	registrationErr           error
	registrationErrors        []error
	workerErrors              []error
	workerCommitOnError       bool
	existingWorker            *task.LifetimeWorkerRecord
	workerWaitErrors          []error
	workerRecordEntered       chan struct{}
	workerRecordRelease       <-chan struct{}
	cleanupRelease            <-chan struct{}
	launchErr                 error
	terminationErr            error
	terminationErrors         []error
	terminationCommitOnError  bool
	existingRegistration      *task.LifetimeRegistrationRecord
	registrationReadErr       error
	existingIntent            *task.LifetimeIntentRecord
	intentReadErr             error
	existingTermination       *task.LifetimeTerminationRecord
	terminationReadErr        error
	invalidLaunchRecord       bool
	ownerKind                 string
	intentRecordContext       context.Context
	registrationRecordContext context.Context
	workerRecordContext       context.Context
	workerWaitRecordContext   context.Context
	intentReadContext         context.Context
	registrationReadContext   context.Context
	launchContextDeadline     time.Time
	terminationRecordContext  context.Context
	terminationReadContext    context.Context
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeIntentContext(ctx context.Context) (*task.LifetimeIntentRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.intentReadContext = ctx
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.intentReadErr != nil {
		return nil, r.intentReadErr
	}
	if r.existingIntent == nil {
		return nil, os.ErrNotExist
	}
	record := *r.existingIntent
	return &record, nil
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeTerminationContext(ctx context.Context) (*task.LifetimeTerminationRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.terminationReadContext = ctx
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.terminationReadErr != nil {
		return nil, r.terminationReadErr
	}
	if r.existingTermination == nil {
		return nil, os.ErrNotExist
	}
	record := *r.existingTermination
	return &record, nil
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.registrationReadContext = ctx
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.registrationReadErr != nil {
		return nil, r.registrationReadErr
	}
	if r.existingRegistration == nil {
		return nil, os.ErrNotExist
	}
	record := *r.existingRegistration
	return &record, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeIntentContext(ctx context.Context, facts taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.intentRecordContext = ctx
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.intents = append(r.intents, facts)
	err := r.intentErr
	r.mu.Unlock()
	r.timeline.add("intent")
	if err != nil {
		return nil, err
	}
	return &task.LifetimeIntentRecord{TaskID: lifetimeGuardianTestTaskID, ScopePlan: facts.ScopePlan}, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.registrationRecordContext = ctx
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.registrations = append(r.registrations, facts)
	err := r.registrationErr
	if len(r.registrationErrors) > 0 {
		err = r.registrationErrors[0]
		r.registrationErrors = r.registrationErrors[1:]
	}
	record := &task.LifetimeRegistrationRecord{
		RootID:    lifetimeGuardianTestRootID,
		TaskID:    lifetimeGuardianTestTaskID,
		OwnerKind: r.ownerKind,
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopeID:   facts.ScopeID,
		BootID:    facts.BootID,
		Deadline:  facts.Deadline.UTC().Format(time.RFC3339Nano),
	}
	if err == nil {
		r.existingRegistration = record
	}
	r.mu.Unlock()
	r.timeline.add("registration")
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeWorkerContext(ctx context.Context, facts taskdir.LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.workerRecordContext = ctx
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.workers = append(r.workers, facts)
	var err error
	if len(r.workerErrors) > 0 {
		err = r.workerErrors[0]
		r.workerErrors = r.workerErrors[1:]
	}
	record := &task.LifetimeWorkerRecord{TaskID: lifetimeGuardianTestTaskID, PID: facts.PID, ProcessStart: facts.ProcessStart}
	if r.existingWorker == nil && (err == nil || r.workerCommitOnError) {
		r.existingWorker = record
	}
	if r.existingWorker != nil {
		record = r.existingWorker
	}
	entered := r.workerRecordEntered
	release := r.workerRecordRelease
	r.mu.Unlock()
	r.timeline.add("worker-record")
	if entered != nil {
		close(entered)
	}
	if release != nil {
		select {
		case <-release:
		case <-r.cleanupRelease:
		}
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeWorkerWaitContext(ctx context.Context, facts taskdir.LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.workerWaitRecordContext = ctx
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.workerWaits = append(r.workerWaits, facts)
	var err error
	if len(r.workerWaitErrors) > 0 {
		err = r.workerWaitErrors[0]
		r.workerWaitErrors = r.workerWaitErrors[1:]
	}
	r.mu.Unlock()
	r.timeline.add("worker-wait-record")
	if err != nil {
		return nil, err
	}
	return &task.LifetimeWorkerWaitRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             lifetimeGuardianTestRootID,
		TaskID:             lifetimeGuardianTestTaskID,
		RegistrationSHA256: strings.Repeat("a", 64),
		WorkerSHA256:       facts.WorkerSHA256,
		ChildPID:           facts.ChildPID,
		Result:             facts.Result,
		ExitCode:           facts.ExitCode,
		ObservedAt:         facts.ObservedAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeLaunch(facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	return r.RecordLifetimeLaunchContext(context.Background(), facts)
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeLaunchContext(ctx context.Context, facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	r.mu.Lock()
	r.launches = append(r.launches, facts)
	err := r.launchErr
	invalid := r.invalidLaunchRecord
	r.launchContextDeadline = deadline
	r.mu.Unlock()
	r.timeline.add("launch-record")
	if err != nil {
		return nil, err
	}
	if invalid {
		return &task.LifetimeLaunchRecord{Sequence: facts.Sequence + 1, PID: facts.PID}, nil
	}
	return &task.LifetimeLaunchRecord{Sequence: facts.Sequence, PID: facts.PID}, nil
}

func (r *lifetimeGuardianTestRecords) RecordLifetimeTerminationContext(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	r.mu.Lock()
	r.terminationRecordContext = ctx
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.terminations = append(r.terminations, cause)
	err := r.terminationErr
	if len(r.terminationErrors) > 0 {
		err = r.terminationErrors[0]
		r.terminationErrors = r.terminationErrors[1:]
	}
	record := &task.LifetimeTerminationRecord{Cause: cause, ScopeEmpty: true}
	if err == nil || r.terminationCommitOnError {
		r.existingTermination = record
	}
	r.mu.Unlock()
	r.timeline.add("termination-record")
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *lifetimeGuardianTestRecords) terminationContexts() (context.Context, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminationRecordContext, r.terminationReadContext
}

func (r *lifetimeGuardianTestRecords) boundaryWriteContexts() (context.Context, context.Context, context.Context, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.intentRecordContext, r.registrationRecordContext, r.workerRecordContext, r.workerWaitRecordContext
}

func (r *lifetimeGuardianTestRecords) boundaryReadContexts() (context.Context, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.intentReadContext, r.registrationReadContext
}

func (r *lifetimeGuardianTestRecords) registrationFacts() taskdir.LifetimeRegistrationFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registrations[0]
}

func (r *lifetimeGuardianTestRecords) launchFacts() taskdir.LifetimeLaunchFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.launches[0]
}

func (r *lifetimeGuardianTestRecords) terminationCauses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.terminations...)
}

func (r *lifetimeGuardianTestRecords) recordedWorkerWaits() []taskdir.LifetimeWorkerWaitFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]taskdir.LifetimeWorkerWaitFacts(nil), r.workerWaits...)
}

type lifetimeGuardianTestScope struct {
	ref      lifetime.Ref
	timeline *lifetimeGuardianTestTimeline

	mu                      sync.Mutex
	observeErr              error
	emptyResults            []bool
	emptyCalls              int
	pendingStartEmptyResult *bool
	pendingStartEmptyErr    error
	pendingStartEmptyCalls  int
	terminateCalls          int
	closeCalls              int
	destroyCalls            int
	blockEmptyCall          int
	emptyEntered            chan struct{}
	emptyRelease            <-chan bool
	terminateErr            error
	terminateErrors         []error
	releaseErrors           []error
	destroyErrors           []error
	bindErr                 error
	trackErr                error
}

func (s *lifetimeGuardianTestScope) Reference() lifetime.Ref {
	return s.ref
}

func (s *lifetimeGuardianTestScope) BindOwnerProcess() error {
	s.mu.Lock()
	err := s.bindErr
	s.mu.Unlock()
	if s.timeline != nil {
		s.timeline.add("scope-bind-owner")
	}
	return err
}

func (s *lifetimeGuardianTestScope) TrackOwnerProcess(lifetime.ProcessRef) error {
	s.mu.Lock()
	err := s.trackErr
	s.mu.Unlock()
	if s.timeline != nil {
		s.timeline.add("scope-track-worker")
	}
	return err
}

func (s *lifetimeGuardianTestScope) ObserveStarted(pid int) (lifetime.LaunchRef, error) {
	s.mu.Lock()
	err := s.observeErr
	s.mu.Unlock()
	if err != nil {
		return lifetime.LaunchRef{}, err
	}
	return lifetime.NewLaunchRef(pid, "start-token", pid)
}

func (s *lifetimeGuardianTestScope) Terminate(ctx context.Context, _ []lifetime.LaunchRef) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.terminateCalls++
	err := s.terminateErr
	if len(s.terminateErrors) > 0 {
		err = s.terminateErrors[0]
		s.terminateErrors = s.terminateErrors[1:]
	}
	s.mu.Unlock()
	if s.timeline != nil {
		s.timeline.add("scope-terminate")
	}
	return err
}

func (s *lifetimeGuardianTestScope) EmptyContext(ctx context.Context, _ []lifetime.LaunchRef) (bool, error) {
	s.mu.Lock()
	s.emptyCalls++
	call := s.emptyCalls
	result := true
	if call <= len(s.emptyResults) {
		result = s.emptyResults[call-1]
	}
	block := call == s.blockEmptyCall
	entered := s.emptyEntered
	release := s.emptyRelease
	s.mu.Unlock()
	if block {
		close(entered)
		select {
		case result = <-release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return result, nil
}

func (s *lifetimeGuardianTestScope) PendingStartEmptyContext(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, lifetime.ErrNilContext
	}
	s.mu.Lock()
	s.pendingStartEmptyCalls++
	result := s.pendingStartEmptyResult
	err := s.pendingStartEmptyErr
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	if result == nil {
		return true, nil
	}
	return *result, nil
}

func (s *lifetimeGuardianTestScope) Release() error {
	s.mu.Lock()
	s.closeCalls++
	var err error
	if len(s.releaseErrors) > 0 {
		err = s.releaseErrors[0]
		s.releaseErrors = s.releaseErrors[1:]
	}
	s.mu.Unlock()
	if s.timeline != nil {
		s.timeline.add("scope-release")
	}
	return err
}

func (s *lifetimeGuardianTestScope) DestroyEmpty() error {
	s.mu.Lock()
	s.destroyCalls++
	var err error
	if len(s.destroyErrors) > 0 {
		err = s.destroyErrors[0]
		s.destroyErrors = s.destroyErrors[1:]
	}
	s.mu.Unlock()
	if s.timeline != nil {
		s.timeline.add("scope-destroy")
	}
	return err
}

func (s *lifetimeGuardianTestScope) counts() (empty, terminate, close int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emptyCalls, s.terminateCalls, s.closeCalls
}

func (s *lifetimeGuardianTestScope) disposalCounts() (destroy, release int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.destroyCalls, s.closeCalls
}

type lifetimeGuardianTestWorker struct {
	mu          sync.Mutex
	startCalls  int
	waitCalls   int
	startErr    error
	waitErr     error
	waitRelease chan struct{}
	releaseOnce sync.Once
	waitEntered chan struct{}
	timeline    *lifetimeGuardianTestTimeline
}

func (w *lifetimeGuardianTestWorker) start(cmd *exec.Cmd) error {
	w.mu.Lock()
	w.startCalls++
	err := w.startErr
	w.mu.Unlock()
	if err == nil {
		cmd.Process = &os.Process{Pid: 303}
	}
	w.timeline.add("worker-start")
	return err
}

func (w *lifetimeGuardianTestWorker) wait(*exec.Cmd) error {
	w.mu.Lock()
	w.waitCalls++
	w.mu.Unlock()
	close(w.waitEntered)
	w.timeline.add("worker-wait")
	<-w.waitRelease
	w.mu.Lock()
	err := w.waitErr
	w.mu.Unlock()
	w.timeline.add("worker-exited")
	return err
}

func (w *lifetimeGuardianTestWorker) release() {
	w.releaseOnce.Do(func() { close(w.waitRelease) })
}

func (w *lifetimeGuardianTestWorker) setWaitError(err error) {
	w.mu.Lock()
	w.waitErr = err
	w.mu.Unlock()
}

func (w *lifetimeGuardianTestWorker) counts() (start, wait int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.startCalls, w.waitCalls
}

type lifetimeGuardianTestFixture struct {
	guardian       *Guardian
	client         *lifetimeCodec
	clientConn     net.Conn
	serverConn     net.Conn
	worker         *lifetimeGuardianTestWorker
	scope          *lifetimeGuardianTestScope
	records        *lifetimeGuardianTestRecords
	owner          *lifetimeGuardianTestOwner
	timeline       *lifetimeGuardianTestTimeline
	nowMu          sync.Mutex
	now            time.Time
	runDone        chan error
	runExited      chan struct{}
	cleanupRelease chan struct{}
}

func newLifetimeGuardianTestFixture(t *testing.T) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithStartError(t, nil)
}

func newLifetimeGuardianTestFixtureWithStartError(t *testing.T, startErr error) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithAssurance(t, startErr, lifetime.KindProcessGroup, lifetime.AssuranceCooperative)
}

func newLifetimeGuardianTestFixtureWithAssurance(t *testing.T, startErr error, kind lifetime.Kind, assurance lifetime.Assurance) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithAssuranceAndProof(t, startErr, kind, assurance, nil)
}

func newLifetimeGuardianTestFixtureWithAssuranceAndProof(t *testing.T, startErr error, kind lifetime.Kind, assurance lifetime.Assurance, callback func(context.Context, bool) error) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithScopeSetup(t, startErr, kind, assurance, callback, nil)
}

func newLifetimeGuardianTestFixtureWithScopeSetup(
	t *testing.T,
	startErr error,
	kind lifetime.Kind,
	assurance lifetime.Assurance,
	callback func(context.Context, bool) error,
	setup func(*lifetimeGuardianTestScope),
) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithOptions(t, startErr, kind, assurance, callback, setup, nil)
}

func newLifetimeGuardianTestFixtureWithOptions(
	t *testing.T,
	startErr error,
	kind lifetime.Kind,
	assurance lifetime.Assurance,
	callback func(context.Context, bool) error,
	setup func(*lifetimeGuardianTestScope),
	configure func(*Options),
) *lifetimeGuardianTestFixture {
	return newLifetimeGuardianTestFixtureWithOptionsAndContext(t, t.Context(), startErr, kind, assurance, callback, setup, configure)
}

func newLifetimeGuardianTestFixtureWithOptionsAndContext(
	t *testing.T,
	runContext context.Context,
	startErr error,
	kind lifetime.Kind,
	assurance lifetime.Assurance,
	callback func(context.Context, bool) error,
	setup func(*lifetimeGuardianTestScope),
	configure func(*Options),
) *lifetimeGuardianTestFixture {
	t.Helper()
	if runContext == nil {
		t.Fatal("lifetime guardian test run context is required")
	}
	fixture, deadline := newLifetimeGuardianTestFixtureState(t, startErr, kind, assurance, callback, setup)
	options := lifetimeGuardianTestOptions(fixture, deadline)
	if configure != nil {
		configure(&options)
	}
	startLifetimeGuardianTestRun(t, fixture, runContext, options)
	return fixture
}

func newLifetimeGuardianTestFixtureState(
	t *testing.T,
	startErr error,
	kind lifetime.Kind,
	assurance lifetime.Assurance,
	callback func(context.Context, bool) error,
	setup func(*lifetimeGuardianTestScope),
) (*lifetimeGuardianTestFixture, time.Time) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	timeline := &lifetimeGuardianTestTimeline{}
	owner := &lifetimeGuardianTestOwner{timeline: timeline}
	records := &lifetimeGuardianTestRecords{timeline: timeline}
	ref, err := lifetime.NewRef(kind, assurance, "test-scope")
	if err != nil {
		t.Fatal(err)
	}
	scope := &lifetimeGuardianTestScope{ref: ref, timeline: timeline}
	if setup != nil {
		setup(scope)
	}
	worker := &lifetimeGuardianTestWorker{
		startErr:    startErr,
		waitRelease: make(chan struct{}),
		waitEntered: make(chan struct{}),
		timeline:    timeline,
	}
	cleanupRelease := make(chan struct{})
	records.cleanupRelease = cleanupRelease
	deadline := time.Now().Add(time.Hour)
	fixture := &lifetimeGuardianTestFixture{
		clientConn:     clientConn,
		serverConn:     serverConn,
		worker:         worker,
		scope:          scope,
		records:        records,
		owner:          owner,
		timeline:       timeline,
		now:            deadline.Add(-time.Minute),
		cleanupRelease: cleanupRelease,
	}
	ownerKind := task.LifetimeOwnerTask
	if callback != nil {
		ownerKind = task.LifetimeOwnerInspection
	}
	records.ownerKind = ownerKind
	return fixture, deadline
}

func lifetimeGuardianTestOptions(
	fixture *lifetimeGuardianTestFixture,
	deadline time.Time,
) Options {
	return Options{
		RootID:    lifetimeGuardianTestRootID,
		TaskID:    lifetimeGuardianTestTaskID,
		OwnerKind: fixture.records.ownerKind,
		Deadline:  deadline,
		Records:   fixture.records,
		AcquireOwner: func(ctx context.Context) (Owner, error) {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			fixture.timeline.add("acquire")
			return fixture.owner, nil
		},
		ReconcileOwned: func(context.Context) error {
			fixture.timeline.add("scope-recovery")
			return nil
		},
		Worker:     exec.Command("unused-lifetime-worker"),
		Connection: fixture.serverConn,
		PlanFactory: func(string, string, string) (lifetime.Plan, error) {
			return lifetime.NewPlan(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "pg:00000000000000000000000000000000")
		},
		ScopeFactory: func(lifetime.Plan) (Scope, error) {
			fixture.timeline.add("scope")
			return fixture.scope, nil
		},
		BootID: func() (string, error) {
			return "test-boot", nil
		},
		StartWorker: fixture.worker.start,
		WaitWorker:  fixture.worker.wait,
		ObserveWorker: func(*exec.Cmd) (lifetime.ProcessRef, error) {
			return lifetime.NewProcessRef(303, "worker-start-token")
		},
		CleanupTimeout: time.Second,
		FinalizationBackoff: func(ctx context.Context, delay time.Duration) error {
			fixture.timeline.addBackoff(delay)
			return waitLifetimeFinalizationBackoff(ctx, delay)
		},
		Now: func() time.Time {
			fixture.nowMu.Lock()
			defer fixture.nowMu.Unlock()
			return fixture.now
		},
	}
}

func startLifetimeGuardianTestRun(
	t *testing.T,
	fixture *lifetimeGuardianTestFixture,
	runContext context.Context,
	options Options,
) {
	t.Helper()
	guardian, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	fixture.guardian = guardian
	fixture.client, err = newLifetimeCodec(fixture.clientConn)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runDone = make(chan error, 1)
	fixture.runExited = make(chan struct{})
	go func() {
		fixture.runDone <- guardian.Run(runContext)
		close(fixture.runExited)
	}()
	t.Cleanup(func() {
		fixture.worker.release()
		close(fixture.cleanupRelease)
		if err := fixture.client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := fixture.serverConn.Close(); err != nil {
			t.Errorf("close server connection: %v", err)
		}
		select {
		case <-fixture.runExited:
		case <-time.After(2 * time.Second):
			t.Error("lifetime guardian test fixture did not stop during cleanup")
			// Retain fixture ownership until Run exits. Returning here would let
			// later cleanups mutate or close the store while the guardian uses it.
			<-fixture.runExited
		}
	})
}

func matchingLifetimeGuardianTestRegistration(options *Options, scopeID string) *task.LifetimeRegistrationRecord {
	return &task.LifetimeRegistrationRecord{
		RootID:    lifetimeGuardianTestRootID,
		TaskID:    lifetimeGuardianTestTaskID,
		OwnerKind: options.OwnerKind,
		ScopeKind: string(lifetime.KindProcessGroup),
		Assurance: string(lifetime.AssuranceCooperative),
		ScopeID:   scopeID,
		BootID:    "test-boot",
		Deadline:  options.Deadline.UTC().Format(time.RFC3339Nano),
	}
}

func lifetimeGuardianTestRecordsFromOptions(t *testing.T, options *Options) *lifetimeGuardianTestRecords {
	t.Helper()
	records, ok := options.Records.(*lifetimeGuardianTestRecords)
	if !ok {
		t.Fatal("guardian test options do not contain test records")
	}
	return records
}
