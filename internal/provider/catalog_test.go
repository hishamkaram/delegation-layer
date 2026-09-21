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
			SupportedModes:   []string{mode},
			SupportedOptions: options,
			Runtime:          RuntimeCapability{RequiredFlags: []string{"--provider-test"}},
			Discoverable:     true,
		},
		Prepare: ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
			return PreparedProfile{}, nil
		}),
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

func TestNewCatalogRejectsDiscoverableProviderWithoutSupportedMode(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	registration.Description.SupportedModes = nil
	_, err := NewCatalog(registration)
	if !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("discoverable provider without a supported mode was accepted: %v", err)
	}
}

func TestNewCatalogRejectsInvalidSupportedMode(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	registration.Description.SupportedModes[0] = "invalid-mode"
	if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("invalid supported mode was accepted: %v", err)
	}
}

func TestCatalogDescriptionsAreDeterministicAndDefensive(t *testing.T) {
	alpha := testRegistration("alpha:print", "read-only", OptionModel, OptionContinuation)
	alpha.Description.SupportedModes = []string{"workspace-write", "read-only"}
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
	if !reflect.DeepEqual(got[0].SupportedModes, []string{"read-only", "workspace-write"}) {
		t.Fatalf("modes were not sorted: %+v", got[0].SupportedModes)
	}
	if !reflect.DeepEqual(got[0].Runtime.RequiredFlags, []string{"--provider-test"}) {
		t.Fatalf("runtime requirements were not retained: %+v", got[0].Runtime)
	}
	got[0].SupportedOptions[0] = "mutated"
	got[0].SupportedModes[0] = "mutated"
	got[0].Runtime.RequiredFlags[0] = "mutated"
	again := catalog.Descriptions()
	if again[0].SupportedOptions[0] == "mutated" || again[0].SupportedModes[0] == "mutated" || again[0].Runtime.RequiredFlags[0] == "mutated" {
		t.Fatal("catalog metadata was mutable through returned slices")
	}
}

func TestCatalogPreservesLegacyContinuationOption(t *testing.T) {
	catalog, err := NewCatalog(testRegistration("alpha:print", "read-only", OptionContinuation))
	if err != nil {
		t.Fatal(err)
	}
	registration, err := catalog.Lookup("alpha:print")
	if err != nil {
		t.Fatal(err)
	}
	if registration.Description.Continuation != ContinuationNative {
		t.Fatalf("legacy continuation option was downgraded: %q", registration.Description.Continuation)
	}
}

func TestCatalogRejectsInconsistentContinuationMetadata(t *testing.T) {
	cases := []struct {
		name         string
		continuation ContinuationMode
		options      []string
	}{
		{name: "unsupported with option", continuation: ContinuationUnsupported, options: []string{OptionContinuation}},
		{name: "native without option", continuation: ContinuationNative},
		{name: "checkpoint", continuation: ContinuationCheckpoint, options: []string{OptionContinuation}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registration := testRegistration("alpha:print", "read-only", testCase.options...)
			registration.Description.Continuation = testCase.continuation
			if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("inconsistent continuation metadata was accepted: %v", err)
			}
		})
	}
}

func TestNewCatalogRejectsMalformedRuntimeCapability(t *testing.T) {
	cases := []struct {
		name string
		edit func(*RuntimeCapability)
	}{
		{name: "empty flag", edit: func(capability *RuntimeCapability) { capability.RequiredFlags = []string{""} }},
		{name: "non-flag", edit: func(capability *RuntimeCapability) { capability.RequiredFlags = []string{"help"} }},
		{name: "duplicate flag", edit: func(capability *RuntimeCapability) { capability.RequiredFlags = []string{"--flag", "--flag"} }},
		{name: "invalid help argument", edit: func(capability *RuntimeCapability) { capability.HelpArgs = []string{"exec help"} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registration := testRegistration("alpha:print", "read-only")
			testCase.edit(&registration.Description.Runtime)
			if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("malformed runtime capability was accepted: %v", err)
			}
		})
	}
}

func TestCatalogRejectsUnsupportedOptionBeforePreparation(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only", OptionContinuation)
	registration.Prepare = ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
		called = true
		return PreparedProfile{}, nil
	})
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

func TestCatalogRejectsUnsupportedModeBeforePreparation(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
		called = true
		return PreparedProfile{}, nil
	})
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "workspace-write", RequestedConfig: task.TaskConfig{Permission: "workspace-write"}}
	if _, err = catalog.Prepare(request); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("unsupported mode was not rejected: %v", err)
	}
	if called {
		t.Fatal("provider preparation ran before mode refusal")
	}
}

func TestCatalogTreatsDefaultEffortAsProviderDefault(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		called = true
		return catalogTestProfile(executionPlanForCatalogTest(request)), nil
	})
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only", Effort: "default"}}
	if _, err = catalog.Prepare(request); err != nil {
		t.Fatalf("provider-default effort was refused: %v", err)
	}
	if !called {
		t.Fatal("provider preparation did not receive provider-default request")
	}
	if request.RequestedConfig.Effort != "default" {
		t.Fatalf("provider-default effort was normalized or discarded: %q", request.RequestedConfig.Effort)
	}
}

func TestCatalogAcceptsRegisteredHistoricalPredicate(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	secondRef := task.PredicateRef{Adapter: "alpha:print", Mode: "read-only", Version: "2", SHA256: task.ComputeSHA256([]byte("alpha:print/read-only/2"))}
	registration.Interpreters = append(registration.Interpreters, catalogTestInterpreter{ref: secondRef})
	registration.Prepare = ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		plan := executionPlanForCatalogTest(request)
		plan.Predicate = secondRef
		return catalogTestProfile(plan), nil
	})
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
	if _, err = catalog.Prepare(request); err != nil {
		t.Fatalf("registered historical predicate was refused: %v", err)
	}
}

func TestCatalogRejectsUnknownPreparedPredicate(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	preparedRef := task.PredicateRef{Adapter: "alpha:print", Mode: "read-only", Version: "unknown", SHA256: task.ComputeSHA256([]byte("unknown"))}
	registration.Prepare = ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		plan := executionPlanForCatalogTest(request)
		plan.Predicate = preparedRef
		return catalogTestProfile(plan), nil
	})
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
	if _, err = catalog.Prepare(request); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("unknown prepared predicate was accepted: %v", err)
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
	registration.Prepare = ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		called++
		return catalogTestProfile(executionPlanForCatalogTest(request)), nil
	})
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

func catalogTestProfile(plan execution.Plan) PreparedProfile {
	return PreparedProfile{Plan: plan, ObservedVersion: "test-1", Effective: task.EffectiveConfig{Containment: "read-only", Approval: "test", Digest: task.ComputeSHA256([]byte("test-config"))}}
}
