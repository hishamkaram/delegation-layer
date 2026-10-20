package claude

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || !slices.Contains(description.SupportedModes, Mode) || !slices.Contains(description.SupportedModes, WorkspaceWriteMode) {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatalf("runtime flag requirements are missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 29 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	checks := []struct {
		index   int
		matches bool
	}{
		{2, registration.Interpreters[2].Reference().Equal(historicalNativeReferenceForMode(Mode))},
		{3, registration.Interpreters[3].Reference().Equal(historicalNativeReferenceForMode(WorkspaceWriteMode))},
		{4, registration.Interpreters[4].Reference().Equal(legacyNativeReferenceForMode(Mode))},
		{5, registration.Interpreters[5].Reference().Equal(legacyNativeReferenceForMode(WorkspaceWriteMode))},
		{6, registration.Interpreters[6].Reference().Equal(legacyPortableReferenceForMode(Mode))},
		{7, registration.Interpreters[7].Reference().Equal(legacyPortableReferenceForMode(WorkspaceWriteMode))},
		{8, registration.Interpreters[8].Reference().Equal(LegacyReference())},
		{13, registration.Interpreters[13].Reference().Equal(previousNativeProbeReferenceForMode(Mode))},
		{14, registration.Interpreters[14].Reference().Equal(previousNativeProbeReferenceForMode(WorkspaceWriteMode))},
		{17, registration.Interpreters[17].Reference().Equal(preAttributionReferenceForMode(Mode))},
		{18, registration.Interpreters[18].Reference().Equal(preAttributionReferenceForMode(WorkspaceWriteMode))},
		{19, registration.Interpreters[19].Reference().Equal(preAttributionInteractionReference(Mode))},
		{20, registration.Interpreters[20].Reference().Equal(preAttributionInteractionReference(WorkspaceWriteMode))},
		{21, registration.Interpreters[21].Reference().Equal(unscopedReference(WorkspaceWriteMode))},
		{22, registration.Interpreters[22].Reference().Equal(unscopedInteractionReference(Mode))},
		{23, registration.Interpreters[23].Reference().Equal(unscopedInteractionReference(WorkspaceWriteMode))},
		{24, registration.Interpreters[24].Reference().Equal(unscopedReference(Mode))},
		{25, registration.Interpreters[25].Reference().Equal(scopedReference(Mode))},
		{26, registration.Interpreters[26].Reference().Equal(scopedReference(WorkspaceWriteMode))},
		{27, registration.Interpreters[27].Reference().Equal(scopedInteractionReference(Mode))},
		{28, registration.Interpreters[28].Reference().Equal(scopedInteractionReference(WorkspaceWriteMode))},
	}
	for _, check := range checks {
		if !check.matches {
			t.Fatalf("legacy predicate reference at index %d is not registered", check.index)
		}
	}
}

func TestInteractionRegistration(t *testing.T) {
	registration := Registration()
	description := Description()
	if !registration.Interpreters[9].Reference().Equal(interactionReference(Mode)) || !registration.Interpreters[10].Reference().Equal(interactionReference(WorkspaceWriteMode)) ||
		!registration.Interpreters[11].Reference().Equal(historicalInteractionReference(Mode)) || !registration.Interpreters[12].Reference().Equal(historicalInteractionReference(WorkspaceWriteMode)) ||
		!registration.Interpreters[15].Reference().Equal(previousInteractionReference(Mode)) || !registration.Interpreters[16].Reference().Equal(previousInteractionReference(WorkspaceWriteMode)) {
		t.Fatal("interactive predicates missing")
	}
	if !description.Interaction.Clarification || description.Interaction.Steering != false {
		t.Fatalf("wrong interaction capabilities: %+v", description.Interaction)
	}
}

func TestRetainedClaudeRevisionsAreReplayOnlyWithOneRecoveryTarget(t *testing.T) {
	registration := Registration()
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		tests := []struct {
			name   string
			source task.PredicateRef
			target task.PredicateRef
		}{
			{name: "native", source: historicalNativeReferenceForMode(mode), target: ReferenceForMode(mode)},
			{name: "previous probe", source: previousNativeProbeReferenceForMode(mode), target: ReferenceForMode(mode)},
			{name: "pre-attribution", source: preAttributionReferenceForMode(mode), target: ReferenceForMode(mode)},
			{name: "interactive", source: historicalInteractionReference(mode), target: interactionReference(mode)},
			{name: "interactive previous probe", source: previousInteractionReference(mode), target: interactionReference(mode)},
			{name: "interactive pre-attribution", source: preAttributionInteractionReference(mode), target: interactionReference(mode)},
			{name: "interactive unscoped", source: unscopedInteractionReference(mode), target: interactionReference(mode)},
		}
		for _, test := range tests {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				resolved, resolveErr := registry.ResolveRecovery(test.source)
				if resolveErr != nil {
					t.Fatalf("resolve recovery: %v", resolveErr)
				}
				if !resolved.Target().Equal(test.target) {
					t.Fatalf("recovery target=%+v want=%+v", resolved.Target(), test.target)
				}
			})
		}
	}
}
