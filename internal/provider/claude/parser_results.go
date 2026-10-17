package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

func applyResultEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.resultSeen {
		state.markSemantic("multiple result events")
		return
	}
	if !state.initSeen {
		state.markSemantic("result precedes system/init")
		return
	}
	result, err := decodeResult(fields, state.init.sessionID)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.resultSeen = true
	state.result = result
}

func decodeResult(fields map[string]json.RawMessage, initSessionID string) (resultState, error) {
	subtype, isError, sessionID, err := decodeResultHeader(fields, initSessionID)
	if err != nil {
		return resultState{}, err
	}
	answer, answerSeen, stopReason, stopReasonSeen, stopReasonNull, diagnostic, errorsPresent, err := decodeResultText(fields)
	if err != nil {
		return resultState{}, err
	}
	metadata, err := decodeResultUsage(fields)
	if err != nil {
		return resultState{}, err
	}
	return resultState{
		sessionID: sessionID, subtype: subtype, isError: isError, isErrorSeen: true,
		answer: answer, answerSeen: answerSeen, stopReason: stopReason,
		stopReasonSeen: stopReasonSeen, stopReasonNull: stopReasonNull,
		diagnostic: diagnostic, errorsPresent: errorsPresent, usage: metadata.usage, modelUsage: metadata.modelUsage,
		totalCostUSD: metadata.totalCostUSD, durationMS: metadata.durationMS, numTurns: metadata.numTurns,
	}, nil
}

func decodeResultHeader(fields map[string]json.RawMessage, initSessionID string) (string, bool, string, error) {
	subtype, err := requiredString(fields, "subtype")
	if err != nil {
		return "", false, "", errors.New("result subtype is missing or not a string")
	}
	if _, known := knownResultSubtypes[subtype]; !known {
		return "", false, "", errors.New("unknown result subtype: " + subtype)
	}
	isError, err := requiredBool(fields, "is_error")
	if err != nil {
		return "", false, "", errors.New("result is_error is missing or not a boolean")
	}
	sessionID, err := requiredString(fields, "session_id")
	if err != nil || !validSessionID(sessionID) || sessionID != initSessionID {
		return "", false, "", errors.New("result session identity conflicts with system/init")
	}
	if err := validateResultFlags(subtype, isError); err != nil {
		return "", false, "", err
	}
	return subtype, isError, sessionID, nil
}

func validateResultFlags(subtype string, isError bool) error {
	if subtype == resultSuccess && isError {
		return errors.New("successful result has is_error=true")
	}
	if subtype != resultSuccess && !isError {
		return errors.New("error result has is_error=false")
	}
	return nil
}

func decodeResultText(fields map[string]json.RawMessage) (string, bool, string, bool, bool, string, bool, error) {
	answer, answerSeen, err := optionalStringField(fields, "result")
	if err != nil {
		return "", false, "", false, false, "", false, errors.New("result answer is not a string")
	}
	stopReason, stopReasonSeen, stopReasonNull, err := optionalStringOrNull(fields, "stop_reason")
	if err != nil {
		return "", false, "", false, false, "", false, errors.New("result stop_reason is not a string or null")
	}
	diagnostic, errorsPresent, err := resultDiagnostic(fields)
	if err != nil {
		return "", false, "", false, false, "", false, err
	}
	return answer, answerSeen, stopReason, stopReasonSeen, stopReasonNull, diagnostic, errorsPresent, nil
}

type resultUsage struct {
	usage         *usageEnvelope
	modelUsage    []modelUsageRecord
	totalCostUSD  *string
	durationMS    *int64
	numTurns      *int64
	usagePresent  bool
	usageReliable bool
}

func decodeResultUsage(fields map[string]json.RawMessage) (resultUsage, error) {
	usage, err := parseUsageEnvelope(fields)
	if err != nil {
		return resultUsage{}, err
	}
	modelUsage, err := parseModelUsage(fields)
	if err != nil {
		return resultUsage{}, err
	}
	totalCostUSD, err := optionalDecimalField(fields, "total_cost_usd")
	if err != nil {
		return resultUsage{}, err
	}
	durationMS, err := optionalNonnegativeIntField(fields, "duration_ms")
	if err != nil {
		return resultUsage{}, err
	}
	numTurns, err := optionalNonnegativeIntField(fields, "num_turns")
	if err != nil {
		return resultUsage{}, err
	}
	if err := validatePermissionDenials(fields); err != nil {
		return resultUsage{}, err
	}
	return resultUsage{usage: usage, modelUsage: modelUsage, totalCostUSD: totalCostUSD, durationMS: durationMS, numTurns: numTurns}, nil
}

func resultDiagnostic(fields map[string]json.RawMessage) (string, bool, error) {
	raw, ok := fields["errors"]
	if !ok {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, errors.New("result errors must be an array of strings")
	}
	var diagnostics []json.RawMessage
	if err := json.Unmarshal(raw, &diagnostics); err != nil || diagnostics == nil {
		return "", false, errors.New("result errors are not an array of strings")
	}
	firstDiagnostic := ""
	for _, rawDiagnostic := range diagnostics {
		diagnostic, err := decodeString(rawDiagnostic)
		if err != nil {
			return "", false, errors.New("result error diagnostic is invalid")
		}
		if !validDiagnostic(diagnostic) {
			return "", false, errors.New("result error diagnostic is invalid")
		}
		if firstDiagnostic == "" && strings.TrimSpace(diagnostic) != "" {
			firstDiagnostic = diagnostic
		}
	}
	return firstDiagnostic, len(diagnostics) != 0, nil
}

func validatePermissionDenials(fields map[string]json.RawMessage) error {
	raw, ok := fields["permission_denials"]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("result permission_denials must be an array")
	}
	var denials []json.RawMessage
	if err := json.Unmarshal(raw, &denials); err != nil || denials == nil {
		return errors.New("result permission_denials are not an array")
	}
	for _, rawDenial := range denials {
		var denial map[string]json.RawMessage
		if err := json.Unmarshal(rawDenial, &denial); err != nil || denial == nil {
			return errors.New("result permission_denials entry is not an object")
		}
	}
	return nil
}
