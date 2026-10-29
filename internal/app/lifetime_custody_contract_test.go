package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// custodyContractRecords is the production record store surface these
// composed tests drive through the guardian and then read back.
type custodyContractRecords interface {
	guardian.Records
	LifetimeRegistrationDigestContext(context.Context) (string, error)
	ReadLifetimeWorkerContext(context.Context) (*task.LifetimeWorkerRecord, error)
}

// custodyContractStore is one production store family: task directories or
// inspection control records.
type custodyContractStore struct {
	store            *taskdir.Store
	records          custodyContractRecords
	acquire          func(context.Context) (guardian.Owner, error)
	rootID, taskID   string
	ownerKind        string
	deadline         time.Time
	registrationPath string
	// observedRecords is the store handed to a probe's guardian; only its
	// registration writer is observed for fixture teardown.
	observedRecords func(*custodyFixtureCleanup) guardian.Records
}

func custodyContractStores() map[string]func(*testing.T) custodyContractStore {
	return map[string]func(*testing.T) custodyContractStore{
		"task":       newTaskCustodyContractStore,
		"inspection": newInspectionCustodyContractStore,
	}
}

func newTaskCustodyContractStore(t *testing.T) custodyContractStore {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(testsupportcore.CanonicalTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, request := newAppTaskInStore(t, store, lifetimeGuardianTestTaskID, false, nil)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	return custodyContractStore{
		store:   store,
		records: td,
		acquire: func(ctx context.Context) (guardian.Owner, error) {
			return td.AcquireLifetimeOwner(ctx)
		},
		rootID: request.RootID, taskID: request.TaskID, ownerKind: task.LifetimeOwnerTask,
		deadline:         time.Now().Add(time.Minute).UTC(),
		registrationPath: filepath.Join(td.Dir, "lifetime", "registration.json"),
		observedRecords: func(cleanup *custodyFixtureCleanup) guardian.Records {
			return taskCustodyRecords{TaskDir: td, cleanup: cleanup}
		},
	}
}

func newInspectionCustodyContractStore(t *testing.T) custodyContractStore {
	t.Helper()
	created := time.Now().UTC()
	store, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged,
		inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	request := operation.Request()
	return custodyContractStore{
		store:   store,
		records: operation,
		acquire: func(ctx context.Context) (guardian.Owner, error) {
			return operation.AcquireLifetimeOwner(ctx)
		},
		rootID: request.RootID, taskID: request.TaskID, ownerKind: task.LifetimeOwnerInspection,
		deadline:         operation.Deadline(),
		registrationPath: filepath.Join(store.Root, "inspections", request.TaskID, "lifetime-registration.json"),
		observedRecords: func(cleanup *custodyFixtureCleanup) guardian.Records {
			return inspectionCustodyRecords{Operation: operation, cleanup: cleanup}
		},
	}
}

func custodyContractPlacement(fill string) *task.LifetimeCustodyPlacement {
	return &task.LifetimeCustodyPlacement{
		Version:  task.LifetimeCustodyPlacementVersion,
		PoolRoot: "/var/lib/delegate/pool",
		PoolID:   strings.Repeat(fill, 64),
	}
}

// custodyContractProbe observes the guardian's selection, scope, and worker
// boundaries against the production store at each boundary.
type custodyContractProbe struct {
	store     custodyContractStore
	placement *task.LifetimeCustodyPlacement
	err       error
	onScope   func()
	// cleanup is set by run before its guardian starts.
	cleanup *custodyFixtureCleanup

	mu                   sync.Mutex
	events               []string
	selections           int
	intentAtSelect       bool
	registrationAtSelect bool
	registrationAtStart  *task.LifetimeRegistrationRecord
}

func (p *custodyContractProbe) add(event string) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *custodyContractProbe) snapshot() (events []string, selections int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events), p.selections
}

func (p *custodyContractProbe) selectPlacement(ctx context.Context) (*task.LifetimeCustodyPlacement, error) {
	_, intentErr := p.store.records.ReadLifetimeIntentContext(ctx)
	_, registrationErr := p.store.records.ReadLifetimeRegistrationContext(ctx)
	p.mu.Lock()
	p.selections++
	p.intentAtSelect = intentErr == nil
	p.registrationAtSelect = !task.RecordAbsent(registrationErr)
	p.mu.Unlock()
	p.add("select")
	return p.placement, p.err
}

type custodyContractScope struct {
	guardian.Scope
	probe *custodyContractProbe
}

func (s custodyContractScope) DestroyEmpty() error {
	s.probe.add("scope-destroy")
	return s.Scope.DestroyEmpty()
}

func (p *custodyContractProbe) configure(options *guardian.Options) {
	options.SelectPlacement = p.selectPlacement
	create := options.ScopeFactory
	options.ScopeFactory = func(plan lifetime.Plan) (guardian.Scope, error) {
		p.add("scope")
		if p.onScope != nil {
			p.onScope()
		}
		scope, err := create(plan)
		if err != nil {
			return nil, err
		}
		return custodyContractScope{Scope: scope, probe: p}, nil
	}
	start := options.StartWorker
	options.StartWorker = func(cmd *exec.Cmd) error {
		registration, err := p.store.records.ReadLifetimeRegistrationContext(context.Background())
		p.mu.Lock()
		if err == nil {
			p.registrationAtStart = registration
		}
		p.mu.Unlock()
		p.add("worker-start")
		return start(cmd)
	}
}

func (p *custodyContractProbe) run(t *testing.T, configure func(*guardian.Options)) *testsupport.RecordFixture {
	t.Helper()
	s := p.store
	p.cleanup = newCustodyFixtureCleanup(t, p)
	return testsupport.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline,
		s.observedRecords(p.cleanup), p.cleanup.acquireOwner(s.acquire),
		func(options *guardian.Options) {
			p.configure(options)
			if configure != nil {
				configure(options)
			}
			// Installed last so it wraps the scenario's retry barrier.
			p.cleanup.wrapFinalizationBackoff(t, options)
		})
}

func completeCustodyContractRun(t *testing.T, fixture *testsupport.RecordFixture) {
	t.Helper()
	fixture.Hello(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if err := fixture.WaitRun(t); err != nil {
		t.Fatalf("guardian with production store: %v", err)
	}
}

func readCustodyContractRegistration(t *testing.T, s custodyContractStore) (*task.LifetimeRegistrationRecord, []byte) {
	t.Helper()
	registration, err := s.records.ReadLifetimeRegistrationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(s.registrationPath)
	if err != nil {
		t.Fatal(err)
	}
	return registration, published
}

// requireCustodyWorkerBinding proves the worker record binds the digest of
// the exact published registration bytes.
func requireCustodyWorkerBinding(t *testing.T, s custodyContractStore, published []byte) {
	t.Helper()
	digest, err := s.records.LifetimeRegistrationDigestContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := s.records.ReadLifetimeWorkerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := task.ComputeSHA256(published); digest != want || worker.RegistrationSHA256 != want {
		t.Fatalf("registration digest %s, worker binding %s, want published digest %s", digest, worker.RegistrationSHA256, want)
	}
}

func requireCustodyPlacement(t *testing.T, label string, got, want *task.LifetimeCustodyPlacement) {
	t.Helper()
	if !sameCustodyPlacement(got, want) {
		t.Fatalf("%s placement = %+v, want %+v", label, got, want)
	}
}

func sameCustodyPlacement(first, second *task.LifetimeCustodyPlacement) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func requireCustodyEventOrder(t *testing.T, events []string, ordered ...string) {
	t.Helper()
	previous := -1
	for _, event := range ordered {
		index := slices.Index(events, event)
		if index <= previous {
			t.Fatalf("events %v do not contain %v in order", events, ordered)
		}
		previous = index
	}
}
