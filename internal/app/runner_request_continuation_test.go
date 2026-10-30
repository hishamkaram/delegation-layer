package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

const continuationRunnerConflictReason = "continuation_runner_conflict"

// continueEntryPoint is the public command whose runner override this suite
// judges. Only continue reaches ValidateContinuationSelection (through
// tasklifecycle.ValidateContinuationMetadata at continue.go); dispatch
// --resume-task builds its continuation from the predecessor's records and
// never calls it, so a runner there is judged by dispatch's ordinary runner and
// provider inspection, not by the continuation comparison.
const continueEntryPoint = "continue --task"

// runWithRunner runs entryPoint with --runner placed before or after the
// command; the fixture's predecessor is the one real valid continuation source.
func runWithRunner(t *testing.T, fixture *recordedEnvironmentFixture, entryPoint, runner string, beforeCommand bool) (int, Response, string) {
	t.Helper()
	args := fixture.entryPointArguments(t, entryPoint, fixture.supervisor.Binding().ConfigPath)
	if beforeCommand {
		args = slices.Insert(args, 2, "--runner", runner)
	} else {
		args = append(args, "--runner", runner)
	}
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, fixture.deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("%s exit=%d stdout=%q stderr=%q: %v", entryPoint, code, &stdout, &stderr, err)
	}
	return code, response, stderr.String()
}

func TestContinuationRunnerOverrideMismatchIsCorrectable(t *testing.T) {
	testCase := recordedEnvironmentCases()[0]
	for _, beforeCommand := range []bool{false, true} {
		t.Run("before="+strconv.FormatBool(beforeCommand), func(t *testing.T) {
			fixture := newRecordedEnvironmentFixture(t, testCase)
			other := filepath.Join(testsupport.CanonicalTempDir(t), "other-delegate-run")
			writeRunnerFixture(t, other, "other runner")
			code, response, _ := runWithRunner(t, fixture, continueEntryPoint, other, beforeCommand)
			if code != 2 {
				t.Fatalf("exit=%d response=%+v", code, response)
			}
			requireRunnerRequestFailure(t, response, continuationRunnerConflictReason, "validate-continuation", "not_created", "correct_request")
			if strings.Contains(response.Error, other) {
				t.Fatal("continuation error exposed the requested runner path")
			}
			fixture.assertPredecessorUnchanged(t)
			assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 0)
		})
	}
}

func TestContinuationRunnerOverrideMismatchHumanError(t *testing.T) {
	fixture := newRecordedEnvironmentFixture(t, recordedEnvironmentCases()[0])
	other := filepath.Join(testsupport.CanonicalTempDir(t), "other-delegate-run")
	writeRunnerFixture(t, other, "other runner")
	args := fixture.entryPointArguments(t, continueEntryPoint, fixture.supervisor.Binding().ConfigPath)
	args = append(slices.DeleteFunc(args, func(arg string) bool { return arg == "--json" }), "--runner", other)
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr, fixture.deps); code != 2 {
		t.Fatalf("human continuation exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if message := stderr.String(); !strings.Contains(message, invalidRequestMessage) || !strings.Contains(message, "omit --runner") || strings.Contains(message, staleIdentityMessage) || strings.Contains(message, other) {
		t.Fatalf("human continuation error is not actionable: %q", message)
	}
}

// TestContinuationRunnerOverrideEqualOrOmittedIsAdmitted keeps the accepted
// cases: the predecessor's own recorded runner and no override both admit.
func TestContinuationRunnerOverrideEqualOrOmittedIsAdmitted(t *testing.T) {
	testCase := recordedEnvironmentCases()[0]
	for _, override := range []string{"omitted", "equal"} {
		t.Run(override, func(t *testing.T) {
			fixture := newRecordedEnvironmentFixture(t, testCase)
			var code int
			var response Response
			if override == "omitted" {
				code, response = fixture.run(t, continueEntryPoint, fixture.supervisor.Binding().ConfigPath)
			} else {
				code, response, _ = runWithRunner(t, fixture, continueEntryPoint, fixture.runner, false)
			}
			if code != 0 || response.TaskRecord != "created" || response.TaskID == "" || response.Failure != nil {
				t.Fatalf("exit=%d response=%+v", code, response)
			}
			fixture.assertPredecessorUnchanged(t)
			assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
		})
	}
}

// TestObservationIgnoresRunnerOverrideThatContinueRefuses proves the refusal
// belongs to continuation alone: status and collect behave identically with and
// without a different --runner on the same valid predecessor, while continue
// with that value is refused.
func TestObservationIgnoresRunnerOverrideThatContinueRefuses(t *testing.T) {
	fixture := newRecordedEnvironmentFixture(t, recordedEnvironmentCases()[0])
	other := filepath.Join(testsupport.CanonicalTempDir(t), "other-delegate-run")
	writeRunnerFixture(t, other, "other runner")
	observe := func(command string, extra ...string) (int, Response) {
		var stdout, stderr bytes.Buffer
		args := append([]string{command, fixture.predecessorID, "--root", fixture.root, "--json"}, extra...)
		code := Run(args, &stdout, &stderr, fixture.deps)
		var response Response
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("%s exit=%d stdout=%q stderr=%q: %v", command, code, &stdout, &stderr, err)
		}
		return code, response
	}
	for _, command := range []string{"status", "collect"} {
		plainCode, plain := observe(command)
		overrideCode, override := observe(command, "--runner", other)
		if plainCode != overrideCode || plain.Status != override.Status || plain.Publication != override.Publication || (plain.Failure == nil) != (override.Failure == nil) {
			t.Fatalf("%s changed with a runner override: plain=%d %+v override=%d %+v", command, plainCode, plain, overrideCode, override)
		}
		if plainCode != 0 || plain.Publication != "committed" || plain.Outcome == nil {
			t.Fatalf("%s did not observe the valid predecessor: exit=%d %+v", command, plainCode, plain)
		}
	}
	code, refused, _ := runWithRunner(t, fixture, continueEntryPoint, other, false)
	if code != 2 || refused.Failure == nil || refused.Failure.ReasonCode != continuationRunnerConflictReason {
		t.Fatalf("continue accepted the different runner: exit=%d %+v", code, refused)
	}
	fixture.assertPredecessorUnchanged(t)
}
