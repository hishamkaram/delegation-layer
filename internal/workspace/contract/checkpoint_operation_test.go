package contract

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func repeatHex(character byte, length int) string { return strings.Repeat(string(character), length) }

// operationFixture is the historical request shape whose canonical digests
// were recorded before checkpoint kinds existed.
func operationFixture(kind string) OperationRequest {
	request := OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: repeatHex('1', 32), OperationID: repeatHex('2', 32), WorkspaceID: repeatHex('3', 32),
		Kind: kind, Subject: Subject{Kind: SubjectOperation, ID: repeatHex('4', 32), SHA256: repeatHex('5', 64)},
		Deadline: "2026-10-21T12:00:00Z", WorkerExecutable: "/fixture/delegate", WorkerSHA256: repeatHex('6', 64),
		Supervisor: task.SupervisorRef{
			ClientExecutable: "/fixture/pueue-client", ClientSHA256: repeatHex('7', 64), ResolvedConfigSHA256: repeatHex('8', 64),
			Endpoint: "unix:/fixture/pueue.sock", ConfigPath: "/fixture/pueue.conf", ConfigDigest: repeatHex('9', 64), ObservedVersion: "fixture",
		},
	}
	switch kind {
	case OperationVerify:
		request.Arguments = []string{"make", "test"}
	case OperationRemove:
		request.Discard = true
	}
	return request
}

func ordinarySelection() *CheckpointSelection {
	return &CheckpointSelection{Kind: CheckpointOrdinary, Cwd: "/repo/sub", CwdIdentity: &DirectoryIdentity{Device: 1, Inode: 3}}
}

func checkpointRequest(selection *CheckpointSelection) OperationRequest {
	request := operationFixture(OperationCheckpoint)
	request.Checkpoint = selection
	if selection != nil && selection.Kind == CheckpointOrdinary {
		request.WorkspaceID = ""
	}
	return request
}

func removalRequest() OperationRequest {
	request := operationFixture(OperationCheckpointRemove)
	request.WorkspaceID = ""
	request.Subject = Subject{Kind: SubjectOperation, ID: repeatHex('2', 32), SHA256: repeatHex('f', 64)}
	request.OperationID = repeatHex('a', 32)
	return request
}

func TestOperationRequestHistoricalKindsKeepCanonicalDigests(t *testing.T) {
	historical := map[string]string{
		OperationProvision: "dbcc08af17c014a0b6bad2720c3228358010f6d3510596f7293ad8fb4294a66f",
		OperationCapture:   "27c367f214d1f16117250ae4ec19e5204ad110829472dbb4deefbcde06cdcd21",
		OperationVerify:    "1f5b3616fa9e24d029f4815f3ed0960576de15aa83914105df65aad2ec58c3c7",
		OperationRemove:    "8e959a48d758e87fa37dc22a21b22aec3db296718cbf42fae98357194e782863",
		OperationForget:    "e286566de374bed41635ddf97d6588a45e40445d932e3e58f5c9619447b279c9",
	}
	for kind, digest := range historical {
		t.Run(kind, func(t *testing.T) {
			request := operationFixture(kind)
			if _, err := ValidateOperation(request, request.RootID); err != nil {
				t.Fatalf("historical %s request is invalid: %v", kind, err)
			}
			data, err := task.MarshalCanonical(request)
			if err != nil {
				t.Fatal(err)
			}
			if got := task.ComputeSHA256(data); got != digest {
				t.Fatalf("canonical digest=%s, want historical %s:\n%s", got, digest, data)
			}
			var decoded OperationRequest
			if err = task.DecodeStrict(data, &decoded); err != nil {
				t.Fatalf("historical bytes no longer decode strictly: %v", err)
			}
			if decoded.Checkpoint != nil || !reflect.DeepEqual(decoded, request) {
				t.Fatalf("decoded=%+v, want %+v", decoded, request)
			}
		})
	}
}

func TestCheckpointRequestRoundTripsSelection(t *testing.T) {
	for _, request := range []OperationRequest{checkpointRequest(ordinarySelection()), checkpointRequest(&CheckpointSelection{Kind: CheckpointManaged})} {
		data, err := task.MarshalCanonical(request)
		if err != nil {
			t.Fatal(err)
		}
		var decoded OperationRequest
		if err = task.DecodeStrict(data, &decoded); err != nil {
			t.Fatalf("checkpoint request does not decode strictly: %v\n%s", err, data)
		}
		if !reflect.DeepEqual(decoded, request) {
			t.Fatalf("decoded=%+v, want %+v", decoded, request)
		}
	}
	managed, err := task.MarshalCanonical(checkpointRequest(&CheckpointSelection{Kind: CheckpointManaged}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(managed), `"checkpoint":{"kind":"managed"}}`+"\n") {
		t.Fatalf("managed selection duplicated caller state: %s", managed)
	}
}

func TestValidateOperationKeepsEarlierKindsWorkspaceScoped(t *testing.T) {
	for _, kind := range []string{OperationProvision, OperationCapture, OperationVerify, OperationRemove, OperationForget} {
		t.Run(kind, func(t *testing.T) {
			for _, selection := range []*CheckpointSelection{ordinarySelection(), {Kind: CheckpointManaged}} {
				request := operationFixture(kind)
				request.Checkpoint = selection
				if _, err := ValidateOperation(request, request.RootID); !errors.Is(err, task.ErrIdentityMismatch) {
					t.Fatalf("%s accepted checkpoint selection %+v: %v", kind, selection, err)
				}
			}
			request := operationFixture(kind)
			request.WorkspaceID = ""
			if _, err := ValidateOperation(request, request.RootID); !errors.Is(err, task.ErrInvalidTaskID) {
				t.Fatalf("%s without workspace error=%v, want invalid task ID", kind, err)
			}
		})
	}
}
