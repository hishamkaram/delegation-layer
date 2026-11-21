package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func scopedWorkerCatalog(t *testing.T, path, marker string) commonprovider.Catalog {
	t.Helper()
	base := modelsFailureCatalog(t, path, nil)
	registration, err := base.Lookup(config.ProviderFixture)
	if err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(t.TempDir(), "model-fixture")
	writeAppTestFile(t, provider, []byte("#!/bin/sh\nprintf started > \"$MODEL_START_MARKER\"\nprintf '%s\\n' '--fixture'\n"))
	if err = os.Chmod(provider, 0o700); err != nil {
		t.Fatal(err)
	}
	registration.Prepare = func(request task.TaskRecord, _ []string) (commonprovider.ProfileCandidate, error) {
		return commonprovider.ProfileCandidate{Directory: request.CanonicalCwd, Inspection: &commonprovider.InspectionDefinition{
			Revision: commonprovider.RuntimeInspectionRevision, Executable: provider,
			Directory: request.CanonicalCwd, Environment: []string{"MODEL_START_MARKER=" + marker}, OutputLimit: commonprovider.MaxInspectionOutput,
		}}, nil
	}
	registration.Models = func() commonprovider.ModelDiscoveryDefinition {
		return commonprovider.ModelDiscoveryDefinition{
			Arguments: []string{"models"}, Source: "fixture", Capability: commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}},
			Project: func([]byte, []byte, []byte, bool) (commonprovider.ModelCatalog, error) {
				return commonprovider.ModelCatalog{Status: "available", ReasonCode: "ok", Complete: true, Source: "fixture", Models: []commonprovider.ModelInfo{{ID: "fixture/model", Name: "Fixture model"}}}, nil
			},
		}
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestScopedModelJournalRetainsBindingAndWorkerRefusesMissingLifetime(t *testing.T) {
	startup := newInspectionWorkerStartupFixture(t, task.RunnerOwnershipManaged)
	f := newScopedModelFixture(t, startup.store)
	a, err := ParseArguments(f.arguments())
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareScopedModels(t.Context(), &a, f.store.Root, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	request, err := admission.BuildRequest(f.store.RootID, "11111111111111111111111111111111", config.ProviderFixture, a.Cwd,
		task.TaskConfig{Permission: config.ModeReadOnly, Budget: inspection.ModelDiscoveryTimeout.String()}, []byte("model discovery"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.ConnectorContext = a.Models.binding.Clone()
	marker := filepath.Join(t.TempDir(), "unexpected-start")
	deps := Dependencies{Catalog: scopedWorkerCatalog(t, f.path, marker), Environment: []string{}}
	candidate, err := prepareModelsCandidate(deps, f.store.Root, request)
	if err != nil {
		t.Fatal(err)
	}
	definition, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding := startup.operation.Request().Binding
	binding.DefinitionRevision, binding.DefinitionSHA256 = definition.Revision, digest
	binding.HelperExecutable, binding.HelperSHA256, binding.WorkerSHA256 = definition.Executable, "", ""
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.Environment, binding.EnvironmentRecorded = definition.Environment, true
	operation, err := inspection.OpenOperation(f.store, request, binding, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	rawPath := filepath.Join(f.store.Root, "inspections", request.TaskID, "request.json")
	before, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	assertScopedModelJournalBinding(t, operation, request.ConnectorContext)
	record := operation.Request()
	facts, workErr, stopErr := runSupervisedInspection(operation, nil, record.SupervisorIdentity(), nil, deps, f.store, record, nil)
	if workErr == nil || stopErr != nil || len(facts) != 0 {
		t.Fatalf("missing project lifetime produced facts: work=%v stop=%v facts=%s", workErr, stopErr, facts)
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scoped inspection started a provider without a project lifetime: %v", err)
	}
	after, err := os.ReadFile(rawPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("worker changed the canonical request: %v", err)
	}
	assertUnguardedModelWorkerGolden(t, f, deps, request, binding)
	assertNoModelTasks(t, f.store)
}

func assertScopedModelJournalBinding(t *testing.T, operation *inspection.Operation, expected *task.ConnectorContext) {
	t.Helper()
	record := operation.Request()
	if !reflect.DeepEqual(record.Task.ConnectorContext, expected) || record.Task.Origin != nil {
		t.Fatalf("journal lost path approval: %+v", record.Task)
	}
	record.Task.ConnectorContext.WorkspaceConstraint.RelativeProject = "mutated reader copy"
	if operation.Request().Task.ConnectorContext.WorkspaceConstraint.RelativeProject != "project" {
		t.Fatal("a request reader mutated immutable journal custody")
	}
}

func assertUnguardedModelWorkerGolden(t *testing.T, f scopedModelFixture, deps Dependencies, request task.TaskRecord, binding inspection.Binding) {
	t.Helper()
	request.TaskID, request.ConnectorContext = "22222222222222222222222222222222", nil
	operation, err := inspection.OpenOperation(f.store, request, binding, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	path := filepath.Join(f.store.Root, "inspections", request.TaskID, "request.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := operation.Request()
	facts, workErr, stopErr := runSupervisedInspection(operation, nil, record.SupervisorIdentity(), nil, deps, f.store, record, nil)
	want := `{"status":"available","reason_code":"ok","complete":true,"source":"fixture","models":[{"id":"fixture/model","name":"Fixture model","efforts":null}],"efforts":null}`
	if workErr != nil || stopErr != nil || string(bytes.TrimSpace(facts)) != want {
		t.Fatalf("unguarded model worker changed: work=%v stop=%v facts=%s", workErr, stopErr, facts)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || bytes.Contains(after, []byte(`"connector_context"`)) {
		t.Fatalf("unguarded canonical request changed: %v", err)
	}
}

func TestScopedModelWorkerExpiredSetupCannotStart(t *testing.T) {
	startup := newInspectionWorkerStartupFixture(t, task.RunnerOwnershipManaged)
	f := newScopedModelFixture(t, startup.store)
	a, err := ParseArguments(f.arguments())
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareScopedModels(t.Context(), &a, f.store.Root, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	request := startup.operation.Request().Task
	request.TaskID, request.CanonicalCwd, request.ConnectorContext = "33333333333333333333333333333333", f.path, a.Models.binding.Clone()
	binding := startup.operation.Request().Binding
	binding.DefinitionRevision = commonprovider.ModelsRevision
	operation, err := inspection.OpenOperation(f.store, request, binding, time.Now().Add(-2*inspection.ModelDiscoveryTimeout))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	setup, cancel := context.WithDeadline(t.Context(), operation.Deadline())
	project, acquireErr := scopedModelCustody(setup, f.store, operation.Request())
	cancel()
	if project != nil || !errors.Is(acquireErr, context.DeadlineExceeded) {
		t.Fatalf("expired setup acquired custody: project=%v error=%v", project, acquireErr)
	}
	preparations := 0
	catalog := scopedModelCatalogWithPrepare(t, f.path, func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		preparations++
		return commonprovider.ProfileCandidate{}, errors.New("expired worker entered native preparation")
	})
	record := operation.Request()
	facts, workErr, stopErr := runSupervisedInspection(operation, nil, record.SupervisorIdentity(), nil, Dependencies{Catalog: catalog}, f.store, record, nil)
	// This fixture supplies no supervisor client. Its stop-owner refusal stays
	// visible; refusal before native preparation proves no provider was started.
	if workErr == nil || !errors.Is(stopErr, task.ErrInvalidPermit) || len(facts) != 0 || preparations != 0 {
		t.Fatalf("expired setup reached work: preparations=%d work=%v stop=%v facts=%s", preparations, workErr, stopErr, facts)
	}
	assertNoModelTasks(t, f.store)
}
