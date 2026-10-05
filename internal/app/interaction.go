package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func fillInteraction(response *Response, td *taskdir.TaskDir) error {
	req, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if !req.RequestedConfig.Interactive {
		return nil
	}
	state, err := td.InteractionState()
	if err != nil {
		return err
	}
	response.Interaction = &state
	return nil
}

func interactionCommand(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse(a.Command)
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	if a.Command == "events" {
		return observeInteraction(a, td, response)
	}
	message, err := interactionMessage(a)
	if err != nil {
		return failed(response, err, 2)
	}
	receipt, err := td.SubmitInteraction(message)
	if err != nil {
		return failed(response, err, 2)
	}
	response.Delivery = &receipt
	if err = fillInteraction(&response, td); err != nil {
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func interactionMessage(a Arguments) (task.InteractionMessage, error) {
	message := task.InteractionMessage{ID: a.MessageID, Kind: a.Command, RequestID: a.RequestID}
	if a.Command == "steer" {
		data, err := readBrief(a.Brief)
		message.Text = string(data)
		return message, err
	}
	file, err := os.Open(a.Answers)
	if err != nil {
		return message, err
	}
	data, readErr := task.ReadBounded(file, task.MaxControlRecordSize)
	if err = errors.Join(readErr, file.Close()); err != nil {
		return message, err
	}
	err = task.DecodeStrict(data, &message.Answers)
	return message, err
}

func observeInteraction(a Arguments, td *taskdir.TaskDir, response Response) commandResult {
	deadline := time.Now().Add(a.Watch)
	for {
		if err := fillInteraction(&response, td); err != nil {
			return failed(response, err, 1)
		}
		if response.Interaction == nil {
			return failed(response, task.ErrInteractionUnavailable, 2)
		}
		events, cursor, err := td.InteractionEvents(a.After)
		if err != nil {
			return failed(response, err, 1)
		}
		response.Events, response.Cursor = events, &cursor
		if len(events) > 0 || response.Interaction.Closed {
			return commandResult{response: response}
		}
		if collectionWatchExpired(a.Watch, deadline) || !waitForCollection(deadline) {
			response.Pending = &PendingResponse{Kind: "interaction"}
			return commandResult{response: response, code: 3}
		}
	}
}

func collectWithInteraction(td *taskdir.TaskDir, ref task.PredicateRef, response *Response) (*task.OutcomeRecord, error) {
	outcome, collectErr := collectAttempt(td, ref)
	interactionErr := fillInteraction(response, td)
	if outcome != nil {
		return outcome, collectErr
	}
	return nil, errors.Join(collectErr, interactionErr)
}

func interactionWaiting(state *task.InteractionState) bool {
	return state != nil && !state.Closed && len(state.PendingRequests) > 0
}

func fillInspectedInteraction(response *Response, td *taskdir.TaskDir, inspection *taskdir.TaskInspection) error {
	err := fillInteraction(response, td)
	if isTerminal(inspection) {
		return nil
	}
	return err
}

func writeHumanInteraction(output *strings.Builder, response Response) error {
	if response.Interaction != nil {
		writeHumanInteractionState(output, *response.Interaction)
	}
	if response.Delivery != nil {
		writeHumanDelivery(output, *response.Delivery)
	}
	for _, event := range response.Events {
		encoded, err := task.MarshalCanonical(event)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "event=%s\n", encoded)
	}
	if response.Cursor != nil {
		fmt.Fprintf(output, "cursor=%d\n", *response.Cursor)
	}
	return nil
}

func writeHumanInteractionState(output *strings.Builder, state task.InteractionState) {
	fmt.Fprintf(output, "interaction turn_id=%q closed=%t deadline=%q clarification=%t steering=%t cursor=%d\n", state.TurnID, state.Closed, state.Deadline, state.Capabilities.Clarification, state.Capabilities.Steering, state.Cursor)
	for _, request := range state.PendingRequests {
		fmt.Fprintf(output, "request=%q blocking=%t\n", request.ID, request.Blocking)
		for _, question := range request.Questions {
			fmt.Fprintf(output, "question=%q text=%q options=%q multiple=%t free_text=%t\n", question.ID, question.Text, question.Options, question.Multiple, question.FreeText)
		}
	}
	for _, delivery := range state.Deliveries {
		writeHumanDelivery(output, delivery)
	}
}

func writeHumanDelivery(output *strings.Builder, delivery task.InteractionDelivery) {
	fmt.Fprintf(output, "delivery message_id=%q state=%s reason=%q\n", delivery.MessageID, delivery.State, delivery.Reason)
}
