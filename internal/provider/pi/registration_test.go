package pi

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionExposesReadOnlyModeAndNativeOptions(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable {
		t.Fatalf("unexpected description: %+v", description)
	}
	if !slices.Equal(description.SupportedModes, []string{ModeReadOnly}) {
		t.Fatalf("supported modes=%q", description.SupportedModes)
	}
	for _, option := range []string{commonprovider.OptionContinuation, commonprovider.OptionModel, commonprovider.OptionEffort} {
		if !slices.Contains(description.SupportedOptions, option) {
			t.Fatalf("supported options=%q missing %q", description.SupportedOptions, option)
		}
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatal("runtime flag requirements are missing")
	}
	registration := Registration()
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 2 {
		t.Fatalf("registration=%+v", registration)
	}
	if !registration.Interpreters[0].Reference().Equal(ReferenceForMode(ModeReadOnly)) {
		t.Fatalf("interpreter reference does not bind read-only mode")
	}
	if !registration.Interpreters[1].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy interpreter reference is not registered")
	}
	if got := ReferenceForMode(config.ModeWorkspaceWrite); got != (task.PredicateRef{}) {
		t.Fatalf("workspace-write unexpectedly has a predicate reference: %+v", got)
	}
}
