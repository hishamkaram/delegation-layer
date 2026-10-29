package guardian

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func lifetimeGuardianTestPlacement(fill string) *task.LifetimeCustodyPlacement {
	return &task.LifetimeCustodyPlacement{
		Version:  task.LifetimeCustodyPlacementVersion,
		PoolRoot: "/var/lib/delegate/pool",
		PoolID:   strings.Repeat(fill, 64),
	}
}

// lifetimeGuardianTestSelector counts selections on the Run goroutine. Tests
// read the count only after Run has returned through runDone.
type lifetimeGuardianTestSelector struct {
	timeline  *lifetimeGuardianTestTimeline
	placement *task.LifetimeCustodyPlacement
	err       error
	calls     int
	before    func(context.Context)
}

func (s *lifetimeGuardianTestSelector) selectPlacement(ctx context.Context) (*task.LifetimeCustodyPlacement, error) {
	s.calls++
	s.timeline.add("select")
	if s.before != nil {
		s.before(ctx)
	}
	return s.placement, s.err
}

func (r *lifetimeGuardianTestRecords) registrationFactsSnapshot() []taskdir.LifetimeRegistrationFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]taskdir.LifetimeRegistrationFacts(nil), r.registrations...)
}

func (r *lifetimeGuardianTestRecords) setExistingRegistration(record *task.LifetimeRegistrationRecord) {
	r.mu.Lock()
	r.existingRegistration = record
	r.mu.Unlock()
}

func newLifetimeGuardianPlacementFixture(
	t *testing.T,
	selector *lifetimeGuardianTestSelector,
	configure func(*Options, *lifetimeGuardianTestRecords),
) *lifetimeGuardianTestFixture {
	t.Helper()
	return newLifetimeGuardianTestFixtureWithOptions(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			if selector != nil {
				selector.timeline = records.timeline
				options.SelectPlacement = selector.selectPlacement
			}
			if configure != nil {
				configure(options, records)
			}
		})
}

func completeLifetimeGuardianPlacementRun(t *testing.T, f *lifetimeGuardianTestFixture) {
	t.Helper()
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func requireLifetimeGuardianPlacement(t *testing.T, label string, got, want *task.LifetimeCustodyPlacement) {
	t.Helper()
	if !sameLifetimeCustodyPlacement(got, want) {
		t.Fatalf("%s placement = %+v, want %+v", label, got, want)
	}
}

func TestLifetimeGuardianSelectsPlacementAfterIntentBeforeScope(t *testing.T) {
	selected := lifetimeGuardianTestPlacement("a")
	want := selected.Clone()
	selector := &lifetimeGuardianTestSelector{placement: selected}
	f := newLifetimeGuardianPlacementFixture(t, selector, func(options *Options, _ *lifetimeGuardianTestRecords) {
		create := options.ScopeFactory
		options.ScopeFactory = func(plan lifetime.Plan) (Scope, error) {
			// The selector's object belongs to the selector after it returns.
			selected.PoolID = strings.Repeat("f", 64)
			return create(plan)
		}
	})
	completeLifetimeGuardianPlacementRun(t, f)
	if selector.calls != 1 {
		t.Fatalf("placement selections = %d, want 1", selector.calls)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "select", "scope", "registration", "worker-start")
	facts := f.records.registrationFactsSnapshot()
	if len(facts) != 1 {
		t.Fatalf("registration writes = %d, want 1", len(facts))
	}
	requireLifetimeGuardianPlacement(t, "registered", facts[0].CustodyPlacement, want)
	if facts[0].CustodyPlacement == selected || facts[0].CustodyPlacement == f.guardian.placement {
		t.Fatal("registration facts alias the selector result or guardian placement")
	}
}

func TestLifetimeGuardianPassesFreshPlacementOnEveryRegistrationWrite(t *testing.T) {
	wantErr := errors.New("registration acknowledgement failed")
	want := lifetimeGuardianTestPlacement("a")
	selector := &lifetimeGuardianTestSelector{placement: want.Clone()}
	f := newLifetimeGuardianPlacementFixture(t, selector, func(_ *Options, records *lifetimeGuardianTestRecords) {
		records.registrationErrors = []error{wantErr, nil}
		records.registrationCommitOnError = true
	})
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want registration acknowledgement failure", err)
	}
	facts := f.records.registrationFactsSnapshot()
	if len(facts) != 2 {
		t.Fatalf("registration writes = %d, want write plus acknowledgement", len(facts))
	}
	for index, fact := range facts {
		requireLifetimeGuardianPlacement(t, "registration write", fact.CustodyPlacement, want)
		if fact.CustodyPlacement == f.guardian.placement {
			t.Fatalf("registration write %d passed the guardian's own placement", index)
		}
	}
	if facts[0].CustodyPlacement == facts[1].CustodyPlacement {
		t.Fatal("registration writes shared one placement pointer")
	}
	if got := f.records.terminationCauses(); len(got) != 1 || got[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("acknowledged registration termination = %v, want recovery", got)
	}
	if start, _ := f.worker.counts(); start != 0 {
		t.Fatalf("worker starts after registration failure = %d, want 0", start)
	}
}

func TestLifetimeGuardianExistingRegistrationPlacementIsNeverReselected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing *task.LifetimeCustodyPlacement
		selector bool
	}{
		{name: "populated", existing: lifetimeGuardianTestPlacement("a"), selector: true},
		{name: "nil", selector: true},
		{name: "populated without selector", existing: lifetimeGuardianTestPlacement("a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selector *lifetimeGuardianTestSelector
			if tc.selector {
				selector = &lifetimeGuardianTestSelector{placement: lifetimeGuardianTestPlacement("b")}
			}
			f := newLifetimeGuardianPlacementFixture(t, selector, func(options *Options, records *lifetimeGuardianTestRecords) {
				existing := matchingLifetimeGuardianTestRegistration(options, "test-scope")
				existing.CustodyPlacement = tc.existing.Clone()
				records.existingRegistration = existing
			})
			completeLifetimeGuardianPlacementRun(t, f)
			if selector != nil && selector.calls != 0 {
				t.Fatalf("placement selections over an existing registration = %d, want 0", selector.calls)
			}
			facts := f.records.registrationFactsSnapshot()
			if len(facts) != 1 {
				t.Fatalf("registration writes = %d, want 1", len(facts))
			}
			requireLifetimeGuardianPlacement(t, "rewritten", facts[0].CustodyPlacement, tc.existing)
			requireLifetimeGuardianPlacement(t, "guardian", f.guardian.placement, tc.existing)
		})
	}
}

func TestLifetimeGuardianPlacementSelectionRefusalCreatesNoScope(t *testing.T) {
	wantErr := errors.New("custody pool unavailable")
	invalid := lifetimeGuardianTestPlacement("a")
	invalid.Version = 2
	relative := lifetimeGuardianTestPlacement("a")
	relative.PoolRoot = "pool"
	for _, tc := range []struct {
		name      string
		placement *task.LifetimeCustodyPlacement
		err       error
		cancel    bool
		want      error
	}{
		{name: "selector error", err: wantErr, want: wantErr},
		{name: "nil placement", want: errLifetimeGuardianInvalid},
		{name: "unsupported version", placement: invalid, want: task.ErrUnsupportedSchema},
		{name: "relative root", placement: relative, want: task.ErrIdentityMismatch},
		{name: "canceled run", placement: lifetimeGuardianTestPlacement("a"), cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			selector := &lifetimeGuardianTestSelector{placement: tc.placement, err: tc.err}
			if tc.cancel {
				selector.before = func(context.Context) { cancel() }
			}
			f := newLifetimeGuardianTestFixtureWithOptionsAndContext(t, runContext, lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget), nil, lifetime.KindProcessGroup,
				lifetime.AssuranceCooperative, nil, nil, func(options *Options) {
					selector.timeline = lifetimeGuardianTestRecordsFromOptions(t, options).timeline
					options.SelectPlacement = selector.selectPlacement
				})
			if err := f.waitRun(t); !errors.Is(err, tc.want) {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			requireLifetimeGuardianNoScope(t, f)
			if selector.calls != 1 {
				t.Fatalf("placement selections = %d, want 1", selector.calls)
			}
			assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "select", "release")
		})
	}
}

func requireLifetimeGuardianNoScope(t *testing.T, f *lifetimeGuardianTestFixture) {
	t.Helper()
	if events := f.timeline.snapshot(); timelineIndex(events, "scope") >= 0 {
		t.Fatalf("refused placement created a scope: %v", events)
	}
	if facts := f.records.registrationFactsSnapshot(); len(facts) != 0 {
		t.Fatalf("refused placement wrote %d registrations", len(facts))
	}
	if start, wait := f.worker.counts(); start != 0 || wait != 0 {
		t.Fatalf("refused placement worker calls = start %d wait %d", start, wait)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("refused placement disposal = destroy %d release %d, want 0/0", destroy, release)
	}
}

func TestLifetimeGuardianRegistrationPreReadFaultCreatesNoScope(t *testing.T) {
	readErr := errors.New("registration read failed")
	markedAbsence := errors.Join(task.ErrIdentityMismatch, os.ErrNotExist)
	for _, tc := range []struct {
		name      string
		configure func(*lifetimeGuardianTestRecords)
		want      error
	}{
		{name: "read fault", configure: func(r *lifetimeGuardianTestRecords) { r.registrationReadErr = readErr }, want: readErr},
		{name: "marked absence", configure: func(r *lifetimeGuardianTestRecords) { r.registrationReadErr = markedAbsence }, want: task.ErrIdentityMismatch},
		{name: "nil record", configure: func(r *lifetimeGuardianTestRecords) { r.registrationReadNil = true }, want: task.ErrEvidenceFault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := &lifetimeGuardianTestSelector{placement: lifetimeGuardianTestPlacement("a")}
			f := newLifetimeGuardianPlacementFixture(t, selector, func(_ *Options, records *lifetimeGuardianTestRecords) {
				tc.configure(records)
			})
			if err := f.waitRun(t); !errors.Is(err, tc.want) {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			if selector.calls != 0 {
				t.Fatalf("placement selections after unreadable registration = %d, want 0", selector.calls)
			}
			requireLifetimeGuardianNoScope(t, f)
		})
	}
}

func TestLifetimeGuardianRegistrationPreReadRunsWithoutSelector(t *testing.T) {
	readErr := errors.New("registration read failed")
	f := newLifetimeGuardianPlacementFixture(t, nil, func(_ *Options, records *lifetimeGuardianTestRecords) {
		records.registrationReadErr = readErr
	})
	if err := f.waitRun(t); !errors.Is(err, readErr) {
		t.Fatalf("Run error = %v, want registration read failure", err)
	}
	requireLifetimeGuardianNoScope(t, f)
}

// A registration that names this exact native scope publishes it, whatever
// its placement. The guardian keeps it resident: no disposal, no adoption or
// acknowledgement of the foreign placement, no worker, no termination record.
func TestLifetimeGuardianForeignPlacementKeepsRegisteredScopeResident(t *testing.T) {
	writeErr := errors.New("registration write failed")
	for _, tc := range []struct {
		name     string
		writeErr error
		want     error
	}{
		{name: "post-write readback", want: errLifetimeGuardianInvalid},
		{name: "ambiguous write", writeErr: writeErr, want: writeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testLifetimeGuardianForeignPlacementResident(t, tc.writeErr, tc.want)
		})
	}
}

func testLifetimeGuardianForeignPlacementResident(t *testing.T, writeErr, want error) {
	t.Helper()
	own := lifetimeGuardianTestPlacement("a")
	retryEntered, retryRelease := make(chan struct{}), make(chan struct{})
	var retryOnce sync.Once
	selector := &lifetimeGuardianTestSelector{placement: own.Clone()}
	f := newLifetimeGuardianPlacementFixture(t, selector, func(options *Options, records *lifetimeGuardianTestRecords) {
		records.registrationPlacement = lifetimeGuardianTestPlacement("b")
		records.registrationErr = writeErr
		records.registrationCommitOnError = true
		options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
			retryOnce.Do(func() { close(retryEntered) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-retryRelease:
				// Resolve the fault so the resident retry can finish.
				records.mu.Lock()
				records.registrationPlacement = nil
				records.registrationErr = nil
				records.existingRegistration.CustodyPlacement = own.Clone()
				records.mu.Unlock()
				return nil
			}
		}
	})
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian returned while a foreign placement named its scope: %v", err)
	case <-retryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("guardian did not keep the registered scope in its resident retry")
	}
	requireLifetimeGuardianResidentScope(t, f)
	close(retryRelease)
	err := f.waitRun(t)
	if !errors.Is(err, want) || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("Run error = %v, want %v and evidence fault", err, want)
	}
	if got := f.records.terminationCauses(); len(got) != 1 || got[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("resolved registration termination = %v, want recovery", got)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("resolved registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	if selector.calls != 1 {
		t.Fatalf("placement selections = %d, want 1", selector.calls)
	}
}

// requireLifetimeGuardianResidentScope proves the scope is retained without
// acknowledgement, termination, worker, or released authority.
func requireLifetimeGuardianResidentScope(t *testing.T, f *lifetimeGuardianTestFixture) {
	t.Helper()
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("foreign placement disposal = destroy %d release %d, want 0/0", destroy, release)
	}
	if facts := f.records.registrationFactsSnapshot(); len(facts) != 1 {
		t.Fatalf("registration writes before resolution = %d, want no acknowledgement", len(facts))
	}
	if got := f.records.terminationCauses(); len(got) != 0 {
		t.Fatalf("foreign placement published termination: %v", got)
	}
	if events := f.timeline.snapshot(); timelineIndex(events, "worker-start") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("foreign placement started a worker or released authority: %v", events)
	}
}

func TestLifetimeGuardianDifferentNativeScopeWithPlacementStillDisposes(t *testing.T) {
	wantErr := errors.New("registration write failed")
	placement := lifetimeGuardianTestPlacement("a")
	selector := &lifetimeGuardianTestSelector{placement: placement.Clone()}
	f := newLifetimeGuardianPlacementFixture(t, selector, func(options *Options, records *lifetimeGuardianTestRecords) {
		records.registrationErr = wantErr
		selector.before = func(context.Context) {
			foreign := matchingLifetimeGuardianTestRegistration(options, "different-scope")
			foreign.CustodyPlacement = placement.Clone()
			records.setExistingRegistration(foreign)
		}
	})
	err := f.waitRun(t)
	if !errors.Is(err, wantErr) || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("Run error = %v, want write failure and identity mismatch", err)
	}
	if got := f.records.terminationCauses(); len(got) != 0 {
		t.Fatalf("different native registration published termination: %v", got)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("different native registration disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	assertTimelineOrder(t, f.timeline.snapshot(), "select", "scope", "registration", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianScopeCollisionSelectsPlacementOnce(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "retry"
		if terminal {
			name = "terminal recovery"
		}
		t.Run(name, func(t *testing.T) {
			testLifetimeGuardianScopeCollisionSelection(t, terminal)
		})
	}
}

func testLifetimeGuardianScopeCollisionSelection(t *testing.T, terminal bool) {
	t.Helper()
	placement := lifetimeGuardianTestPlacement("a")
	selector := &lifetimeGuardianTestSelector{placement: placement.Clone()}
	scopeCalls := 0
	f := newLifetimeGuardianPlacementFixture(t, selector, func(options *Options, records *lifetimeGuardianTestRecords) {
		create := options.ScopeFactory
		options.ScopeFactory = func(plan lifetime.Plan) (Scope, error) {
			scopeCalls++
			if scopeCalls == 1 {
				return nil, lifetime.ErrScopeExists
			}
			return create(plan)
		}
		options.ReconcileOwned = func(context.Context) error {
			if terminal {
				records.mu.Lock()
				records.existingTermination = &task.LifetimeTerminationRecord{ScopeEmpty: true}
				records.mu.Unlock()
			}
			return nil
		}
	})
	if terminal {
		if err := f.waitRun(t); !errors.Is(err, task.ErrTerminalTask) {
			t.Fatalf("Run error = %v, want terminal task after recovery", err)
		}
		if start, wait := f.worker.counts(); start != 0 || wait != 0 || scopeCalls != 1 {
			t.Fatalf("terminal recovery relaunched: scope %d start %d wait %d", scopeCalls, start, wait)
		}
	} else {
		completeLifetimeGuardianPlacementRun(t, f)
		facts := f.records.registrationFactsSnapshot()
		if scopeCalls != 2 || len(facts) != 1 {
			t.Fatalf("collision retry = scope calls %d registrations %d, want 2/1", scopeCalls, len(facts))
		}
		requireLifetimeGuardianPlacement(t, "registered", facts[0].CustodyPlacement, placement)
	}
	if selector.calls != 1 {
		t.Fatalf("placement selections across a scope collision = %d, want 1", selector.calls)
	}
}

func TestLifetimeGuardianPlacementSelectorOwnerBoundary(t *testing.T) {
	for _, tc := range []struct {
		ownerKind string
		refused   bool
	}{
		{ownerKind: task.LifetimeOwnerTask},
		{ownerKind: task.LifetimeOwnerInspection},
		{ownerKind: task.LifetimeOwnerWorkspace, refused: true},
	} {
		t.Run(tc.ownerKind, func(t *testing.T) {
			fixture, deadline := newLifetimeGuardianTestFixtureState(t, lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget), nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil)
			t.Cleanup(func() {
				closeWorkspaceGuardianTestConnection(t, fixture.serverConn)
				closeWorkspaceGuardianTestConnection(t, fixture.clientConn)
			})
			options := lifetimeGuardianTestOptions(fixture, deadline)
			options.OwnerKind = tc.ownerKind
			if _, err := New(options); err != nil {
				t.Fatalf("New without a selector: %v", err)
			}
			options.SelectPlacement = (&lifetimeGuardianTestSelector{timeline: fixture.timeline}).selectPlacement
			_, err := New(options)
			if tc.refused != errors.Is(err, errLifetimeGuardianInvalid) || (!tc.refused && err != nil) {
				t.Fatalf("New with a selector for %s = %v, refused want %t", tc.ownerKind, err, tc.refused)
			}
		})
	}
}

func TestWorkspaceGuardianRegistersWithoutPlacement(t *testing.T) {
	var operation *taskdir.ControlLifetime
	f := newLifetimeGuardianTestFixtureWithOptions(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			operation = newGuardianCapacityControlLifetime(t, options)
		})
	// The registration is durable before the worker starts; fixture cleanup
	// owns the rest of the run.
	select {
	case <-f.worker.waitEntered:
	case err := <-f.runDone:
		t.Fatalf("workspace guardian returned before starting its worker: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("workspace guardian did not start its worker")
	}
	registration, err := operation.ReadLifetimeRegistrationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if registration.CustodyPlacement != nil || f.guardian.placement != nil {
		t.Fatalf("workspace registration placement = %+v, guardian %+v; want nil", registration.CustodyPlacement, f.guardian.placement)
	}
}
