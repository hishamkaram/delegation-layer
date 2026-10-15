package codex

import (
	"errors"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInteractiveContinuationBuildsFreshNativeProfile(t *testing.T) {
	request, now := profileReconstructionFixture(t)
	request.RequestedConfig.Interactive = true
	request.PriorSession = &task.PriorSession{
		Provider: Provider, ConversationID: testThreadID, PredecessorTaskID: testTaskID,
	}
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	profile := finalizeProfile(t, candidate, now)
	if !profile.Plan.Predicate.Equal(interactionReference(Mode)) {
		t.Fatalf("interactive predicate=%+v", profile.Plan.Predicate)
	}
	wantArguments := []string{"app-server", "--listen", "stdio://", "--enable", "default_mode_request_user_input"}
	if !slices.Equal(profile.Plan.Arguments, wantArguments) {
		t.Fatalf("interactive argv=%q want=%q", profile.Plan.Arguments, wantArguments)
	}
	if len(profile.Plan.OutputArtifacts) != 0 || profile.Plan.OutputWriterContract != "" || profile.Identity != nil {
		t.Fatalf("interactive profile retained noninteractive output/identity: %+v", profile.Plan)
	}
	if profile.Interaction == nil {
		t.Fatal("interactive profile has no interaction protocol")
	}
	protocol, err := profile.Interaction(task.SessionExpectation{Required: true, ID: testThreadID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recordProtocol, ok := protocol.(execution.CurrentRecordInteractionProtocol)
	if !ok || !recordProtocol.UseRecordCapture() {
		t.Fatalf("interactive protocol is not the current record-capture protocol: %T", protocol)
	}
	if !hasCodexInterpreter(interactionReference(Mode)) {
		t.Fatal("current Codex interactive interpreter is not registered")
	}

	meta := task.MetaRecord{
		Predicate: task.PredicateRef{Adapter: Provider, Mode: Mode, Version: predicateVersion, SHA256: ""},
		EffectiveConfig: task.EffectiveConfig{
			Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision},
		},
	}
	if _, err := PrepareExistingCandidate(request, meta); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("noninteractive predecessor predicate was reused: %v", err)
	}
}

func hasCodexInterpreter(reference task.PredicateRef) bool {
	for _, interpreter := range Registration().Interpreters {
		if interpreter.Reference().Equal(reference) {
			return true
		}
	}
	return false
}
