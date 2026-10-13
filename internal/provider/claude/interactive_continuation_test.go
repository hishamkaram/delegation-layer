package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInteractiveContinuationBuildsFreshNativeProfile(t *testing.T) {
	home := canonicalTempDir(t)
	workspace := canonicalTempDir(t)
	bin := t.TempDir()
	if writeErr := os.WriteFile(filepath.Join(bin, "claude"), []byte("claude fixture"), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)

	request := profileRequest()
	request.CanonicalCwd = workspace
	request.RequestedConfig.Interactive = true
	request.PriorSession = &task.PriorSession{
		Provider: Provider, ConversationID: "11111111-1111-4111-8111-111111111111",
		PredecessorTaskID: "abcdef0123456789abcdef0123456789",
	}
	profile := finalizedInteractiveProfile(t, request)
	assertInteractiveLaunch(t, request, profile)
	assertInteractiveProtocol(t, request, profile)
	assertNoninteractivePredecessorRejected(t, request)
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func finalizedInteractiveProfile(t *testing.T, request task.TaskRecord) commonprovider.PreparedProfile {
	t.Helper()
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable,
		SHA256:     candidate.Inspection.ExecutableSHA256,
		Version:    "Claude fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func assertInteractiveLaunch(t *testing.T, request task.TaskRecord, profile commonprovider.PreparedProfile) {
	t.Helper()
	if !profile.Plan.Predicate.Equal(interactionReference(Mode)) {
		t.Fatalf("interactive predicate=%+v", profile.Plan.Predicate)
	}
	wantArguments, inputs, err := printArgumentsWithPermission(request, nativeDefaultPermission(Mode))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profile.Plan.Arguments, wantArguments) || len(inputs) != 0 || len(profile.Plan.InputFiles) != 0 {
		t.Fatalf("interactive launch contract argv=%q inputs=%+v want=%q", profile.Plan.Arguments, profile.Plan.InputFiles, wantArguments)
	}
	if strings.Contains(strings.Join(profile.Plan.Arguments, " "), "--input-format text") ||
		strings.Contains(strings.Join(profile.Plan.Arguments, " "), "--permission-prompts none") ||
		!slices.Contains(profile.Plan.Arguments, "--resume") {
		t.Fatalf("interactive profile reused noninteractive launch flags: %q", profile.Plan.Arguments)
	}
	if profile.Identity != nil || profile.Plan.OutputWriterContract != "" || len(profile.Plan.OutputArtifacts) != 0 {
		t.Fatalf("interactive profile retained noninteractive result plumbing: %+v", profile.Plan)
	}
}

func assertInteractiveProtocol(t *testing.T, request task.TaskRecord, profile commonprovider.PreparedProfile) {
	t.Helper()
	if profile.Interaction == nil {
		t.Fatal("interactive profile has no interaction protocol")
	}
	protocol, err := profile.Interaction(task.SessionExpectation{Required: true, ID: request.PriorSession.ConversationID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recordProtocol, ok := protocol.(execution.CurrentRecordInteractionProtocol)
	if !ok || !recordProtocol.UseRecordCapture() {
		t.Fatalf("interactive protocol is not the current record-capture protocol: %T", protocol)
	}
	if !hasClaudeInterpreter(interactionReference(Mode)) {
		t.Fatal("current Claude interactive interpreter is not registered")
	}
}

func assertNoninteractivePredecessorRejected(t *testing.T, request task.TaskRecord) {
	t.Helper()
	meta := task.MetaRecord{
		Predicate:       ReferenceForMode(Mode),
		EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}},
	}
	if _, err := PrepareExistingCandidate(request, meta); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("noninteractive predecessor predicate was reused: %v", err)
	}
}

func hasClaudeInterpreter(reference task.PredicateRef) bool {
	for _, interpreter := range Registration().Interpreters {
		if interpreter.Reference().Equal(reference) {
			return true
		}
	}
	return false
}
