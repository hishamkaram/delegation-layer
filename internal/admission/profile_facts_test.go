package admission

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionFactsCompareCanonicalObjectsBeforeFinalize(t *testing.T) {
	if !inspectionFactsMatch(json.RawMessage(`{"z":2,"a":1}`), json.RawMessage(`{"a":1,"z":2}`)) {
		t.Fatal("equivalent canonical facts differed only by object field order")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`{"eligible":false}`)) {
		t.Fatal("changed inspection facts were accepted")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`not-json`)) {
		t.Fatal("malformed fresh facts were accepted")
	}
	runtime := commonprovider.RuntimeFacts{Executable: "/bin/provider", Version: "provider-1", SHA256: strings.Repeat("a", 64)}
	stored, err := commonprovider.EncodeInspectionFacts(runtime, json.RawMessage(`{"eligible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	changedRuntime := runtime
	changedRuntime.Executable = "/opt/provider-new"
	changedRuntime.Version = "provider-2"
	changedRuntime.SHA256 = strings.Repeat("b", 64)
	fresh, err := commonprovider.EncodeInspectionFacts(changedRuntime, json.RawMessage(`{"eligible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !inspectionFactsMatch(stored, fresh) {
		t.Fatal("runtime identity drift changed the native inspection contract")
	}
	changedNative, err := commonprovider.EncodeInspectionFacts(changedRuntime, json.RawMessage(`{"eligible":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if inspectionFactsMatch(stored, changedNative) {
		t.Fatal("native inspection drift was accepted with runtime identity drift")
	}
	nativeWithRuntimeKey := json.RawMessage(`{"runtime":{"provider":"native"},"eligible":true}`)
	if !inspectionFactsMatchForDefinition(nativeWithRuntimeKey, nativeWithRuntimeKey, false) {
		t.Fatal("native-only projection with a runtime key was decoded as an envelope")
	}
}

// refreshedFactsCandidate has a mixed native and runtime inspection and the
// given refreshed-facts decision.
func refreshedFactsCandidate(decision func(stored, fresh json.RawMessage) error) commonprovider.ProfileCandidate {
	definition := commonprovider.InspectionDefinition{Arguments: []string{"--native"}, Runtime: &commonprovider.RuntimeProbeDefinition{}}
	return commonprovider.ProfileCandidate{Inspection: &definition, AcceptRefreshedNativeFacts: decision}
}

// refreshedFactsEnvelope encodes native with runtime facts; an empty native
// string encodes an envelope without a native projection.
func refreshedFactsEnvelope(t *testing.T, native string) json.RawMessage {
	t.Helper()
	var projection json.RawMessage
	if native != "" {
		projection = json.RawMessage(native)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: "/bin/provider", Version: "provider-1", SHA256: strings.Repeat("a", 64)}, projection)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func requireRefreshedFactsFault(t *testing.T, name string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) || !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, commonprovider.ErrProfileUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Errorf("%s: err=%v, want %v as an evidence fault", name, err, want)
	}
}

// TestRefreshedNativeFactsKeepExactEqualityWithoutDecision keeps every
// provider without a decision on canonical equality: a changed projection is
// refused with a fixed message naming the change.
func TestRefreshedNativeFactsKeepExactEqualityWithoutDecision(t *testing.T) {
	candidate := refreshedFactsCandidate(nil)
	stored := refreshedFactsEnvelope(t, `{"eligible":true}`)
	if err := refreshedInspectionFactsError(candidate, stored, refreshedFactsEnvelope(t, `{"eligible":true}`)); err != nil {
		t.Fatalf("equal facts refused: %v", err)
	}
	requireRefreshedFactsFault(t, "changed", refreshedInspectionFactsError(candidate, stored, refreshedFactsEnvelope(t, `{"eligible":false}`)), errInspectionFactsChanged)
	static := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{Arguments: []string{"--native"}}}
	requireRefreshedFactsFault(t, "static changed", refreshedInspectionFactsError(static, json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)), errInspectionFactsChanged)
	requireRefreshedFactsFault(t, "static malformed", refreshedInspectionFactsError(static, json.RawMessage(`{"a":1}`), json.RawMessage(`not-json`)), errInspectionFactsMalformed)
}

// TestRefreshedNativeFactsDecisionSeesOnlyValidatedProjections never hands
// malformed evidence to the provider and passes a valid difference as the two
// canonical native projections.
func TestRefreshedNativeFactsDecisionSeesOnlyValidatedProjections(t *testing.T) {
	var seen [][2]string
	candidate := refreshedFactsCandidate(func(stored, fresh json.RawMessage) error {
		seen = append(seen, [2]string{string(stored), string(fresh)})
		return nil
	})
	stored := refreshedFactsEnvelope(t, `{"z":false,"a":1}`)
	malformed := map[string]json.RawMessage{
		"not json":         json.RawMessage(`not-json`),
		"without runtime":  json.RawMessage(`{"z":true,"a":1}`),
		"missing native":   refreshedFactsEnvelope(t, ``),
		"array projection": refreshedFactsEnvelope(t, `[1]`),
		"duplicate field":  json.RawMessage(strings.Replace(string(refreshedFactsEnvelope(t, `{"z":true}`)), `{"z":true}`, `{"z":true,"z":false}`, 1)),
	}
	for name, fresh := range malformed {
		requireRefreshedFactsFault(t, name, refreshedInspectionFactsError(candidate, stored, fresh), errInspectionFactsMalformed)
	}
	if len(seen) != 0 {
		t.Fatalf("malformed evidence reached the provider decision: %q", seen)
	}
	if err := refreshedInspectionFactsError(candidate, stored, refreshedFactsEnvelope(t, `{"a":1,"z":true}`)); err != nil {
		t.Fatalf("accepted difference refused: %v", err)
	}
	if want := [2]string{"{\"a\":1,\"z\":false}\n", "{\"a\":1,\"z\":true}\n"}; len(seen) != 1 || seen[0] != want {
		t.Fatalf("decision saw %q, want canonical projections %q", seen, want)
	}
}

// TestRefreshedNativeFactsDecisionIsNormalized maps only a whole provider
// refusal to its fixed evidence fault. A provider sentinel, a joined or
// unknown cause, or any other error becomes the fixed contract fault, and no
// provider text crosses the boundary.
func TestRefreshedNativeFactsDecisionIsNormalized(t *testing.T) {
	invalid := commonprovider.NativeFactsRefusalError{Reason: commonprovider.NativeFactsInvalid}
	changed := commonprovider.NativeFactsRefusalError{Reason: commonprovider.NativeCapabilityChanged}
	tests := map[string]struct {
		decision error
		want     error
	}{
		"invalid projection":    {decision: invalid, want: errInspectionFactsMalformed},
		"capability changed":    {decision: changed, want: errInspectionFactsChanged},
		"wrapped refusal":       {decision: fmt.Errorf("secret detail: %w", changed), want: errInspectionFactsChanged},
		"provider sentinel":     {decision: commonprovider.ErrProfileUnavailable, want: errInspectionFactsDecision},
		"sentinel with refusal": {decision: fmt.Errorf("%w: %w", commonprovider.ErrProfileUnavailable, changed), want: errInspectionFactsDecision},
		"joined fault":          {decision: errors.Join(changed, task.ErrIdentityMismatch), want: errInspectionFactsDecision},
		"mixed reasons":         {decision: errors.Join(changed, invalid), want: errInspectionFactsDecision},
		"unknown reason":        {decision: commonprovider.NativeFactsRefusalError{Reason: 9}, want: errInspectionFactsDecision},
		"arbitrary error":       {decision: errors.New("secret token"), want: errInspectionFactsDecision},
	}
	stored, fresh := refreshedFactsEnvelope(t, `{"capable":false}`), refreshedFactsEnvelope(t, `{"capable":true}`)
	for name, test := range tests {
		candidate := refreshedFactsCandidate(func(json.RawMessage, json.RawMessage) error { return test.decision })
		requireRefreshedFactsFault(t, name, refreshedInspectionFactsError(candidate, stored, fresh), test.want)
	}
}
