package antigravity

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || len(description.SupportedModes) != 2 || description.SupportedModes[0] != ModeReadOnly || description.SupportedModes[1] != ModeWorkspaceWrite {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatalf("runtime flag requirements are missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil {
		t.Fatalf("registration did not expose preparation and interpreters: %+v", registration)
	}
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatalf("invalid or duplicate registered reference: %v", err)
	}
	if _, err := registry.Resolve(NewPrintInterpreter().Reference()); err != nil {
		t.Fatalf("legacy envelope predicate is not resolvable: %v", err)
	}
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		for _, ref := range []task.PredicateRef{
			newHistoricalCurrentPrintInterpreterForMode(mode).Reference(),
			NewCurrentPrintInterpreterForMode(mode).Reference(),
			newHistoricalRecordPrintInterpreter(mode).Reference(),
			NewRecordPrintInterpreter(mode).Reference(),
		} {
			if _, err := registry.Resolve(ref); err != nil {
				t.Fatalf("required predicate %+v is not resolvable: %v", ref, err)
			}
		}
	}
}
