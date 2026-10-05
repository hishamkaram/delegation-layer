package task

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	ErrInteractionUnavailable = errors.New("task interaction is unavailable")
	ErrInteractionConflict    = errors.New("interaction message conflicts with existing request")
	ErrInteractionClosed      = errors.New("task interaction has ended")
	ErrInteractionRequest     = errors.New("interaction request is stale or invalid")
)

// InteractionCapabilities describes native interaction, not live readiness.
type InteractionCapabilities struct {
	SupportedModes []string `json:"supported_modes,omitempty"`
	Clarification  bool     `json:"clarification"`
	Steering       bool     `json:"steering"`
}

type InteractionQuestion struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Options  []string `json:"options,omitempty"`
	Multiple bool     `json:"multiple"`
	FreeText bool     `json:"free_text"`
}

type InteractionRequest struct {
	ID        string                `json:"id"`
	TurnID    string                `json:"turn_id"`
	Questions []InteractionQuestion `json:"questions"`
	Blocking  bool                  `json:"blocking"`
}

// InteractionMessage is immutable once admitted to a task inbox.
type InteractionMessage struct {
	ID        string              `json:"id"`
	Kind      string              `json:"kind"`
	RequestID string              `json:"request_id,omitempty"`
	TurnID    string              `json:"turn_id"`
	Answers   map[string][]string `json:"answers,omitempty"`
	Text      string              `json:"text,omitempty"`
}

type InteractionDelivery struct {
	MessageID string `json:"message_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

type InteractionEvent struct {
	Deadline     string                   `json:"deadline,omitempty"`
	Sequence     uint64                   `json:"sequence"`
	Kind         string                   `json:"kind"`
	TurnID       string                   `json:"turn_id,omitempty"`
	Request      *InteractionRequest      `json:"request,omitempty"`
	RequestID    string                   `json:"request_id,omitempty"`
	Message      *InteractionMessage      `json:"message,omitempty"`
	Delivery     *InteractionDelivery     `json:"delivery,omitempty"`
	Capabilities *InteractionCapabilities `json:"capabilities,omitempty"`
}

type InteractionState struct {
	Deadline        string                  `json:"deadline,omitempty"`
	Capabilities    InteractionCapabilities `json:"capabilities"`
	TurnID          string                  `json:"turn_id,omitempty"`
	PendingRequests []InteractionRequest    `json:"pending_requests"`
	Deliveries      []InteractionDelivery   `json:"deliveries"`
	Cursor          uint64                  `json:"cursor"`
	Closed          bool                    `json:"closed"`
}

// ApplyInteractionEvent reconstructs observation from the durable journal.
func ApplyInteractionEvent(state *InteractionState, event InteractionEvent) error {
	if event.Sequence != state.Cursor+1 {
		return ErrEvidenceFault
	}
	if state.Closed {
		return ErrInteractionClosed
	}
	if err := applyInteractionEvent(state, event); err != nil {
		return err
	}
	state.Cursor = event.Sequence
	return nil
}

func applyInteractionEvent(state *InteractionState, event InteractionEvent) error {
	switch event.Kind {
	case "ready":
		if event.TurnID == "" || event.Capabilities == nil || state.TurnID != "" {
			return ErrInteractionConflict
		}
		if event.Deadline != "" {
			if err := validateTimestamp(event.Deadline); err != nil {
				return err
			}
		}
		state.TurnID, state.Capabilities = event.TurnID, *event.Capabilities
		state.Deadline = event.Deadline
	case "request":
		return applyInteractionRequest(state, event.Request)
	case "resolved":
		if !slices.ContainsFunc(state.PendingRequests, func(r InteractionRequest) bool { return r.ID == event.RequestID }) {
			return ErrInteractionRequest
		}
		state.PendingRequests = slices.DeleteFunc(state.PendingRequests, func(r InteractionRequest) bool { return r.ID == event.RequestID })
	case "message":
		return applyInteractionMessage(state, event.Message)
	case "delivery":
		return applyInteractionDelivery(state, event.Delivery)
	case "closed":
		state.Closed = true
		state.PendingRequests = nil
		for i := range state.Deliveries {
			if state.Deliveries[i].State == "queued" {
				state.Deliveries[i].State = "rejected"
				state.Deliveries[i].Reason = "task interaction ended before delivery"
			}
		}
	default:
		return ErrInteractionRequest
	}
	return nil
}

func applyInteractionRequest(state *InteractionState, request *InteractionRequest) error {
	if request == nil || request.ID == "" || request.TurnID == "" || request.TurnID != state.TurnID || !state.Capabilities.Clarification {
		return ErrInteractionRequest
	}
	if slices.ContainsFunc(state.PendingRequests, func(r InteractionRequest) bool { return r.ID == request.ID }) {
		return ErrInteractionConflict
	}
	if err := validateInteractionQuestions(request.Questions); err != nil {
		return err
	}
	state.PendingRequests = append(state.PendingRequests, *request)
	return nil
}

func validateInteractionQuestions(questions []InteractionQuestion) error {
	if len(questions) == 0 {
		return ErrInteractionRequest
	}
	seen := make(map[string]bool, len(questions))
	for _, question := range questions {
		if !nonblank(question.ID) || seen[question.ID] || !nonblank(question.Text) || !utf8.ValidString(question.Text) {
			return ErrInteractionRequest
		}
		if len(question.Options) == 0 && !question.FreeText {
			return ErrInteractionRequest
		}
		seen[question.ID] = true
	}
	return nil
}

func applyInteractionMessage(state *InteractionState, message *InteractionMessage) error {
	if message == nil {
		return ErrInteractionRequest
	}
	if slices.ContainsFunc(state.Deliveries, func(d InteractionDelivery) bool { return d.MessageID == message.ID }) {
		return ErrInteractionConflict
	}
	if err := ValidateInteractionMessage(*message, *state); err != nil {
		return err
	}
	state.Deliveries = append(state.Deliveries, InteractionDelivery{MessageID: message.ID, State: "queued"})
	return nil
}

func applyInteractionDelivery(state *InteractionState, delivery *InteractionDelivery) error {
	if delivery == nil {
		return ErrInteractionRequest
	}
	for index, previous := range state.Deliveries {
		if previous.MessageID != delivery.MessageID {
			continue
		}
		if !validInteractionDelivery(previous.State, delivery.State) {
			return ErrInteractionConflict
		}
		state.Deliveries[index] = *delivery
		return nil
	}
	return ErrInteractionRequest
}

func validInteractionDelivery(previous, next string) bool {
	switch previous {
	case "queued":
		return next == "unknown" || next == "rejected"
	case "unknown":
		return next == "sent" || next == "acknowledged" || next == "rejected"
	case "sent":
		return next == "acknowledged" || next == "rejected"
	default:
		return false
	}
}

func ValidateInteractionMessage(message InteractionMessage, state InteractionState) error {
	if err := ValidateTaskID(message.ID); err != nil {
		return err
	}
	if state.Closed {
		return ErrInteractionClosed
	}
	if message.TurnID == "" || message.TurnID != state.TurnID {
		return ErrInteractionUnavailable
	}
	switch message.Kind {
	case "steer":
		return validateInteractionSteer(message, state)
	case "reply":
		if !state.Capabilities.Clarification || message.Text != "" {
			return ErrInteractionUnavailable
		}
		for _, request := range state.PendingRequests {
			if request.ID == message.RequestID {
				return validateInteractionAnswers(message.Answers, request.Questions)
			}
		}
		return ErrInteractionRequest
	default:
		return ErrInteractionRequest
	}
}

func validateInteractionAnswers(answers map[string][]string, questions []InteractionQuestion) error {
	if len(answers) != len(questions) || len(answers) == 0 {
		return ErrInteractionRequest
	}
	for _, question := range questions {
		values := answers[question.ID]
		if len(values) == 0 || (!question.Multiple && len(values) != 1) {
			return ErrInteractionRequest
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || (!question.FreeText && !slices.Contains(question.Options, value)) {
				return fmt.Errorf("%w: answer for %s", ErrInteractionRequest, question.ID)
			}
		}
	}
	return nil
}

func validateInteractionSteer(message InteractionMessage, state InteractionState) error {
	if !state.Capabilities.Steering {
		return ErrInteractionUnavailable
	}
	if strings.TrimSpace(message.Text) == "" || len(message.Text) > MaxBriefSize || !utf8.ValidString(message.Text) || message.RequestID != "" || len(message.Answers) != 0 {
		return ErrInteractionRequest
	}
	return nil
}
