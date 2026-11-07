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
	if registration.Prepare == nil {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	registry := retainedRegistry(t)
	for _, ref := range []task.PredicateRef{
		ReferenceForMode(Mode),
		ReferenceForMode(WorkspaceWriteMode),
		historicalNativeReferenceForMode(Mode),
		historicalNativeReferenceForMode(WorkspaceWriteMode),
		legacyNativeReferenceForMode(Mode),
		legacyNativeReferenceForMode(WorkspaceWriteMode),
		legacyPortableReferenceForMode(Mode),
		legacyPortableReferenceForMode(WorkspaceWriteMode),
		LegacyReference(),
		previousNativeProbeReferenceForMode(Mode),
		previousNativeProbeReferenceForMode(WorkspaceWriteMode),
		preAttributionReferenceForMode(Mode),
		preAttributionReferenceForMode(WorkspaceWriteMode),
		preAttributionInteractionReference(Mode),
		preAttributionInteractionReference(WorkspaceWriteMode),
		unscopedReference(WorkspaceWriteMode),
		unscopedInteractionReference(Mode),
		unscopedInteractionReference(WorkspaceWriteMode),
		unscopedReference(Mode),
		scopedReference(Mode),
		scopedReference(WorkspaceWriteMode),
		scopedInteractionReference(Mode),
		scopedInteractionReference(WorkspaceWriteMode),
		projectionReference(Mode),
		projectionReference(WorkspaceWriteMode),
		advisoryReference(Mode),
		advisoryReference(WorkspaceWriteMode),
		advisoryInteractionReference(Mode),
		advisoryInteractionReference(WorkspaceWriteMode),
	} {
		if _, err := registry.Resolve(ref); err != nil {
			t.Fatalf("required predicate %+v is not resolvable: %v", ref, err)
		}
	}
}

func TestInteractionRegistration(t *testing.T) {
	description := Description()
	registry := retainedRegistry(t)
	for _, ref := range []task.PredicateRef{
		interactionReference(Mode), interactionReference(WorkspaceWriteMode),
		historicalInteractionReference(Mode), historicalInteractionReference(WorkspaceWriteMode),
		previousInteractionReference(Mode), previousInteractionReference(WorkspaceWriteMode),
	} {
		if _, err := registry.Resolve(ref); err != nil {
			t.Fatalf("interactive predicate %+v is not resolvable: %v", ref, err)
		}
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
