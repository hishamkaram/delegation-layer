package supervisor

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type selectedControlRouting struct {
	controlRoutingFunc
	saved        func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	continuation func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	manual       func(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error)
}

type savedRoutingFixtureConfig struct {
	Shared savedRoutingFixtureShared `json:"shared"`
	Daemon savedRoutingFixtureDaemon `json:"daemon"`
}

type savedRoutingFixtureShared struct {
	snapshotSharedConfig
	Permissions uint32 `json:"unix_socket_permissions"`
}

type savedRoutingFixtureDaemon struct {
	ShellCommand []string `json:"shell_command"`
}

func newSavedControlFixture(t *testing.T) parentMigrationFixture {
	t.Helper()
	store, err := taskdir.InitStore(snapshotFixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	base := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := task.MarshalCanonical(savedRoutingFixtureConfig{Shared: savedRoutingFixtureShared{snapshotSharedConfig: snapshotSharedConfig{
		PueueDirectory: filepath.Join(base, "data"), RuntimeDirectory: filepath.Join(base, "run"), UnixSocketPath: filepath.Join(base, "socket"),
		AliasFile: filepath.Join(base, "aliases.yml"), PIDPath: filepath.Join(base, "run", "pueue.pid"), SharedSecretPath: filepath.Join(base, "data", "shared_secret"),
		DaemonCert: filepath.Join(base, "data", "certs", "daemon.cert"), DaemonKey: filepath.Join(base, "data", "certs", "daemon.key"),
	}, Permissions: 0o700}, Daemon: savedRoutingFixtureDaemon{ShellCommand: []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(store.Root), config, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := parentMigrationFixture{store: store, status: filepath.Join(store.Root, "status.json")}
	if err = os.WriteFile(fixture.status, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.options = pueue.Options{Environment: []string{"HOME=" + store.Root, "REFERENCED_STATUS=" + fixture.status}, PrivateMaintenanceEnvironment: []string{}, DaemonStartGuard: referencedMigrationTestGuard{}, DaemonWaitOwner: referencedMigrationTestWait{}, WaitForPendingClose: true}
	fixture.target = parentMigrationPair(t, fixture, "routing")
	return fixture
}

func (r selectedControlRouting) RecoverSaved(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	if r.saved == nil {
		return infrastructure.Disposition{}, errors.New("unexpected saved routing")
	}
	return r.saved(ctx, store, selected, source)
}

func (r selectedControlRouting) PrepareContinuation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	if r.continuation == nil {
		return infrastructure.Disposition{}, errors.New("unexpected continuation routing")
	}
	return r.continuation(ctx, store, selected, source)
}

func (r selectedControlRouting) MigrateOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	if r.manual == nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, errors.New("unexpected manual routing")
	}
	return r.manual(ctx, store, selected)
}

// These fixtures exercise public control decisions and durable target effects.
// Their finite command scripts confer no native resident or daemon authority.
func TestControlRuntimeSavedReplyRequiresFreshReadyProof(t *testing.T) {
	for _, contactError := range []bool{false, true} {
		t.Run(map[bool]string{false: "unready reply", true: "unknown contact"}[contactError], func(t *testing.T) {
			exerciseSavedControlReply(t, contactError)
		})
	}
}

func exerciseSavedControlReply(t *testing.T, contactError bool) {
	t.Helper()
	fixture := newSavedControlFixture(t)
	source, err := NewSavedBindingSource(fixture.store.RootID, infrastructure.SavedBindingTaskSubmission, strings.Repeat("a", 32), "", fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("unknown routed outcome")
	calls := 0
	routing := selectedControlRouting{saved: func(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || observed != source || selected.Root != fixture.store.Root || selected.RootID != fixture.store.RootID {
			t.Fatal("saved routing changed canonical target or subject")
		}
		if _, finite := ctx.Deadline(); !finite {
			t.Fatal("saved routing lost finite control context")
		}
		if contactError {
			return infrastructure.Disposition{}, unknown
		}
		return infrastructure.Disposition{Kind: infrastructure.DispositionWarmReuse}, nil
	}}
	before := parentMigrationTree(t, fixture.store.Root)
	client, err := (ControlRuntime{Options: fixture.options, Routing: routing}).RecoverSaved(t.Context(), fixture.store, fixture.target, source)
	if client != nil {
		t.Fatal(errors.Join(errors.New("unready reply produced a client"), client.CloseAfterPending(err)))
	}
	want := ErrInfrastructureAuthorityUnavailable
	if contactError {
		want = unknown
	}
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("saved reply replaced proof or retried contact: calls=%d err=%v", calls, err)
	}
	if !maps.Equal(before, parentMigrationTree(t, fixture.store.Root)) {
		t.Fatal("saved cold observation/contact entered local recovery")
	}
}

func TestControlRuntimeManualPreservesReportAndSelectedContext(t *testing.T) {
	store := newGenerationStore(t)
	report := infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationNotInitialized, Legacy: infrastructure.ManualMigrationLegacyNotPresent, Active: infrastructure.ManualMigrationActiveNone}
	calls := 0
	routing := selectedControlRouting{manual: func(ctx context.Context, actual *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
		calls++
		if actual != store || selected.Root != store.Root || selected.RootID != store.RootID {
			t.Fatal("manual routing replaced actual target")
		}
		origin, found, err := infrastructure.ReadOrigin(selected.ControlEnvironment)
		if err != nil || !found || origin.RootID() != store.RootID {
			t.Fatal("manual routing lost selected target overlay")
		}
		if _, finite := ctx.Deadline(); !finite {
			t.Fatal("manual routing lost finite deadline")
		}
		return infrastructure.Disposition{}, report, nil
	}}
	before := observationTree(t, store.Root)
	disposition, observed, err := (ControlRuntime{Routing: routing}).MigrateOwn(t.Context(), store)
	if err != nil || disposition != (infrastructure.Disposition{}) || observed != report || calls != 1 {
		t.Fatalf("valid not-initialized race lost report: calls=%d err=%v", calls, err)
	}
	if !slices.Equal(before, observationTree(t, store.Root)) {
		t.Fatal("manual routing performed local initialization")
	}
}

type routedStoreRetentionError struct {
	store *taskdir.Store
	cause error
}

func (e *routedStoreRetentionError) Error() string                 { return e.cause.Error() }
func (e *routedStoreRetentionError) Unwrap() error                 { return e.cause }
func (e *routedStoreRetentionError) RetainedStore() *taskdir.Store { return e.store }

func TestControlRuntimeOpenSavedRetainsCleanupStore(t *testing.T) {
	fixture := newSavedControlFixture(t)
	source, err := NewSavedBindingSource(fixture.store.RootID, infrastructure.SavedBindingTaskMetadata, strings.Repeat("a", 32), "", fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	retained := &routedStoreRetentionError{store: fixture.store, cause: errors.New("owned startup cleanup unresolved")}
	runtime := ControlRuntime{Options: fixture.options, OpenStore: func(context.Context, string) (*taskdir.Store, error) { return fixture.store, nil }, Routing: selectedControlRouting{saved: func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		return infrastructure.Disposition{}, retained
	}}}
	client, err := runtime.OpenSaved(t.Context(), fixture.store.Root, fixture.target, source)
	if client != nil || !errors.Is(err, retained) || !CleanupRetainsStore(err, fixture.store) {
		t.Fatalf("routed cleanup owner lost: %v", err)
	}
	if _, pinErr := fixture.store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("routed cleanup prematurely closed borrowed store: %v", pinErr)
	}
	retained.store = nil
	if CleanupRetainsStore(err, fixture.store) {
		t.Fatal("settled cleanup still retains store")
	}
}

func TestRoutedCleanupKeepsExactOwnedRootAdmission(t *testing.T) {
	store := newGenerationStore(t)
	foreign := newGenerationStore(t)
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	t.Cleanup(func() {
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	child := &routedStoreRetentionError{store: store, cause: errors.New("retained startup cleanup")}
	cause := errors.Join(errors.New("independent storage fault"), child)
	retained := ReleaseOwnedRootAdmission(lease, cause)
	var parent *RootAdmissionCleanupError
	if !errors.As(retained, &parent) || parent.Lease() != lease || !CleanupRetainsStore(retained, store) {
		t.Fatal("routed child lost exact parent admission owner")
	}
	if CleanupRetainsStore(retained, foreign) || !lease.RetainsStore(store) {
		t.Fatal("retention changed or invented borrowed Store identity")
	}
	child.store = nil
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if CleanupRetainsStore(retained, store) {
		t.Fatal("settled cleanup retained historical Store")
	}
}

func TestSubmittedQueueRepairRoutesCommittedSubmission(t *testing.T) {
	fixture := newSavedControlFixture(t)
	request := supervisorfixture.NewInspectionRequest(t, fixture.store)
	digest := task.ComputeSHA256([]byte("routing fixture"))
	meta := task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID,
		RequestedConfig: request.RequestedConfig, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: fixture.target.ClientExecutable, ProviderVersion: "fixture-v2",
		PublisherBuild: "routing-fixture", PublisherVersion: "routing-fixture", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: fixture.target, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := fixture.store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission := commitRoutedTaskSubmission(t, td, fixture.target)
	refused := errors.New("fixture refuses after submitted-source selection")
	calls := 0
	routing := selectedControlRouting{saved: func(_ context.Context, store *taskdir.Store, _ infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		if store != fixture.store || source.Kind != infrastructure.SavedBindingTaskSubmission || source.OwnerID != submission.TaskID || source.RootID != submission.RootID {
			t.Fatal("submitted queue repair selected a preparation slot")
		}
		if sourceErr := source.ValidateBinding(submission.Supervisor); sourceErr != nil {
			t.Fatal(sourceErr)
		}
		return infrastructure.Disposition{}, refused
	}}
	err = RepairQueueForSubmittedTask(t.Context(), fixture.store, submission, &meta, AdmissionOptions{Control: fixture.options, Routing: routing})
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("submitted queue repair bypassed exact recovery source: calls=%d err=%v", calls, err)
	}
}

func commitRoutedTaskSubmission(t *testing.T, td *taskdir.TaskDir, binding task.SupervisorRef) *task.SubmitRecord {
	t.Helper()
	permit, err := td.PrepareSubmission(binding)
	if permit != nil {
		t.Cleanup(func() {
			if closeErr := permit.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	return submission
}
