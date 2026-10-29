package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestNativeFactsRefusalClassifiesTheWholeError(t *testing.T) {
	invalid, changed := NativeFactsRefusalError{Reason: NativeFactsInvalid}, NativeFactsRefusalError{Reason: NativeCapabilityChanged}
	tests := []struct {
		name   string
		err    error
		reason NativeFactsRefusalReason
		ok     bool
	}{
		{name: "invalid", err: invalid, reason: NativeFactsInvalid, ok: true},
		{name: "changed", err: changed, reason: NativeCapabilityChanged, ok: true},
		{name: "wrapped", err: fmt.Errorf("context: %w", changed), reason: NativeCapabilityChanged, ok: true},
		{name: "joined same reason", err: errors.Join(changed, changed), reason: NativeCapabilityChanged, ok: true},
		{name: "nil", err: nil},
		{name: "unknown reason", err: NativeFactsRefusalError{Reason: 9}},
		{name: "zero reason", err: NativeFactsRefusalError{}},
		{name: "plain error", err: errors.New("native detail")},
		{name: "provider sentinel", err: ErrProfileUnavailable},
		{name: "sentinel wrapping a refusal", err: fmt.Errorf("%w: %w", ErrProfileUnavailable, changed)},
		{name: "joined with another cause", err: errors.Join(changed, task.ErrEvidenceFault)},
		{name: "mixed reasons", err: errors.Join(changed, invalid)},
	}
	for _, test := range tests {
		reason, ok := NativeFactsRefusal(test.err)
		if ok != test.ok || (ok && reason != test.reason) {
			t.Errorf("%s: reason=%d ok=%v, want %d/%v", test.name, reason, ok, test.reason, test.ok)
		}
	}
}

func TestNativeCapabilityUnavailableRequiresTheWholeUnavailableRefusal(t *testing.T) {
	changed := NativeFactsRefusalError{Reason: NativeCapabilityChanged}
	tests := map[string]struct {
		err  error
		want bool
	}{
		"selector refusal":          {err: fmt.Errorf("%w: %w", ErrProfileUnavailable, changed), want: true},
		"catalog-wrapped refusal":   {err: fmt.Errorf("%w: %w", ErrProfileUnavailable, fmt.Errorf("%w: %w", ErrProfileUnavailable, changed)), want: true},
		"unavailable only":          {err: ErrProfileUnavailable},
		"refusal only":              {err: changed},
		"invalid projection":        {err: fmt.Errorf("%w: %w", ErrProfileUnavailable, NativeFactsRefusalError{Reason: NativeFactsInvalid})},
		"joined with another fault": {err: errors.Join(fmt.Errorf("%w: %w", ErrProfileUnavailable, changed), task.ErrEvidenceFault)},
	}
	for name, test := range tests {
		if got := NativeCapabilityUnavailable(test.err); got != test.want {
			t.Errorf("%s: NativeCapabilityUnavailable=%v, want %v", name, got, test.want)
		}
	}
}

func TestNativeFactsRefusalTextIsFixed(t *testing.T) {
	for _, refusal := range []NativeFactsRefusalError{{Reason: NativeFactsInvalid}, {Reason: NativeCapabilityChanged}, {Reason: 9}} {
		if message := refusal.Error(); message == "" || strings.ContainsAny(message, "{}\"") {
			t.Errorf("refusal %d message=%q", refusal.Reason, message)
		}
	}
}

// mixedCatalogCandidate is a candidate with a native projection and a runtime
// probe whose refreshed-facts decision counts its calls.
func mixedCatalogCandidate(t *testing.T, request task.TaskRecord, decisions *int) ProfileCandidate {
	t.Helper()
	definition, err := NewRuntimeInspectionDefinition(CLIInfo{Path: "/bin/provider", SHA256: strings.Repeat("a", 64)}, request.CanonicalCwd, []string{"LANG=C"},
		RuntimeCapability{RequiredFlags: []string{"--provider-test"}})
	if err != nil {
		t.Fatal(err)
	}
	definition.Arguments = []string{"--native"}
	definition.Project = func([]byte) (json.RawMessage, error) { return json.RawMessage(`{"capable":true}`), nil }
	candidate, err := ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
		return catalogTestProfile(executionPlanForCatalogTest(request)), nil
	})(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Inspection = &definition
	candidate.AcceptRefreshedNativeFacts = func(json.RawMessage, json.RawMessage) error {
		*decisions++
		return nil
	}
	return candidate
}

// TestCatalogForwardsRefreshedNativeFactsDecision keeps the provider's
// decision through every catalog preparation operation and refuses it
// without a mixed native and runtime inspection.
func TestCatalogForwardsRefreshedNativeFactsDecision(t *testing.T) {
	decisions := 0
	shape := "mixed"
	registration := testRegistration("alpha:print", "read-only")
	prepare := func(request task.TaskRecord, _ []string) (ProfileCandidate, error) {
		candidate := mixedCatalogCandidate(t, request, &decisions)
		switch shape {
		case "runtime only":
			candidate.Inspection.Arguments, candidate.Inspection.Project = nil, nil
		case "no runtime":
			candidate.Inspection.Runtime = nil
		case "no inspection":
			candidate.Inspection = nil
		}
		return candidate, nil
	}
	registration.Prepare = prepare
	registration.PrepareExisting = func(request task.TaskRecord, _ task.MetaRecord, environment []string) (ProfileCandidate, error) {
		return prepare(request, environment)
	}
	registration.PrepareSuccessor = registration.PrepareExisting
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := catalogPreparationRequest()
	predecessor := task.MetaRecord{Predicate: task.FixturePredicateRef()}
	operations := map[string]func() (ProfileCandidate, error){
		"fresh": func() (ProfileCandidate, error) { return catalog.Candidate(request, catalogTestEnvironment()) },
		"existing": func() (ProfileCandidate, error) {
			return catalog.ExistingCandidate(request, predecessor, catalogTestEnvironment())
		},
		"successor": func() (ProfileCandidate, error) {
			return catalog.SuccessorCandidate(request, predecessor, catalogTestEnvironment())
		},
	}
	for name, operation := range operations {
		shape = "mixed"
		candidate, candidateErr := operation()
		if candidateErr != nil || candidate.AcceptRefreshedNativeFacts == nil {
			t.Fatalf("%s lost the decision: %v", name, candidateErr)
		}
		before := decisions
		if decisionErr := candidate.AcceptRefreshedNativeFacts(json.RawMessage(`{}`), json.RawMessage(`{}`)); decisionErr != nil || decisions != before+1 {
			t.Fatalf("%s decision was not the provider's: %v", name, decisionErr)
		}
		for _, invalid := range []string{"runtime only", "no runtime", "no inspection"} {
			shape = invalid
			if _, candidateErr = operation(); !errors.Is(candidateErr, ErrProfileUnavailable) {
				t.Errorf("%s accepted a decision with %s: %v", name, invalid, candidateErr)
			}
		}
	}
}
