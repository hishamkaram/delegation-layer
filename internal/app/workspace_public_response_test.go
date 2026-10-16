package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestManagedAdmissionFailureSurvivesPublicJSONWriter(t *testing.T) {
	for _, command := range []string{"dispatch", "continue"} {
		for _, state := range []string{"provisioning", "busy", "preparation_failed"} {
			t.Run(command+"/"+state, func(t *testing.T) {
				assertManagedAdmissionWireRecovery(t, command, state)
			})
		}
	}
}

func assertManagedAdmissionWireRecovery(t *testing.T, command, state string) {
	t.Helper()
	response := newResponse(command)
	response.TaskRecord = "not_created"
	operationID := strings.Repeat("a", 32)
	description := WorkspaceResponse{ID: strings.Repeat("b", 32), State: state}
	if state == "busy" {
		description.Path = "/checkout"
	}
	result := failed(response, withDispatchStage("prepare-workspace", workspace.ErrPending), 3)
	setManagedAdmissionPendingFailure("/state", operationID, description, &result)
	expected := *result.response.Failure
	var output bytes.Buffer
	if err := writeJSONResponse(&output, command, &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Failure == nil || *decoded.Failure != expected {
		t.Fatalf("wire replaced workspace admission recovery: got %+v want %+v", decoded.Failure, expected)
	}
	if decoded.TaskRecord != "not_created" {
		t.Fatalf("wire changed task-record authority: %+v", decoded)
	}
	if state == "preparation_failed" {
		if result.code != 1 || decoded.Failure.NextAction != "stop_and_report" {
			t.Fatalf("failed preparation became retryable: %+v", decoded)
		}
	} else if result.code != 3 || decoded.Failure.NextAction != "watch_workspace" || decoded.Failure.RecoveryCommand != workspaceStatusCommand("/state", description.ID) {
		t.Fatalf("pending preparation lost retry authority: %+v", decoded)
	}
}

func TestManagedAdmissionConcreteFaultIsNotReplacedByPendingRecovery(t *testing.T) {
	response := newResponse("continue")
	response.TaskRecord = "not_created"
	result := failed(response, errors.Join(workspace.ErrPending, errors.New("concrete storage fault")), 1)
	setManagedAdmissionPendingFailure("/state", "", WorkspaceResponse{ID: strings.Repeat("b", 32), State: "busy"}, &result)
	if result.response.Failure != nil || result.code != 1 {
		t.Fatalf("joined concrete fault became pending: %+v", result)
	}
}

func TestManagedAdmissionCloseFaultInvalidatesPendingRecovery(t *testing.T) {
	response := newResponse("continue")
	response.TaskRecord = "not_created"
	result := failed(response, withDispatchStage("prepare-workspace", workspace.ErrPending), 3)
	setManagedAdmissionPendingFailure("/state", "", WorkspaceResponse{ID: strings.Repeat("b", 32), State: "busy"}, &result)
	mergeCommandClose(&result, func() error { return errors.New("concrete storage close fault") })
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "continue", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if result.code != 1 || decoded.Failure == nil || decoded.Failure.NextAction != "stop_and_report" || decoded.Failure.Code != "workspace_preparation_failed" {
		t.Fatalf("new close fault retained a stale pending acknowledgement: %+v", decoded)
	}
}
