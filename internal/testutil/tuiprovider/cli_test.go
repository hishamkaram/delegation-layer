package tuiprovider

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestFixtureRuntimeSurvivesCanonicalInspectionAndProviderScopes(t *testing.T) {
	definition, err := provider.NewRuntimeInspectionDefinition(provider.CLIInfo{Path: "/fixture/provider"}, "/fixture/work", []string{}, provider.RuntimeCapability{RequiredFlags: []string{"--output"}})
	if err != nil {
		t.Fatal(err)
	}
	original := provider.ProfileCandidate{
		Inspection: &definition,
		Finalize: func(json.RawMessage, time.Time) (provider.PreparedProfile, error) {
			return provider.PreparedProfile{Plan: execution.Plan{Environment: []string{}}}, nil
		},
	}
	bound, err := bindFixtureRuntime(original, "/fixture/private runtime")
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := bound.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DELEGATE_CONTRIBUTOR_RUNTIME=/fixture/private runtime"}
	if !reflect.DeepEqual(saved.Environment, want) || !reflect.DeepEqual(saved.Runtime.Environment, want) {
		t.Fatalf("canonical inspection lost exact fixture scope: inspection=%v probe=%v", saved.Environment, saved.Runtime.Environment)
	}
	profile, err := bound.Finalize(nil, time.Time{})
	if err != nil || !reflect.DeepEqual(profile.Plan.Environment, want) {
		t.Fatalf("provider reconstruction lost exact fixture scope: %v, %v", profile.Plan.Environment, err)
	}
	if len(original.Inspection.Environment) != 0 || len(original.Inspection.Runtime.Environment) != 0 {
		t.Fatal("binding an invocation scope mutated the contributor's original definition")
	}
}
