package provider

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
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
	description := Description{
		ID:               id,
		SupportedModes:   []string{mode},
		SupportedOptions: options,
		Runtime:          RuntimeCapability{RequiredFlags: []string{"--provider-test"}},
		Discoverable:     true,
	}
	if mode == config.ModeReadOnly {
		description.ReadOnly = ReadOnlyCapability{Supported: true, Mechanism: ReadOnlyNativePlan, Containment: ReadOnlyProviderOwned}
	}
	return Registration{
		Description: description,
		Prepare: ReadyCandidate(func(task.TaskRecord) (PreparedProfile, error) {
			return PreparedProfile{}, nil
		}),
		Interpreters: []predicate.Interpreter{catalogTestInterpreter{ref: ref}},
	}
}

func TestNewCatalogRejectsReadOnlyModeWithoutCapability(t *testing.T) {
	registration := testRegistration("alpha:print", config.ModeReadOnly)
	registration.Description.ReadOnly = ReadOnlyCapability{}
	if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("read-only mode without capability was accepted: %v", err)
	}
}

func TestNewCatalogRejectsInvalidReadOnlyCapability(t *testing.T) {
	registration := testRegistration("alpha:print", config.ModeReadOnly)
	registration.Description.ReadOnly.Mechanism = "provider-default"
	if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("invalid read-only mechanism was accepted: %v", err)
	}
}

func TestNewCatalogRejectsReadOnlyCapabilityWithoutMode(t *testing.T) {
	registration := testRegistration("alpha:print", "workspace-write")
	registration.Description.ReadOnly = ReadOnlyCapability{Supported: true, Mechanism: ReadOnlyNativePlan, Containment: ReadOnlyProviderOwned}
	if _, err := NewCatalog(registration); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("read-only capability without mode was accepted: %v", err)
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

func TestCatalogValidatesProviderContinuationAfterSharedIdentity(t *testing.T) {
	registration := testRegistration("alpha:print", config.ModeReadOnly, OptionContinuation)
	registration.Description.Continuation = ContinuationNative
	want := errors.New("model changed")
	calls := 0
	registration.ValidateContinuation = func(predecessor task.TaskRecord, predecessorMeta task.MetaRecord, successor task.TaskRecord) error {
		calls++
		if predecessor.Provider != "alpha:print" || predecessorMeta.Predicate.Adapter != "alpha:print" || successor.Provider != "alpha:print" {
			t.Fatalf("validator received unexpected records: predecessor=%+v meta=%+v successor=%+v", predecessor, predecessorMeta, successor)
		}
		return want
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := task.TaskRecord{Provider: "alpha:print", Mode: config.ModeReadOnly}
	predecessorMeta := task.MetaRecord{Predicate: task.PredicateRef{Adapter: "alpha:print", Mode: config.ModeReadOnly, Version: "fixture", SHA256: strings.Repeat("a", 64)}}
	successor := task.TaskRecord{Provider: "alpha:print", Mode: config.ModeReadOnly}
	if err = catalog.ValidateContinuation(predecessor, predecessorMeta, successor); !errors.Is(err, want) {
		t.Fatalf("continuation validation error = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("validator calls=%d want=1", calls)
	}
	successor.Mode = config.ModeWorkspaceWrite
	if err = catalog.ValidateContinuation(predecessor, predecessorMeta, successor); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-mode continuation error = %v, want identity mismatch", err)
	}
	if calls != 1 {
		t.Fatalf("identity mismatch called provider validator %d times", calls)
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

func TestCatalogKeepsHistoricalReadOnlyDescriptionWithoutLaunchMetadata(t *testing.T) {
	ref := task.FixturePredicateRef()
	interp := catalogTestInterpreter{ref: ref}
	_, err := NewCatalog(Registration{
		Description: Description{
			ID:             ref.Adapter,
			SupportedModes: []string{config.ModeReadOnly},
		},
		Interpreters: []predicate.Interpreter{interp},
	})
	if err != nil {
		t.Fatalf("historical-only read-only description was rejected: %v", err)
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

type catalogPreparationProbe struct {
	calls []string
	meta  task.MetaRecord
}

func (p *catalogPreparationProbe) fresh(version string) PrepareCandidate {
	return ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		p.calls = append(p.calls, version)
		profile := catalogTestProfile(executionPlanForCatalogTest(request))
		profile.ObservedVersion = version
		return profile, nil
	})
}

func (p *catalogPreparationProbe) withMeta(version string) PrepareExistingCandidate {
	return func(request task.TaskRecord, meta task.MetaRecord) (ProfileCandidate, error) {
		p.meta = meta
		return p.fresh(version)(request)
	}
}

func finalizedCatalogVersion(t *testing.T, candidate ProfileCandidate) string {
	t.Helper()
	profile, err := candidate.Finalize(nil, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("candidate did not finalize: %v", err)
	}
	return profile.ObservedVersion
}

func catalogPreparationRequest() task.TaskRecord {
	return task.TaskRecord{Provider: "alpha:print", Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
}

func TestCatalogPreparationOperationsSelectTheirOwnHook(t *testing.T) {
	probe := &catalogPreparationProbe{}
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = probe.fresh("fresh")
	registration.PrepareExisting = probe.withMeta("existing")
	registration.PrepareSuccessor = probe.withMeta("successor")
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := catalogPreparationRequest()
	predecessor := task.MetaRecord{Predicate: task.FixturePredicateRef()}
	operations := map[string]func() (ProfileCandidate, error){
		"fresh":     func() (ProfileCandidate, error) { return catalog.Candidate(request) },
		"existing":  func() (ProfileCandidate, error) { return catalog.ExistingCandidate(request, predecessor) },
		"successor": func() (ProfileCandidate, error) { return catalog.SuccessorCandidate(request, predecessor) },
	}
	for want, operation := range operations {
		probe.calls, probe.meta = nil, task.MetaRecord{}
		candidate, candidateErr := operation()
		if candidateErr != nil {
			t.Fatalf("%s preparation refused: %v", want, candidateErr)
		}
		if got := finalizedCatalogVersion(t, candidate); got != want || len(probe.calls) != 1 {
			t.Fatalf("%s preparation used %q with calls %v", want, got, probe.calls)
		}
		if want != "fresh" && !probe.meta.Predicate.Equal(predecessor.Predicate) {
			t.Fatalf("%s preparation did not receive the predecessor record: %+v", want, probe.meta)
		}
	}
}

func TestCatalogSuccessorCandidateFallsBackToRecordedPreparation(t *testing.T) {
	tests := []struct {
		name     string
		existing bool
		want     string
	}{
		{name: "existing hook", existing: true, want: "existing"},
		{name: "fresh preparation", want: "fresh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &catalogPreparationProbe{}
			registration := testRegistration("alpha:print", "read-only")
			registration.Prepare = probe.fresh("fresh")
			if test.existing {
				registration.PrepareExisting = probe.withMeta("existing")
			}
			catalog, err := NewCatalog(registration)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := catalog.SuccessorCandidate(catalogPreparationRequest(), task.MetaRecord{})
			if err != nil {
				t.Fatalf("successor preparation refused: %v", err)
			}
			if got := finalizedCatalogVersion(t, candidate); got != test.want {
				t.Fatalf("successor fallback used %q, want %q", got, test.want)
			}
		})
	}
}

func TestCatalogSuccessorCandidateKeepsHistoricalOnlyRegistrationUnlaunchable(t *testing.T) {
	probe := &catalogPreparationProbe{}
	ref := task.FixturePredicateRef()
	catalog, err := NewCatalog(Registration{Description: Description{ID: ref.Adapter}, PrepareSuccessor: probe.withMeta("successor"), Interpreters: []predicate.Interpreter{catalogTestInterpreter{ref: ref}}})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{Provider: ref.Adapter, Mode: "read-only", CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Permission: "read-only"}}
	if _, err = catalog.SuccessorCandidate(request, task.MetaRecord{}); !errors.Is(err, ErrProfileUnavailable) || len(probe.calls) != 0 {
		t.Fatalf("historical-only registration acquired successor authority: %v calls=%v", err, probe.calls)
	}
}

func TestCatalogSuccessorCandidateSharesRequestAndCandidateValidation(t *testing.T) {
	cause := errors.New("provider refused successor")
	tests := []struct {
		name    string
		request func(task.TaskRecord) task.TaskRecord
		hook    func(task.TaskRecord, task.MetaRecord) (ProfileCandidate, error)
		want    error
		called  bool
	}{
		{
			name: "unsupported option before preparation",
			request: func(request task.TaskRecord) task.TaskRecord {
				request.RequestedConfig.NativeTimeout = "1s"
				return request
			},
			want: ErrUnsupportedOption,
		},
		{
			name: "provider error",
			hook: func(task.TaskRecord, task.MetaRecord) (ProfileCandidate, error) { return ProfileCandidate{}, cause },
			want: cause, called: true,
		},
		{
			name: "directory mismatch",
			hook: func(request task.TaskRecord, meta task.MetaRecord) (ProfileCandidate, error) {
				candidate, err := (&catalogPreparationProbe{}).withMeta("successor")(request, meta)
				candidate.Directory = "/elsewhere"
				return candidate, err
			},
			want: task.ErrIdentityMismatch, called: true,
		},
		{
			name: "missing finalizer",
			hook: func(request task.TaskRecord, meta task.MetaRecord) (ProfileCandidate, error) {
				candidate, err := (&catalogPreparationProbe{}).withMeta("successor")(request, meta)
				candidate.Finalize = nil
				return candidate, err
			},
			want: ErrProfileUnavailable, called: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			registration := testRegistration("alpha:print", "read-only")
			registration.PrepareSuccessor = func(request task.TaskRecord, meta task.MetaRecord) (ProfileCandidate, error) {
				called = true
				return test.hook(request, meta)
			}
			catalog, err := NewCatalog(registration)
			if err != nil {
				t.Fatal(err)
			}
			request := catalogPreparationRequest()
			if test.request != nil {
				request = test.request(request)
			}
			if _, err = catalog.SuccessorCandidate(request, task.MetaRecord{}); !errors.Is(err, test.want) || called != test.called {
				t.Fatalf("successor validation = %v, hook called=%t; want %v/%t", err, called, test.want, test.called)
			}
		})
	}
}

func TestCatalogSuccessorCandidateBindsRegistryToFinalizedPredicate(t *testing.T) {
	registration := testRegistration("alpha:print", "read-only")
	unknown := task.PredicateRef{Adapter: "alpha:print", Mode: "read-only", Version: "unknown", SHA256: task.ComputeSHA256([]byte("unknown"))}
	registration.PrepareSuccessor = func(request task.TaskRecord, _ task.MetaRecord) (ProfileCandidate, error) {
		return ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
			plan := executionPlanForCatalogTest(request)
			plan.Predicate = unknown
			return catalogTestProfile(plan), nil
		})(request)
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.SuccessorCandidate(catalogPreparationRequest(), task.MetaRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.Finalize(nil, time.Unix(0, 0)); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("successor finalized an unregistered predicate: %v", err)
	}
}

func TestNewCatalogDiscoverableCommandRequirements(t *testing.T) {
	cases := []struct {
		name    string
		runtime RuntimeCapability
		invalid bool
	}{
		{name: "required flags only", runtime: RuntimeCapability{RequiredFlags: []string{"--json"}}},
		{name: "help command only", runtime: RuntimeCapability{HelpArgs: []string{"acp"}}},
		{name: "help command and flags", runtime: RuntimeCapability{HelpArgs: []string{"acp"}, RequiredFlags: []string{"--json"}}},
		{name: "version arguments only", runtime: RuntimeCapability{VersionArgs: []string{"--version"}}, invalid: true},
		{name: "neither", runtime: RuntimeCapability{}, invalid: true},
		{name: "help command with duplicate argument", runtime: RuntimeCapability{HelpArgs: []string{"acp", "acp"}}, invalid: true},
		{name: "help command with empty argument", runtime: RuntimeCapability{HelpArgs: []string{""}}, invalid: true},
		{name: "help command with malformed flag", runtime: RuntimeCapability{HelpArgs: []string{"acp"}, RequiredFlags: []string{"json"}}, invalid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			registration := testRegistration("alpha:print", "read-only")
			registration.Description.Runtime = testCase.runtime
			catalog, err := NewCatalog(registration)
			if testCase.invalid {
				if !errors.Is(err, ErrInvalidDescriptor) {
					t.Fatalf("invalid runtime capability was accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := catalog.Descriptions()[0].Runtime
			if !reflect.DeepEqual(got.HelpArgs, testCase.runtime.HelpArgs) || !reflect.DeepEqual(got.RequiredFlags, testCase.runtime.RequiredFlags) {
				t.Fatalf("runtime capability was not retained: %+v", got)
			}
		})
	}
}
