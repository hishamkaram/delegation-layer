package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestExternalInspectionHandoffPublicConsumersPreserveActiveWork(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	external, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)
	operation := newInspectionPollTestOperation(t, store, external)
	startExternalHandoffInspection(t, operation)
	requestBefore := operation.Request()
	requestPath := filepath.Join(store.Root, "inspections", requestBefore.TaskID, "request.json")
	requestBytes := readSupervisorProbeTestFile(t, requestPath)
	privateBase := filepath.Dir(current.Binding().ConfigPath)
	identityPath := filepath.Join(privateBase, "daemon.identity.json")
	identityBytes := readSupervisorProbeTestFile(t, identityPath)
	var destructiveControl atomic.Bool
	deps.SupervisorOptions.Observer = func(event pueue.CommandEvent) {
		for _, command := range []string{"shutdown", "kill", "remove", "restart", "edit"} {
			if slices.Contains(event.Argv, command) {
				destructiveControl.Store(true)
			}
		}
	}

	before := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "status"}, deps)
	assertHandoffSupervisorResult(t, before, "external", "run_supervisor_migrate")
	if before.response.SupervisorControl.ActiveWork != "unknown" || before.response.SupervisorControl.LegacyReachability != "not_managed" {
		t.Fatalf("external status claimed an observation: %+v", before.response.SupervisorControl)
	}
	migrated := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "migrate"}, deps)
	assertHandoffSupervisorResult(t, migrated, "migrated", "retry_dispatch")
	if migrated.response.SupervisorControl.ActiveWork != "unknown" || migrated.response.SupervisorControl.LegacyReachability != "not_managed" {
		t.Fatalf("routing handoff claimed empty queues: %+v", migrated.response.SupervisorControl)
	}
	after := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "status"}, deps)
	assertHandoffSupervisorResult(t, after, "migrated", "none")
	assertExternalHandoffHead(t, store, legacy, current.Binding())
	assertSupervisorProbeTestFile(t, requestPath, requestBytes)
	assertSupervisorProbeTestFile(t, identityPath, identityBytes)
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
	assertSupervisorProbeTestDaemonStartCount(t, privateBase, 1)
	if destructiveControl.Load() {
		t.Fatal("routing handoff invoked a destructive supervisor control")
	}
	finishExternalHandoffInspection(t, store, requestBefore)
}

func TestExternalInspectionHandoffRetryKeepsPublishedTarget(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)
	successor := privateInspectionGroup(store.RootID, current.Binding())
	// Model a crash after the private owner and pending edge are durable, but
	// before the inspection journal commits its new head.
	if err := inspection.PrepareGroupMigration(t.Context(), store, legacy, successor); err != nil {
		t.Fatal(err)
	}
	staged, _ := stagePublishedGenerationSuccessor(t, store.Root, current.Binding(), current.Binding().ClientExecutable, current.Binding().DaemonExecutable)
	if staged == current.Binding() {
		t.Fatal("fixture did not stage a different installation")
	}
	result := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "migrate"}, deps)
	assertHandoffSupervisorResult(t, result, "migrated", "retry_dispatch")
	assertExternalHandoffHead(t, store, legacy, current.Binding())
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found || published != current.Binding() {
		t.Fatalf("handoff selected staged bytes over its published owner: found=%t binding=%+v err=%v", found, published, err)
	}
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
	assertSupervisorProbeTestDaemonStartCount(t, filepath.Dir(current.Binding().ConfigPath), 1)
}

func TestExternalInspectionHandoffCreatesPrivateSupervisorForExternalOnlyRoot(t *testing.T) {
	templateStore, deps, template := newSupervisorProbeTestFixture(t)
	store := newBareInspectionStore(t)
	_, legacy, externalLog := newExternalInspectionHandoffFixture(t, store)
	externalCalls := readSupervisorProbeTestFile(t, externalLog)
	bundle := canonicalAppTestTempDir(t)
	for _, source := range []string{template.Binding().ClientExecutable, template.Binding().DaemonExecutable} {
		data := readSupervisorProbeTestFile(t, source)
		data = []byte(strings.ReplaceAll(string(data), templateStore.RootID, store.RootID))
		writeRunnerFixture(t, filepath.Join(bundle, filepath.Base(source)), string(data))
	}
	deps.InitialSupervisorExecutable = filepath.Join(bundle, "pueue")
	privateBase := filepath.Join(store.Root, ".supervisor")
	if _, err := os.Stat(privateBase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external-only fixture already contains private state: %v", err)
	}
	before := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "status"}, deps)
	assertHandoffSupervisorResult(t, before, "external", "run_supervisor_migrate")
	if _, err := os.Stat(privateBase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created private state: %v", err)
	}
	migrated := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "migrate"}, deps)
	assertHandoffSupervisorResult(t, migrated, "migrated", "retry_dispatch")
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		t.Fatalf("external-only handoff did not publish a private owner: found=%t err=%v", found, err)
	}
	assertExternalHandoffHead(t, store, legacy, published)
	assertSupervisorProbeTestDaemonStartCount(t, privateBase, 1)
	assertSupervisorProbeTestFile(t, externalLog, externalCalls)
	complete, err := pueue.PrivateSupervisorMigrationComplete(store.Root, published)
	if err != nil || !complete {
		t.Fatalf("new private owner is not complete: complete=%t err=%v", complete, err)
	}
}

func TestExternalInspectionHandoffPreservesOrdinaryTasksAndLateAdmission(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	external, legacy, _ := newExternalInspectionHandoffFixture(t, store)
	operation := newInspectionPollTestOperation(t, store, external)
	startExternalHandoffInspection(t, operation)
	request := operation.Request().Task
	request.TaskID = strings.Repeat("b", 32)
	created := createExternalHandoffTask(t, store, request, external.Binding())
	metaPath := filepath.Join(created.Dir, "meta.json")
	metaBefore := readSupervisorProbeTestFile(t, metaPath)
	result := supervisorCommand(Arguments{Root: store.Root, SupervisorAction: "migrate"}, deps)
	assertHandoffSupervisorResult(t, result, "migrated", "retry_dispatch")
	assertExternalHandoffHead(t, store, legacy, current.Binding())

	finishExternalHandoffTask(t, created, external.Binding())
	finishExternalHandoffInspection(t, store, operation.Request())
	// The old inspection can finish admission after the routing head commits.
	// Its new ordinary record must retain the inspection's external binding.
	late := createExternalHandoffTask(t, store, operation.Request().Task, external.Binding())
	finishExternalHandoffTask(t, late, external.Binding())
	assertSupervisorProbeTestFile(t, metaPath, metaBefore)
	rebound, err := rebindContinuationSupervisor(t.Context(), store.Root, external.Binding(), deps)
	if err != nil || rebound != external.Binding() {
		t.Fatalf("historical continuation changed supervisors: binding=%+v err=%v", rebound, err)
	}
}

func TestExternalInspectionHandoffAdoptsLegacyPrivateOwner(t *testing.T) {
	store, _, current := newSupervisorProbeTestFixture(t)
	_, legacy, _ := newExternalInspectionHandoffFixture(t, store)
	base := filepath.Dir(current.Binding().ConfigPath)
	identity := historicalPrivateDaemonIdentity{Executable: current.Binding().DaemonExecutable, SHA256: current.Binding().DaemonSHA256}
	identityBytes, err := task.MarshalCanonical(identity)
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(base, "daemon.identity.json")
	if err = os.WriteFile(identityPath, identityBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(base, "supervisor.identity.json")); err != nil {
		t.Fatal(err)
	}
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		return handoffExternalInspectionGroup(ctx, store, current, legacy, privateInspectionGroup(store.RootID, current.Binding()))
	})
	if err != nil {
		t.Fatalf("legacy private owner was not adopted: %v", err)
	}
	assertExternalHandoffHead(t, store, legacy, current.Binding())
	assertSupervisorProbeTestFile(t, identityPath, identityBytes)
	complete, err := pueue.PrivateSupervisorMigrationComplete(store.Root, current.Binding())
	if err != nil || !complete {
		t.Fatalf("handoff committed before private lifecycle completion: complete=%t err=%v", complete, err)
	}
	assertSupervisorProbeTestDaemonStartCount(t, base, 1)
}

func TestExternalInspectionHandoffRejectsWrongSuccessorBeforePublication(t *testing.T) {
	store, _, current := newSupervisorProbeTestFixture(t)
	_, legacy, _ := newExternalInspectionHandoffFixture(t, store)
	successor := privateInspectionGroup(store.RootID, current.Binding())
	successor.Supervisor.ClientSHA256 = task.ComputeSHA256([]byte("not the selected client"))
	err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		return handoffExternalInspectionGroup(ctx, store, current, legacy, successor)
	})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unbound successor error = %v", err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), store)
	if err != nil || migrated || head != legacy {
		t.Fatalf("unbound successor changed the head: migrated=%t head=%+v err=%v", migrated, head, err)
	}
	for _, name := range []string{"migration.json", "migration-complete.json"} {
		if _, err = os.Stat(filepath.Join(store.Root, "inspection-group", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unbound successor published %s: %v", name, err)
		}
	}
}

func TestExternalInspectionHandoffPreservesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, canceled := range []context.Context{nil, ctx} {
		if _, err := prepareInspectionHandoffSupervisor(canceled, "", Dependencies{}, pueue.Options{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("prepare error = %v", err)
		}
		if err := handoffExternalInspectionGroup(canceled, nil, nil, inspection.GroupRequestRecord{}, inspection.GroupRequestRecord{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("handoff error = %v", err)
		}
	}
}

func newExternalInspectionHandoffFixture(t *testing.T, store *taskdir.Store) (*pueue.Client, inspection.GroupRequestRecord, string) {
	t.Helper()
	client, logPath, _ := newUncertainGroupSupervisorWithConfigAndTimeout(t, store.RootID, "", 10*time.Second)
	settleSupervisorProbeTestClient(t, client, nil)
	group, err := inspection.OpenGroup(store, client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	request := group.Request()
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	return client, request, logPath
}

func assertHandoffSupervisorResult(t *testing.T, result commandResult, state, action string) {
	t.Helper()
	if result.code != 0 || result.err != nil || result.response.SupervisorControl == nil {
		t.Fatalf("supervisor command failed: code=%d err=%v response=%+v", result.code, result.err, result.response)
	}
	if got := result.response.SupervisorControl; got.State != state || got.NextAction != action {
		t.Fatalf("supervisor control = %+v, want state=%s action=%s", got, state, action)
	}
}

func assertExternalHandoffHead(t *testing.T, store *taskdir.Store, legacy inspection.GroupRequestRecord, binding task.SupervisorRef) {
	t.Helper()
	head, migrated, err := inspection.ReadGroupHead(t.Context(), store)
	if err != nil || !migrated || head != privateInspectionGroup(store.RootID, binding) {
		t.Fatalf("handoff head = %+v migrated=%t err=%v", head, migrated, err)
	}
	seed, err := inspection.ReadGroupRequest(t.Context(), store)
	if err != nil || seed != legacy {
		t.Fatalf("handoff rewrote the historical root: seed=%+v err=%v", seed, err)
	}
	for _, name := range []string{"migration.json", "legacy-supervisor.json", "migration-history.json"} {
		if _, err = os.Stat(filepath.Join(store.Root, ".supervisor", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("journal routing handoff created private migration state %s: %v", name, err)
		}
	}
}

func startExternalHandoffInspection(t *testing.T, operation *inspection.Operation) {
	t.Helper()
	now := time.Now().UTC()
	submission, err := operation.ClaimSubmission(now)
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(now)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = operation.ReadWorkerResultContext(t.Context()); !task.RecordAbsent(err) {
		t.Fatalf("fixture inspection is not unfinished: %v", err)
	}
}

func finishExternalHandoffInspection(t *testing.T, store *taskdir.Store, request inspection.RequestRecord) {
	t.Helper()
	historical, err := inspection.OpenGroupHistorical(t.Context(), store, request.Binding.Supervisor)
	if err != nil {
		t.Fatalf("historical group became unusable: %v", err)
	}
	if err = historical.Close(); err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.LoadOperationContext(t.Context(), store, request.TaskID)
	if err != nil {
		t.Fatalf("active external inspection became unusable: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if !reflect.DeepEqual(operation.Request(), request) {
		t.Fatal("handoff replaced an active inspection's immutable request")
	}
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || !succeeded || result == nil || result.Reason != inspection.ResultEligible {
		t.Fatalf("historical external completion was rejected: result=%+v succeeded=%t err=%v", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if err != nil || evaluation.decision != inspectionAdmissionEligible || string(evaluation.facts) != "{\"eligible\":true}\n" {
		t.Fatalf("historical admission replay failed: decision=%d facts=%s err=%v", evaluation.decision, evaluation.facts, err)
	}
}

func createExternalHandoffTask(t *testing.T, store *taskdir.Store, request task.TaskRecord, supervisor task.SupervisorRef) *taskdir.TaskDir {
	t.Helper()
	digest := task.ComputeSHA256([]byte("handoff-effective"))
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: request.TaskID,
		RequestedConfig: request.RequestedConfig,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment:     "fixture-only", Approval: "never", ProviderExecutable: "/usr/bin/true", ProviderVersion: "fixture-v1",
		RunnerOwnership: task.RunnerOwnershipManaged, PublisherBuild: "handoff-test", PublisherVersion: "handoff-test",
		Environment: []string{"LANG=C"}, EnvironmentRecorded: true, Predicate: task.FixturePredicateRef(),
		SupervisorConfig: supervisor, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission, err := td.PrepareSubmission(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := submission.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = submission.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = submission.Release(); err != nil {
		t.Fatal(err)
	}
	return td
}

func finishExternalHandoffTask(t *testing.T, td *taskdir.TaskDir, supervisor task.SupervisorRef) {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil || meta == nil || meta.SupervisorConfig != supervisor {
		t.Fatalf("ordinary task lost its external binding: meta=%+v err=%v", meta, err)
	}
	if _, err = td.ReadSubmission(); err != nil {
		t.Fatal(err)
	}
	sealExternalHandoffTask(t, td)
	outcome, cleanupErr, err := td.Collect(task.FixturePredicateRef())
	if err != nil || cleanupErr != nil || outcome == nil {
		t.Fatalf("historical ordinary task could not publish: outcome=%+v cleanup=%v err=%v", outcome, cleanupErr, err)
	}
	inspected, err := td.Inspect()
	if err != nil || inspected == nil || inspected.Publication != task.PublicationCommitted {
		t.Fatalf("historical ordinary publication not readable: inspection=%+v err=%v", inspected, err)
	}
}

func sealExternalHandoffTask(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := start.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(1); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
}
