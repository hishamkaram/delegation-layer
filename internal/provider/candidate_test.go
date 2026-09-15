package provider

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadyCandidatePreservesProfileAndRefusesInspectionFacts(t *testing.T) {
	request := artifactRequest()
	profile := PreparedProfile{
		Plan: artifactPlan(request), ObservedVersion: "provider-1",
		Effective: task.EffectiveConfig{Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config"))},
	}
	calls := 0
	candidate, err := ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
		calls++
		return profile, nil
	})(request)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection != nil {
		t.Fatal("ready provider unexpectedly requested a native inspection")
	}
	prepared, err := candidate.Finalize(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.Executable != profile.Plan.Executable || !task.CompareEffectiveConfigs(prepared.Effective, profile.Effective) || calls != 1 {
		t.Fatal("finalization changed policy or reran preparation")
	}
	for _, facts := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`null`)} {
		if _, err = candidate.Finalize(facts, time.Now()); !errors.Is(err, ErrProfileUnavailable) {
			t.Fatalf("unexpected facts accepted: %v", err)
		}
	}
}

func TestReadyCandidatePreservesPreparationRefusal(t *testing.T) {
	want := errors.New("static policy unavailable")
	candidate, err := ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
		return PreparedProfile{}, want
	})(artifactRequest())
	if !errors.Is(err, want) || candidate.Finalize != nil {
		t.Fatalf("refused preparation acquired a finalizer: %v", err)
	}
	if _, err = ReadyCandidate(nil)(artifactRequest()); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("nil factory accepted: %v", err)
	}
}

func TestCandidateRejectsWritableStateBeforeInspection(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	candidate := ProfileCandidate{
		Directory:     filepath.Join(base, "workspace"),
		WritableRoots: []string{filepath.Join(base, "native-cache")},
		Inspection: &InspectionDefinition{Project: func([]byte) (json.RawMessage, error) {
			t.Fatal("static placement invoked native projection")
			return nil, nil
		}},
	}
	if err = candidate.ValidateStatePlacement(filepath.Join(base, "native-cache", "state")); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("provider-writable state accepted: %v", err)
	}
	if err = candidate.ValidateStatePlacement(filepath.Join(base, "state")); err != nil {
		t.Fatalf("isolated state refused: %v", err)
	}
}

func TestReadyCandidateIsolatesFactoryAndFinalizationStorage(t *testing.T) {
	request := artifactRequest()
	profile := PreparedProfile{
		Plan: artifactPlan(request), ObservedVersion: "provider-1",
		WritableRoots: []string{"/native-cache"},
		Effective: task.EffectiveConfig{
			Containment: "read-only", Approval: "never", Digest: task.ComputeSHA256([]byte("config")),
			Policy: &task.PolicyDetails{
				ProfileRevision: "profile-v1", RuntimeSHA256: task.ComputeSHA256([]byte("runtime")),
				Workspace: request.CanonicalCwd, WritableRoots: []string{"/native-cache"},
				Sources: []task.PolicySourceDigest{{Path: "/settings.json", Kind: "settings"}},
			},
		},
	}
	profile.Plan.Environment = []string{"LANG=C"}
	candidate, err := ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) { return profile, nil })(request)
	if err != nil {
		t.Fatal(err)
	}
	want, err := candidate.Finalize(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(p *PreparedProfile) {
		p.Plan.Arguments[0] = "changed"
		p.Plan.Environment[0] = "changed"
		p.Plan.InputFiles[0].Content = "changed"
		p.Plan.OutputArtifacts[0].Name = "changed"
		p.WritableRoots[0] = "/changed"
		p.Effective.Policy.ProfileRevision = "changed"
		p.Effective.Policy.WritableRoots[0] = "/changed"
		p.Effective.Policy.Sources[0].Kind = "changed"
	}
	mutate(&profile)
	got, err := candidate.Finalize(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("factory mutation changed captured profile")
	}
	if candidate.WritableRoots[0] != "/native-cache" {
		t.Fatal("factory mutation changed placement policy")
	}
	mutate(&got)
	candidate.WritableRoots[0] = "/candidate-mutation"
	again, err := candidate.Finalize(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatal("result or placement mutation changed later finalization")
	}
}

func TestCatalogRequiresInspectionBeforeFinalization(t *testing.T) {
	request := artifactRequest()
	registration := testRegistration(request.Provider, request.Mode)
	profile := catalogTestProfile(executionPlanForCatalogTest(request))
	calls := 0
	definition := inspectionTestDefinition()
	registration.Prepare = func(task.TaskRecord) (ProfileCandidate, error) {
		return ProfileCandidate{
			Directory: request.CanonicalCwd, Inspection: definition,
			Finalize: func(facts json.RawMessage, _ time.Time) (PreparedProfile, error) {
				calls++
				if string(facts) != `{"eligible":true}` {
					return PreparedProfile{}, ErrProfileUnavailable
				}
				return profile, nil
			},
		}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Prepare(request); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("inspection bypass: %v", err)
	}
	if calls != 0 {
		t.Fatal("compatibility preparation called inspection finalizer")
	}
	candidate, err := catalog.Candidate(request)
	if err != nil {
		t.Fatal(err)
	}
	definition.Arguments[0] = "mutated"
	definition.Environment[0] = "MUTATED=yes"
	if candidate.Inspection.Arguments[0] != "fixed-argument" || candidate.Inspection.Environment[0] != "LANG=C" {
		t.Fatal("candidate aliases inspection factory storage")
	}
	if _, err = candidate.Finalize(nil, time.Now()); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("missing facts accepted: %v", err)
	}
	if _, err = candidate.Finalize(json.RawMessage(`{"eligible":true}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	profile.Plan.Predicate.Version = "uncertified"
	if _, err = candidate.Finalize(json.RawMessage(`{"eligible":true}`), time.Now()); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("inspection bypassed certification: %v", err)
	}
	profile.Plan.Predicate = executionPlanForCatalogTest(request).Predicate
	profile.WritableRoots = []string{"/new-writable-root"}
	if _, err = candidate.Finalize(json.RawMessage(`{"eligible":true}`), time.Now()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("inspection expanded placement policy: %v", err)
	}
}
