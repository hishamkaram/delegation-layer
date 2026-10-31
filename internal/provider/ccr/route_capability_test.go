package ccr

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// requireOppositeRouteCapability finalizes candidate with the route
// capability opposite to its recorded launch. A route-preserving launch
// refuses the removed flag as an unavailable profile whose whole cause is
// that capability; a modeled launch keeps its recorded predicate and argv on
// a CLI that has gained the flag.
func requireOppositeRouteCapability(t *testing.T, label string, candidate commonprovider.ProfileCandidate, recorded commonprovider.PreparedProfile, route bool) {
	t.Helper()
	changed, err := candidate.Finalize(routeFacts(t, candidate, !route), time.Now())
	if route {
		if !errors.Is(err, commonprovider.ErrProfileUnavailable) || !commonprovider.NativeCapabilityUnavailable(err) {
			t.Fatalf("%s removed route capability error=%v, want an unavailable required capability", label, err)
		}
		return
	}
	if err != nil || !changed.Plan.Predicate.Equal(recorded.Plan.Predicate) || !slices.Equal(changed.Plan.Arguments, recorded.Plan.Arguments) ||
		slices.Contains(changed.Plan.Arguments, "--resume-route") {
		t.Fatalf("%s added route capability changed the modeled launch: %+v %q err=%v (recorded %+v %q)",
			label, changed.Plan.Predicate, changed.Plan.Arguments, err, recorded.Plan.Predicate, recorded.Plan.Arguments)
	}
}

// modeledRouteReferences are the exact recorded references whose modeled
// launch is checked by the route inspection.
func modeledRouteReferences(mode string) []task.PredicateRef {
	return []task.PredicateRef{modeledReference(mode), advisoryReference(mode, false), scopedReference(mode, false), preAttributionReference(mode, false), preCredentialReference(mode, false)}
}

// TestRefreshedFactsDecisionBelongsOnlyToModeledRouteContracts supplies the
// decision only for an exact modeled reference with route inspection: a fresh
// admission, a route-preserving launch, and a legacy inspection keep exact
// fact equality.
func TestRefreshedFactsDecisionBelongsOnlyToModeledRouteContracts(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, reference := range modeledRouteReferences(mode) {
			if refreshedFactsDecision(reference, mode) == nil {
				t.Errorf("%s has no refreshed-facts decision", reference.Version)
			}
		}
		exact := []task.PredicateRef{
			{},
			Reference(mode), advisoryReference(mode, true), scopedReference(mode, true), preAttributionReference(mode, true),
			preCredentialReference(mode, true), previousReference(mode), HistoricalReference(mode),
			{Adapter: Provider, Mode: mode, Version: "unknown", SHA256: task.ComputeSHA256([]byte("unknown"))},
		}
		for _, reference := range exact {
			if refreshedFactsDecision(reference, mode) != nil {
				t.Errorf("%q gained a refreshed-facts decision", reference.Version)
			}
		}
	}
}

// TestModeledRouteDecisionAcceptsOnlyAnAddedRouteCapability accepts the
// recorded capability and its addition, refuses a removal as a capability
// change, and refuses every malformed projection as invalid.
func TestModeledRouteDecisionAcceptsOnlyAnAddedRouteCapability(t *testing.T) {
	decide := refreshedFactsDecision(modeledReference(config.ModeReadOnly), config.ModeReadOnly)
	without, with := json.RawMessage(`{"resume_route":false}`), json.RawMessage(`{"resume_route":true}`)
	for name, pair := range map[string][2]json.RawMessage{"unchanged": {without, without}, "added": {without, with}} {
		if err := decide(pair[0], pair[1]); err != nil {
			t.Errorf("%s capability refused: %v", name, err)
		}
	}
	if reason, ok := commonprovider.NativeFactsRefusal(decide(with, without)); !ok || reason != commonprovider.NativeCapabilityChanged {
		t.Errorf("removed capability reason=%d ok=%v", reason, ok)
	}
	for name, projection := range map[string]string{
		"missing":   `{}`,
		"null":      `{"resume_route":null}`,
		"wrong":     `{"resume_route":"true"}`,
		"duplicate": `{"resume_route":false,"resume_route":true}`,
		"unknown":   `{"resume_route":true,"other":true}`,
		"not json":  `{`,
	} {
		for side, err := range map[string]error{"stored": decide(json.RawMessage(projection), with), "fresh": decide(without, json.RawMessage(projection))} {
			if reason, ok := commonprovider.NativeFactsRefusal(err); !ok || reason != commonprovider.NativeFactsInvalid {
				t.Errorf("%s %s projection reason=%d ok=%v err=%v", side, name, reason, ok, err)
			}
		}
	}
}

// TestRouteSelectorKeepsMalformedFactsDistinctFromCapabilityChange keeps the
// selector's unavailable identity for malformed facts without labeling them a
// capability change.
func TestRouteSelectorKeepsMalformedFactsDistinctFromCapabilityChange(t *testing.T) {
	for _, reference := range []task.PredicateRef{Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly)} {
		facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: "/bin/ccr", SHA256: task.ComputeSHA256([]byte("ccr")), Version: "fixture"},
			json.RawMessage(`{"resume_route":null}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = selectPreparation(reference, config.ModeReadOnly, facts)
		if !errors.Is(err, commonprovider.ErrProfileUnavailable) || commonprovider.NativeCapabilityUnavailable(err) {
			t.Errorf("%s malformed facts error=%v", reference.Version, err)
		}
	}
}
