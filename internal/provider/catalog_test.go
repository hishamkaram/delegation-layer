package provider

import (
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type catalogTestInterpreter struct{ ref task.PredicateRef }

func (i catalogTestInterpreter) Reference() task.PredicateRef { return i.ref }

func (catalogTestInterpreter) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{Verdict: task.VerdictCommitted}, nil
}

func testRegistration(id, mode string, options ...string) Registration {
	ref := task.PredicateRef{Adapter: id, Mode: mode, Version: "1", SHA256: task.ComputeSHA256([]byte(id + "/" + mode))}
	return Registration{
		Description: Description{
			ID:               id,
			SupportedOptions: options,
			Discoverable:     true,
			Profiles: []CertifiedProfile{{
				Mode:            mode,
				Approval:        "test",
				Status:          "Executed",
				ProviderVersion: "test-1",
				OS:              "test",
				Arch:            "test",
				RuntimeSHA256:   task.ComputeSHA256([]byte("runtime")),
				ProfileRevision: "test-1",
				Predicate:       ref,
			}},
		},
		Prepare: func(task.TaskRecord) (PreparedProfile, error) {
			return PreparedProfile{}, nil
		},
		Interpreters: []predicate.Interpreter{catalogTestInterpreter{ref: ref}},
	}
}

func TestNewCatalogRejectsDuplicateProviderIDs(t *testing.T) {
	first := testRegistration("alpha:print", "read-only")
	second := testRegistration("alpha:print", "read-only")
	_, err := NewCatalog(first, second)
	if !errors.Is(err, ErrDuplicateProvider) {
		t.Fatalf("duplicate provider was accepted: %v", err)
	}
}

func TestNewCatalogRejectsDuplicateExactPredicateReferences(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	registration.Interpreters = append(registration.Interpreters, registration.Interpreters[0])
	_, err := NewCatalog(registration)
	if !errors.Is(err, ErrDuplicatePredicate) {
		t.Fatalf("duplicate predicate was accepted: %v", err)
	}
}

func TestNewCatalogRejectsDiscoverableProviderWithoutCertifiedProfile(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	registration.Description.Profiles = nil
	_, err := NewCatalog(registration)
	if !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("discoverable provider without a profile was accepted: %v", err)
	}
}

func TestNewCatalogRejectsProfileWithoutRegisteredInterpreter(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	ref := task.PredicateRef{Adapter: registration.Description.ID, Mode: "read-only", Version: "other", SHA256: task.ComputeSHA256([]byte("other"))}
	registration.Interpreters = []predicate.Interpreter{catalogTestInterpreter{ref: ref}}
	_, err := NewCatalog(registration)
	if !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("profile without an interpreter was accepted: %v", err)
	}
}

func TestCatalogDescriptionsAreDeterministicAndDefensive(t *testing.T) {
	alpha := testRegistration("alpha:print", "read-only", OptionModel, OptionContinuation)
	alpha.Description.Profiles[0].ProviderVersion = "z"
	zulu := testRegistration("zulu:print", "read-only", OptionNativeTimeout)
	catalog, err := NewCatalog(zulu, alpha)
	if err != nil {
		t.Fatal(err)
	}
	got := catalog.Descriptions()
	if len(got) != 2 || got[0].ID != "alpha:print" || got[1].ID != "zulu:print" {
		t.Fatalf("descriptions were not sorted by ID: %+v", got)
	}
	if !reflect.DeepEqual(got[0].SupportedOptions, []string{OptionContinuation, OptionModel}) {
		t.Fatalf("options were not sorted: %+v", got[0].SupportedOptions)
	}
	got[0].SupportedOptions[0] = "mutated"
	got[0].Profiles[0].ProviderVersion = "mutated"
	again := catalog.Descriptions()
	if again[0].SupportedOptions[0] == "mutated" || again[0].Profiles[0].ProviderVersion == "mutated" {
		t.Fatal("catalog metadata was mutable through returned slices")
	}
}

func TestCatalogRejectsUnsupportedOptionBeforePreparation(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only", OptionContinuation)
	registration.Prepare = func(task.TaskRecord) (PreparedProfile, error) {
		called = true
		return PreparedProfile{}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", RequestedConfig: task.TaskConfig{Permission: "read-only", Model: "explicit"}}
	if _, err = catalog.Prepare(request); !errors.Is(err, ErrUnsupportedOption) {
		t.Fatalf("unsupported model was not rejected: %v", err)
	}
	if called {
		t.Fatal("provider preparation ran before capability refusal")
	}
}

func TestCatalogKeepsHistoricalInterpreterWithoutLaunchProfile(t *testing.T) {
	ref := task.FixturePredicateRef()
	interp := catalogTestInterpreter{ref: ref}
	catalog, err := NewCatalog(Registration{Description: Description{ID: ref.Adapter}, Interpreters: []predicate.Interpreter{interp}})
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Descriptions(); len(got) != 0 {
		t.Fatalf("historical-only registration was exposed: %+v", got)
	}
	if _, err = catalog.Prepare(task.TaskRecord{Provider: ref.Adapter}); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("historical-only registration acquired launch authority: %v", err)
	}
	if _, err = catalog.Registry().Resolve(ref); err != nil {
		t.Fatalf("historical interpreter was not registered: %v", err)
	}
}

func TestCatalogAcceptsValidRequestAndDoesNotTouchExternalState(t *testing.T) {
	called := 0
	registration := testRegistration("alpha:print", "read-only", OptionNativeTimeout)
	registration.Prepare = func(request task.TaskRecord) (PreparedProfile, error) {
		called++
		return PreparedProfile{Plan: executionPlanForCatalogTest(request)}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only", NativeTimeout: "1s"}}
	if _, err = catalog.Prepare(request); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if called != 1 {
		t.Fatalf("preparation call count=%d, want 1", called)
	}
	if _, err = catalog.Registry().Resolve(task.FixturePredicateRef()); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("catalog unexpectedly acquired an unregistered external predicate: %v", err)
	}
}

func executionPlanForCatalogTest(request task.TaskRecord) execution.Plan {
	return execution.Plan{Executable: "/bin/provider", Directory: request.CanonicalCwd, Predicate: task.PredicateRef{Adapter: request.Provider, Mode: request.Mode, Version: "1", SHA256: task.ComputeSHA256([]byte(request.Provider + "/" + request.Mode))}}
}
