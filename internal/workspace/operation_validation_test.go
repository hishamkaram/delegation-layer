package workspace

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestOperationResultValidationRejectsMalformedVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OperationResult)
	}{
		{name: "successful result has a reason", mutate: func(result *OperationResult) { result.ReasonCode = "unexpected" }},
		{name: "failed result has no reason", mutate: func(result *OperationResult) { result.Status = "failed" }},
		{name: "failed result has unsafe reason", mutate: func(result *OperationResult) { result.Status, result.ReasonCode = "failed", "worker failed" }},
		{name: "capture has unknown artifact", mutate: func(result *OperationResult) { result.Artifacts[0].Name = "diagnostic.log" }},
		{name: "capture has duplicate artifact", mutate: func(result *OperationResult) { result.Artifacts[1].Name = result.Artifacts[0].Name }},
		{name: "capture artifact is truncated", mutate: func(result *OperationResult) { result.Artifacts[0].Truncated = true }},
		{name: "successful capture is missing an artifact", mutate: func(result *OperationResult) { result.Artifacts = result.Artifacts[:1] }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			operation := newOperationValidationFixture(t, OperationCapture)
			valid := operationValidationCaptureResult(operation)
			result := valid
			result.Artifacts = append([]Artifact(nil), valid.Artifacts...)
			testCase.mutate(&result)
			if err := operation.RecordResult(t.Context(), result); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("RecordResult error=%v, want identity mismatch", err)
			}
		})
	}

	operation := newOperationValidationFixture(t, OperationCapture)
	valid := operationValidationCaptureResult(operation)
	if err := operation.RecordResult(t.Context(), valid); err != nil {
		t.Fatalf("valid RecordResult failed: %v", err)
	}
	got, err := operation.Result(t.Context())
	if err != nil {
		t.Fatalf("valid Result failed: %v", err)
	}
	if !reflect.DeepEqual(got, valid) {
		t.Fatalf("stored result=%+v, want %+v", got, valid)
	}
}

func TestOperationResultValidationKeepsFiniteFailureDiagnostics(t *testing.T) {
	operation := newOperationValidationFixture(t, OperationVerify)
	result := operationValidationVerifyResult(operation)
	result.Status = "failed"
	result.ReasonCode = "future_workspace_failure"
	result.CommandExit = nil
	if err := operation.RecordResult(t.Context(), result); err != nil {
		t.Fatalf("failed result with finite diagnostics was rejected: %v", err)
	}
	got, err := operation.Result(t.Context())
	if err != nil {
		t.Fatalf("failed Result failed: %v", err)
	}
	if !reflect.DeepEqual(got, result) {
		t.Fatalf("stored failure=%+v, want %+v", got, result)
	}
}

func TestOperationResultValidationRejectsMalformedVerifyArtifacts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OperationResult)
	}{
		{name: "verify has unknown artifact", mutate: func(result *OperationResult) { result.Artifacts[0].Name = "diff.patch" }},
		{name: "verify has duplicate artifact", mutate: func(result *OperationResult) { result.Artifacts[1].Name = result.Artifacts[0].Name }},
		{name: "verify omits artifact", mutate: func(result *OperationResult) {
			result.Artifacts[0].Omitted = "limit"
			result.Artifacts[0].Length, result.Artifacts[0].SHA256, result.Artifacts[0].Truncated = 0, "", false
		}},
		{name: "verify truncation has short length", mutate: func(result *OperationResult) { result.Artifacts[0].Length = ArtifactLimit - 1 }},
		{name: "successful verify is missing an artifact", mutate: func(result *OperationResult) { result.Artifacts = result.Artifacts[:1] }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			operation := newOperationValidationFixture(t, OperationVerify)
			result := operationValidationVerifyResult(operation)
			testCase.mutate(&result)
			if err := operation.RecordResult(t.Context(), result); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("RecordResult error=%v, want identity mismatch", err)
			}
		})
	}
}

func TestOperationResultValidationAcceptsProducerShapes(t *testing.T) {
	cases := []struct {
		name string
		kind string
		make func(*Operation) OperationResult
	}{
		{name: "capture allows omitted overflow", kind: OperationCapture, make: operationValidationCaptureResultWithOmission},
		{name: "verify allows nonzero command and truncation", kind: OperationVerify, make: operationValidationVerifyResult},
		{name: "lifecycle operation has no artifacts", kind: OperationProvision, make: operationValidationProvisionResult},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			operation := newOperationValidationFixture(t, testCase.kind)
			result := testCase.make(operation)
			if err := operation.RecordResult(t.Context(), result); err != nil {
				t.Fatalf("RecordResult failed: %v", err)
			}
			if _, err := operation.Result(t.Context()); err != nil {
				t.Fatalf("Result failed: %v", err)
			}
		})
	}
}

func TestOperationResultValidationRejectsMalformedStoredResult(t *testing.T) {
	operation := newOperationValidationFixture(t, OperationVerify)
	result := operationValidationVerifyResult(operation)
	result.ReasonCode = "unexpected"
	if _, err := operation.control.PutContext(t.Context(), "result.json", result); err != nil {
		t.Fatalf("seed malformed result: %v", err)
	}
	if _, err := operation.Result(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("Result error=%v, want identity mismatch", err)
	}
}

func newOperationValidationFixture(t *testing.T, kind string) *Operation {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	digest := testSHA('d')
	request := OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: testID('e'), WorkspaceID: testID('f'),
		Kind: kind, Subject: subjectFor('a'), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/bin/worker", WorkerSHA256: digest,
		Supervisor: task.SupervisorRef{
			ClientExecutable: "/fixture/pueue-client", ClientSHA256: testSHA('b'), ResolvedConfigSHA256: testSHA('c'),
			Endpoint: "unix:/fixture/pueue.sock", ConfigPath: "/fixture/pueue.conf", ConfigDigest: testSHA('1'), ObservedVersion: "fixture",
		},
	}
	if kind == OperationVerify {
		request.Arguments = []string{"/bin/true"}
	}
	operation, err := OpenOperation(t.Context(), store, request)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return operation
}

func operationValidationBaseResult(operation *Operation, status string) OperationResult {
	request := operation.Request()
	return OperationResult{SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: operation.Digest(), Status: status}
}

func operationValidationCaptureResult(operation *Operation) OperationResult {
	result := operationValidationBaseResult(operation, "succeeded")
	result.Artifacts = []Artifact{
		{Name: "diff.patch", Length: 0, SHA256: testSHA('2')},
		{Name: "name-status.z", Length: 0, SHA256: testSHA('3')},
	}
	return result
}

func operationValidationCaptureResultWithOmission(operation *Operation) OperationResult {
	result := operationValidationCaptureResult(operation)
	result.Artifacts[0] = Artifact{Name: "diff.patch", Omitted: "limit"}
	return result
}

func operationValidationVerifyResult(operation *Operation) OperationResult {
	result := operationValidationBaseResult(operation, "succeeded")
	exitCode := 7
	result.CommandExit = &exitCode
	result.Artifacts = []Artifact{
		{Name: "stdout", Length: ArtifactLimit, SHA256: testSHA('4'), Truncated: true},
		{Name: "stderr", Length: 0, SHA256: testSHA('5')},
	}
	return result
}

func operationValidationProvisionResult(operation *Operation) OperationResult {
	return operationValidationBaseResult(operation, "succeeded")
}
