package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

var errInjectedCheckpointFault = errors.New("injected checkpoint fault")

// checkpointWire encodes result through the public JSON writer and decodes
// it into a generic map, so omitted fields are distinguishable from zeros.
func checkpointWire(t *testing.T, result commandResult) map[string]any {
	t.Helper()
	var stdout bytes.Buffer
	if err := writeJSONResponse(&stdout, "checkpoint", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	checkpoint, ok := wire["checkpoint"].(map[string]any)
	if !ok {
		t.Fatalf("response has no checkpoint wrapper: %s", stdout.String())
	}
	return checkpoint
}

// wireText reads an optional string field; omitted and empty are equal.
func wireText(wire map[string]any, key string) string {
	if text, ok := wire[key].(string); ok {
		return text
	}
	return ""
}

// selectedRemovalCase is one exact-removal projection and its wire result.
type selectedRemovalCase struct {
	name     string
	selected *workspaces.CheckpointRemovalDescription
	err      error
	code     int
	next     string
	state    string
}

func requireSelectedRemovalWire(t *testing.T, tc selectedRemovalCase, checkpointID string, result commandResult) {
	t.Helper()
	wire := checkpointWire(t, result)
	if result.code != tc.code || wire["id"] != checkpointID || wire["operation_id"] != tc.selected.OperationID || wireText(wire, "next_action") != tc.next || wire["current"] != nil {
		t.Fatalf("result = exit %d %v; want exit %d next %q", result.code, wire, tc.code, tc.next)
	}
	operation, shown := wire["operation"].(map[string]any)
	if shown != (tc.state != "") || (shown && operation["state"] != tc.state) {
		t.Fatalf("operation = %v, want state %q", wire["operation"], tc.state)
	}
	if (tc.code == 1) != (result.err != nil) || (tc.err != nil && tc.code == 1 && !errors.Is(result.err, tc.err)) {
		t.Fatalf("error = %v for exit %d", result.err, result.code)
	}
}

func TestCheckpointSelectedRemovalKeepsAuthenticatedIDThroughFaults(t *testing.T) {
	checkpointID, operationID := strings.Repeat("c", 32), strings.Repeat("d", 32)
	selected := func(state string) *workspaces.CheckpointRemovalDescription {
		return &workspaces.CheckpointRemovalDescription{OperationID: operationID, State: state}
	}
	cases := []selectedRemovalCase{
		{"concrete fault after authentication", selected(""), errInjectedCheckpointFault, 1, checkpointStopAndReport, ""},
		{"deadline joined with a concrete fault", selected(""), errors.Join(workspace.ErrPending, context.DeadlineExceeded, errInjectedCheckpointFault), 1, checkpointStopAndReport, ""},
		{"pending joined with an unknown observation", selected(workspaces.CheckpointPending), errors.Join(workspace.ErrPending, pueue.ErrUnknown), 1, checkpointStopAndReport, workspaces.CheckpointPending},
		{"pure observation expiry", selected(""), errors.Join(workspace.ErrPending, context.DeadlineExceeded), 3, checkpointWatch, ""},
		{"clean receipt absence", selected(workspaces.CheckpointPending), workspace.ErrPending, 3, checkpointWatch, workspaces.CheckpointPending},
		{"qualified removal", selected(workspace.CheckpointNothingToRemove), nil, 0, "", workspace.CheckpointNothingToRemove},
		{"qualified unavailable attempt", selected(workspace.CheckpointUnavailable), nil, 1, checkpointStopAndReport, workspace.CheckpointUnavailable},
		{"unrecognized state", selected("guessed"), nil, 1, checkpointStopAndReport, "guessed"},
		{"unset state without an error", selected(""), nil, 1, checkpointStopAndReport, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := checkpointRemovalResult(newResponse("checkpoint"), &CheckpointResponse{}, checkpointID, workspaces.CheckpointDescription{}, tc.selected, tc.err)
			requireSelectedRemovalWire(t, tc, checkpointID, result)
		})
	}
	unauthenticated := checkpointRemovalResult(newResponse("checkpoint"), &CheckpointResponse{}, checkpointID, workspaces.CheckpointDescription{}, nil, task.ErrIdentityMismatch)
	if wire := checkpointWire(t, unauthenticated); unauthenticated.code != 1 || wire["id"] != nil || wire["operation_id"] != nil {
		t.Fatalf("unauthenticated selector = exit %d %v", unauthenticated.code, wire)
	}
}

func TestCheckpointDescriptionSeparatesObservationFromRequestedCreation(t *testing.T) {
	id := strings.Repeat("c", 32)
	described := func(state string, removal *workspaces.CheckpointRemovalDescription) workspaces.CheckpointDescription {
		return workspaces.CheckpointDescription{ID: id, Selection: workspace.CheckpointOrdinary, State: state, Removal: removal}
	}
	removalIn := func(state string) *workspaces.CheckpointRemovalDescription {
		return &workspaces.CheckpointRemovalDescription{OperationID: strings.Repeat("d", 32), Attempts: 1, State: state}
	}
	cases := []struct {
		name        string
		description workspaces.CheckpointDescription
		err         error
		requested   bool
		code        int
		next        string
	}{
		{"observed unavailable creation", described(workspace.CheckpointUnavailable, nil), nil, false, 0, checkpointStopAndReport},
		{"requested unavailable creation", described(workspace.CheckpointUnavailable, nil), nil, true, 1, checkpointStopAndReport},
		{"observed failed latest removal", described(workspace.CheckpointCheckpointed, removalIn(workspace.CheckpointUnavailable)), nil, false, 0, checkpointStopAndReport},
		{"pending latest removal", described(workspace.CheckpointCheckpointed, removalIn(workspaces.CheckpointPending)), workspace.ErrPending, false, 3, checkpointWatch},
		{"pending creation", described(workspaces.CheckpointPending, nil), workspace.ErrPending, false, 3, checkpointWatch},
		{"unreadable description during pure expiry", workspaces.CheckpointDescription{}, errors.Join(workspace.ErrPending, context.DeadlineExceeded), false, 3, checkpointWatch},
		{"deadline joined with a concrete fault", workspaces.CheckpointDescription{}, errors.Join(workspace.ErrPending, context.DeadlineExceeded, errInjectedCheckpointFault), false, 1, checkpointStopAndReport},
		{"unrecognized creation state", described("guessed", nil), nil, false, 1, checkpointStopAndReport},
		{"no description without an error", workspaces.CheckpointDescription{}, nil, false, 1, checkpointStopAndReport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := checkpointDescriptionResult(newResponse("checkpoint"), &CheckpointResponse{}, tc.description, tc.err, tc.requested)
			wire := checkpointWire(t, result)
			if result.code != tc.code || wireText(wire, "next_action") != tc.next || (wire["current"] == nil) != (tc.description.ID == "") {
				t.Fatalf("result = exit %d %v; want exit %d next %q", result.code, wire, tc.code, tc.next)
			}
			if tc.description.ID == "" && (wire["id"] != nil || strings.Contains(result.response.Error, "usable")) {
				t.Fatalf("unreadable description invented facts: %v", wire)
			}
		})
	}
}

// requireUnauthorizedAction asserts a Begin result that borrowed no ID.
func requireUnauthorizedAction(t *testing.T, label string, result commandResult, done bool, code int, wantErr error, reason, next string) {
	t.Helper()
	wire := checkpointWire(t, result)
	if !done || result.code != code || wire["id"] != nil || wire["operation_id"] != nil || wireText(wire, "reason_code") != reason || wireText(wire, "next_action") != next {
		t.Fatalf("%s = exit %d %v", label, result.code, wire)
	}
	if (wantErr == nil) != (result.err == nil) || (wantErr != nil && !errors.Is(result.err, wantErr)) {
		t.Fatalf("%s error = %v, want %v", label, result.err, wantErr)
	}
}

func TestCheckpointAcknowledgementSeparatesRefusedAndFailedActions(t *testing.T) {
	checkpoint, result, done := checkpointAcknowledgement(newResponse("checkpoint"), checkpointCreateAction, "", workspaces.ActionAcknowledgement{}, workspace.ErrBusy)
	if checkpoint != nil {
		t.Fatalf("refused creation acknowledged %+v", checkpoint)
	}
	requireUnauthorizedAction(t, "refused busy creation", result, done, 3, nil, checkpointNotAuthorized, "retry_create")
	fault := errors.Join(workspace.ErrPending, errInjectedCheckpointFault)
	_, result, done = checkpointAcknowledgement(newResponse("checkpoint"), checkpointRemoveAction, strings.Repeat("c", 32), workspaces.ActionAcknowledgement{}, fault)
	requireUnauthorizedAction(t, "unauthorized concrete removal fault", result, done, 1, errInjectedCheckpointFault, "", checkpointStopAndReport)
	_, result, done = checkpointAcknowledgement(newResponse("checkpoint"), checkpointCreateAction, "", workspaces.ActionAcknowledgement{}, nil)
	requireUnauthorizedAction(t, "unacknowledged success", result, done, 1, task.ErrEvidenceFault, "", checkpointStopAndReport)
}

func TestCheckpointHumanOutputSeparatesHistoryFromSelection(t *testing.T) {
	latest := &workspaces.CheckpointRemovalDescription{OperationID: strings.Repeat("e", 32), Attempts: 2, State: workspaces.CheckpointPending}
	current := workspaces.CheckpointDescription{
		ID: strings.Repeat("c", 32), Selection: workspace.CheckpointOrdinary, Cwd: "/repo path", State: workspace.CheckpointCheckpointed,
		CommitOID: strings.Repeat("1", 40), SnapshotRef: "refs/delegate/checkpoint", Removal: latest,
	}
	selected := &workspaces.CheckpointRemovalDescription{OperationID: strings.Repeat("d", 32), Attempts: 2, State: workspace.CheckpointUnavailable, ReasonCode: workspace.CheckpointRemovalFailed}
	response := newResponse("checkpoint")
	response.Checkpoint = &CheckpointResponse{ID: current.ID, OperationID: selected.OperationID, Current: &current, Operation: selected, NextAction: checkpointStopAndReport}
	var output strings.Builder
	if err := writeHumanResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"checkpoint_command id=" + current.ID + " operation_id=" + selected.OperationID + " next_action=stop_and_report\n",
		"checkpoint=" + current.ID + " selection=ordinary state=checkpointed usable=false commit=" + current.CommitOID + " ref=refs/delegate/checkpoint cwd=\"/repo path\"\n",
		"checkpoint_removal latest operation_id=" + latest.OperationID + " attempts=2 state=pending\n",
		"checkpoint_removal selected operation_id=" + selected.OperationID + " attempts=2 state=unavailable reason_code=checkpoint_removal_failed\n",
	} {
		if !strings.Contains(output.String(), line) {
			t.Fatalf("human output %q omits %q", output.String(), line)
		}
	}
}

func TestCheckpointPublicWriterFailureExitsOne(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"--root", store.Root, "checkpoint", "list", "--json"},
		{"--root", store.Root, "checkpoint", "list"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(argv, &stdout, &stderr, Dependencies{}); code != 0 {
			t.Fatalf("%v = exit %d, stderr %q", argv, code, stderr.String())
		}
		if code := Run(argv, failingCheckpointWriter{}, &stderr, Dependencies{}); code != 1 {
			t.Fatalf("%v with a failed writer = exit %d", argv, code)
		}
	}
}

type failingCheckpointWriter struct{}

func (failingCheckpointWriter) Write([]byte) (int, error) { return 0, errInjectedCheckpointFault }
