package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
	guardian       *lifetimeGuardian
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
	configure func(*lifetimeGuardianOptions),
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
	configure func(*lifetimeGuardianOptions),
) *lifetimeGuardianTestFixture {
	t.Helper()
	if runContext == nil {
		t.Fatal("lifetime guardian test run context is required")
	}
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
	f := &lifetimeGuardianTestFixture{
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
	options := lifetimeGuardianOptions{
		rootID:    lifetimeGuardianTestRootID,
		taskID:    lifetimeGuardianTestTaskID,
		ownerKind: ownerKind,
		deadline:  deadline,
		records:   records,
		acquireOwner: func(ctx context.Context) (lifetimeGuardianOwner, error) {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			timeline.add("acquire")
			return owner, nil
		},
		reconcileOwned: func(context.Context) error {
			timeline.add("scope-recovery")
			return nil
		},
		worker:     exec.Command("unused-lifetime-worker"),
		connection: serverConn,
		planFactory: func(string, string, string) (lifetime.Plan, error) {
			return lifetime.NewPlan(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "pg:00000000000000000000000000000000")
		},
		scopeFactory: func(lifetime.Plan) (lifetimeGuardianScope, error) {
			timeline.add("scope")
			return scope, nil
		},
		bootID: func() (string, error) {
			return "test-boot", nil
		},
		startWorker: worker.start,
		waitWorker:  worker.wait,
		observeWorker: func(*exec.Cmd) (lifetime.ProcessRef, error) {
			return lifetime.NewProcessRef(303, "worker-start-token")
		},
		cleanupTimeout: time.Second,
		finalizationBackoff: func(ctx context.Context, delay time.Duration) error {
			timeline.addBackoff(delay)
			return waitLifetimeFinalizationBackoff(ctx, delay)
		},
		now: func() time.Time {
			f.nowMu.Lock()
			defer f.nowMu.Unlock()
			return f.now
		},
	}
	if configure != nil {
		configure(&options)
	}
	g, err := newLifetimeGuardian(options)
	if err != nil {
		t.Fatal(err)
	}
	f.guardian = g
	f.client, err = newLifetimeCodec(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	f.runDone = make(chan error, 1)
	f.runExited = make(chan struct{})
	go func() {
		f.runDone <- g.Run(runContext)
		close(f.runExited)
	}()
	t.Cleanup(func() {
		worker.release()
		close(f.cleanupRelease)
		if err := f.client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
		if err := serverConn.Close(); err != nil {
			t.Errorf("close server connection: %v", err)
		}
		select {
		case <-f.runExited:
		case <-time.After(2 * time.Second):
			t.Error("lifetime guardian test fixture did not stop during cleanup")
			// Retain fixture ownership until Run exits. Returning here would let
			// later cleanups mutate or close the store while the guardian uses it.
			<-f.runExited
		}
	})
	return f
}

func matchingLifetimeGuardianTestRegistration(options *lifetimeGuardianOptions, scopeID string) *task.LifetimeRegistrationRecord {
	return &task.LifetimeRegistrationRecord{
		RootID:    lifetimeGuardianTestRootID,
		TaskID:    lifetimeGuardianTestTaskID,
		OwnerKind: options.ownerKind,
		ScopeKind: string(lifetime.KindProcessGroup),
		Assurance: string(lifetime.AssuranceCooperative),
		ScopeID:   scopeID,
		BootID:    "test-boot",
		Deadline:  options.deadline.UTC().Format(time.RFC3339Nano),
	}
}

func lifetimeGuardianTestRecordsFromOptions(t *testing.T, options *lifetimeGuardianOptions) *lifetimeGuardianTestRecords {
	t.Helper()
	records, ok := options.records.(*lifetimeGuardianTestRecords)
	if !ok {
		t.Fatal("guardian test options do not contain test records")
	}
	return records
}

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
		func(options *lifetimeGuardianOptions) {
			options.stopPoll = time.Millisecond
			options.userStop = func() (bool, error) {
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
					func(options *lifetimeGuardianOptions) {
						options.now = time.Now
						options.deadline = time.Now().Add(350 * time.Millisecond)
						options.stopPoll = tc.interval
						options.userStop = func() (bool, error) {
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
		func(options *lifetimeGuardianOptions) {
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
	guardian := &lifetimeGuardian{options: lifetimeGuardianOptions{records: records}}
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
	guardian := &lifetimeGuardian{options: lifetimeGuardianOptions{
		records: records,
		reconcileOwned: func(context.Context) error {
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
		func(options *lifetimeGuardianOptions) {
			create := options.scopeFactory
			options.scopeFactory = func(plan lifetime.Plan) (lifetimeGuardianScope, error) {
				scopeCalls++
				if scopeCalls == 1 {
					return nil, lifetime.ErrScopeExists
				}
				return create(plan)
			}
			options.reconcileOwned = func(ctx context.Context) error {
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
		func(options *lifetimeGuardianOptions) {
			options.scopeFactory = func(lifetime.Plan) (lifetimeGuardianScope, error) {
				scopeCalls++
				return nil, lifetime.ErrScopeExists
			}
			options.reconcileOwned = func(context.Context) error { return wantErr }
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			options.scopeFactory = func(lifetime.Plan) (lifetimeGuardianScope, error) {
				scopeCalls++
				return nil, lifetime.ErrScopeExists
			}
			options.reconcileOwned = func(context.Context) error {
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
		func(options *lifetimeGuardianOptions) {
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
		func(options *lifetimeGuardianOptions) {
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
		func(options *lifetimeGuardianOptions) {
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErrors = []error{wantErr, nil}
			records.existingRegistration = matchingLifetimeGuardianTestRegistration(options, "test-scope")
			records.terminationErr = wantTerminationErr
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErr = wantErr
			records.registrationReadErr = wantReadErr
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.registrationErr = wantErr
			records.registrationReadErr = wantReadErr
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
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
		func(options *lifetimeGuardianOptions) {
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
	f.guardian.options.cleanupTimeout = 100 * time.Millisecond
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
		func(options *lifetimeGuardianOptions) {
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
			func(options *lifetimeGuardianOptions) {
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.workerErrors = []error{wantErr}
			records.workerCommitOnError = true
			options.waitRecordRetry = func(context.Context) error { return nil }
			options.finalizationBackoff = func(context.Context, time.Duration) error { return nil }
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
		func(options *lifetimeGuardianOptions) {
			lifetimeGuardianTestRecordsFromOptions(t, options).workerWaitErrors = []error{wantErr, nil}
			options.waitRecordRetry = func(ctx context.Context) error {
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
		func(options *lifetimeGuardianOptions) {
			options.deadline = time.Now().Add(500 * time.Millisecond)
			options.now = time.Now
			options.cleanupTimeout = 100 * time.Millisecond
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

func TestLifetimeGuardianProofBatchResidesBetweenBatches(t *testing.T) {
	f := newLifetimeGuardianProofBatchFixture(t)
	result := make(chan error, 1)
	go func() { result <- f.guardian.persistTermination(t.Context()) }()
	select {
	case err := <-result:
		t.Fatalf("proof retry escaped before the next batch: %v", err)
	case <-f.backoffEntered:
	}
	empty, terminate, _ := f.scope.counts()
	if empty != 6 || terminate != 3 {
		t.Fatalf("first proof batch calls empty=%d terminate=%d, want 6/3", empty, terminate)
	}
	select {
	case err := <-result:
		t.Fatalf("proof retry busy-looped while backoff was held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if delays := f.timeline.backoffSnapshot(); len(delays) != 1 || delays[0] != 100*time.Millisecond {
		t.Fatalf("backoff delays = %v, want [100ms]", delays)
	}
	close(f.backoffRelease)
	if err := <-result; err != nil {
		t.Fatalf("proof retry after backoff: %v", err)
	}
	empty, terminate, _ = f.scope.counts()
	if empty != 7 || terminate != 3 {
		t.Fatalf("next proof batch calls empty=%d terminate=%d, want 7/3", empty, terminate)
	}
}

type lifetimeGuardianProofBatchFixture struct {
	guardian       *lifetimeGuardian
	scope          *lifetimeGuardianTestScope
	timeline       *lifetimeGuardianTestTimeline
	backoffEntered chan struct{}
	backoffRelease chan struct{}
}

func newLifetimeGuardianProofBatchFixture(t *testing.T) *lifetimeGuardianProofBatchFixture {
	t.Helper()
	timeline := &lifetimeGuardianTestTimeline{}
	records := &lifetimeGuardianTestRecords{timeline: timeline, ownerKind: task.LifetimeOwnerTask}
	scope := &lifetimeGuardianTestScope{
		timeline:     timeline,
		emptyResults: []bool{false, false, false, false, false, false, true},
	}
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Error(err)
		}
		if err := clientConn.Close(); err != nil {
			t.Error(err)
		}
	})
	backoffEntered := make(chan struct{})
	backoffRelease := make(chan struct{})
	var backoffOnce sync.Once
	guardian, err := newLifetimeGuardian(lifetimeGuardianOptions{
		rootID:    lifetimeGuardianTestRootID,
		taskID:    lifetimeGuardianTestTaskID,
		ownerKind: task.LifetimeOwnerTask,
		deadline:  time.Now().Add(time.Hour),
		records:   records,
		acquireOwner: func(context.Context) (lifetimeGuardianOwner, error) {
			return &lifetimeGuardianTestOwner{timeline: timeline}, nil
		},
		reconcileOwned: func(context.Context) error { return nil },
		worker:         exec.Command("unused-lifetime-worker"),
		connection:     serverConn,
		scopeFactory: func(lifetime.Plan) (lifetimeGuardianScope, error) {
			return scope, nil
		},
		finalizationBackoff: func(ctx context.Context, delay time.Duration) error {
			timeline.addBackoff(delay)
			backoffOnce.Do(func() { close(backoffEntered) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-backoffRelease:
				return nil
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	guardian.scope = scope
	guardian.registrationRecorded = true
	guardian.terminationCause = task.LifetimeTerminationRecovery
	return &lifetimeGuardianProofBatchFixture{
		guardian:       guardian,
		scope:          scope,
		timeline:       timeline,
		backoffEntered: backoffEntered,
		backoffRelease: backoffRelease,
	}
}

func TestLifetimeGuardianResidentProofRetryHoldsAuthority(t *testing.T) {
	backoffEntered := make(chan struct{})
	backoffRelease := make(chan struct{})
	var backoffOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.emptyResults = []bool{false, false, false, false, false, false, false, false, false, false}
		},
		func(options *lifetimeGuardianOptions) {
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				backoffOnce.Do(func() { close(backoffEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-backoffRelease:
					return nil
				}
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err == nil {
		t.Fatal("close succeeded despite a nonempty proof")
	}
	f.worker.release()
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian returned while the next proof batch was blocked: %v", err)
	case <-backoffEntered:
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("resident proof retry released scope: destroy=%d release=%d", destroy, release)
	}
	if timelineIndex(f.timeline.snapshot(), "release") >= 0 {
		t.Fatal("resident proof retry released the owner")
	}
	close(backoffRelease)
	if err := f.waitRun(t); err == nil {
		t.Fatal("Run lost the original proof failure")
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("resident proof retry disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianRetriesDestroyReleaseAndOwnerPhases(t *testing.T) {
	destroyErr := errors.New("transient destroy failure")
	scopeReleaseErr := errors.New("transient scope release failure")
	ownerReleaseErr := errors.New("transient owner release failure")
	f := newLifetimeGuardianTestFixtureWithScopeSetup(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.destroyErrors = []error{destroyErr, nil}
			scope.releaseErrors = []error{scopeReleaseErr, nil}
		},
	)
	f.owner.setReleaseErrors([]error{ownerReleaseErr, nil})
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run with transient cleanup failures: %v", err)
	}
	if destroy, scopeRelease := f.scope.disposalCounts(); destroy != 2 || scopeRelease != 2 {
		t.Fatalf("cleanup retries = destroy %d scope-release %d, want 2/2", destroy, scopeRelease)
	}
	f.owner.mu.Lock()
	ownerReleases := f.owner.releases
	f.owner.mu.Unlock()
	if ownerReleases != 2 {
		t.Fatalf("owner release calls = %d, want 2", ownerReleases)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "scope-destroy") >= timelineIndex(events, "scope-release") || timelineIndex(events, "scope-release") >= timelineIndex(events, "release") {
		t.Fatalf("cleanup phase order = %v", events)
	}
}

func TestLifetimeGuardianRefusesTerminalReplayBeforeScopeOrWorkerStart(t *testing.T) {
	for _, ownerKind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		t.Run(ownerKind, func(t *testing.T) {
			assertLifetimeGuardianRefusesTerminalReplay(t, ownerKind)
		})
	}
}

func assertLifetimeGuardianRefusesTerminalReplay(t *testing.T, ownerKind string) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if closeErr := serverConn.Close(); closeErr != nil {
			t.Errorf("close server connection: %v", closeErr)
		}
	})
	t.Cleanup(func() {
		if closeErr := clientConn.Close(); closeErr != nil {
			t.Errorf("close client connection: %v", closeErr)
		}
	})
	timeline := &lifetimeGuardianTestTimeline{}
	owner := &lifetimeGuardianTestOwner{timeline: timeline}
	records := &lifetimeGuardianTestRecords{
		timeline:            timeline,
		existingTermination: &task.LifetimeTerminationRecord{ScopeEmpty: true},
	}
	scopeCalls := 0
	startCalls := 0
	guardian, err := newLifetimeGuardian(lifetimeGuardianOptions{
		rootID: lifetimeGuardianTestRootID, taskID: lifetimeGuardianTestTaskID,
		ownerKind: ownerKind, deadline: time.Now().Add(time.Hour), records: records,
		acquireOwner:   func(context.Context) (lifetimeGuardianOwner, error) { return owner, nil },
		reconcileOwned: func(context.Context) error { return nil },
		worker:         exec.Command("unused-lifetime-worker"), connection: serverConn,
		scopeFactory: func(lifetime.Plan) (lifetimeGuardianScope, error) {
			scopeCalls++
			return nil, errors.New("scope must not be created")
		},
		startWorker: func(*exec.Cmd) error {
			startCalls++
			return errors.New("worker must not start")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = guardian.Run(t.Context()); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("Run error = %v, want terminal task", err)
	}
	if scopeCalls != 0 || startCalls != 0 || len(records.intents) != 0 {
		t.Fatalf("terminal replay created work: scope=%d start=%d intents=%d", scopeCalls, startCalls, len(records.intents))
	}
	if owner.releases != 1 {
		t.Fatalf("owner releases = %d, want 1", owner.releases)
	}
}

func TestLifetimeGuardianRefusesFreshExpiredWorkBeforeCreatingScope(t *testing.T) {
	for _, ownerKind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		t.Run(ownerKind, func(t *testing.T) {
			assertLifetimeGuardianRefusesFreshExpiredWork(t, ownerKind)
		})
	}
}

func assertLifetimeGuardianRefusesFreshExpiredWork(t *testing.T, ownerKind string) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if closeErr := serverConn.Close(); closeErr != nil {
			t.Errorf("close server connection: %v", closeErr)
		}
	})
	t.Cleanup(func() {
		if closeErr := clientConn.Close(); closeErr != nil {
			t.Errorf("close client connection: %v", closeErr)
		}
	})
	timeline := &lifetimeGuardianTestTimeline{}
	owner := &lifetimeGuardianTestOwner{timeline: timeline}
	records := &lifetimeGuardianTestRecords{timeline: timeline, ownerKind: ownerKind}
	scopeCalls := 0
	startCalls := 0
	deadline := time.Now().Add(-time.Second)
	guardian, err := newLifetimeGuardian(lifetimeGuardianOptions{
		rootID: lifetimeGuardianTestRootID, taskID: lifetimeGuardianTestTaskID,
		ownerKind: ownerKind, deadline: deadline, records: records,
		acquireOwner:   func(context.Context) (lifetimeGuardianOwner, error) { return owner, nil },
		reconcileOwned: func(context.Context) error { return nil },
		worker:         exec.Command("unused-lifetime-worker"), connection: serverConn,
		scopeFactory: func(lifetime.Plan) (lifetimeGuardianScope, error) {
			scopeCalls++
			return nil, errors.New("scope must not be created")
		},
		startWorker: func(*exec.Cmd) error {
			startCalls++
			return errors.New("worker must not start")
		},
		now: func() time.Time { return deadline },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = guardian.Run(t.Context()); !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("Run error = %v, want deadline", err)
	}
	if scopeCalls != 0 || startCalls != 0 || len(records.intents) != 0 || len(records.registrations) != 0 {
		t.Fatalf("expired work created state: scope=%d start=%d intents=%d registrations=%d",
			scopeCalls, startCalls, len(records.intents), len(records.registrations))
	}
	if owner.releases != 1 {
		t.Fatalf("owner releases = %d, want 1", owner.releases)
	}
}

func TestLifetimeGuardianRejectsLaunchBeyondSharedLimitBeforeAck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches; sequence++ {
		f.callMust(t, "begin", f.message(lifetimeMessageBegin, sequence, 0))
		f.callMust(t, "failed", f.message(lifetimeMessageFailed, sequence, 0))
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, task.MaxLifetimeLaunches+1, 0)); err == nil {
		t.Fatal("launch beyond shared limit was acknowledged")
	}
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianDoesNotDestroyBeforeTerminationIsDurable(t *testing.T) {
	wantErr := errors.New("termination write failed")
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
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.terminationErr = wantErr
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
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
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err == nil {
		t.Fatal("close succeeded despite termination write failure")
	}
	f.worker.release()
	waitForLifetimeGuardianTerminationRetry(t, retryEntered)
	destroy, release := f.scope.disposalCounts()
	if destroy != 0 || release != 0 {
		t.Fatalf("scope disposal before durable termination = destroy %d release %d, want 0/0", destroy, release)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "scope-destroy") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("scope or owner was released before durable termination: %v", events)
	}
	close(retryRelease)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want original termination failure", err)
	}
	if destroy, release = f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("scope disposal after durable termination = destroy %d release %d, want 1/1", destroy, release)
	}
	assertFrozenLifetimeGuardianTerminationCause(t, f.records.terminationCauses())
	assertTimelineOrder(t, f.timeline.snapshot(), "termination-record", "scope-destroy", "scope-release", "release")
}

func waitForLifetimeGuardianTerminationRetry(t *testing.T, retryEntered <-chan struct{}) {
	t.Helper()
	select {
	case <-retryEntered:
	case <-time.After(time.Second):
		t.Fatal("guardian did not retain authority for termination retry")
	}
}

func assertFrozenLifetimeGuardianTerminationCause(t *testing.T, causes []string) {
	t.Helper()
	if len(causes) < 2 {
		t.Fatalf("termination retry attempts = %v, want at least two", causes)
	}
	for _, cause := range causes[1:] {
		if cause != causes[0] {
			t.Fatalf("termination retry changed its frozen cause: %v", causes)
		}
	}
}

func TestLifetimeGuardianAcknowledgesAmbiguousTerminationWrite(t *testing.T) {
	wantErr := errors.New("termination acknowledgement lost")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *lifetimeGuardianOptions) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.terminationErrors = []error{wantErr}
			records.terminationCommitOnError = true
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run after ambiguous committed termination: %v", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("ambiguous committed termination disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianDeadlineFencesBegin(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	f.setNow(time.Now().Add(2 * time.Hour))
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err == nil {
		t.Fatal("begin after deadline was accepted")
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil && !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("run: %v", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationBudget)
	}
	start, wait := f.worker.counts()
	if start != 1 || wait != 1 {
		t.Fatalf("worker calls = start %d wait %d, want 1/1", start, wait)
	}
}

func TestLifetimeGuardianFailedStartClearsAuthorization(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFailed, 1, 0)); err != nil {
		t.Fatalf("failed 1: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 2, 0)); err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFailed, 2, 0)); err != nil {
		t.Fatalf("failed 2: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(f.records.launches); got != 0 {
		t.Fatalf("launch records = %d, want 0", got)
	}
}

func TestLifetimeGuardianLaunchRecordUsesWorkContext(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 404))

	f.records.mu.Lock()
	launchDeadline := f.records.launchContextDeadline
	f.records.mu.Unlock()
	if launchDeadline.IsZero() || !launchDeadline.Equal(f.guardian.options.deadline) {
		t.Fatalf("launch record deadline = %v, want guardian deadline %v", launchDeadline, f.guardian.options.deadline)
	}

	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianLaunchRecordFailureCleansBeforeRefusal(t *testing.T) {
	wantErr := errors.New("launch record write failed")
	assertLifetimeGuardianLaunchRecordFailure(t, wantErr, func(records *lifetimeGuardianTestRecords) {
		records.launchErr = wantErr
	})
}

func TestLifetimeGuardianInvalidLaunchRecordCleansBeforeRefusal(t *testing.T) {
	assertLifetimeGuardianLaunchRecordFailure(t, errLifetimeGuardianLaunchRecord, func(records *lifetimeGuardianTestRecords) {
		records.invalidLaunchRecord = true
	})
}

func assertLifetimeGuardianLaunchRecordFailure(t *testing.T, wantErr error, configure func(*lifetimeGuardianTestRecords)) {
	t.Helper()
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	configure(f.records)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 404)); err == nil {
		t.Fatal("started was accepted despite an invalid launch record")
	}
	f.worker.release()
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want %v", err, wantErr)
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 {
		t.Fatalf("termination causes = %v, want one recovery record", causes)
	}
	events := f.timeline.snapshot()
	assertTimelineOrder(t, events, "launch-record", "scope-terminate", "termination-record")
	if f.guardian.authorizedSequence != 0 {
		t.Fatalf("authorized sequence = %d, want cleared after guardian cleanup", f.guardian.authorizedSequence)
	}
	f.scope.mu.Lock()
	pendingProofCalls := f.scope.pendingStartEmptyCalls
	terminateCalls := f.scope.terminateCalls
	f.scope.mu.Unlock()
	if pendingProofCalls != 0 {
		t.Fatalf("pending-start proof calls = %d, want 0 after guardian cleanup", pendingProofCalls)
	}
	if terminateCalls != 1 {
		t.Fatalf("guardian terminate calls = %d, want 1", terminateCalls)
	}
}

func TestLifetimeGuardianWorkerStartFailureRecovers(t *testing.T) {
	startErr := errors.New("worker start failed")
	f := newLifetimeGuardianTestFixtureWithStartError(t, startErr)
	if err := f.waitRun(t); !errors.Is(err, startErr) {
		t.Fatalf("run error = %v, want worker start failure", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
	start, wait := f.worker.counts()
	if start != 1 || wait != 0 {
		t.Fatalf("worker calls = start %d wait %d, want 1/0", start, wait)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].Result != task.LifetimeWorkerWaitNotStarted || waits[0].WorkerSHA256 != "" || waits[0].ChildPID != 0 || waits[0].ExitCode != nil {
		t.Fatalf("worker completion dispositions = %+v, want one not-started record", waits)
	}
}

func TestLifetimeGuardianStopTerminatesBeforeAck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 202)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStop, 0, 0)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("terminate calls = %d, want 1", terminate)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationBudget)
	}
}

func TestLifetimeGuardianAcceptsConcurrentCleanupOnlyAfterEmptyRecheck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	f.scope.terminateErr = lifetime.ErrUnverifiableLaunch
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 212)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStop, 0, 0)); err != nil {
		t.Fatalf("stop after concurrent cleanup: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	empty, terminate, _ := f.scope.counts()
	if empty != 2 || terminate != 1 {
		t.Fatalf("scope calls empty=%d terminate=%d, want 2/1", empty, terminate)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v", causes)
	}
}

func TestLifetimeGuardianPendingCooperativeStartUsesBoundOwnerProof(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending cooperative start was acknowledged")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("bound owner was rejected as unverifiable: %v", err)
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("cooperative scope terminate calls = %d, want 1 through bound owner proof", terminate)
	}
}

func TestLifetimeGuardianPendingStartFailsClosedWithoutProof(t *testing.T) {
	wantErr := lifetime.ErrUnverifiableLaunch
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
		func(options *lifetimeGuardianOptions) {
			options.finalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					return nil
				}
			}
		},
	)
	f.scope.pendingStartEmptyErr = wantErr
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged without an emptiness proof")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian escaped before pending-start proof became available: %v", err)
	case <-retryEntered:
	}
	if causes := f.records.terminationCauses(); len(causes) != 0 {
		t.Fatalf("termination recorded without pending-start proof: %v", causes)
	}
	f.scope.mu.Lock()
	pendingCalls := f.scope.pendingStartEmptyCalls
	f.scope.mu.Unlock()
	if pendingCalls == 0 {
		t.Fatal("pending-start proof was not attempted")
	}
	f.scope.mu.Lock()
	f.scope.pendingStartEmptyErr = nil
	f.scope.mu.Unlock()
	close(retryRelease)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want ErrUnverifiableLaunch", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("finalized pending start disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianPendingStartTerminatesDiscoveredScopeBeforeRecord(t *testing.T) {
	pendingEmpty := false
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative)
	f.scope.pendingStartEmptyResult = &pendingEmpty
	f.scope.emptyResults = []bool{false, true}
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged before scope termination")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); err == nil {
		t.Fatal("closed control channel must remain visible to the caller")
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("pending-start scope terminate calls = %d, want 1", terminate)
	}
}

func TestLifetimeGuardianPendingExactStartRecordsOnlyAfterWorkerExitAndEmptyProof(t *testing.T) {
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindCgroupV2, lifetime.AssuranceExact)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending exact start was acknowledged before proof")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	if causes := f.records.terminationCauses(); len(causes) != 0 {
		t.Fatalf("termination recorded before worker exit: %v", causes)
	}
	f.worker.release()
	if err := f.waitRun(t); err == nil {
		t.Fatal("closed control channel must remain visible to the caller")
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "worker-exited") >= timelineIndex(events, "termination-record") {
		t.Fatalf("termination order = %v", events)
	}
}

func TestLifetimeGuardianPendingStartFinalizesAfterNonzeroWorkerExit(t *testing.T) {
	waitErr := errors.New("worker exited after pending start was fenced")
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindCgroupV2, lifetime.AssuranceExact)
	f.worker.setWaitError(waitErr)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged before the worker exited")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); !errors.Is(err, waitErr) {
		t.Fatalf("run error = %v, want exact worker error", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	events := f.timeline.snapshot()
	assertTimelineOrder(t, events, "worker-exited", "termination-record")
}

func TestLifetimeGuardianWorkerLossRecoversWithoutOutcome(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianWorkerLost) {
		t.Fatalf("run error = %v, want worker loss", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
}

func TestLifetimeGuardianControlLossRecovers(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Close(); err != nil {
		t.Fatalf("close control: %v", err)
	}
	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianWorkerLost) {
		t.Fatalf("run error = %v, want control loss recovery", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
}

func (f *lifetimeGuardianTestFixture) waitForWorker(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-f.worker.waitEntered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLifetimeGuardianDoesNotAckBeforeEmptyProof(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f.scope.emptyResults = []bool{false}
	f.scope.blockEmptyCall = 1
	f.scope.emptyEntered = emptyEntered
	f.scope.emptyRelease = emptyRelease
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 303)); err != nil {
		t.Fatalf("started: %v", err)
	}
	finishDone := make(chan error, 1)
	go func() { finishDone <- f.call(t, f.message(lifetimeMessageFinish, 1, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("finish did not reach empty observation")
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish acknowledged before empty proof: %v", err)
	default:
	}
	emptyRelease <- false
	if err := <-finishDone; err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianFinishUsesImmutableWorkDeadline(t *testing.T) {
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.blockEmptyCall = 1
			scope.emptyEntered = emptyEntered
			scope.emptyRelease = emptyRelease
		},
		func(options *lifetimeGuardianOptions) {
			options.cleanupTimeout = 20 * time.Millisecond
			options.deadline = time.Now().Add(time.Second)
			options.now = time.Now
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 303))
	finishDone := make(chan error, 1)
	go func() { finishDone <- f.call(t, f.message(lifetimeMessageFinish, 1, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("finish did not reach empty observation")
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish used the shorter cleanup fallback: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	emptyRelease <- true
	if err := <-finishDone; err != nil {
		t.Fatalf("finish after empty proof: %v", err)
	}
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianCloseUsesImmutableWorkDeadline(t *testing.T) {
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.blockEmptyCall = 1
			scope.emptyEntered = emptyEntered
			scope.emptyRelease = emptyRelease
		},
		func(options *lifetimeGuardianOptions) {
			options.cleanupTimeout = 100 * time.Millisecond
			options.deadline = time.Now().Add(2 * time.Second)
			options.now = time.Now
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("close did not reach empty observation")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("close used the shorter cleanup fallback: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	emptyRelease <- true
	if err := <-closeDone; err != nil {
		t.Fatalf("close after empty proof: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianRejectsInvalidOrdering(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err == nil {
		t.Fatal("begin before hello was accepted")
	}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFinish, 1, 0)); err == nil {
		t.Fatal("finish without active launch was accepted")
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianRegistrationUsesBootID(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := f.records.registrationFacts()
	if facts.BootID != "test-boot" {
		t.Fatalf("registration boot ID = %q, want test-boot", facts.BootID)
	}
}

type lifetimeGuardianDeadlineConn struct {
	net.Conn
	deadline chan time.Time
	once     sync.Once
}

func (c *lifetimeGuardianDeadlineConn) recordDeadline(deadline time.Time) {
	c.once.Do(func() { c.deadline <- deadline })
}

func (c *lifetimeGuardianDeadlineConn) SetDeadline(deadline time.Time) error {
	c.recordDeadline(deadline)
	return c.Conn.SetDeadline(deadline)
}

func (c *lifetimeGuardianDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.recordDeadline(deadline)
	return c.Conn.SetReadDeadline(deadline)
}

func TestLifetimeGuardianReaderUsesTaskDeadlineAndCancellation(t *testing.T) {
	serverConn, peerConn := net.Pipe()
	defer func() {
		if err := peerConn.Close(); err != nil {
			t.Errorf("close peer connection: %v", err)
		}
	}()
	conn := &lifetimeGuardianDeadlineConn{Conn: serverConn, deadline: make(chan time.Time, 1)}
	codec, err := newLifetimeCodec(conn)
	if err != nil {
		t.Fatal(err)
	}
	taskDeadline := time.Now().Add(2 * time.Minute)
	receiveCtx, cancel := context.WithDeadline(context.Background(), taskDeadline)
	defer cancel()
	reader := startLifetimeGuardianReader(codec, receiveCtx)
	select {
	case got := <-conn.deadline:
		if got.Before(taskDeadline.Add(-time.Second)) || got.After(taskDeadline.Add(time.Second)) {
			t.Fatalf("receive deadline = %v, want task deadline %v", got, taskDeadline)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not establish the task deadline")
	}
	cancel()
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("reader did not stop when its context was canceled")
	}
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeWorkerWaitContext(ctx context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.existingCompletion == nil {
		return nil, os.ErrNotExist
	}
	record := *r.existingCompletion
	return &record, nil
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeUnlaunchedContext(ctx context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, os.ErrNotExist
}

func (r *lifetimeGuardianTestRecords) TaskLaunchRefusedContext(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	return false, ctx.Err()
}
