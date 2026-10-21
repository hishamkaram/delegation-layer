package contract

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// operationScopeCase expects success unless want names the refusal identity
// or refused marks a validator error without a sentinel identity.
type operationScopeCase struct {
	name    string
	request func() OperationRequest
	want    error
	refused bool
}

func checkpointCreationScopeCases() []operationScopeCase {
	ordinary := func(mutate func(*OperationRequest)) func() OperationRequest {
		return func() OperationRequest {
			request := checkpointRequest(ordinarySelection())
			mutate(&request)
			return request
		}
	}
	managed := func(mutate func(*OperationRequest)) func() OperationRequest {
		return func() OperationRequest {
			request := checkpointRequest(&CheckpointSelection{Kind: CheckpointManaged})
			mutate(&request)
			return request
		}
	}
	keep := func(*OperationRequest) {}
	return []operationScopeCase{
		{name: "ordinary", request: ordinary(keep)},
		{name: "managed", request: managed(keep)},
		{name: "absent selector has no default", request: ordinary(func(request *OperationRequest) { request.Checkpoint = nil }), want: task.ErrIdentityMismatch},
		{name: "absent selector with workspace has no default", request: managed(func(request *OperationRequest) { request.Checkpoint = nil }), want: task.ErrIdentityMismatch},
		{name: "empty selection kind", request: ordinary(func(request *OperationRequest) { request.Checkpoint.Kind = "" }), want: task.ErrIdentityMismatch},
		{name: "unknown selection kind", request: ordinary(func(request *OperationRequest) { request.Checkpoint.Kind = "Ordinary" }), want: task.ErrIdentityMismatch},
		{name: "ordinary with workspace", request: ordinary(func(request *OperationRequest) { request.WorkspaceID = repeatHex('3', 32) }), want: task.ErrIdentityMismatch},
		{name: "ordinary relative cwd", request: ordinary(func(request *OperationRequest) { request.Checkpoint.Cwd = "repo/sub" }), want: task.ErrIdentityMismatch},
		{name: "ordinary unclean cwd", request: ordinary(func(request *OperationRequest) { request.Checkpoint.Cwd = "/repo/../repo/sub" }), want: task.ErrIdentityMismatch},
		{name: "ordinary empty cwd", request: ordinary(func(request *OperationRequest) { request.Checkpoint.Cwd = "" }), want: task.ErrIdentityMismatch},
		{name: "ordinary without identity", request: ordinary(func(request *OperationRequest) { request.Checkpoint.CwdIdentity = nil }), want: task.ErrIdentityMismatch},
		{name: "ordinary without inode", request: ordinary(func(request *OperationRequest) { request.Checkpoint.CwdIdentity.Inode = 0 }), want: task.ErrIdentityMismatch},
		{name: "managed without workspace", request: managed(func(request *OperationRequest) { request.WorkspaceID = "" }), want: task.ErrInvalidTaskID},
		{name: "managed with malformed workspace", request: managed(func(request *OperationRequest) { request.WorkspaceID = "ws" }), want: task.ErrInvalidTaskID},
		{name: "managed duplicates cwd", request: managed(func(request *OperationRequest) { request.Checkpoint.Cwd = "/repo" }), want: task.ErrIdentityMismatch},
		{name: "managed duplicates identity", request: managed(func(request *OperationRequest) {
			request.Checkpoint.CwdIdentity = &DirectoryIdentity{Device: 1, Inode: 3}
		}), want: task.ErrIdentityMismatch},
		{name: "creation has arguments", request: ordinary(func(request *OperationRequest) { request.Arguments = []string{"x"} }), want: task.ErrIdentityMismatch},
		{name: "creation discards", request: managed(func(request *OperationRequest) { request.Discard = true }), want: task.ErrIdentityMismatch},
	}
}

func checkpointRemovalScopeCases() []operationScopeCase {
	removal := func(mutate func(*OperationRequest)) func() OperationRequest {
		return func() OperationRequest {
			request := removalRequest()
			mutate(&request)
			return request
		}
	}
	return []operationScopeCase{
		{name: "removal", request: removal(func(*OperationRequest) {})},
		{name: "removal with workspace", request: removal(func(request *OperationRequest) { request.WorkspaceID = repeatHex('3', 32) }), want: task.ErrIdentityMismatch},
		{name: "removal with selection", request: removal(func(request *OperationRequest) { request.Checkpoint = &CheckpointSelection{Kind: CheckpointManaged} }), want: task.ErrIdentityMismatch},
		{name: "removal subject is a task", request: removal(func(request *OperationRequest) { request.Subject.Kind = SubjectTask }), want: task.ErrIdentityMismatch},
		{name: "removal subject digest malformed", request: removal(func(request *OperationRequest) { request.Subject.SHA256 = "abc" }), refused: true},
		{name: "removal discards", request: removal(func(request *OperationRequest) { request.Discard = true }), want: task.ErrIdentityMismatch},
		{name: "removal has arguments", request: removal(func(request *OperationRequest) { request.Arguments = []string{"x"} }), want: task.ErrIdentityMismatch},
	}
}

func TestValidateOperationCheckpointSelectionAndWorkspaceCombinations(t *testing.T) {
	cases := append(checkpointCreationScopeCases(), checkpointRemovalScopeCases()...)
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := testCase.request()
			_, err := ValidateOperation(request, request.RootID)
			switch {
			case testCase.want != nil:
				if !errors.Is(err, testCase.want) {
					t.Fatalf("error=%v, want %v", err, testCase.want)
				}
			case testCase.refused:
				if err == nil {
					t.Fatal("invalid request accepted")
				}
			case err != nil:
				t.Fatalf("valid request rejected: %v", err)
			}
		})
	}
}
