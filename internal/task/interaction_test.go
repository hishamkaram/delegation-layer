package task

import (
	"errors"
	"strings"
	"testing"
)

func interactionFixture(t *testing.T) InteractionState {
	t.Helper()
	state := InteractionState{}
	events := []InteractionEvent{
		{Sequence: 1, Kind: "ready", TurnID: "turn-1", Capabilities: &InteractionCapabilities{Clarification: true, Steering: true}},
		{Sequence: 2, Kind: "request", Request: &InteractionRequest{ID: "request-1", TurnID: "turn-1", Blocking: true, Questions: []InteractionQuestion{{ID: "choice", Text: "Choose a destination", Options: []string{"staging", "production"}}}}},
	}
	for _, event := range events {
		if err := ApplyInteractionEvent(&state, event); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestInteractionAnswersAreBoundToOutstandingQuestions(t *testing.T) {
	state := interactionFixture(t)
	message := InteractionMessage{ID: strings.Repeat("a", 32), Kind: "reply", TurnID: "turn-1", RequestID: "request-1", Answers: map[string][]string{"choice": {"staging"}}}
	if err := ValidateInteractionMessage(message, state); err != nil {
		t.Fatal(err)
	}
	for _, answers := range []map[string][]string{nil, {"wrong": {"staging"}}, {"choice": {"staging", "production"}}, {"choice": {"arbitrary"}}, {"choice": {""}}} {
		message.Answers = answers
		if err := ValidateInteractionMessage(message, state); !errors.Is(err, ErrInteractionRequest) {
			t.Fatalf("answers %v: %v", answers, err)
		}
	}
}

func TestInteractionDeliveryCannotReplayOrRegress(t *testing.T) {
	state := interactionFixture(t)
	message := InteractionMessage{ID: strings.Repeat("a", 32), Kind: "steer", TurnID: "turn-1", Text: "Focus on latency"}
	if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: 3, Kind: "message", Message: &message}); err != nil {
		t.Fatal(err)
	}
	for i, status := range []string{"unknown", "sent", "acknowledged"} {
		if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: uint64(i + 4), Kind: "delivery", Delivery: &InteractionDelivery{MessageID: message.ID, State: status}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"queued", "unknown", "sent", "rejected"} {
		if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: 7, Kind: "delivery", Delivery: &InteractionDelivery{MessageID: message.ID, State: status}}); !errors.Is(err, ErrInteractionConflict) {
			t.Fatalf("regression %s: %v", status, err)
		}
	}
}

func TestInteractionCloseRejectsQueuedButPreservesUncertainDelivery(t *testing.T) {
	state := interactionFixture(t)
	for i, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		message := InteractionMessage{ID: id, Kind: "steer", TurnID: "turn-1", Text: "Focus"}
		if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: uint64(i + 3), Kind: "message", Message: &message}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: 5, Kind: "delivery", Delivery: &InteractionDelivery{MessageID: state.Deliveries[0].MessageID, State: "unknown"}}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: 6, Kind: "closed"}); err != nil {
		t.Fatal(err)
	}
	if !state.Closed || len(state.PendingRequests) != 0 || state.Deliveries[0].State != "unknown" || state.Deliveries[1].State != "rejected" {
		t.Fatalf("bad closure: %+v", state)
	}
	if err := ApplyInteractionEvent(&state, InteractionEvent{Sequence: 7, Kind: "ready"}); !errors.Is(err, ErrInteractionClosed) {
		t.Fatalf("reopened closed interaction: %v", err)
	}
}
