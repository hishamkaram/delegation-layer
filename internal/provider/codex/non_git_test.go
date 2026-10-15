package codex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestNativeNonGitLaunchPreservesRecordedContracts(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, historical := range []bool{false, true} {
			for _, resume := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/historical=%t/resume=%t", mode, historical, resume), func(t *testing.T) {
					testRecordedNativeLaunch(t, mode, historical, resume)
				})
			}
		}
	}
}

func testRecordedNativeLaunch(t *testing.T, mode string, historical, resume bool) {
	t.Helper()
	request, _ := profileReconstructionFixture(t)
	request.Mode, request.RequestedConfig.Permission = mode, mode
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "custom-model", "high"
	if err := os.RemoveAll(filepath.Join(request.CanonicalCwd, ".git")); err != nil {
		t.Fatal(err)
	}
	ref := ReferenceForMode(mode)
	if historical {
		ref = historicalV3ReferenceForMode(mode)
	}
	meta := task.MetaRecord{
		Predicate: ref, RuntimePreflightSHA256: testSpecHash,
		EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}},
	}
	request.PriorSession = nil
	if resume {
		request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testThreadID, PredecessorTaskID: testTaskID}
	}
	candidate, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	args := profile.Plan.Arguments
	if slices.Contains(args, "--skip-git-repo-check") == historical ||
		slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--skip-git-repo-check") == historical ||
		!profile.Plan.Predicate.Equal(ref) || profile.Plan.OutputArtifacts[0].ArgumentIndex != 15 ||
		!slices.Contains(args, `model_reasoning_effort="high"`) || !slices.Contains(args, "custom-model") ||
		slices.Contains(args, "resume") != resume {
		t.Fatalf("historical=%t resume=%t profile=%+v requirements=%+v", historical, resume, profile, candidate.Inspection.Runtime)
	}
	if historical {
		assertHistoricalNativeArguments(t, request, args, resume)
	}
}

func assertHistoricalNativeArguments(t *testing.T, request task.TaskRecord, args []string, resume bool) {
	t.Helper()
	want := []string{
		"exec", "--json", "--color", "never", "--sandbox", request.Mode, "-c", `approval_policy="never"`,
		"--model", "custom-model", "-c", `model_reasoning_effort="high"`, "--cd", request.CanonicalCwd, "--output-last-message", "",
	}
	if resume {
		want = append(want, "resume", testThreadID)
	}
	want = append(want, "-")
	if !slices.Equal(args, want) {
		t.Fatalf("historical launch changed: got=%q want=%q", args, want)
	}
}

func TestCodexRecoveryHasOneProductionTarget(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration(), ReviewRegistration())
	if err != nil {
		t.Fatal(err)
	}
	registry := catalog.Registry()
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		sources := []task.PredicateRef{historicalCurrentRuntimeReferenceForMode(mode), historicalRuntimeReferenceForMode(mode)}
		if mode == Mode {
			sources = append(sources, LegacyReference())
		}
		for _, source := range sources {
			recovery, err := registry.ResolveRecovery(source)
			if err != nil || !recovery.Target().Equal(historicalV3ReferenceForMode(mode)) {
				t.Fatalf("source=%+v recovery=%v err=%v", source, recovery, err)
			}
		}
		for _, source := range []task.PredicateRef{historicalV3ReferenceForMode(mode), ReferenceForMode(mode)} {
			if _, err := registry.Resolve(source); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.ResolveRecovery(source); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
				t.Fatalf("unexpected recovery for %+v: %v", source, err)
			}
		}
	}
}

func TestStartupDiagnosticUsesOnlySealedFacts(t *testing.T) {
	for _, test := range []struct {
		name        string
		stdout      []byte
		exit        int
		invocation  string
		runnerError string
		want        string
	}{
		{name: "early exit", exit: 1, want: "provider_startup_failed"},
		{name: "zero exit", want: "invalid_output"},
		{name: "signal", exit: -1, want: "provider_failed"},
		{name: "runner error", exit: 1, runnerError: "runner failure", want: "provider_failed"},
		{name: "start failure", exit: 1, invocation: task.InvocationStartFailed, runnerError: "start failure", want: "provider_failed"},
		{name: "malformed nonempty", stdout: []byte("{"), exit: 1, want: "malformed_json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stderr := &trackingEOFReader{data: []byte("private-token\nNot inside a trusted directory\n")}
			seal := validSeal(test.stdout, nil)
			seal.ExitCode, seal.Error = test.exit, test.runnerError
			if test.invocation != "" {
				seal.InvocationState = test.invocation
			}
			var answer bytes.Buffer
			result, err := NewInterpreter().Evaluate(predicate.Input{Seal: seal}, &testEvidence{stdout: test.stdout, stderrReader: stderr}, &answer)
			if err != nil || result.Verdict != task.VerdictRejected || len(result.Diagnostics) != 1 ||
				result.Diagnostics[0].Code != test.want || !stderr.eof || answer.Len() != 0 {
				t.Fatalf("result=%+v err=%v drained=%t", result, err, stderr.eof)
			}
			if bytes.Contains([]byte(result.Diagnostics[0].Message), []byte("private-token")) {
				t.Fatal("stderr leaked into diagnostic")
			}
			seal.Predicate = historicalV3ReferenceForMode(Mode)
			old, err := newHistoricalV3Interpreter(Mode).Evaluate(predicate.Input{Seal: seal}, &testEvidence{stdout: test.stdout}, &answer)
			if err != nil || len(old.Diagnostics) != 1 {
				t.Fatalf("historical diagnostic changed: %+v err=%v", old, err)
			}
			assertHistoricalStartupDiagnostic(t, seal, old.Diagnostics, result.Diagnostics, test.want)
		})
	}
	readErr := errors.New("stderr read failed")
	seal := alteredSeal(nil, nil, func(seal *task.ProviderExitRecord) { seal.ExitCode = 1 })
	_, err := NewInterpreter().Evaluate(predicate.Input{Seal: seal}, &testEvidence{stderrReader: &errorReader{err: readErr}}, &bytes.Buffer{})
	if !errors.Is(err, readErr) {
		t.Fatalf("lost stderr read error: %v", err)
	}
}

func TestHistoricalV3ContractHashesRemainImmutable(t *testing.T) {
	if task.ComputeSHA256([]byte(contractV3)) != "2d549451e555032a58bdd46fbb48a31fd0b57d17df0a6aa267c1d7d8ee3a6d18" {
		t.Fatal("historical read-only v3 contract changed")
	}
	if task.ComputeSHA256([]byte(workspaceWriteContractV3)) != "68a71daf9ad69ef5864b7402ca96fd4bf48f3e972cd71f8949c63f6b027fd7c2" {
		t.Fatal("historical workspace-write v3 contract changed")
	}
}

func assertHistoricalStartupDiagnostic(t *testing.T, seal task.ProviderExitRecord, old, current []task.OutputDiagnostic, want string) {
	t.Helper()
	if want == "provider_startup_failed" {
		if old[0] != currentDiagnostic(nil, sealRefusal(seal)) {
			t.Fatalf("historical startup diagnostic changed: %+v", old)
		}
	} else if !reflect.DeepEqual(old, current) {
		t.Fatalf("historical diagnostic changed: old=%+v current=%+v", old, current)
	}
}
