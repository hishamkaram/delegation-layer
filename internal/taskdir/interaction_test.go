package taskdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func interactiveTask(t *testing.T) *TaskDir {
	t.Helper()
	s := testStore(t)
	req, meta, brief := preparedInput(t, s)
	req.RequestedConfig.Interactive = true
	meta.RequestedConfig = req.RequestedConfig
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	permit := consumeStart(t, td)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, td.RecordStarted(0))
	must(t, td.BeginInteraction())
	must(t, td.RecordInteraction(task.InteractionEvent{Kind: "ready", TurnID: "turn-1", Capabilities: &task.InteractionCapabilities{Steering: true, Clarification: true}}))
	return td
}

func TestInteractionInboxRetryNeverRedelivers(t *testing.T) {
	td := interactiveTask(t)
	message := task.InteractionMessage{ID: strings.Repeat("a", 32), Kind: "steer", Text: "Examine latency"}
	receipt, err := td.SubmitInteraction(message)
	must(t, err)
	if receipt.State != "queued" {
		t.Fatalf("receipt: %+v", receipt)
	}
	next, err := td.NextInteraction()
	must(t, err)
	if next == nil || next.ID != message.ID || next.TurnID != "turn-1" {
		t.Fatalf("wrong delivery: %+v", next)
	}
	// Simulate a crash at the ambiguity boundary: no sent or acknowledged record.
	observer, err := td.store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, observer.Close()) })
	receipt, err = observer.SubmitInteraction(message)
	must(t, err)
	if receipt.State != "unknown" {
		t.Fatalf("invented delivery certainty: %+v", receipt)
	}
	next, err = td.NextInteraction()
	must(t, err)
	if next != nil {
		t.Fatal("ambiguous delivery replayed")
	}
	message.Text = "Changed payload"
	if _, err = observer.SubmitInteraction(message); !errors.Is(err, task.ErrInteractionConflict) {
		t.Fatalf("changed replay: %v", err)
	}
}

func TestInteractionIdentityAndSequenceAreValidated(t *testing.T) {
	td := interactiveTask(t)
	path := filepath.Join(td.Dir, "interaction", "00000000000000000002.json")
	data := []byte(`{"schema_version":1,"root_id":"wrong","task_id":"wrong","event":{"sequence":2,"kind":"closed"}}`)
	must(t, os.WriteFile(path, data, 0o600))
	if _, err := td.InteractionState(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign event accepted: %v", err)
	}
}

func TestInteractionSealClosesInboxAndPreservesReplayReceipt(t *testing.T) {
	td := interactiveTask(t)
	message := task.InteractionMessage{ID: strings.Repeat("a", 32), Kind: "steer", Text: "Focus"}
	_, err := td.SubmitInteraction(message)
	must(t, err)
	must(t, td.SealInteraction())
	receipt, err := td.SubmitInteraction(message)
	must(t, err)
	if receipt.State != "rejected" {
		t.Fatalf("queued message survived closure: %+v", receipt)
	}
	message.ID = strings.Repeat("b", 32)
	if _, err = td.SubmitInteraction(message); !errors.Is(err, task.ErrInteractionClosed) {
		t.Fatalf("closed inbox accepted message: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(td.Dir, "raw", "interaction"))
	must(t, err)
	if !strings.Contains(string(data), `"kind":"closed"`) {
		t.Fatal("sealed transcript missing closure")
	}
}

func TestInteractionReservationRequiresRunnerOwnership(t *testing.T) {
	td := interactiveTask(t)
	observer, err := td.store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, observer.Close()) })
	if _, err = observer.NextInteraction(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("observer got delivery authority: %v", err)
	}
}

func TestInteractionDeadlineClosesObservationAndSubmission(t *testing.T) {
	td := interactiveTask(t)
	path := filepath.Join(td.Dir, "interaction", "00000000000000000001.json")
	data, err := os.ReadFile(path)
	must(t, err)
	var record interactionRecord
	must(t, task.DecodeStrict(data, &record))
	record.Event.Deadline = "2000-01-01T00:00:00Z"
	data, err = task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(path, data, 0o600))
	state, err := td.InteractionState()
	must(t, err)
	if !state.Closed || len(state.PendingRequests) != 0 {
		t.Fatalf("expired channel still open: %+v", state)
	}
	if _, err = td.SubmitInteraction(task.InteractionMessage{ID: strings.Repeat("c", 32), Kind: "steer", Text: "late"}); !errors.Is(err, task.ErrInteractionClosed) {
		t.Fatalf("expired submission: %v", err)
	}
}

func TestInteractionConcurrentRetryHasOneDurableMessage(t *testing.T) {
	td := interactiveTask(t)
	message := task.InteractionMessage{ID: strings.Repeat("d", 32), Kind: "steer", Text: "once"}
	results := make(chan error, 8)
	for range 8 {
		go func() { _, err := td.SubmitInteraction(message); results <- err }()
	}
	for range 8 {
		must(t, <-results)
	}
	events, _, err := td.InteractionEvents(0)
	must(t, err)
	count := 0
	for _, event := range events {
		if event.Kind == "message" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent retries created %d messages", count)
	}
}

func TestInteractionEventPagesDoNotSkipRecords(t *testing.T) {
	td := interactiveTask(t)
	for index := range 105 {
		message := task.InteractionMessage{ID: fmt.Sprintf("%032x", index), Kind: "steer", Text: "bounded"}
		_, err := td.SubmitInteraction(message)
		must(t, err)
	}
	first, cursor, err := td.InteractionEvents(0)
	must(t, err)
	if len(first) != 100 || cursor != 100 {
		t.Fatalf("first page %d cursor %d", len(first), cursor)
	}
	second, next, err := td.InteractionEvents(cursor)
	must(t, err)
	if len(second) != 6 || second[0].Sequence != 101 || next != 106 {
		t.Fatalf("second page %d cursor %d", len(second), next)
	}
	if _, _, err = td.InteractionEvents(next + 1); !errors.Is(err, task.ErrInteractionRequest) {
		t.Fatalf("future cursor accepted: %v", err)
	}
}

func TestInteractionObserverCannotCloseJournal(t *testing.T) {
	td := interactiveTask(t)
	observer, err := td.store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, observer.Close()) })
	if err = observer.SealInteraction(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("observer sealed journal: %v", err)
	}
	state, err := td.InteractionState()
	must(t, err)
	if state.Closed {
		t.Fatal("failed observer seal closed the journal")
	}
}

func TestInteractionQueuedTaskRemainsPending(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	req.RequestedConfig.Interactive = true
	meta.RequestedConfig = req.RequestedConfig
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	state, err := td.InteractionState()
	must(t, err)
	if state.Closed || state.TurnID != "" {
		t.Fatalf("queued task falsely closed: %+v", state)
	}
}

func TestInteractionOversizedQuestionNeverEntersJournal(t *testing.T) {
	td := interactiveTask(t)
	request := task.InteractionRequest{ID: "large", TurnID: "turn-1", Questions: []task.InteractionQuestion{{ID: "q", Text: strings.Repeat("x", 700*1024), FreeText: true}}}
	err := td.RecordInteraction(task.InteractionEvent{Kind: "request", Request: &request})
	if !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("unobservable question accepted: %v", err)
	}
	state, err := td.InteractionState()
	must(t, err)
	if len(state.PendingRequests) != 0 || state.Cursor != 1 {
		t.Fatalf("rejected question persisted: %+v", state)
	}
	must(t, td.SealInteraction())
}

func TestInteractionUncertainReservationNeverAuthorizesWrite(t *testing.T) {
	td := interactiveTask(t)
	message := task.InteractionMessage{ID: strings.Repeat("e", 32), Kind: "steer", Text: "once"}
	_, err := td.SubmitInteraction(message)
	must(t, err)
	injector := &testFaultInjector{failPostLinkDir: true}
	td.store.faultInjector = injector
	next, err := td.NextInteraction()
	if err == nil || next != nil {
		t.Fatalf("uncertain reservation authorized delivery: %+v %v", next, err)
	}
	injector.failPostLinkDir = false
	next, err = td.NextInteraction()
	must(t, err)
	if next != nil {
		t.Fatal("ambiguous reservation replayed after barrier recovery")
	}
	receipt, err := td.SubmitInteraction(message)
	must(t, err)
	if receipt.State != "unknown" {
		t.Fatalf("uncertain reservation reported as certain: %+v", receipt)
	}
}

func TestInteractionRunnerLossBeforeStartedReceiptClosesObservation(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	req.RequestedConfig.Interactive = true
	meta.RequestedConfig = req.RequestedConfig
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	permit := consumeStart(t, td)
	t.Cleanup(func() { must(t, permit.Release()) })
	observer, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, observer.Close()) })
	state, err := observer.InteractionState()
	must(t, err)
	if state.Closed {
		t.Fatal("live runner falsely closed before recording start")
	}
	must(t, permit.Release())
	state, err = observer.InteractionState()
	must(t, err)
	if !state.Closed {
		t.Fatal("lost runner left interaction open before recording start")
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "interaction")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created an interaction journal: %v", err)
	}
}
