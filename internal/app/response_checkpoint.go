package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// Command guidance for checkpoint responses. It never replaces a receipt's
// own reason code, which stays inside the projected descriptions.
const (
	checkpointWatch         = "watch_checkpoint"
	checkpointStopAndReport = "stop_and_report"
	checkpointNotAuthorized = "requested_action_not_authorized"
	checkpointRetryPrefix   = "retry_"
	checkpointCreateAction  = "create"
	checkpointRemoveAction  = "remove"
)

var (
	// errCheckpointUnavailable fails a requested creation whose qualified
	// receipt is unavailable. Observing that history is not a failure.
	errCheckpointUnavailable = errors.New("checkpoint is unavailable")
	// errCheckpointRemovalUnavailable fails a requested or selected removal
	// attempt whose qualified receipt is unavailable.
	errCheckpointRemovalUnavailable = errors.New("checkpoint removal attempt is unavailable")
	// errCheckpointUnknownState rejects a described state outside the
	// delivered checkpoint vocabulary instead of guessing its exit.
	errCheckpointUnknownState = fmt.Errorf("%w: unrecognized checkpoint state", task.ErrEvidenceFault)
)

// CheckpointResponse is one checkpoint command's projection. ID is the
// acknowledged or authenticated creation; OperationID names only an
// acknowledged or authenticated registered removal attempt. Both survive a
// later unreadable description, which is then omitted rather than zeroed.
// Current.Removal is the latest attempt; Operation is the selected one.
type CheckpointResponse struct {
	ID          string                                       `json:"id,omitempty"`
	OperationID string                                       `json:"operation_id,omitempty"`
	Current     *workspacescore.CheckpointDescription        `json:"current,omitempty"`
	Operation   *workspacescore.CheckpointRemovalDescription `json:"operation,omitempty"`
	ReasonCode  string                                       `json:"reason_code,omitempty"`
	NextAction  string                                       `json:"next_action,omitempty"`
}

// checkpointFault is every concrete checkpoint failure: exit 1 with the known
// acknowledgements retained and no automatic repetition.
func checkpointFault(response Response, checkpoint *CheckpointResponse, err error) commandResult {
	if checkpoint == nil {
		checkpoint = &CheckpointResponse{}
	}
	checkpoint.NextAction = checkpointStopAndReport
	response.Checkpoint = checkpoint
	return failed(response, err, 1)
}

// checkpointAcknowledgement classifies a Begin result. An unauthorized
// pending or busy result is a refusal that borrows no other operation's ID. A
// concrete error fails with the acknowledged IDs kept. An authorized pending
// action has no qualified receipt yet, so it is watched, never observed or
// repeated here. done is false only for an authorized action Begin reported
// complete, whose qualified receipt still chooses the exit. A removal
// acknowledgement also authenticates checkpointID, which Begin loaded.
func checkpointAcknowledgement(response Response, action, checkpointID string, ack workspacescore.ActionAcknowledgement, actionErr error) (checkpoint *CheckpointResponse, result commandResult, done bool) {
	if !ack.Authorized() {
		if workspacescore.PendingError(actionErr) {
			response.Checkpoint = &CheckpointResponse{ReasonCode: checkpointNotAuthorized, NextAction: checkpointRetryPrefix + action}
			return nil, commandResult{response: response, code: 3}, true
		}
		if actionErr == nil {
			actionErr = task.ErrEvidenceFault
		}
		return nil, checkpointFault(response, nil, actionErr), true
	}
	checkpoint = &CheckpointResponse{ID: ack.OperationID()}
	if action == checkpointRemoveAction {
		checkpoint = &CheckpointResponse{ID: checkpointID, OperationID: ack.OperationID()}
	}
	response.Checkpoint = checkpoint
	if actionErr != nil && !workspacescore.PendingError(actionErr) {
		return checkpoint, checkpointFault(response, checkpoint, actionErr), true
	}
	if actionErr != nil {
		return checkpoint, pendingCheckpointResult(response), true
	}
	return checkpoint, commandResult{}, false
}

// checkpointDescriptionResult projects a creation and latest-removal
// observation. A requested creation fails when its qualified receipt is
// unavailable; an observation of that history succeeds and says to stop.
func checkpointDescriptionResult(response Response, checkpoint *CheckpointResponse, description workspacescore.CheckpointDescription, observeErr error, requested bool) commandResult {
	if description.ID != "" {
		checkpoint.ID, checkpoint.Current = description.ID, &description
	}
	response.Checkpoint = checkpoint
	if observeErr != nil && !workspacescore.PendingError(observeErr) {
		return checkpointFault(response, checkpoint, observeErr)
	}
	if description.ID == "" {
		return unreadCheckpointResult(response, checkpoint, observeErr)
	}
	creation, removal := checkpointCreationVerdict(description.State), checkpointVerdictSettled
	if description.Removal != nil {
		removal = checkpointRemovalVerdict(description.Removal.State)
	}
	switch {
	case creation == checkpointVerdictUnknown || removal == checkpointVerdictUnknown:
		return checkpointFault(response, checkpoint, errCheckpointUnknownState)
	case creation == checkpointVerdictPending || removal == checkpointVerdictPending:
		return pendingCheckpointResult(response)
	case creation == checkpointVerdictUnavailable && requested:
		return checkpointFault(response, checkpoint, errCheckpointUnavailable)
	case creation == checkpointVerdictUnavailable || removal == checkpointVerdictUnavailable:
		checkpoint.NextAction = checkpointStopAndReport
	}
	return commandResult{response: response}
}

// checkpointRemovalResult projects one exact removal attempt beside the
// current metadata. Only the selected attempt's qualified state chooses the
// exit; a newer attempt never replaces it. A selected attempt authenticates
// checkpointID; its state is shown only when a receipt or clean absence set it.
func checkpointRemovalResult(response Response, checkpoint *CheckpointResponse, checkpointID string, current workspacescore.CheckpointDescription, selected *workspacescore.CheckpointRemovalDescription, observeErr error) commandResult {
	if selected != nil {
		checkpoint.ID, checkpoint.OperationID = checkpointID, selected.OperationID
		if selected.State != "" {
			checkpoint.Operation = selected
		}
	}
	if current.ID != "" {
		checkpoint.ID, checkpoint.Current = current.ID, &current
	}
	response.Checkpoint = checkpoint
	if observeErr != nil && !workspacescore.PendingError(observeErr) {
		return checkpointFault(response, checkpoint, observeErr)
	}
	if checkpoint.Operation == nil {
		return unreadCheckpointResult(response, checkpoint, observeErr)
	}
	switch checkpointRemovalVerdict(selected.State) {
	case checkpointVerdictPending:
		return pendingCheckpointResult(response)
	case checkpointVerdictUnavailable:
		return checkpointFault(response, checkpoint, errCheckpointRemovalUnavailable)
	case checkpointVerdictSettled:
		return commandResult{response: response}
	case checkpointVerdictUnknown:
	}
	return checkpointFault(response, checkpoint, errCheckpointUnknownState)
}

// unreadCheckpointResult is an observation with no readable state: pending
// only when the whole error is pending, never inferred from absence alone.
func unreadCheckpointResult(response Response, checkpoint *CheckpointResponse, observeErr error) commandResult {
	if observeErr == nil {
		return checkpointFault(response, checkpoint, errCheckpointUnknownState)
	}
	return pendingCheckpointResult(response)
}

func pendingCheckpointResult(response Response) commandResult {
	response.Checkpoint.NextAction = checkpointWatch
	return commandResult{response: response, code: 3}
}

// checkpointVerdict classifies one described state for exit selection only.
type checkpointVerdict uint8

const (
	checkpointVerdictPending checkpointVerdict = iota + 1
	checkpointVerdictSettled
	checkpointVerdictUnavailable
	checkpointVerdictUnknown
)

func checkpointCreationVerdict(state string) checkpointVerdict {
	switch state {
	case workspacescore.CheckpointPending:
		return checkpointVerdictPending
	case workspace.CheckpointCheckpointed:
		return checkpointVerdictSettled
	case workspace.CheckpointUnavailable:
		return checkpointVerdictUnavailable
	default:
		return checkpointVerdictUnknown
	}
}

func checkpointRemovalVerdict(state string) checkpointVerdict {
	switch state {
	case workspacescore.CheckpointPending:
		return checkpointVerdictPending
	case workspace.CheckpointRemoved, workspace.CheckpointNothingToRemove:
		return checkpointVerdictSettled
	case workspace.CheckpointUnavailable:
		return checkpointVerdictUnavailable
	default:
		return checkpointVerdictUnknown
	}
}

func writeHumanCheckpointResponse(output *strings.Builder, response Response) {
	if checkpoint := response.Checkpoint; checkpoint != nil {
		writeHumanCheckpointAction(output, *checkpoint)
		if checkpoint.Current != nil {
			writeHumanCheckpoint(output, *checkpoint.Current)
		}
		if checkpoint.Operation != nil {
			writeHumanCheckpointRemoval(output, "selected", *checkpoint.Operation)
		}
	}
	if response.Checkpoints != nil {
		for _, item := range *response.Checkpoints {
			writeHumanCheckpoint(output, item)
		}
	}
	if response.CheckpointCursor != "" {
		fmt.Fprintf(output, "checkpoint_cursor=%s\n", response.CheckpointCursor)
	}
}

func writeHumanCheckpointAction(output *strings.Builder, checkpoint CheckpointResponse) {
	fields := []struct{ name, value string }{
		{"id", checkpoint.ID},
		{"operation_id", checkpoint.OperationID},
		{"reason_code", checkpoint.ReasonCode},
		{"next_action", checkpoint.NextAction},
	}
	output.WriteString("checkpoint_command")
	for _, field := range fields {
		if field.value != "" {
			fmt.Fprintf(output, " %s=%s", field.name, field.value)
		}
	}
	output.WriteByte('\n')
}

func writeHumanCheckpoint(output *strings.Builder, checkpoint workspacescore.CheckpointDescription) {
	fmt.Fprintf(output, "checkpoint=%s selection=%s state=%s usable=%t", checkpoint.ID, checkpoint.Selection, checkpoint.State, checkpoint.Usable)
	fields := []struct{ name, value string }{
		{"reason_code", checkpoint.ReasonCode},
		{"workspace", checkpoint.WorkspaceID},
		{"commit", checkpoint.CommitOID},
		{"ref", checkpoint.SnapshotRef},
	}
	for _, field := range fields {
		if field.value != "" {
			fmt.Fprintf(output, " %s=%s", field.name, field.value)
		}
	}
	if checkpoint.Cwd != "" {
		fmt.Fprintf(output, " cwd=%q", checkpoint.Cwd)
	}
	output.WriteByte('\n')
	if checkpoint.Removal != nil {
		writeHumanCheckpointRemoval(output, "latest", *checkpoint.Removal)
	}
}

func writeHumanCheckpointRemoval(output *strings.Builder, role string, removal workspacescore.CheckpointRemovalDescription) {
	fmt.Fprintf(output, "checkpoint_removal %s operation_id=%s attempts=%d state=%s", role, removal.OperationID, removal.Attempts, removal.State)
	if removal.ReasonCode != "" {
		fmt.Fprintf(output, " reason_code=%s", removal.ReasonCode)
	}
	output.WriteByte('\n')
}
