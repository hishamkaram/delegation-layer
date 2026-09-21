package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPrepareExistingCandidateUsesCatalogHistoricalHook(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := task.FixturePredicateRef()
	interpreter, err := predicate.Default().Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		Provider:     config.ProviderFixture,
		Mode:         config.ModeReadOnly,
		CanonicalCwd: workspace,
		RequestedConfig: task.TaskConfig{
			Permission: config.ModeReadOnly,
		},
	}
	profileCandidate := func(version string) commonprovider.ProfileCandidate {
		profile := PreparedProfile{
			Plan: execution.Plan{
				Executable: "/bin/true",
				Directory:  workspace,
				Predicate:  ref,
			},
			ObservedVersion: version,
			Effective: task.EffectiveConfig{
				Containment: config.ModeReadOnly,
				Approval:    "never",
				Digest:      task.ComputeSHA256([]byte(version)),
			},
		}
		return commonprovider.ProfileCandidate{
			Directory: workspace,
			Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
				return profile, nil
			},
		}
	}
	var prepareCalls, existingCalls int
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{
			ID:             config.ProviderFixture,
			SupportedModes: []string{config.ModeReadOnly},
			Runtime:        commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}},
			Discoverable:   true,
		},
		Prepare: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			prepareCalls++
			return profileCandidate("portable"), nil
		},
		PrepareExisting: func(task.TaskRecord, task.MetaRecord) (commonprovider.ProfileCandidate, error) {
			existingCalls++
			return profileCandidate("legacy"), nil
		},
		Interpreters: []predicate.Interpreter{interpreter},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := (Dependencies{
		Catalog: catalog,
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			prepareCalls++
			return profileCandidate("portable"), nil
		},
	}).normalized()
	candidate, facts, err := prepareExistingCandidateContext(context.Background(), deps, root, request, task.MetaRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if existingCalls != 1 || prepareCalls != 0 {
		t.Fatalf("historical catalog hook calls: existing=%d prepare=%d", existingCalls, prepareCalls)
	}
	if candidate.Finalize == nil || facts != nil {
		t.Fatalf("historical candidate was not reconstructed cleanly: candidate=%+v facts=%s", candidate, facts)
	}
}

type appInspectionProofFixture struct {
	store         *taskdir.Store
	operation     *inspection.Operation
	req           task.TaskRecord
	meta          task.MetaRecord
	candidate     commonprovider.ProfileCandidate
	workerPath    string
	finalizeCalls int
	projectCalls  int
}

func newAppInspectionProofFixture(t *testing.T, workerSuccess bool) *appInspectionProofFixture {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		closeAppInspectionStore(t, store)
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        strings.Repeat("7", 32),
		Provider:      "fixture:test",
		Mode:          "read-only",
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256([]byte("inspection brief")),
		BriefLength: int64(len("inspection brief")),
	}
	rawBase := t.TempDir()
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		closeAppInspectionStore(t, store)
		t.Fatal(err)
	}
	helperPath := filepath.Join(base, "inspection-helper")
	workerPath := filepath.Join(base, "inspection-worker")
	writeAppInspectionExecutable(t, helperPath, []byte("helper bytes"))
	writeAppInspectionExecutable(t, workerPath, []byte("worker bytes"))
	workerSHA, err := commonprovider.FingerprintExecutable(workerPath)
	if err != nil {
		closeAppInspectionStore(t, store)
		t.Fatal(err)
	}
	definition := commonprovider.InspectionDefinition{
		Revision:         "fixture-inspection-v1",
		Executable:       helperPath,
		ExecutableSHA256: task.ComputeSHA256([]byte("helper descriptor")),
		Arguments:        []string{"--fixed"},
		Directory:        workspace,
		Environment:      []string{"LANG=C"},
		OutputLimit:      4096,
	}
	fixture := &appInspectionProofFixture{store: store, req: request, workerPath: workerPath}
	definition.Project = func(_ []byte) (json.RawMessage, error) {
		fixture.projectCalls++
		return json.RawMessage(`{"eligible":true}`), nil
	}
	snapshot, definitionSHA, err := definition.Snapshot()
	if err != nil {
		closeAppInspectionStore(t, store)
		t.Fatal(err)
	}
	supervisor := task.SupervisorRef{
		ClientExecutable:     filepath.Join(base, "pueue"),
		ClientSHA256:         task.ComputeSHA256([]byte("pueue client")),
		ResolvedConfigSHA256: task.ComputeSHA256([]byte("pueue resolved config")),
		Endpoint:             filepath.Join(base, "pueue.sock"),
		ConfigPath:           filepath.Join(base, "pueue.yml"),
		ConfigDigest:         task.ComputeSHA256([]byte("pueue config")),
		ObservedVersion:      "4.0.4",
	}
	binding := inspection.Binding{
		DefinitionRevision: snapshot.Revision,
		DefinitionSHA256:   definitionSHA,
		HelperExecutable:   snapshot.Executable,
		HelperSHA256:       snapshot.ExecutableSHA256,
		WorkerExecutable:   workerPath,
		WorkerSHA256:       workerSHA,
		Supervisor:         supervisor,
	}
	operation, err := inspection.OpenOperation(store, request, binding, time.Unix(100, 0).UTC())
	if err != nil {
		closeAppInspectionStore(t, store)
		t.Fatal(err)
	}
	fixture.operation = operation
	profile := appInspectionPreparedProfile(request, workspace, nil)
	fixture.meta = task.MetaRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             request.RootID,
		TaskID:             request.TaskID,
		RequestedConfig:    request.RequestedConfig,
		EffectiveConfig:    profile.Effective,
		Containment:        profile.Effective.Containment,
		Approval:           profile.Effective.Approval,
		ProviderExecutable: profile.Plan.Executable,
		ProviderVersion:    profile.ObservedVersion,
		PublisherBuild:     "app-test",
		PublisherVersion:   "app-test",
		Predicate:          profile.Plan.Predicate,
		SupervisorConfig:   supervisor,
		CreatedAt:          time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
	}
	fixture.candidate = commonprovider.ProfileCandidate{
		Directory:     workspace,
		WritableRoots: nil,
		Inspection:    &snapshot,
		Finalize: func(_ json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			fixture.finalizeCalls++
			return profile, nil
		},
	}
	start, err := operation.ClaimStart(time.Unix(101, 0).UTC())
	if err != nil {
		closeAppInspectionFixture(t, operation, store)
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		closeAppInspectionFixture(t, operation, store)
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultEligible, json.RawMessage(`{"eligible":true}`), time.Unix(102, 0).UTC()); err != nil {
		closeAppInspectionFixture(t, operation, store)
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(7); err != nil {
		closeAppInspectionFixture(t, operation, store)
		t.Fatal(err)
	}
	if workerSuccess {
		if err = operation.RecordWorkerSuccess(7); err != nil {
			closeAppInspectionFixture(t, operation, store)
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func closeAppInspectionStore(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Error(err)
	}
}

func closeAppInspectionFixture(t *testing.T, operation *inspection.Operation, store *taskdir.Store) {
	t.Helper()
	if operation != nil {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	}
	if store != nil {
		closeAppInspectionStore(t, store)
	}
}

func writeAppInspectionExecutable(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func appInspectionPreparedProfile(req task.TaskRecord, directory string, writableRoots []string) commonprovider.PreparedProfile {
	return commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: "/bin/true",
			Directory:  directory,
			Predicate:  task.FixturePredicateRef(),
		},
		ObservedVersion: "fixture-v2",
		Effective: task.EffectiveConfig{
			Containment: "fixture-only",
			Approval:    "never",
			Digest:      task.ComputeSHA256([]byte("inspection-profile")),
		},
		WritableRoots: writableRoots,
	}
}

func assertAppInspectionNoOrdinaryTasks(t *testing.T, store *taskdir.Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.Root, "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("inspection path created ordinary task records: %v", entries)
	}
}

func TestInspectionFinalizerCannotExpandDirectoryOrWritableRoots(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	allowed := filepath.Join(filepath.Dir(request.CanonicalCwd), "provider-cache")
	candidate := commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: []string{allowed},
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			return appInspectionPreparedProfile(request, filepath.Join(filepath.Dir(request.CanonicalCwd), "other-workspace"), []string{allowed}), nil
		},
	}
	if profile, err := finalizeCandidate(candidate, request, nil); profile.Plan.Executable != "" || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("finalizer changed cwd: profile=%+v err=%v", profile, err)
	}

	candidate.Finalize = func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
		return appInspectionPreparedProfile(request, request.CanonicalCwd, []string{allowed, filepath.Join(filepath.Dir(request.CanonicalCwd), "expanded-cache")}), nil
	}
	if profile, err := finalizeCandidate(candidate, request, nil); profile.Plan.Executable != "" || !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("finalizer expanded writable roots: profile=%+v err=%v", profile, err)
	}
	assertAppInspectionNoOrdinaryTasks(t, store)
}

func TestStoredInspectionProofBindsFullRequestAndProfileAndWorkerSuccess(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	profile, err := prepareMatchedProfile(deps, fixture.store.Root, fixture.req, fixture.meta)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Plan.Directory != fixture.req.CanonicalCwd || fixture.finalizeCalls != 1 || fixture.projectCalls != 0 {
		t.Fatalf("valid proof crossed wrong callback boundary: profile=%+v finalizers=%d projectors=%d", profile, fixture.finalizeCalls, fixture.projectCalls)
	}
	assertAppInspectionNoOrdinaryTasks(t, fixture.store)
}

func TestStoredInspectionProofRejectsChangedBindingsBeforeFinalize(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*appInspectionProofFixture, *task.TaskRecord, *task.MetaRecord, *commonprovider.ProfileCandidate) error
	}{
		{
			name: "full request",
			mutate: func(_ *appInspectionProofFixture, request *task.TaskRecord, _ *task.MetaRecord, _ *commonprovider.ProfileCandidate) error {
				request.RequestedConfig.Model = "changed-request"
				return nil
			},
		},
		{
			name: "definition",
			mutate: func(_ *appInspectionProofFixture, _ *task.TaskRecord, _ *task.MetaRecord, candidate *commonprovider.ProfileCandidate) error {
				definition := *candidate.Inspection
				definition.Arguments = []string{"--changed"}
				candidate.Inspection = &definition
				return nil
			},
		},
		{
			name: "helper hash",
			mutate: func(_ *appInspectionProofFixture, _ *task.TaskRecord, _ *task.MetaRecord, candidate *commonprovider.ProfileCandidate) error {
				definition := *candidate.Inspection
				definition.ExecutableSHA256 = task.ComputeSHA256([]byte("changed-helper"))
				candidate.Inspection = &definition
				return nil
			},
		},
		{
			name: "worker hash",
			mutate: func(fixture *appInspectionProofFixture, _ *task.TaskRecord, _ *task.MetaRecord, _ *commonprovider.ProfileCandidate) error {
				return os.WriteFile(fixture.workerPath, []byte("changed worker bytes"), 0o700)
			},
		},
		{
			name: "supervisor binding",
			mutate: func(_ *appInspectionProofFixture, _ *task.TaskRecord, meta *task.MetaRecord, _ *commonprovider.ProfileCandidate) error {
				meta.SupervisorConfig.ConfigDigest = task.ComputeSHA256([]byte("changed-supervisor"))
				return nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAppInspectionProofFixture(t, true)
			request := fixture.req
			meta := fixture.meta
			candidate := fixture.candidate
			if err := test.mutate(fixture, &request, &meta, &candidate); err != nil {
				t.Fatal(err)
			}
			deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
				return candidate, nil
			}}
			profile, err := prepareMatchedProfile(deps, fixture.store.Root, request, meta)
			if profile.Plan.Executable != "" || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("changed %s was accepted: profile=%+v err=%v", test.name, profile, err)
			}
			if fixture.finalizeCalls != 0 || fixture.projectCalls != 0 {
				t.Fatalf("changed %s crossed finalizer/native boundary: finalizers=%d projectors=%d", test.name, fixture.finalizeCalls, fixture.projectCalls)
			}
			assertAppInspectionNoOrdinaryTasks(t, fixture.store)
		})
	}
}

func TestMissingInspectionProofRefusesBeforeNativeOrFinalize(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, false)
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	profile, err := prepareMatchedProfile(deps, fixture.store.Root, fixture.req, fixture.meta)
	if profile.Plan.Executable != "" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing proof was accepted: profile=%+v err=%v", profile, err)
	}
	if fixture.finalizeCalls != 0 || fixture.projectCalls != 0 {
		t.Fatalf("missing proof crossed callback boundary: finalizers=%d projectors=%d", fixture.finalizeCalls, fixture.projectCalls)
	}
	assertAppInspectionNoOrdinaryTasks(t, fixture.store)
}

func TestBadInspectionProofRefusesBeforeNativeOrFinalize(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	candidate := fixture.candidate
	definition := *candidate.Inspection
	definition.Revision = "refreshed-definition"
	candidate.Inspection = &definition
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return candidate, nil
	}}
	profile, err := prepareMatchedProfile(deps, fixture.store.Root, fixture.req, fixture.meta)
	if profile.Plan.Executable != "" || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("bad proof was accepted: profile=%+v err=%v", profile, err)
	}
	if fixture.finalizeCalls != 0 || fixture.projectCalls != 0 {
		t.Fatalf("bad proof crossed callback boundary: finalizers=%d projectors=%d", fixture.finalizeCalls, fixture.projectCalls)
	}
	assertAppInspectionNoOrdinaryTasks(t, fixture.store)
}

func TestOrdinarySubmissionRemainsBoundToInspectedRunner(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.req.TaskID)}
	if err := validateSubmissionRunner(Dependencies{}, td, &fixture.req, &fixture.meta, fixture.workerPath); err != nil {
		t.Fatalf("admitted worker was rejected: %v", err)
	}
	if err := validateSubmissionRunner(Dependencies{}, td, &fixture.req, &fixture.meta, fixture.candidate.Inspection.Executable); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different runner crossed binding: %v", err)
	}
	if err := os.WriteFile(fixture.workerPath, []byte("changed worker bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateSubmissionRunner(Dependencies{}, td, &fixture.req, &fixture.meta, fixture.workerPath); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed inspected runner was accepted: %v", err)
	}
}

func TestOrdinarySubmissionRemainsBoundWithoutInspectionJournal(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	meta := &task.MetaRecord{RunnerExecutable: "/tmp/recorded-runner"}
	if err := validateSubmissionRunner(Dependencies{}, td, req, meta, meta.RunnerExecutable); err != nil {
		t.Fatalf("recorded worker was rejected without inspection evidence: %v", err)
	}
	if err := validateSubmissionRunner(Dependencies{}, td, req, meta, "/tmp/other-runner"); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different worker crossed recorded binding without inspection evidence: %v", err)
	}
}

func TestInspectionFactsCompareCanonicalObjectsBeforeFinalize(t *testing.T) {
	if !inspectionFactsMatch(json.RawMessage(`{"z":2,"a":1}`), json.RawMessage(`{"a":1,"z":2}`)) {
		t.Fatal("equivalent canonical facts differed only by object field order")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`{"eligible":false}`)) {
		t.Fatal("changed inspection facts were accepted")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`not-json`)) {
		t.Fatal("malformed fresh facts were accepted")
	}
}
