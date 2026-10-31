package workspaces

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// wrappedError is a single-cause wrapper, the shape stage and context wrapping take.
type wrappedError struct{ cause error }

func (e wrappedError) Error() string { return "wrapped: " + e.cause.Error() }

func (e wrappedError) Unwrap() error { return e.cause }

func TestRequireManagedRunnerJudgesOnlyManagedWorkspaceRequests(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		request DispatchRequest
		refused bool
	}{
		{"no selector, managed", DispatchRequest{RunnerOwnership: task.RunnerOwnershipManaged}, false},
		{"no selector, custom", DispatchRequest{RunnerOwnership: task.RunnerOwnershipCustom}, false},
		{"no selector, unset", DispatchRequest{}, false},
		{"isolation, managed", DispatchRequest{Isolation: "git-worktree", RunnerOwnership: task.RunnerOwnershipManaged}, false},
		{"workspace, managed", DispatchRequest{WorkspaceID: strings.Repeat("a", 32), RunnerOwnership: task.RunnerOwnershipManaged}, false},
		{"isolation, custom", DispatchRequest{Isolation: "git-worktree", RunnerOwnership: task.RunnerOwnershipCustom}, true},
		{"workspace, custom", DispatchRequest{WorkspaceID: strings.Repeat("a", 32), RunnerOwnership: task.RunnerOwnershipCustom}, true},
		{"isolation, unset", DispatchRequest{Isolation: "git-worktree"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := RequireManagedRunner(testCase.request)
			if (err != nil) != testCase.refused {
				t.Fatalf("RequireManagedRunner(%+v) = %v, refused=%v", testCase.request, err, testCase.refused)
			}
		})
	}
}

// TestManagedRunnerRequestRefusalKeepsOldIdentityAndNarrowLeaf proves the
// refusal still matches task.ErrInvalidPermit, stays recognizable through
// ordinary wrapping, and never widens: the bare old sentinel and any joined
// fault are not this refusal.
func TestManagedRunnerRequestRefusalKeepsOldIdentityAndNarrowLeaf(t *testing.T) {
	refusal := RequireManagedRunner(DispatchRequest{Isolation: "git-worktree", RunnerOwnership: task.RunnerOwnershipCustom})
	if !errors.Is(refusal, task.ErrInvalidPermit) {
		t.Fatalf("refusal lost task.ErrInvalidPermit: %v", refusal)
	}
	if !task.AllErrorCauses(wrappedError{refusal}, IsManagedRunnerRequest) {
		t.Fatal("wrapped refusal is not recognized")
	}
	if task.AllErrorCauses(task.ErrInvalidPermit, IsManagedRunnerRequest) || task.AllErrorCauses(wrappedError{task.ErrInvalidPermit}, IsManagedRunnerRequest) {
		t.Fatal("an unrelated invalid permit was recognized as the runner refusal")
	}
	if task.AllErrorCauses(errors.Join(refusal, errors.New("storage fault")), IsManagedRunnerRequest) {
		t.Fatal("a joined fault was recognized as the runner refusal")
	}
	if errors.Unwrap(refusal) != nil {
		t.Fatal("refusal exposes a cause that would widen its identity")
	}
}

// claimsEveryTargetError is an unrelated error whose Is method matches anything.
type claimsEveryTargetError struct{}

func (claimsEveryTargetError) Error() string { return "unrelated" }

func (claimsEveryTargetError) Is(error) bool { return true }

// TestManagedRunnerRequestPredicateIsExactLeafIdentity proves the leaf
// predicate accepts only the owned value itself. Ordinary wrapping is the
// traversal's job, and an unrelated Is method never owns the leaf.
func TestManagedRunnerRequestPredicateIsExactLeafIdentity(t *testing.T) {
	refusal := RequireManagedRunner(DispatchRequest{Isolation: "git-worktree", RunnerOwnership: task.RunnerOwnershipCustom})
	if !IsManagedRunnerRequest(refusal) {
		t.Fatal("the owned refusal is not its own exact leaf")
	}
	wrapped := wrappedError{refusal}
	if IsManagedRunnerRequest(wrapped) {
		t.Fatal("a wrapper was accepted as the exact leaf")
	}
	if !task.AllErrorCauses(wrapped, IsManagedRunnerRequest) {
		t.Fatal("whole-error traversal lost an ordinarily wrapped refusal")
	}
	if IsManagedRunnerRequest(claimsEveryTargetError{}) || task.AllErrorCauses(claimsEveryTargetError{}, IsManagedRunnerRequest) {
		t.Fatal("an unrelated Is method was accepted as the owned leaf")
	}
}
