package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type provisionRecoveryCase struct {
	name      string
	expectErr error
}

func recoverProvisionOperation(t *testing.T, fixture *workspacefixture.RecoveryFixture, name string) (*workspace.Operation, string, error) {
	t.Helper()
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: fixture.Resource.Request().TaskID, SHA256: fixture.Resource.Request().RequestSHA256}
	operation := workspacefixture.OpenRecoveryOperation(t, fixture, strings.Repeat("d", 32), workspace.OperationProvision, subject)
	request := operation.Request()
	if _, err := fixture.Resource.AssociateOperation(t.Context(), "provision.json", request); err != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.Restart(t)

	spec := OperationSpec{Purpose: "provision.json", Kind: workspace.OperationProvision, Subject: subject, Budget: time.Minute}
	switch name {
	case "conflicting":
		spec.Subject = workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("changed admission"))}
	case "unreadable":
		fixture.CorruptResourceRecord(t, "provision.json")
	}
	recovered, err := ensureWorkspaceOperation(t.Context(), fixture.Store, fixture.Resource, OperationOptions{}, spec)
	return recovered, request.OperationID, err
}

func assertProvisionRecovery(t *testing.T, recovered *workspace.Operation, recoverErr, expectedErr error, operationID string) {
	t.Helper()
	if expectedErr != nil {
		if recoverErr == nil || !errors.Is(recoverErr, expectedErr) {
			t.Fatalf("recovery error=%v, want %v", recoverErr, expectedErr)
		}
		if recovered != nil {
			if closeErr := recovered.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatal("recovery returned an operation after rejecting the durable purpose record")
		}
		return
	}
	if recoverErr != nil || recovered == nil {
		t.Fatalf("matching recovery operation=%v err=%v", recovered, recoverErr)
	}
	if recovered.Request().OperationID != operationID || recovered.Request().Kind != workspace.OperationProvision {
		t.Fatalf("recovered operation=%+v", recovered.Request())
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceProvisionPurposeRecoveryKeepsCreateOnceMapping(t *testing.T) {
	cases := []provisionRecoveryCase{
		{name: "matching"},
		{name: "conflicting", expectErr: task.ErrIdentityMismatch},
		{name: "unreadable", expectErr: task.ErrEvidenceFault},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			recovered, operationID, recoverErr := recoverProvisionOperation(t, fixture, testCase.name)
			assertProvisionRecovery(t, recovered, recoverErr, testCase.expectErr, operationID)
		})
	}
}
