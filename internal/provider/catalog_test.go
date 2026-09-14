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

func TestNewCatalogRejectsUnexecutedIncompleteOrMismatchedProfiles(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Registration)
	}{
		{
			name: "unexecuted status",
			change: func(registration *Registration) {
				registration.Description.Profiles[0].Status = "Documented"
			},
		},
		{
			name: "missing runtime digest",
			change: func(registration *Registration) {
				registration.Description.Profiles[0].RuntimeSHA256 = ""
			},
		},
		{
			name: "malformed runtime digest",
			change: func(registration *Registration) {
				registration.Description.Profiles[0].RuntimeSHA256 = "not-a-sha256"
			},
		},
		{
			name: "predicate mode mismatch",
			change: func(registration *Registration) {
				ref := registration.Description.Profiles[0].Predicate
				ref.Mode = "workspace-write"
				registration.Description.Profiles[0].Predicate = ref
				registration.Interpreters[0] = catalogTestInterpreter{ref: ref}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registration := testRegistration("alpha:print", "read-only")
			tc.change(&registration)
			if _, err := NewCatalog(registration); err == nil {
				t.Fatal("invalid certified profile was accepted")
			}
		})
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

func TestCatalogTreatsDefaultEffortAsProviderDefault(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = func(request task.TaskRecord) (PreparedProfile, error) {
		called = true
		return PreparedProfile{Plan: executionPlanForCatalogTest(request)}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		Provider:     "alpha:print",
		Mode:         "read-only",
		CanonicalCwd: "/workspace",
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Effort:     "default",
		},
	}
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

func TestCatalogAcceptsPreparedPredicateFromAnyMatchingModeProfile(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	secondRef := task.PredicateRef{
		Adapter: "alpha:print",
		Mode:    "read-only",
		Version: "2",
		SHA256:  task.ComputeSHA256([]byte("alpha:print/read-only/2")),
	}
	registration.Description.Profiles = append(registration.Description.Profiles, CertifiedProfile{
		Mode:            "read-only",
		Approval:        "test",
		Status:          "Executed",
		ProviderVersion: "test-2",
		OS:              "test",
		Arch:            "test",
		RuntimeSHA256:   task.ComputeSHA256([]byte("runtime-2")),
		ProfileRevision: "test-2",
		Predicate:       secondRef,
	})
	registration.Interpreters = append(registration.Interpreters, catalogTestInterpreter{ref: secondRef})
	registration.Prepare = func(request task.TaskRecord) (PreparedProfile, error) {
		plan := executionPlanForCatalogTest(request)
		plan.Predicate = secondRef
		return PreparedProfile{Plan: plan}, nil
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
	if _, err = catalog.Prepare(request); err != nil {
		t.Fatalf("matching second certified profile was refused: %v", err)
	}
}

func TestCatalogRejectsPreparedHistoricalOrUnknownPredicate(t *testing.T) {
	cases := []struct {
		name        string
		preparedRef func(task.PredicateRef) task.PredicateRef
		addInterp   bool
	}{
		{
			name: "unknown predicate",
			preparedRef: func(ref task.PredicateRef) task.PredicateRef {
				ref.Version = "unknown"
				ref.SHA256 = task.ComputeSHA256([]byte("unknown"))
				return ref
			},
		},
		{
			name: "registered historical predicate",
			preparedRef: func(ref task.PredicateRef) task.PredicateRef {
				ref.Version = "historical"
				ref.SHA256 = task.ComputeSHA256([]byte("historical"))
				return ref
			},
			addInterp: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registration := testRegistration("alpha:print", "read-only")
			currentRef := registration.Description.Profiles[0].Predicate
			preparedRef := tc.preparedRef(currentRef)
			if tc.addInterp {
				registration.Interpreters = append(registration.Interpreters, catalogTestInterpreter{ref: preparedRef})
			}
			registration.Prepare = func(request task.TaskRecord) (PreparedProfile, error) {
				plan := executionPlanForCatalogTest(request)
				plan.Predicate = preparedRef
				return PreparedProfile{Plan: plan}, nil
			}
			catalog, err := NewCatalog(registration)
			if err != nil {
				t.Fatal(err)
			}
			request := task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
			if _, err = catalog.Prepare(request); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("uncertified prepared predicate was accepted: %v", err)
			}
		})
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
