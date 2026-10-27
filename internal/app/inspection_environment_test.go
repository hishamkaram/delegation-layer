package app

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// environmentReceiptCatalog returns a discoverable fixture catalog whose
// preparation records the launch scope it receives and then refuses.
func environmentReceiptCatalog(t *testing.T, receipts *[][]string) commonprovider.Catalog {
	t.Helper()
	interpreter, err := predicate.Default().Resolve(task.FixturePredicateRef())
	testsupport.RequireAppTest(t, err)
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{
			ID:             config.ProviderFixture,
			SupportedModes: []string{config.ModeReadOnly},
			ReadOnly:       commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
			Discoverable:   true,
			Runtime:        commonprovider.RuntimeCapability{RequiredFlags: []string{"--dispatch-only"}},
		},
		Interpreters: []predicate.Interpreter{interpreter},
		Prepare: func(_ task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			*receipts = append(*receipts, slices.Clone(environment))
			return commonprovider.ProfileCandidate{}, commonprovider.ErrAuthenticationBlocked
		},
		Models: func() commonprovider.ModelDiscoveryDefinition { return commonprovider.ModelDiscoveryDefinition{} },
	})
	testsupport.RequireAppTest(t, err)
	return catalog
}

func TestModelsCandidatePreparesInCompositionScope(t *testing.T) {
	ambient := os.Environ()
	var receipts [][]string
	catalog := environmentReceiptCatalog(t, &receipts)
	request := task.TaskRecord{Provider: config.ProviderFixture, CanonicalCwd: t.TempDir()}

	_, err := prepareModelsCandidate(Dependencies{ResourceChecker: fixtureResources, Catalog: catalog}, t.TempDir(), request)
	var stage *admission.StageError
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.As(err, &stage) || stage.Stage() != "prepare-provider" || len(receipts) != 0 {
		t.Fatalf("uncaptured models scope = %v, receipts=%q", err, receipts)
	}

	for _, environment := range [][]string{{"PATH=/caller-bin", "HOME=/caller-home"}, {}} {
		receipts = nil
		_, err = prepareModelsCandidate(Dependencies{ResourceChecker: fixtureResources, Catalog: catalog, Environment: environment}, t.TempDir(), request)
		if !errors.Is(err, commonprovider.ErrAuthenticationBlocked) || !errors.As(err, &stage) || stage.Stage() != "prepare-provider" {
			t.Fatalf("models preparation failure changed stage: %v", err)
		}
		if len(receipts) != 1 || receipts[0] == nil || !slices.Equal(receipts[0], environment) {
			t.Fatalf("models preparation received %q, want %q", receipts, environment)
		}
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("models preparation changed the process environment")
	}
}

func TestInspectionWorkerScopeSelectsBindingEnvironment(t *testing.T) {
	ambient := os.Environ()
	caller := []string{"PATH=/caller-bin", "HOME=/caller-home"}
	binding := []string{"PATH=/binding-bin", "HOME=/binding-home"}
	var receipts [][]string
	deps := Dependencies{
		ResourceChecker: fixtureResources,
		Catalog:         environmentReceiptCatalog(t, &receipts),
		Environment:     caller,
		PrepareCandidate: func(_ task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			receipts = append(receipts, slices.Clone(environment))
			return commonprovider.ProfileCandidate{}, commonprovider.ErrAuthenticationBlocked
		},
	}
	scoped, err := scopedDependencies(deps, binding, true)
	testsupport.RequireAppTest(t, err)
	if !slices.Equal(scoped.Environment, binding) || !slices.Equal(deps.Environment, caller) {
		t.Fatalf("worker scope = %q, caller = %q", scoped.Environment, deps.Environment)
	}

	request := task.TaskRecord{Provider: config.ProviderFixture, CanonicalCwd: t.TempDir()}
	if _, err = prepareModelsCandidate(scoped, t.TempDir(), request); !errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
		t.Fatalf("worker models preparation = %v", err)
	}
	if _, err = prepareInspectionWorkerCandidate(scoped, t.TempDir(), request, nil); !errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
		t.Fatalf("worker runtime preparation = %v", err)
	}
	if len(receipts) != 2 || !slices.Equal(receipts[0], binding) || !slices.Equal(receipts[1], binding) {
		t.Fatalf("worker preparations received %q, want binding scope %q", receipts, binding)
	}

	unrecorded, err := scopedDependencies(deps, nil, false)
	testsupport.RequireAppTest(t, err)
	if !slices.Equal(unrecorded.Environment, caller) {
		t.Fatalf("unrecorded binding scope = %q, want caller %q", unrecorded.Environment, caller)
	}
	if _, err = scopedDependencies(deps, []string{"PATH=/a", "PATH=/b"}, true); err == nil {
		t.Fatal("worker accepted an invalid recorded binding scope")
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("worker scope selection changed the process environment")
	}
}

func TestSupervisorControlScopeBindsCapturedEnvironmentOnly(t *testing.T) {
	t.Setenv("HOME", "/ambient-home")
	caller := []string{"HOME=/caller-home", "PATH=/caller-bin", "PROVIDER_SECRET=hidden"}
	captured := Dependencies{ResourceChecker: fixtureResources, Environment: caller}
	want := []string{"HOME=/caller-home", "PATH=/caller-bin"}
	if got := captured.supervisorControlScope().Environment; !slices.Equal(got, want) {
		t.Fatalf("captured control scope = %q, want %q", got, want)
	}
	if got := captured.storeDependencies().supervisorOptions.Environment; !slices.Equal(got, want) {
		t.Fatalf("store observation control scope = %q, want %q", got, want)
	}
	if got := supervisorControlOptions(captured).Environment; !slices.Equal(got, want) {
		t.Fatalf("supervisor command control scope = %q, want %q", got, want)
	}
	if got := (Dependencies{ResourceChecker: fixtureResources}).supervisorControlScope().Environment; got != nil {
		t.Fatalf("standalone control scope = %q, want pueue's nil default", got)
	}
	explicit := Dependencies{ResourceChecker: fixtureResources, Environment: caller, SupervisorOptions: pueue.Options{Environment: []string{}}}
	if got := explicit.supervisorControlScope().Environment; got == nil || len(got) != 0 {
		t.Fatalf("explicit empty control environment changed: %#v", got)
	}
}
