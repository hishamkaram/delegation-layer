package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestLifetimeCustodyNewRegistrationIsUnplacedBeforeWorkerThroughProductionStores(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			probe := &custodyContractProbe{store: s}
			probe.onScope = func() {
				if intent, err := s.records.ReadLifetimeIntentContext(t.Context()); err != nil || intent == nil {
					t.Errorf("scope created before durable intent: %+v, %v", intent, err)
				}
				if _, err := s.records.ReadLifetimeRegistrationContext(t.Context()); !task.RecordAbsent(err) {
					t.Errorf("registration before scope creation = %v, want clean absence", err)
				}
			}
			completeCustodyContractRun(t, probe.run(t, nil))

			requireCustodyEventOrder(t, probe.snapshot(), "scope", "worker-start")
			if probe.registrationAtStart == nil {
				t.Fatal("worker started before a durable registration")
			}
			requireCustodyPlacement(t, "registration at worker start", probe.registrationAtStart.CustodyPlacement, nil)
			registration, published := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "persisted", registration.CustodyPlacement, nil)
			if bytes.Contains(published, []byte("custody_placement")) {
				t.Fatalf("new registration gained a placement: %s", published)
			}
			requireCustodyWorkerBinding(t, s, published)
		})
	}
}

func TestLifetimeCustodyUnplacedRegistrationKeepsLegacyBytes(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			completeCustodyContractRun(t, testsupport.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline,
				s.records, s.acquire, nil))
			registration, published := readCustodyContractRegistration(t, s)
			if registration.CustodyPlacement != nil || bytes.Contains(published, []byte("custody_placement")) {
				t.Fatalf("unplaced registration gained a placement: %s", published)
			}
			canonical, err := task.MarshalCanonical(*registration)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, published) {
				t.Fatalf("registration bytes are not canonical:\n got %s\nwant %s", published, canonical)
			}
			requireCustodyWorkerBinding(t, s, published)
		})
	}
}

func TestLifetimeCustodyExistingRegistrationKeepsPlacementAndBytes(t *testing.T) {
	for name, open := range custodyContractStores() {
		for _, existing := range []*task.LifetimeCustodyPlacement{custodyContractPlacement("a"), nil} {
			label := "nil"
			if existing != nil {
				label = "populated"
			}
			t.Run(name+"/"+label, func(t *testing.T) {
				s := open(t)
				seedCustodyContractRegistration(t, s, "test-scope", existing)
				before, err := os.ReadFile(s.registrationPath)
				if err != nil {
					t.Fatal(err)
				}
				probe := &custodyContractProbe{store: s}
				completeCustodyContractRun(t, probe.run(t, nil))
				registration, published := readCustodyContractRegistration(t, s)
				if !bytes.Equal(published, before) {
					t.Fatalf("existing registration was rewritten:\n got %s\nwant %s", published, before)
				}
				requireCustodyPlacement(t, "existing", registration.CustodyPlacement, existing)
				requireCustodyWorkerBinding(t, s, published)
			})
		}
	}
}

func TestLifetimeCustodyPostLinkRegistrationFailureFinalizesPublishedRecord(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			requireCustodyRecordAbsent(t, "registration", s.records.ReadLifetimeRegistrationContext)
			fault := &lifetimeContractPostLinkFault{target: filepath.Base(s.registrationPath)}
			s.store.SetFaultInjector(fault)
			t.Cleanup(func() { s.store.SetFaultInjector(nil) })
			probe := &custodyContractProbe{store: s}
			if err := probe.run(t, nil).WaitRun(t); err == nil {
				t.Fatal("guardian succeeded after an ambiguous registration write")
			}
			if !fault.didFire() {
				t.Fatal("production store did not exercise the ambiguous registration write")
			}
			registration, _ := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "published", registration.CustodyPlacement, nil)
			termination, err := s.records.ReadLifetimeTerminationContext(t.Context())
			if err != nil || !termination.ScopeEmpty || termination.Cause != task.LifetimeTerminationRecovery {
				t.Fatalf("termination = %+v, err %v; want empty recovery termination", termination, err)
			}
			requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
			events := probe.snapshot()
			if slices.Contains(events, "worker-start") || !slices.Contains(events, "scope-destroy") {
				t.Fatalf("ambiguous registration events = %v, want disposal without worker", events)
			}
		})
	}
}

func TestLifetimeCustodyScopeCollisionPublishesNewUnplacedRegistration(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			requireCustodyRecordAbsent(t, "registration", s.records.ReadLifetimeRegistrationContext)
			probe := &custodyContractProbe{store: s}
			collisions := 0
			scopeCalls := 0
			fixture := probe.run(t, func(options *guardian.Options) {
				create := options.ScopeFactory
				options.ScopeFactory = func(plan lifetime.Plan) (guardian.Scope, error) {
					scopeCalls++
					if collisions == 0 {
						collisions++
						return nil, lifetime.ErrScopeExists
					}
					return create(plan)
				}
			})
			completeCustodyContractRun(t, fixture)
			if scopeCalls != 2 || collisions != 1 {
				t.Fatalf("scope calls %d over %d collisions, want 2/1", scopeCalls, collisions)
			}
			registration, published := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "registered after collision", registration.CustodyPlacement, nil)
			requireCustodyWorkerBinding(t, s, published)
		})
	}
}

func TestLifetimeCustodyDifferentNativeRegistrationDisposesOnlyNewScope(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			placement := custodyContractPlacement("a")
			probe := &custodyContractProbe{store: s}
			probe.onScope = func() { recordForeignCustodyRegistration(t, s, "foreign-scope", placement) }
			if err := probe.run(t, nil).WaitRun(t); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("guardian error = %v, want identity mismatch", err)
			}
			registration, _ := readCustodyContractRegistration(t, s)
			if registration.ScopeID != "foreign-scope" {
				t.Fatalf("foreign registration changed: %+v", registration)
			}
			requireCustodyRecordAbsent(t, "termination", s.records.ReadLifetimeTerminationContext)
			requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
			if events := probe.snapshot(); !slices.Contains(events, "scope-destroy") || slices.Contains(events, "worker-start") {
				t.Fatalf("different native registration events = %v, want disposal without worker", events)
			}
		})
	}
}

// A foreign registration that names this exact native scope publishes it. The
// guardian keeps that scope resident; the test removes the foreign record only
// to let the resident retry reach its existing unregistered disposal.
func TestLifetimeCustodySameScopeForeignPlacementStaysResident(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			testLifetimeCustodySameScopeForeignPlacement(t, open(t))
		})
	}
}

func testLifetimeCustodySameScopeForeignPlacement(t *testing.T, s custodyContractStore) {
	t.Helper()
	probe := &custodyContractProbe{store: s}
	probe.onScope = func() { recordForeignCustodyRegistration(t, s, "test-scope", custodyContractPlacement("b")) }
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
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
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("guardian did not keep the published scope in its resident retry")
	}
	registration, _ := readCustodyContractRegistration(t, s)
	requireCustodyPlacement(t, "foreign", registration.CustodyPlacement, custodyContractPlacement("b"))
	requireCustodyRecordAbsent(t, "termination", s.records.ReadLifetimeTerminationContext)
	requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
	if events := probe.snapshot(); slices.Contains(events, "scope-destroy") || slices.Contains(events, "worker-start") {
		t.Fatalf("foreign placement events = %v, want resident scope without worker", events)
	}
	if err := os.Remove(s.registrationPath); err != nil {
		t.Fatal(err)
	}
	close(release)
	err := fixture.WaitRun(t)
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("guardian error = %v, want evidence fault and identity mismatch", err)
	}
	if events := probe.snapshot(); !slices.Contains(events, "scope-destroy") {
		t.Fatalf("resolved foreign placement events = %v, want disposal", events)
	}
}

func seedCustodyContractRegistration(t *testing.T, s custodyContractStore, scopeID string, placement *task.LifetimeCustodyPlacement) {
	t.Helper()
	owner, err := s.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	recordForeignCustodyRegistration(t, s, scopeID, placement)
}

// recordForeignCustodyRegistration writes the guardian's own intent and a
// registration through the production writer, as a writer other than the
// running guardian would.
func recordForeignCustodyRegistration(t *testing.T, s custodyContractStore, scopeID string, placement *task.LifetimeCustodyPlacement) {
	t.Helper()
	ctx := context.Background()
	_, err := s.records.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{
		ScopeKind: string(lifetime.KindProcessGroup),
		Assurance: string(lifetime.AssuranceCooperative),
		ScopePlan: "pg:" + strings.Repeat("0", 32),
		BootID:    "test-boot",
		Deadline:  s.deadline,
	})
	if err != nil {
		t.Error(err)
		return
	}
	_, err = s.records.RecordLifetimeRegistrationContext(ctx, custodyRegistrationFacts(s, scopeID, placement))
	if err != nil {
		t.Error(err)
	}
}

// custodyRegistrationFacts names the record fixture's native scope kind, boot,
// and deadline with the given scope identifier and placement.
func custodyRegistrationFacts(s custodyContractStore, scopeID string, placement *task.LifetimeCustodyPlacement) taskdir.LifetimeRegistrationFacts {
	return taskdir.LifetimeRegistrationFacts{
		ScopeKind:        string(lifetime.KindProcessGroup),
		Assurance:        string(lifetime.AssuranceCooperative),
		ScopeID:          scopeID,
		BootID:           "test-boot",
		Deadline:         s.deadline,
		CustodyPlacement: placement,
	}
}

func requireCustodyRecordAbsent[T any](t *testing.T, label string, read func(context.Context) (T, error)) {
	t.Helper()
	if _, err := read(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("%s record = %v, want clean absence", label, err)
	}
}
