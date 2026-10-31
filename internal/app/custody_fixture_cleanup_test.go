package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// errCustodyCleanupRefused reports that fixture teardown has no authority to
// remove a registration; the guardian then keeps its resident retry.
var errCustodyCleanupRefused = errors.New("custody fixture cleanup has no removal authority")

// custodyRegistrationAttempt freezes one guardian registration write: the
// facts it sent through the production writer and what that writer returned.
type custodyRegistrationAttempt struct {
	facts  taskdir.LifetimeRegistrationFacts
	record *task.LifetimeRegistrationRecord
	err    error
}

// acknowledged is conservative: the guardian can accept a write only when the
// writer succeeded with a record carrying the placement the guardian sent.
func (a custodyRegistrationAttempt) acknowledged() bool {
	return a.err == nil && a.record != nil && sameCustodyPlacement(a.record.CustodyPlacement, a.facts.CustodyPlacement)
}

// conflictsWith reports whether registration names this fixture's owner and
// the exact native scope the attempt sent, with a different custody placement.
func (a custodyRegistrationAttempt) conflictsWith(s custodyContractStore, registration *task.LifetimeRegistrationRecord) bool {
	return registration != nil &&
		registration.RootID == s.rootID && registration.OwnerKind == s.ownerKind && registration.TaskID == s.taskID &&
		registration.ScopeKind == a.facts.ScopeKind && registration.Assurance == a.facts.Assurance &&
		registration.ScopeID == a.facts.ScopeID && registration.BootID == a.facts.BootID &&
		registration.Deadline == a.facts.Deadline.UTC().Format(time.RFC3339Nano) &&
		!sameCustodyPlacement(registration.CustodyPlacement, a.facts.CustodyPlacement)
}

// custodyFixtureCleanup lets a probe's record fixture finish when its test
// ends while the guardian intentionally keeps an unacknowledged registration
// that names its own native scope with a conflicting placement. It owns the
// teardown request and the guardian facts it observed. Removal happens only
// inside the guardian's resident retry after the request; the guardian's
// canonical absence resolver then disposes the fixture scope.
type custodyFixtureCleanup struct {
	t     *testing.T
	probe *custodyContractProbe
	// requested closes when the test context ends, just before any cleanup
	// runs, including the fixture join; request lets a test close it earlier.
	requested   chan struct{}
	requestOnce sync.Once

	mu        sync.Mutex
	attempts  []custodyRegistrationAttempt
	leaseHeld bool
	reported  bool
}

func newCustodyFixtureCleanup(t *testing.T, probe *custodyContractProbe) *custodyFixtureCleanup {
	t.Helper()
	c := &custodyFixtureCleanup{t: t, probe: probe, requested: make(chan struct{})}
	context.AfterFunc(t.Context(), c.request)
	return c
}

func (c *custodyFixtureCleanup) request() {
	c.requestOnce.Do(func() { close(c.requested) })
}

func (c *custodyFixtureCleanup) isRequested() bool {
	select {
	case <-c.requested:
		return true
	default:
		return false
	}
}

func (c *custodyFixtureCleanup) observeRegistration(facts taskdir.LifetimeRegistrationFacts, record *task.LifetimeRegistrationRecord, err error) {
	attempt := custodyRegistrationAttempt{facts: facts, err: err}
	attempt.facts.CustodyPlacement = facts.CustodyPlacement.Clone()
	if record != nil {
		frozen := *record
		frozen.CustodyPlacement = record.CustodyPlacement.Clone()
		attempt.record = &frozen
	}
	c.mu.Lock()
	c.attempts = append(c.attempts, attempt)
	c.mu.Unlock()
}

func (c *custodyFixtureCleanup) setLeaseHeld(held bool) {
	c.mu.Lock()
	c.leaseHeld = held
	c.mu.Unlock()
}

// acquireOwner observes whether the guardian still holds its owner lease.
func (c *custodyFixtureCleanup) acquireOwner(acquire func(context.Context) (guardian.Owner, error)) func(context.Context) (guardian.Owner, error) {
	return func(ctx context.Context) (guardian.Owner, error) {
		owner, err := acquire(ctx)
		if err != nil {
			return owner, err
		}
		c.setLeaseHeld(true)
		return custodyObservedOwner{Owner: owner, cleanup: c}, nil
	}
}

type custodyObservedOwner struct {
	guardian.Owner
	cleanup *custodyFixtureCleanup
}

func (o custodyObservedOwner) Release() error {
	err := o.Owner.Release()
	if err == nil {
		o.cleanup.setLeaseHeld(false)
	}
	return err
}

// wrapFinalizationBackoff keeps the configured retry wait unchanged until the
// teardown request. Afterwards that wait observes cancellation, and a
// successful removal lets the guardian retry its resolver at once.
func (c *custodyFixtureCleanup) wrapFinalizationBackoff(t *testing.T, options *guardian.Options) {
	t.Helper()
	wait := options.FinalizationBackoff
	if wait == nil {
		t.Fatal("record fixture configured no finalization backoff")
	}
	options.FinalizationBackoff = func(ctx context.Context, delay time.Duration) error {
		waitErr := c.waitUntilRequested(ctx, delay, wait)
		if !c.isRequested() {
			return waitErr
		}
		if err := c.removeOwnedConflict(ctx); err != nil {
			c.reportRefusal(err)
			return waitErr
		}
		c.t.Logf("removed unacknowledged conflicting registration %s at teardown", c.probe.store.registrationPath)
		return nil
	}
}

// waitUntilRequested runs the configured wait under a context that the
// teardown request also cancels. Its watcher is joined before returning.
func (c *custodyFixtureCleanup) waitUntilRequested(ctx context.Context, delay time.Duration, wait func(context.Context, time.Duration) error) error {
	waitCtx, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-c.requested:
			stop()
		case <-waitCtx.Done():
		}
	}()
	err := wait(waitCtx, delay)
	stop()
	<-watched
	return err
}

func (c *custodyFixtureCleanup) reportRefusal(err error) {
	c.mu.Lock()
	first := !c.reported
	c.reported = true
	c.mu.Unlock()
	if first {
		c.t.Logf("custody fixture teardown kept the registration: %v", err)
	}
}

// removeOwnedConflict freshly proves, while the guardian waits in its retry,
// that the published registration is this fixture's unacknowledged conflict
// and removes exactly those bytes. Every other state keeps the registration.
func (c *custodyFixtureCleanup) removeOwnedConflict(ctx context.Context) error {
	attempts, err := c.residentAttempts()
	if err != nil {
		return err
	}
	s := c.probe.store
	if _, err = s.records.ReadLifetimeWorkerContext(ctx); !task.RecordAbsent(err) {
		return fmt.Errorf("%w: worker record is not cleanly absent: %w", errCustodyCleanupRefused, err)
	}
	if _, err = s.records.ReadLifetimeTerminationContext(ctx); !task.RecordAbsent(err) {
		return fmt.Errorf("%w: termination record is not cleanly absent: %w", errCustodyCleanupRefused, err)
	}
	registration, err := s.records.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: read registration: %w", errCustodyCleanupRefused, err)
	}
	for _, attempt := range attempts {
		if !attempt.conflictsWith(s, registration) {
			return fmt.Errorf("%w: registration is not this run's same-scope conflict", errCustodyCleanupRefused)
		}
	}
	published, err := os.ReadFile(s.registrationPath)
	if err != nil {
		return fmt.Errorf("%w: read registration bytes: %w", errCustodyCleanupRefused, err)
	}
	canonical, err := task.MarshalCanonical(*registration)
	if err != nil {
		return fmt.Errorf("%w: encode registration: %w", errCustodyCleanupRefused, err)
	}
	if !bytes.Equal(canonical, published) {
		return fmt.Errorf("%w: published bytes are not the verified registration", errCustodyCleanupRefused)
	}
	return os.Remove(s.registrationPath)
}

// residentAttempts requires the held lease, a scope neither started nor
// destroyed, and observed registration writes none of which was acknowledged.
func (c *custodyFixtureCleanup) residentAttempts() ([]custodyRegistrationAttempt, error) {
	events := c.probe.snapshot()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.leaseHeld:
		return nil, fmt.Errorf("%w: guardian does not hold its owner lease", errCustodyCleanupRefused)
	case slices.Contains(events, "worker-start"), slices.Contains(events, "scope-destroy"):
		return nil, fmt.Errorf("%w: scope is not resident and unstarted: %v", errCustodyCleanupRefused, events)
	case len(c.attempts) == 0:
		return nil, fmt.Errorf("%w: guardian attempted no registration", errCustodyCleanupRefused)
	}
	for _, attempt := range c.attempts {
		if attempt.acknowledged() {
			return nil, fmt.Errorf("%w: registration write may be acknowledged", errCustodyCleanupRefused)
		}
	}
	return slices.Clone(c.attempts), nil
}

// taskCustodyRecords and inspectionCustodyRecords embed the production stores,
// so every optional reader the guardian discovers stays promoted; only the
// registration writer is observed.
type taskCustodyRecords struct {
	*taskdir.TaskDir
	cleanup *custodyFixtureCleanup
}

func (r taskCustodyRecords) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	record, err := r.TaskDir.RecordLifetimeRegistrationContext(ctx, facts)
	r.cleanup.observeRegistration(facts, record, err)
	return record, err
}

type inspectionCustodyRecords struct {
	*inspection.Operation
	cleanup *custodyFixtureCleanup
}

func (r inspectionCustodyRecords) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	record, err := r.Operation.RecordLifetimeRegistrationContext(ctx, facts)
	r.cleanup.observeRegistration(facts, record, err)
	return record, err
}

// Teardown heals the guardian's resident same-scope conflict whether the
// request preceded the guardian's retry wait or arrived while the scenario
// barrier held it there.
func TestCustodyFixtureCleanupHealsResidentConflictAtTeardown(t *testing.T) {
	for name, open := range custodyContractStores() {
		for _, requestAtScope := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/request_at_scope=%t", name, requestAtScope), func(t *testing.T) {
				s := open(t)
				probe := &custodyContractProbe{store: s}
				var runErr error
				t.Run("resident at teardown", func(t *testing.T) {
					leaveCustodyConflictResident(t, probe, requestAtScope, &runErr)
				})
				if !errors.Is(runErr, task.ErrEvidenceFault) || !errors.Is(runErr, task.ErrIdentityMismatch) {
					t.Fatalf("guardian error = %v, want evidence fault and identity mismatch", runErr)
				}
				requireCustodyRecordAbsent(t, "registration", s.records.ReadLifetimeRegistrationContext)
				requireCustodyRecordAbsent(t, "termination", s.records.ReadLifetimeTerminationContext)
				requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
				if events := probe.snapshot(); !slices.Contains(events, "scope-destroy") || slices.Contains(events, "worker-start") {
					t.Fatalf("healed conflict events = %v, want unregistered disposal without worker", events)
				}
			})
		}
	}
}

// leaveCustodyConflictResident ends its test while the guardian keeps the
// injected same-scope conflict. Its cleanups await the guardian's own exit
// before restoring the injected fault, so only teardown healing passes.
func leaveCustodyConflictResident(t *testing.T, probe *custodyContractProbe, requestAtScope bool, runErr *error) {
	t.Helper()
	s := probe.store
	requested, entered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	probe.onScope = func() {
		recordForeignCustodyRegistration(t, s, "test-scope", custodyContractPlacement("b"))
		if requestAtScope {
			probe.cleanup.request()
			close(requested)
		}
	}
	fixture := probe.run(t, func(options *guardian.Options) {
		options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		}
	})
	// Runs second, before the fixture join: restores the injected fault even
	// when teardown healing is broken, so a failure stays finite.
	t.Cleanup(func() {
		close(release)
		if err := os.Remove(s.registrationPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Error(err)
		}
	})
	// Runs first, after the test context ends: the guardian must exit alone.
	t.Cleanup(func() { *runErr = fixture.WaitRun(t) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("guardian did not keep the injected same-scope conflict in its resident retry")
	}
	if requestAtScope {
		select {
		case <-requested:
		default:
			t.Fatal("teardown was not requested before the resident retry")
		}
	} else {
		registration, _ := readCustodyContractRegistration(t, s)
		requireCustodyPlacement(t, "conflict before teardown", registration.CustodyPlacement, custodyContractPlacement("b"))
	}
}

type custodyCleanupRefusalCase struct {
	label          string
	persistedScope string
	persisted      *task.LifetimeCustodyPlacement
	observed       bool
	acknowledged   bool
	leaseReleased  bool
	workerStarted  bool
	unreadable     bool
	wantRemoved    bool
}

// Only the owned conflict is removed. A matching, acknowledged, foreign-scope,
// or unreadable registration, a missing write, a released lease, or a started
// worker never authorizes removal, and the published bytes stay intact.
func TestCustodyFixtureCleanupRemovesOnlyOwnedConflict(t *testing.T) {
	a, b := custodyContractPlacement("a"), custodyContractPlacement("b")
	for name, open := range custodyContractStores() {
		for _, tc := range []custodyCleanupRefusalCase{
			{label: "owned conflict", persistedScope: "test-scope", persisted: b, observed: true, wantRemoved: true},
			{label: "matching", persistedScope: "test-scope", persisted: a, observed: true},
			{label: "acknowledged", persistedScope: "test-scope", persisted: b, observed: true, acknowledged: true},
			{label: "foreign scope", persistedScope: "foreign-scope", persisted: b, observed: true},
			{label: "unreadable", persistedScope: "test-scope", persisted: b, observed: true, unreadable: true},
			{label: "unobserved", persistedScope: "test-scope", persisted: b},
			{label: "lease released", persistedScope: "test-scope", persisted: b, observed: true, leaseReleased: true},
			{label: "worker started", persistedScope: "test-scope", persisted: b, observed: true, workerStarted: true},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				requireCustodyCleanupDecision(t, open(t), tc, a)
			})
		}
	}
}

func requireCustodyCleanupDecision(t *testing.T, s custodyContractStore, tc custodyCleanupRefusalCase, sent *task.LifetimeCustodyPlacement) {
	t.Helper()
	seedCustodyContractRegistration(t, s, tc.persistedScope, tc.persisted)
	if tc.unreadable {
		if err := os.Remove(s.registrationPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.registrationPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(s.registrationPath)
	if err != nil {
		t.Fatal(err)
	}
	probe := &custodyContractProbe{store: s}
	cleanup := newCustodyFixtureCleanup(t, probe)
	cleanup.leaseHeld = !tc.leaseReleased
	if tc.workerStarted {
		probe.add("worker-start")
	}
	if tc.observed {
		attempt := custodyRegistrationAttempt{facts: custodyRegistrationFacts(s, "test-scope", sent.Clone()), err: task.ErrIdentityMismatch}
		if tc.acknowledged {
			attempt.err, attempt.record = nil, &task.LifetimeRegistrationRecord{CustodyPlacement: sent.Clone()}
		}
		cleanup.attempts = append(cleanup.attempts, attempt)
	}
	err = cleanup.removeOwnedConflict(t.Context())
	if tc.wantRemoved {
		if err != nil {
			t.Fatalf("owned conflict removal = %v", err)
		}
		requireCustodyRecordAbsent(t, "registration", s.records.ReadLifetimeRegistrationContext)
		return
	}
	if !errors.Is(err, errCustodyCleanupRefused) {
		t.Fatalf("removal = %v, want refusal", err)
	}
	if after, readErr := os.ReadFile(s.registrationPath); readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("refused cleanup changed the registration: err %v\n got %s\nwant %s", readErr, after, before)
	}
}
