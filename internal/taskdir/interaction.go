package taskdir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	maxInteractionRecords   = 8192
	maxInteractionBytes     = 16 * task.MaxControlRecordSize
	interactionCloseReserve = 512
	interactionPageBytes    = task.MaxControlRecordSize / 2
	interactionStateBytes   = task.MaxControlRecordSize / 4
)

type interactionSnapshot struct {
	task.InteractionState
	encodedBytes int
}

type interactionRecord struct {
	SchemaVersion int                   `json:"schema_version"`
	RootID        string                `json:"root_id"`
	TaskID        string                `json:"task_id"`
	Event         task.InteractionEvent `json:"event"`
}

// BeginInteraction is available only to the runner after its start permit is consumed.
func (td *TaskDir) BeginInteraction() error {
	state, err := td.ownedRunner()
	if err != nil {
		return err
	}
	if state.sealed || state.poisoned != nil {
		state.mu.Unlock()
		return task.ErrEvidenceFault
	}
	state.mu.Unlock()
	req, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if !req.RequestedConfig.Interactive {
		return task.ErrInteractionUnavailable
	}
	if err = td.store.mkdir(filepath.Join(td.Dir, "interaction"), td.store.faultInjector); err != nil {
		return err
	}
	lock, err := td.store.openLock(filepath.Join(td.Dir, ".interaction.lock"), LockLevelRun)
	if err != nil {
		return err
	}
	return lock.Close()
}

func (td *TaskDir) withInteraction(fn func() error) (resultErr error) {
	lock, err := td.store.openExistingLock(filepath.Join(td.Dir, ".interaction.lock"), LockLevelRun)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = lock.LockEX(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Unlock()) }()
	return fn()
}

func (td *TaskDir) interactionRecords() ([]task.InteractionEvent, interactionSnapshot, error) {
	state := interactionSnapshot{InteractionState: task.InteractionState{PendingRequests: []task.InteractionRequest{}, Deliveries: []task.InteractionDelivery{}}}
	var events []task.InteractionEvent
	for sequence := uint64(1); ; sequence++ {
		var record interactionRecord
		path := filepath.Join(td.Dir, "interaction", fmt.Sprintf("%020d.json", sequence))
		data, err := td.store.readBytes(path, task.MaxControlRecordSize)
		if errors.Is(err, os.ErrNotExist) {
			return events, state, nil
		}
		if err != nil {
			return nil, state, err
		}
		state.encodedBytes += len(data)
		if state.encodedBytes > maxInteractionBytes || sequence > maxInteractionRecords {
			return nil, state, task.ErrControlRecordTooBig
		}
		if err = task.DecodeStrict(data, &record); err != nil {
			return nil, state, err
		}
		if record.SchemaVersion != task.SchemaVersion || record.RootID != td.store.RootID || record.TaskID != td.TaskID || record.Event.Sequence != sequence {
			return nil, state, task.ErrIdentityMismatch
		}
		if err = task.ApplyInteractionEvent(&state.InteractionState, record.Event); err != nil {
			return nil, state, err
		}
		events = append(events, record.Event)
	}
}

func (td *TaskDir) appendInteraction(state interactionSnapshot, event task.InteractionEvent) error {
	event.Sequence = state.Cursor + 1
	if err := task.ApplyInteractionEvent(&state.InteractionState, event); err != nil {
		return err
	}
	data, err := marshalControlRecord(interactionRecord{task.SchemaVersion, td.store.RootID, td.TaskID, event})
	if err != nil {
		return err
	}
	if len(data) > interactionPageBytes {
		return task.ErrControlRecordTooBig
	}
	if err = validateInteractionSnapshotSize(state.InteractionState); err != nil {
		return err
	}
	reserve := interactionCloseReserve
	if event.Kind == "closed" {
		reserve = 0
	}
	if state.encodedBytes+len(data)+reserve > maxInteractionBytes || state.Cursor > maxInteractionRecords || (state.Cursor == maxInteractionRecords && event.Kind != "closed") {
		return task.ErrControlRecordTooBig
	}
	_, cleanup, err := td.store.stageAndCommit(filepath.Join(td.Dir, "interaction"), fmt.Sprintf("%020d.json", event.Sequence), data, td.store.faultInjector)
	return errors.Join(err, cleanup)
}

// RecordInteraction publishes only runner-observed protocol transitions.
func (td *TaskDir) RecordInteraction(event task.InteractionEvent) error {
	owned, err := td.ownedRunner()
	if err != nil {
		return err
	}
	if owned.sealed || owned.poisoned != nil {
		owned.mu.Unlock()
		return task.ErrEvidenceFault
	}
	owned.mu.Unlock()
	return td.withInteraction(func() error {
		_, state, err := td.interactionRecords()
		if err != nil {
			return err
		}
		return td.appendInteraction(state, event)
	})
}

func (td *TaskDir) InteractionState() (task.InteractionState, error) {
	_, state, err := td.interactionRecords()
	if err == nil && !state.Closed {
		if openErr := td.interactionOpen(state.InteractionState); errors.Is(openErr, task.ErrInteractionClosed) {
			state.Closed = true
			state.PendingRequests = nil
		} else if openErr != nil && !errors.Is(openErr, task.ErrInteractionUnavailable) {
			return state.InteractionState, openErr
		}
	}
	return state.InteractionState, err
}

// InteractionEvents returns a bounded page. The cursor advances only over returned records.
func (td *TaskDir) InteractionEvents(after uint64) ([]task.InteractionEvent, uint64, error) {
	events, state, err := td.interactionRecords()
	if err != nil {
		return nil, after, err
	}
	if after > state.Cursor {
		return nil, after, task.ErrInteractionRequest
	}
	end := after
	bytes := 0
	for end < state.Cursor && end-after < 100 {
		encoded, encodeErr := task.MarshalCanonical(events[end])
		if encodeErr != nil {
			return nil, after, encodeErr
		}
		if end > after && bytes+len(encoded)+1 > interactionPageBytes {
			break
		}
		bytes += len(encoded) + 1
		end++
	}
	return events[after:end], end, nil
}

func (td *TaskDir) interactionOpen(state task.InteractionState) error {
	for _, name := range []string{"provider.exit", "outcome.json"} {
		exists, err := td.store.entryExists(filepath.Join(td.Dir, name))
		if err != nil {
			return err
		}
		if exists {
			return task.ErrInteractionClosed
		}
	}
	req, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if !req.RequestedConfig.Interactive {
		return task.ErrInteractionUnavailable
	}
	stops, err := td.ReadStopRecords()
	if err != nil {
		return err
	}
	if len(stops) > 0 {
		return task.ErrInteractionClosed
	}
	// A queued task has no runner yet. Once the launch guard exists, loss of
	// the runner is terminal even if it crashed before recording its start.
	consumed, err := td.store.entryExists(filepath.Join(td.Dir, "provider.start"))
	if err != nil {
		return err
	}
	if !consumed {
		return task.ErrInteractionUnavailable
	}
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return err
	}
	if released {
		return task.ErrInteractionClosed
	}
	return td.interactionDeadlineOpen(req, state)
}

func (td *TaskDir) interactionDeadlineOpen(req *task.TaskRecord, state task.InteractionState) error {
	var started task.ProviderStartedRecord
	err := td.store.readRecord(filepath.Join(td.Dir, "provider.started.json"), &started)
	if errors.Is(err, os.ErrNotExist) {
		return task.ErrInteractionUnavailable
	}
	if err != nil {
		return err
	}
	when, err := time.Parse(time.RFC3339Nano, started.StartedAt)
	if err != nil {
		return err
	}
	deadline := when.Add(time.Duration(req.BudgetNanos - started.DiagnosticNanos))
	if state.Deadline != "" {
		deadline, err = time.Parse(time.RFC3339Nano, state.Deadline)
		if err != nil {
			return err
		}
	}
	if !time.Now().Before(deadline) {
		return task.ErrInteractionClosed
	}
	return nil
}

// SubmitInteraction is idempotent by message ID and does not contact a provider.
func (td *TaskDir) SubmitInteraction(message task.InteractionMessage) (receipt task.InteractionDelivery, resultErr error) {
	resultErr = td.withInteraction(func() error {
		events, state, err := td.interactionRecords()
		if err != nil {
			return err
		}
		existing, exists, replayErr := interactionReplay(message, events, state.InteractionState)
		if exists || replayErr != nil {
			receipt = existing
			if replayErr != nil {
				return replayErr
			}
			// A previously visible link may have survived a failed directory barrier.
			// Re-establish durability before acknowledging a retry.
			return td.store.barrierDir(filepath.Join(td.Dir, "interaction"))
		}
		if err = td.interactionOpen(state.InteractionState); err != nil {
			return err
		}
		message.TurnID = state.TurnID
		if err = task.ValidateInteractionMessage(message, state.InteractionState); err != nil {
			return err
		}
		if interactionReplyExists(message, events) {
			return task.ErrInteractionConflict
		}
		if err = td.appendInteraction(state, task.InteractionEvent{Kind: "message", Message: &message}); err != nil {
			return err
		}
		receipt = task.InteractionDelivery{MessageID: message.ID, State: "queued"}
		return nil
	})
	return receipt, resultErr
}

// NextInteraction reserves a single delivery before any bytes enter native stdin.
// An attempted message is never selected again, even after a crash.
func (td *TaskDir) NextInteraction() (message *task.InteractionMessage, resultErr error) {
	owned, err := td.ownedRunner()
	if err != nil {
		return nil, err
	}
	if owned.sealed || owned.poisoned != nil {
		owned.mu.Unlock()
		return nil, task.ErrEvidenceFault
	}
	owned.mu.Unlock()
	resultErr = td.withInteraction(func() error {
		events, state, err := td.interactionRecords()
		if err != nil {
			return err
		}
		if err = td.interactionOpen(state.InteractionState); err != nil {
			return err
		}
		if state.Closed {
			return nil
		}
		next := nextQueuedInteraction(events, state.InteractionState)
		if next == nil {
			return nil
		}
		delivery := task.InteractionDelivery{MessageID: next.ID, State: "unknown", Reason: "delivery attempted; native acknowledgement pending"}
		if validationErr := task.ValidateInteractionMessage(*next, state.InteractionState); validationErr != nil {
			delivery.State = "rejected"
			delivery.Reason = validationErr.Error()
		}
		if err = td.appendInteraction(state, task.InteractionEvent{Kind: "delivery", Delivery: &delivery}); err != nil {
			return err
		}
		if delivery.State != "rejected" {
			message = next
		}

		return nil
	})
	return message, resultErr
}

// SealInteraction snapshots the closed journal into the sealed raw evidence set.
func (td *TaskDir) SealInteraction() error {
	owned, err := td.ownedRunner()
	if err != nil {
		return err
	}
	invalid := owned.sealed || owned.poisoned != nil
	owned.mu.Unlock()
	if invalid {
		return task.ErrEvidenceFault
	}
	return td.withInteraction(func() error {
		events, state, err := td.interactionRecords()
		if err != nil {
			return err
		}
		if !state.Closed {
			if err = td.appendInteraction(state, task.InteractionEvent{Kind: "closed"}); err != nil {
				return err
			}
			events, _, err = td.interactionRecords()
			if err != nil {
				return err
			}
		}
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		for _, event := range events {
			if err = encoder.Encode(event); err != nil {
				return err
			}
		}
		return td.writeInteractionEvidence(buffer.Bytes())
	})
}

func (td *TaskDir) writeInteractionEvidence(data []byte) error {
	state, err := td.ownedRunner()
	if err != nil {
		return err
	}
	defer state.mu.Unlock()
	if state.sealed || state.poisoned != nil {
		return task.ErrEvidenceFault
	}
	_, cleanup, err := td.store.stageAndCommit(filepath.Join(td.Dir, "raw"), "interaction", data, td.store.faultInjector)
	return errors.Join(err, cleanup)
}

func interactionReplay(message task.InteractionMessage, events []task.InteractionEvent, state task.InteractionState) (task.InteractionDelivery, bool, error) {
	for _, event := range events {
		if event.Message == nil || event.Message.ID != message.ID {
			continue
		}
		message.TurnID = event.Message.TurnID
		a, aErr := task.MarshalCanonical(message)
		b, bErr := task.MarshalCanonical(event.Message)
		if err := errors.Join(aErr, bErr); err != nil {
			return task.InteractionDelivery{}, true, err
		}
		if !bytes.Equal(a, b) {
			return task.InteractionDelivery{}, true, task.ErrInteractionConflict
		}
		for _, delivery := range state.Deliveries {
			if delivery.MessageID == message.ID {
				return delivery, true, nil
			}
		}
		return task.InteractionDelivery{}, true, task.ErrEvidenceFault
	}
	return task.InteractionDelivery{}, false, nil
}

func interactionReplyExists(message task.InteractionMessage, events []task.InteractionEvent) bool {
	if message.Kind != "reply" {
		return false
	}
	for _, event := range events {
		if event.Message != nil && event.Message.Kind == "reply" && event.Message.RequestID == message.RequestID {
			return true
		}
	}
	return false
}

func nextQueuedInteraction(events []task.InteractionEvent, state task.InteractionState) *task.InteractionMessage {
	queued := map[string]bool{}
	for _, delivery := range state.Deliveries {
		queued[delivery.MessageID] = delivery.State == "queued"
	}
	for _, event := range events {
		if event.Message != nil && queued[event.Message.ID] {
			copyMessage := *event.Message
			return &copyMessage
		}
	}
	return nil
}

// Reserve response space for the full state, an event page and the CLI envelope.
// Check closure as well: rejecting queued messages must remain observable.
func validateInteractionSnapshotSize(state task.InteractionState) error {
	state.Deliveries = append([]task.InteractionDelivery(nil), state.Deliveries...)
	for range 2 {
		encoded, err := task.MarshalCanonical(state)
		if err != nil {
			return err
		}
		if len(encoded) > interactionStateBytes {
			return task.ErrControlRecordTooBig
		}
		if state.Closed {
			return nil
		}
		if err = task.ApplyInteractionEvent(&state, task.InteractionEvent{Sequence: state.Cursor + 1, Kind: "closed"}); err != nil {
			return err
		}
	}
	return nil
}
