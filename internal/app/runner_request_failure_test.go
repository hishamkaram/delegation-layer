package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// runnerRequestRefusals returns each owned refusal as its producer returns it,
// with the stage the public command attaches.
func runnerRequestRefusals(t *testing.T) []struct {
	name, stage, reason string
	err                 error
} {
	t.Helper()
	managed := workspaces.RequireManagedRunner(workspaces.DispatchRequest{Isolation: "git-worktree", RunnerOwnership: task.RunnerOwnershipCustom})
	continuation := supervisorcore.ValidateContinuationSelection(t.TempDir(), supervisorcore.AdmissionSelection{Runner: "/other"}, &task.MetaRecord{RunnerExecutable: "/recorded"})
	if managed == nil || continuation == nil {
		t.Fatalf("producers accepted a conflicting runner: managed=%v continuation=%v", managed, continuation)
	}
	return []struct {
		name, stage, reason string
		err                 error
	}{
		{"managed workspace", "prepare-workspace", managedWorkspaceRunnerReason, managed},
		{"continuation", "validate-continuation", continuationRunnerReason, continuation},
	}
}

func TestRunnerRequestRefusalClassifiesAsCorrectableRequest(t *testing.T) {
	for _, refusal := range runnerRequestRefusals(t) {
		t.Run(refusal.name, func(t *testing.T) {
			wrapped := admission.WithStage(refusal.stage, fmt.Errorf("wrapped: %w", refusal.err))
			for record, next := range map[string]string{"not_created": "correct_request", "unknown": "stop_and_report"} {
				failure := dispatchFailure(wrapped, record)
				if failure.Code != "invalid_request" || failure.ReasonCode != refusal.reason || failure.Stage != refusal.stage || failure.NextAction != next {
					t.Fatalf("record %s failure = %+v, want invalid_request/%s at %s with %s", record, failure, refusal.reason, refusal.stage, next)
				}
			}
			if code := classifyCode(wrapped, 1); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

// TestRunnerRequestRefusalJoinedFaultKeepsExistingClassification proves one
// recognized leaf cannot relabel a failure that also carries any other fault.
func TestRunnerRequestRefusalJoinedFaultKeepsExistingClassification(t *testing.T) {
	faults := map[string]error{
		"storage":  errors.New("storage fault"),
		"close":    errors.New("store close fault"),
		"evidence": task.ErrEvidenceFault,
		"identity": task.ErrIdentityMismatch,
	}
	for _, refusal := range runnerRequestRefusals(t) {
		for name, fault := range faults {
			t.Run(refusal.name+"/"+name, func(t *testing.T) {
				joined := admission.WithStage(refusal.stage, errors.Join(refusal.err, fault))
				failure := dispatchFailure(joined, "not_created")
				if failure.Code == "invalid_request" || failure.ReasonCode != "" || failure.NextAction != "stop_and_report" {
					t.Fatalf("joined fault was relabeled as a correctable request: %+v", failure)
				}
				if dispatchPublicError(failure, joined) != nil {
					t.Fatal("joined fault received runner correction guidance")
				}
			})
		}
	}
}

// TestRunnerRequestRefusalLeavesOtherIdentityFaultsUnchanged keeps genuine
// saved-identity, permit, and conflict failures on their existing codes.
func TestRunnerRequestRefusalLeavesOtherIdentityFaultsUnchanged(t *testing.T) {
	configConflict := supervisorcore.ValidateContinuationSelection(t.TempDir(), supervisorcore.AdmissionSelection{ConfigPath: "/external.yml"}, &task.MetaRecord{
		RunnerExecutable: "/recorded", SupervisorConfig: task.SupervisorRef{ConfigPath: "/saved.yml"},
	})
	for _, testCase := range []struct {
		name, stage, record string
		err                 error
		code, next          string
	}{
		{"saved identity at continuation", "validate-continuation", "not_created", task.ErrIdentityMismatch, "task_identity_mismatch", "stop_and_report"},
		{"wrapped saved identity", "validate-continuation", "not_created", fmt.Errorf("%w: saved record differs", task.ErrIdentityMismatch), "task_identity_mismatch", "stop_and_report"},
		{"unstarted retry runner mismatch", "existing-task", "created", task.ErrIdentityMismatch, "task_identity_mismatch", "check_status"},
		{"explicit supervisor config conflict", "validate-continuation", "not_created", configConflict, "task_request_conflict", "stop_and_report"},
		{"other invalid permit", "prepare-workspace", "not_created", task.ErrInvalidPermit, "workspace_preparation_failed", "stop_and_report"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			failure := dispatchFailure(admission.WithStage(testCase.stage, testCase.err), testCase.record)
			if failure.Code != testCase.code || failure.ReasonCode != "" || failure.NextAction != testCase.next {
				t.Fatalf("failure = %+v, want %s with %s and no reason", failure, testCase.code, testCase.next)
			}
		})
	}
}
