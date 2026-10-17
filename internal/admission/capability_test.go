package admission

import (
	"errors"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeCapabilityEvidenceDoesNotAliasDefinition(t *testing.T) {
	candidate := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{
		Runtime: &commonprovider.RuntimeProbeDefinition{
			HelpArgs: []string{"--help"}, RequiredFlags: []string{"--json"},
		},
	}}
	evidence := RuntimeCapabilityFor(candidate, commonprovider.PreparedProfile{ObservedVersion: "observed-version"})
	if evidence == nil {
		t.Fatal("runtime evidence was omitted")
	}
	candidate.Inspection.Runtime.HelpArgs[0] = "changed-argument"
	candidate.Inspection.Runtime.RequiredFlags[0] = "changed-flag"
	if evidence.HelpArgs[0] != "--help" || evidence.RequiredFlags[0] != "--json" {
		t.Fatalf("runtime definition mutated detached evidence: %+v", evidence)
	}
	evidence.HelpArgs[0] = "projection-argument"
	evidence.RequiredFlags[0] = "projection-flag"
	if candidate.Inspection.Runtime.HelpArgs[0] != "changed-argument" || candidate.Inspection.Runtime.RequiredFlags[0] != "changed-flag" {
		t.Fatal("evidence projection mutated the provider definition")
	}
}

func TestCapabilityEvidenceRequiresDescriptionReader(t *testing.T) {
	_, err := readCapabilityEvidence(nil, task.TaskRecord{}, commonprovider.ProfileCandidate{}, commonprovider.PreparedProfile{})
	var staged *StageError
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.As(err, &staged) || staged.Stage() != "select-provider" {
		t.Fatalf("missing description reader did not fail at its observation boundary: %v", err)
	}
}
