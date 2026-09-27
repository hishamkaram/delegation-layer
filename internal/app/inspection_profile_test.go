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
				Executable: "/usr/bin/true",
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

func TestPrepareMatchedProfileForRunnerRejectsChangedCustomInspectionWorker(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	if _, err := prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, fixture.workerPath, ""); err != nil {
		t.Fatalf("unchanged custom inspection worker was rejected: %v", err)
	}

	writeAppInspectionExecutable(t, fixture.workerPath, []byte("changed custom worker"))
	if _, err := prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, fixture.workerPath, ""); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed custom inspection worker crossed provider admission: %v", err)
	}
}

func TestPrepareMatchedProfileForRunnerAcceptsStateRunnerAfterLegacyInspection(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, true, "")
	stateRunner, err := installStateRunner(fixture.store.Root, fixture.workerPath)
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	if _, err = prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, stateRunner, ""); err != nil {
		t.Fatalf("verified state-root runner was rejected for a legacy inspection: %v", err)
	}
}

func TestPrepareMatchedProfileForRunnerRejectsStateRunnerSuccessorForCustomInspection(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	fixture.meta.RunnerExecutable = fixture.workerPath
	fixture.meta.RunnerOwnership = task.RunnerOwnershipCustom
	stateRunner, err := installStateRunner(fixture.store.Root, fixture.workerPath)
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	if _, err = prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, stateRunner, task.RunnerOwnershipCustom); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("state-root successor crossed custom inspection binding: %v", err)
	}
}

func TestPrepareMatchedProfileForRunnerRechecksManagedOrdinaryRunnerDigest(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	runner, err := installStateRunner(fixture.store.Root, fixture.workerPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := appInspectionPreparedProfile(fixture.req, fixture.req.CanonicalCwd, nil)
	var finalizeCalls int
	candidate := commonprovider.ProfileCandidate{
		Directory: fixture.req.CanonicalCwd,
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			finalizeCalls++
			return profile, nil
		},
	}
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return candidate, nil
	}}
	meta := fixture.meta
	meta.RunnerExecutable = runner
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	prepare := func() error {
		_, prepareErr := prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, meta, fixture.store, runner, task.RunnerOwnershipManaged)
		return prepareErr
	}
	if err = prepare(); err != nil {
		t.Fatalf("unchanged managed runner was rejected: %v", err)
	}
	if err = os.WriteFile(runner, []byte("changed managed runner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = prepare(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed content-addressed managed runner crossed provider boundary: %v", err)
	}
	if finalizeCalls != 1 {
		t.Fatalf("profile finalized after runner integrity failure: calls=%d, want 1", finalizeCalls)
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
	return newAppInspectionProofFixtureWithOwnership(t, workerSuccess, task.RunnerOwnershipCustom)
}

func newAppInspectionProofFixtureWithOwnership(t *testing.T, workerSuccess bool, runnerOwnership string) *appInspectionProofFixture {
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
		RunnerOwnership:    runnerOwnership,
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
			Executable: "/usr/bin/true",
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

func TestStoredInspectionProofSurvivesWorkerUpgrade(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{
			name: "worker replaced in place",
			mutate: func(path string) error {
				return os.WriteFile(path, []byte("upgraded worker bytes"), 0o700)
			},
		},
		{
			name:   "old package removed",
			mutate: os.Remove,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAppInspectionProofFixture(t, true)
			if err := test.mutate(fixture.workerPath); err != nil {
				t.Fatal(err)
			}
			deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
				return fixture.candidate, nil
			}}
			profile, err := prepareMatchedProfile(deps, fixture.store.Root, fixture.req, fixture.meta)
			if err != nil {
				t.Fatalf("completed inspection proof did not survive worker upgrade: %v", err)
			}
			if profile.Plan.Directory != fixture.req.CanonicalCwd || fixture.finalizeCalls != 1 || fixture.projectCalls != 0 {
				t.Fatalf("upgraded worker changed proof use: profile=%+v finalizers=%d projectors=%d", profile, fixture.finalizeCalls, fixture.projectCalls)
			}
			assertAppInspectionNoOrdinaryTasks(t, fixture.store)
		})
	}
}

func TestStoredInspectionProofAcceptsSupervisorInstallProvenanceUpgrade(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	meta := fixture.meta
	meta.SupervisorConfig.ClientExecutable = "/new/install/bin/pueue"
	meta.SupervisorConfig.ClientSHA256 = task.ComputeSHA256([]byte("new pueue client"))
	meta.SupervisorConfig.DaemonExecutable = "/new/install/libexec/pueued"
	meta.SupervisorConfig.DaemonSHA256 = task.ComputeSHA256([]byte("new pueued daemon"))
	meta.SupervisorConfig.ObservedVersion = "pueue 4.1.0"
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		return fixture.candidate, nil
	}}
	profile, err := prepareMatchedProfile(deps, fixture.store.Root, fixture.req, meta)
	if err != nil {
		t.Fatalf("compatible supervisor install upgrade rejected completed proof: %v", err)
	}
	if profile.Plan.Directory != fixture.req.CanonicalCwd || fixture.finalizeCalls != 1 || fixture.projectCalls != 0 {
		t.Fatalf("compatible supervisor install changed proof use: profile=%+v finalizers=%d projectors=%d", profile, fixture.finalizeCalls, fixture.projectCalls)
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

func TestManagedRetryAcceptsCurrentRunnerAfterInspectionWorkerUpgrade(t *testing.T) {
	fixture := newAppInspectionProofFixture(t, true)
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.req.TaskID)}
	meta := fixture.meta
	meta.RunnerExecutable = filepath.Join(t.TempDir(), stateRunnerName)
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	source := filepath.Join(t.TempDir(), stateRunnerName)
	writeRunnerFixture(t, source, "current managed runner")
	currentRunner, err := installStateRunner(fixture.store.Root, source)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateManagedRetryRunner(td, &meta, currentRunner); err != nil {
		t.Fatalf("managed retry rejected the verified current runner: %v", err)
	}
	if err = validateSubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("strict submission accepted a runner that differs from the immutable historical binding: %v", err)
	}
	custom := filepath.Join(t.TempDir(), "custom-runner")
	writeRunnerFixture(t, custom, "custom runner")
	if err = validateManagedRetryRunner(td, &meta, custom); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("managed retry accepted an unverified custom executable: %v", err)
	}
}

func TestManagedRetryRejectsChangedInspectedWorkerEvenWithProof(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, true, task.RunnerOwnershipManaged)
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.req.TaskID)}
	meta := fixture.meta
	meta.RunnerExecutable = fixture.workerPath
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	if err := os.WriteFile(fixture.workerPath, []byte("replaced managed worker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateInspectionSubmissionRunner(fixture.operation, fixture.store, td, &fixture.req, &meta, fixture.workerPath); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("retry accepted changed bytes at the inspected worker path: %v", err)
	}
}

func TestManagedRetryProofAcceptsVerifiedStateRootSuccessor(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, true, task.RunnerOwnershipManaged)
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.req.TaskID)}
	source := filepath.Join(t.TempDir(), stateRunnerName)
	writeRunnerFixture(t, source, "current managed inspection worker")
	runner, err := installStateRunner(fixture.store.Root, source)
	if err != nil {
		t.Fatal(err)
	}
	meta := fixture.meta
	meta.RunnerExecutable = runner
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	if err = validateManagedRetrySubmissionRunner(Dependencies{}, td, &fixture.req, &meta, runner); err != nil {
		t.Fatalf("proof-only managed retry rejected a verified state-root successor: %v", err)
	}
}

func TestManagedRetrySubmissionRequiresInspectionUpgradeAuthority(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, false, task.RunnerOwnershipManaged)
	oldSource := filepath.Join(t.TempDir(), stateRunnerName)
	writeRunnerFixture(t, oldSource, "old managed runner")
	oldRunner, err := installStateRunner(fixture.store.Root, oldSource)
	if err != nil {
		t.Fatal(err)
	}
	currentSource := filepath.Join(t.TempDir(), stateRunnerName)
	writeRunnerFixture(t, currentSource, "current managed runner")
	currentRunner, err := installStateRunner(fixture.store.Root, currentSource)
	if err != nil {
		t.Fatal(err)
	}
	meta := fixture.meta
	meta.RunnerExecutable = oldRunner
	td, err := fixture.store.CreateTask(fixture.req.TaskID, &fixture.req, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = validateManagedRetrySubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("legacy retry without ownership metadata crossed inspection binding: %v", err)
	}
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	if err = validateManagedRetrySubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("managed retry crossed inspection binding without authorization: %v", err)
	}
	if err = fixture.operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = validateManagedRetrySubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); err != nil {
		t.Fatalf("authorized managed retry was rejected: %v", err)
	}
}

func TestSubmissionAcceptsAuthorizedManagedInspectionWorkerSuccessor(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, false, task.RunnerOwnershipManaged)
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.req.TaskID)}
	meta := fixture.meta
	source := filepath.Join(t.TempDir(), stateRunnerName)
	writeRunnerFixture(t, source, "current managed inspection worker")
	currentRunner, err := installStateRunner(fixture.store.Root, source)
	if err != nil {
		t.Fatal(err)
	}
	meta.RunnerExecutable = currentRunner
	meta.RunnerOwnership = task.RunnerOwnershipManaged
	if err = validateSubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unapproved inspection worker successor was accepted: %v", err)
	}
	if err = fixture.operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = validateSubmissionRunner(Dependencies{}, td, &fixture.req, &meta, currentRunner); err != nil {
		t.Fatalf("authorized managed inspection worker successor was rejected: %v", err)
	}

	customMeta := meta
	customMeta.RunnerOwnership = task.RunnerOwnershipCustom
	if err = validateSubmissionRunner(Dependencies{}, td, &fixture.req, &customMeta, currentRunner); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("inspection migration authority bypassed custom task ownership: %v", err)
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
