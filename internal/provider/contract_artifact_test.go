package provider

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func artifactRequest() task.TaskRecord {
	return task.TaskRecord{
		Provider:        "alpha:print",
		Mode:            "read-only",
		CanonicalCwd:    "/workspace",
		RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "1s"},
		BudgetNanos:     1_000_000_000,
	}
}

func artifactPlan(request task.TaskRecord) execution.Plan {
	ref := task.PredicateRef{Adapter: request.Provider, Mode: request.Mode, Version: "1", SHA256: task.ComputeSHA256([]byte(request.Provider + "/" + request.Mode))}
	return execution.Plan{
		Executable:           "/bin/provider",
		Arguments:            []string{"--settings", "", "--extra", "", "--output", "", "-"},
		Directory:            request.CanonicalCwd,
		Predicate:            ref,
		InputFiles:           []task.InputFile{{Name: "settings.json", ArgumentIndex: 1, Content: `{"mode":"read-only"}`}},
		OutputArtifacts:      []task.OutputArtifact{{Name: "answer.txt", ArgumentIndex: 5}},
		OutputWriterContract: task.OutputWriterProcessExitEOF,
	}
}

func TestPreparedProfileValidateRejectsNonEmptyBindingSlots(t *testing.T) {
	request := artifactRequest()
	plan := artifactPlan(request)
	plan.Arguments[1] = "ambient-settings.json"
	profile := PreparedProfile{Plan: plan, ObservedVersion: "provider-1", Effective: task.EffectiveConfig{Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config"))}}
	if err := profile.Validate(request); err == nil {
		t.Fatal("non-empty input binding slot was accepted")
	}
	plan = artifactPlan(request)
	plan.Arguments[5] = "ambient-answer.txt"
	profile.Plan = plan
	if err := profile.Validate(request); err == nil {
		t.Fatal("non-empty output binding slot was accepted")
	}
}

func TestCatalogNormalizesPreparedDeclarationsAndChecksCertifiedWriter(t *testing.T) {
	request := artifactRequest()
	registration := testRegistration(request.Provider, request.Mode)
	registration.Description.Profiles[0].OutputWriterContract = task.OutputWriterProcessExitEOF
	registration.Prepare = func(req task.TaskRecord) (PreparedProfile, error) {
		plan := artifactPlan(req)
		plan.InputFiles = append(plan.InputFiles, task.InputFile{Name: "a.json", ArgumentIndex: 3, Content: "a"})
		return PreparedProfile{Plan: plan}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := catalog.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Plan.InputFiles) != 2 || prepared.Plan.InputFiles[0].Name != "a.json" || prepared.Plan.InputFiles[1].Name != "settings.json" {
		t.Fatalf("prepared input declarations were not normalized: %+v", prepared.Plan.InputFiles)
	}
	if len(prepared.Plan.OutputArtifacts) != 1 || prepared.Plan.OutputArtifacts[0].Name != "answer.txt" {
		t.Fatalf("prepared output declarations changed: %+v", prepared.Plan.OutputArtifacts)
	}
	prepared.Plan.InputFiles[0].Content = "x"
	preparedAgain, err := catalog.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if preparedAgain.Plan.InputFiles[0].Content != "a" {
		t.Fatal("catalog returned shared input content")
	}

	registration.Description.Profiles[0].OutputWriterContract = "snapshot-only-v1"
	if _, err = NewCatalog(registration); err == nil {
		t.Fatal("unsupported certified writer contract was accepted")
	}
}

func TestCatalogRejectsPreparedWriterContractNotCertifiedByPredicateProfile(t *testing.T) {
	request := artifactRequest()
	plan := artifactPlan(request)
	registration := testRegistration(request.Provider, request.Mode)
	registration.Prepare = func(task.TaskRecord) (PreparedProfile, error) {
		return PreparedProfile{Plan: plan}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Prepare(request); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("uncertified output writer contract was accepted: %v", err)
	}
}

func TestPreparedProfileMatchesInputContentAndOutputDeclarations(t *testing.T) {
	request := artifactRequest()
	plan := artifactPlan(request)
	profile := PreparedProfile{Plan: plan, ObservedVersion: "provider-1", Effective: task.EffectiveConfig{Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config"))}}
	meta := task.MetaRecord{ProviderExecutable: plan.Executable, ProviderVersion: profile.ObservedVersion, EffectiveConfig: profile.Effective, Predicate: plan.Predicate, InputFiles: []task.InputFile{{Name: "settings.json", ArgumentIndex: 1, Content: `{"mode":"read-only"}`}}, OutputArtifacts: []task.OutputArtifact{{Name: "answer.txt", ArgumentIndex: 5}}, OutputWriterContract: task.OutputWriterProcessExitEOF}
	if err := profile.Matches(request, meta); err != nil {
		t.Fatal(err)
	}
	profile.Plan.InputFiles[0].Content = `{"mode":"workspace-write"}`
	if !errors.Is(profile.Matches(request, meta), task.ErrIdentityMismatch) {
		t.Fatal("changed input content matched immutable metadata")
	}
	profile.Plan = plan
	meta.OutputWriterContract = ""
	if !errors.Is(profile.Matches(request, meta), task.ErrIdentityMismatch) {
		t.Fatal("changed output writer contract matched immutable metadata")
	}
}

func TestCatalogSelectsMatchingWriterContractAcrossCertifiedProfiles(t *testing.T) {
	request := artifactRequest()
	registration := testRegistration(request.Provider, request.Mode)
	withOutput := registration.Description.Profiles[0]
	withOutput.OutputWriterContract = task.OutputWriterProcessExitEOF
	registration.Description.Profiles = append(registration.Description.Profiles, withOutput)
	registration.Prepare = func(req task.TaskRecord) (PreparedProfile, error) {
		return PreparedProfile{Plan: artifactPlan(req)}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Prepare(request); err != nil {
		t.Fatalf("matching certified writer contract was hidden by another profile: %v", err)
	}
}
