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

func TestLifetimeCustodyPlacementRegistersBeforeWorkerThroughProductionStores(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			want := custodyContractPlacement("a")
			selected := want.Clone()
			probe := &custodyContractProbe{store: s, placement: selected}
			// The selector's object belongs to the selector once it returns.
			probe.onScope = func() { selected.PoolID = strings.Repeat("f", 64) }
			completeCustodyContractRun(t, probe.run(t, nil))

			events, selections := probe.snapshot()
			if selections != 1 || !probe.intentAtSelect || probe.registrationAtSelect {
				t.Fatalf("selection = %d calls, intent present %t, registration present %t; want 1/true/false",
					selections, probe.intentAtSelect, probe.registrationAtSelect)
			}
			requireCustodyEventOrder(t, events, "select", "scope", "worker-start")
			if probe.registrationAtStart == nil {
				t.Fatal("worker started before a durable registration")
			}
			requireCustodyPlacement(t, "registration at worker start", probe.registrationAtStart.CustodyPlacement, want)
			registration, published := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "persisted", registration.CustodyPlacement, want)
			if !bytes.Contains(published, []byte(`"custody_placement":{"version":1,`)) {
				t.Fatalf("published registration lacks placement: %s", published)
			}
			requireCustodyWorkerBinding(t, s, published)
		})
	}
}

func TestLifetimeCustodyRegistrationWithoutSelectorKeepsLegacyBytes(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			completeCustodyContractRun(t, testsupport.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline,
				s.records, s.acquire, nil))
			registration, published := readCustodyContractRegistration(t, s)
			if registration.CustodyPlacement != nil || bytes.Contains(published, []byte("custody_placement")) {
				t.Fatalf("registration without a selector gained a placement: %s", published)
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

func TestLifetimeCustodyExistingRegistrationIsNeverReselected(t *testing.T) {
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
				probe := &custodyContractProbe{store: s, placement: custodyContractPlacement("b")}
				completeCustodyContractRun(t, probe.run(t, nil))
				if _, selections := probe.snapshot(); selections != 0 {
					t.Fatalf("placement selections over an existing registration = %d, want 0", selections)
				}
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

func TestLifetimeCustodySelectionRefusalLeavesNoScope(t *testing.T) {
	wantErr := errors.New("custody pool unavailable")
	invalid := custodyContractPlacement("a")
	invalid.Version = 2
	for name, open := range custodyContractStores() {
		for _, tc := range []struct {
			label     string
			placement *task.LifetimeCustodyPlacement
			err       error
			want      error
		}{
			{label: "selector error", err: wantErr, want: wantErr},
			{label: "invalid placement", placement: invalid, want: task.ErrUnsupportedSchema},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				s := open(t)
				probe := &custodyContractProbe{store: s, placement: tc.placement, err: tc.err}
				if err := probe.run(t, nil).WaitRun(t); !errors.Is(err, tc.want) {
					t.Fatalf("guardian error = %v, want %v", err, tc.want)
				}
				events, selections := probe.snapshot()
				if selections != 1 || slices.Contains(events, "scope") || slices.Contains(events, "worker-start") {
					t.Fatalf("refused selection events = %v after %d selections", events, selections)
				}
				requireCustodyRecordAbsent(t, "registration", s.records.ReadLifetimeRegistrationContext)
				requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
			})
		}
	}
}

func TestLifetimeCustodyPostLinkRegistrationFailureFinalizesPublishedRecord(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			fault := &lifetimeContractPostLinkFault{target: filepath.Base(s.registrationPath)}
			s.store.SetFaultInjector(fault)
			t.Cleanup(func() { s.store.SetFaultInjector(nil) })
			want := custodyContractPlacement("a")
			probe := &custodyContractProbe{store: s, placement: want.Clone()}
			if err := probe.run(t, nil).WaitRun(t); err == nil {
				t.Fatal("guardian succeeded after an ambiguous registration write")
			}
			if !fault.didFire() {
				t.Fatal("production store did not exercise the ambiguous registration write")
			}
			registration, _ := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "published", registration.CustodyPlacement, want)
			termination, err := s.records.ReadLifetimeTerminationContext(t.Context())
			if err != nil || !termination.ScopeEmpty || termination.Cause != task.LifetimeTerminationRecovery {
				t.Fatalf("termination = %+v, err %v; want empty recovery termination", termination, err)
			}
			requireCustodyRecordAbsent(t, "worker", s.records.ReadLifetimeWorkerContext)
			events, selections := probe.snapshot()
			if selections != 1 || slices.Contains(events, "worker-start") || !slices.Contains(events, "scope-destroy") {
				t.Fatalf("ambiguous registration events = %v after %d selections", events, selections)
			}
		})
	}
}

func TestLifetimeCustodyScopeCollisionSelectsOnce(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			want := custodyContractPlacement("a")
			probe := &custodyContractProbe{store: s, placement: want.Clone()}
			collisions := 0
			fixture := probe.run(t, func(options *guardian.Options) {
				create := options.ScopeFactory
				options.ScopeFactory = func(plan lifetime.Plan) (guardian.Scope, error) {
					if collisions == 0 {
						collisions++
						return nil, lifetime.ErrScopeExists
					}
					return create(plan)
				}
			})
			completeCustodyContractRun(t, fixture)
			if _, selections := probe.snapshot(); selections != 1 || collisions != 1 {
				t.Fatalf("selections %d over %d collisions, want 1/1", selections, collisions)
			}
			registration, published := readCustodyContractRegistration(t, s)
			requireCustodyPlacement(t, "registered after collision", registration.CustodyPlacement, want)
			requireCustodyWorkerBinding(t, s, published)
		})
	}
}

func TestLifetimeCustodyDifferentNativeRegistrationDisposesOnlyNewScope(t *testing.T) {
	for name, open := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			placement := custodyContractPlacement("a")
			probe := &custodyContractProbe{store: s, placement: placement.Clone()}
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
			if events, _ := probe.snapshot(); !slices.Contains(events, "scope-destroy") || slices.Contains(events, "worker-start") {
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
	probe := &custodyContractProbe{store: s, placement: custodyContractPlacement("a")}
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
	if events, _ := probe.snapshot(); slices.Contains(events, "scope-destroy") || slices.Contains(events, "worker-start") {
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
	if events, _ := probe.snapshot(); !slices.Contains(events, "scope-destroy") {
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
