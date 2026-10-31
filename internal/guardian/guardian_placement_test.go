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
	configure func(*Options, *lifetimeGuardianTestRecords),
) *lifetimeGuardianTestFixture {
	t.Helper()
	return newLifetimeGuardianTestFixtureWithOptions(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
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

func TestLifetimeGuardianRegistersWithoutPlacementAfterIntentBeforeScope(t *testing.T) {
	f := newLifetimeGuardianPlacementFixture(t, nil)
	completeLifetimeGuardianPlacementRun(t, f)
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "scope", "registration", "worker-start")
	facts := f.records.registrationFactsSnapshot()
	if len(facts) != 1 {
		t.Fatalf("registration writes = %d, want 1", len(facts))
	}
	requireLifetimeGuardianPlacement(t, "registered", facts[0].CustodyPlacement, nil)
	requireLifetimeGuardianPlacement(t, "guardian", f.guardian.placement, nil)
}

func TestLifetimeGuardianPassesFreshPlacementOnEveryRegistrationWrite(t *testing.T) {
	wantErr := errors.New("registration acknowledgement failed")
	want := lifetimeGuardianTestPlacement("a")
	f := newLifetimeGuardianPlacementFixture(t, func(options *Options, records *lifetimeGuardianTestRecords) {
		existing := matchingLifetimeGuardianTestRegistration(options, "test-scope")
		existing.CustodyPlacement = want.Clone()
		records.existingRegistration = existing
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

func TestLifetimeGuardianExistingRegistrationPlacementIsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing *task.LifetimeCustodyPlacement
	}{
		{name: "populated", existing: lifetimeGuardianTestPlacement("a")},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifetimeGuardianPlacementFixture(t, func(options *Options, records *lifetimeGuardianTestRecords) {
				existing := matchingLifetimeGuardianTestRegistration(options, "test-scope")
				existing.CustodyPlacement = tc.existing.Clone()
				records.existingRegistration = existing
			})
			completeLifetimeGuardianPlacementRun(t, f)
			facts := f.records.registrationFactsSnapshot()
			if len(facts) != 1 {
				t.Fatalf("registration writes = %d, want 1", len(facts))
			}
			requireLifetimeGuardianPlacement(t, "rewritten", facts[0].CustodyPlacement, tc.existing)
			requireLifetimeGuardianPlacement(t, "guardian", f.guardian.placement, tc.existing)
		})
	}
}

func TestLifetimeGuardianCanceledPlacementReadCreatesNoScope(t *testing.T) {
	runContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(t, runContext, lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget), nil, lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative, nil, nil, func(options *Options) {
			options.Records = cancelPlacementReadRecords{lifetimeGuardianTestRecords: lifetimeGuardianTestRecordsFromOptions(t, options), cancel: cancel}
		})
	if err := f.waitRun(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", err)
	}
	requireLifetimeGuardianNoScope(t, f)
	assertTimelineOrder(t, f.timeline.snapshot(), "acquire", "intent", "release")
}

type cancelPlacementReadRecords struct {
	*lifetimeGuardianTestRecords
	cancel context.CancelFunc
}

func (r cancelPlacementReadRecords) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	r.cancel()
	return r.lifetimeGuardianTestRecords.ReadLifetimeRegistrationContext(ctx)
}

// The reader may retain its record. The resolver must take its own placement
// snapshot even when the reader itself does not clone.
func TestLifetimeGuardianPlacementClonesRegistrationReaderResult(t *testing.T) {
	want := lifetimeGuardianTestPlacement("a")
	registration := &task.LifetimeRegistrationRecord{CustodyPlacement: want.Clone()}
	g := &Guardian{options: Options{Records: placementReaderRecords{registration: registration}}}
	placement, err := g.resolvePlacement(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if placement == registration.CustodyPlacement {
		t.Fatal("resolved placement aliases the registration reader's result")
	}
	registration.CustodyPlacement.PoolID = strings.Repeat("f", 64)
	requireLifetimeGuardianPlacement(t, "resolved snapshot", placement, want)
}

type placementReaderRecords struct {
	Records
	registration *task.LifetimeRegistrationRecord
}

func (r placementReaderRecords) ReadLifetimeRegistrationContext(context.Context) (*task.LifetimeRegistrationRecord, error) {
	return r.registration, nil
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
			f := newLifetimeGuardianPlacementFixture(t, func(_ *Options, records *lifetimeGuardianTestRecords) {
				tc.configure(records)
			})
			if err := f.waitRun(t); !errors.Is(err, tc.want) {
				t.Fatalf("Run error = %v, want %v", err, tc.want)
			}
			requireLifetimeGuardianNoScope(t, f)
		})
	}
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
	var own *task.LifetimeCustodyPlacement
	retryEntered, retryRelease := make(chan struct{}), make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianPlacementFixture(t, func(options *Options, records *lifetimeGuardianTestRecords) {
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
	f := newLifetimeGuardianPlacementFixture(t, func(options *Options, records *lifetimeGuardianTestRecords) {
		records.registrationErr = wantErr
		create := options.ScopeFactory
		options.ScopeFactory = func(plan lifetime.Plan) (Scope, error) {
			foreign := matchingLifetimeGuardianTestRegistration(options, "different-scope")
			foreign.CustodyPlacement = placement.Clone()
			records.setExistingRegistration(foreign)
			return create(plan)
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
	assertTimelineOrder(t, f.timeline.snapshot(), "intent", "scope", "registration", "scope-destroy", "scope-release", "release")
}

func TestLifetimeGuardianScopeCollisionKeepsNewRegistrationUnplaced(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "retry"
		if terminal {
			name = "terminal recovery"
		}
		t.Run(name, func(t *testing.T) {
			testLifetimeGuardianScopeCollisionPlacement(t, terminal)
		})
	}
}

func testLifetimeGuardianScopeCollisionPlacement(t *testing.T, terminal bool) {
	t.Helper()
	scopeCalls := 0
	f := newLifetimeGuardianPlacementFixture(t, func(options *Options, records *lifetimeGuardianTestRecords) {
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
		requireLifetimeGuardianPlacement(t, "registered", facts[0].CustodyPlacement, nil)
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
