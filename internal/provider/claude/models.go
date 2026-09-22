package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	modelDiscoverySource = "claude stream-json initialize"
	modelRequestID       = "delegation-layer-model-discovery"
)

var errClaudeControlFailure = errors.New("claude model discovery control request failed")

// ModelDiscovery asks Claude Code's stream-json control channel for its
// initialization model metadata. No user message is sent.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments: []string{
			"--print",
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
		},
		Source: modelDiscoverySource,
		Capability: commonprovider.RuntimeCapability{RequiredFlags: []string{
			"--print", "--input-format", "--output-format", "--verbose",
		}},
		NewExchange: func() commonprovider.ModelExchange { return &claudeModelExchange{} },
		Project:     projectModels,
	}
}

type claudeModelExchange struct {
	started bool
	done    bool
}

func (e *claudeModelExchange) Start() ([][]byte, error) {
	if e == nil || e.started {
		return nil, errors.New("claude model exchange already started")
	}
	e.started = true
	message := map[string]any{
		"request":    map[string]string{"subtype": "initialize"},
		"request_id": modelRequestID,
		"type":       "control_request",
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode claude model initialize: %w", err)
	}
	return [][]byte{data}, nil
}

func (e *claudeModelExchange) Accept(line []byte) ([][]byte, bool, error) {
	if e == nil || !e.started || e.done {
		return nil, false, errors.New("claude model exchange is not accepting replies")
	}
	decoded, err := decodeClaudeLine(line, "claude control response")
	if err != nil {
		return nil, false, err
	}
	if decoded.telemetry {
		return nil, false, nil
	}
	if decoded.response.RequestID != modelRequestID {
		return nil, false, errors.New("claude model control response has the wrong request id")
	}
	switch decoded.response.Subtype {
	case "success":
		if !isJSONObject(decoded.response.Response) {
			return nil, false, errors.New("claude model control response has no result object")
		}
	case "error":
		if !hasControlError(decoded.response) {
			return nil, false, errors.New("claude control error has no error field")
		}
	default:
		return nil, false, errors.New("claude model initialize returned an unknown control response")
	}
	e.done = true
	return nil, true, nil
}

func projectModels(stdout, stderr, help []byte, success bool) (commonprovider.ModelCatalog, error) {
	efforts, effortsListed := commonprovider.ParseListedModelEfforts(help, "--effort")
	if !success {
		return nativeFailure(stdout, stderr, efforts, effortsListed)
	}
	models, err := parseInitialization(stdout)
	if errors.Is(err, errClaudeControlFailure) {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		return finishCatalog(baseCatalog("failed", "native_protocol_error", efforts, effortsListed))
	}
	if err != nil {
		if commonprovider.ModelAuthFailure(stdout, stderr) {
			return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, effortsListed))
		}
		return commonprovider.ModelCatalog{}, err
	}
	if len(models) == 0 {
		return finishCatalog(baseCatalog("unavailable", "models_unavailable", efforts, effortsListed))
	}
	catalog := baseCatalog("available", "ok", efforts, effortsListed)
	catalog.Complete = true
	catalog.Models = models
	return finishCatalog(catalog)
}

func nativeFailure(stdout, stderr []byte, efforts []string, listed bool) (commonprovider.ModelCatalog, error) {
	if commonprovider.ModelAuthFailure(stdout, stderr) {
		return finishCatalog(baseCatalog("blocked", "authentication_unavailable", efforts, listed))
	}
	status, reason := "failed", "native_command_failed"
	if commonprovider.ModelInterfaceUnavailable(stdout, stderr) {
		status, reason = "unavailable", "interface_unavailable"
	}
	return finishCatalog(baseCatalog(status, reason, efforts, listed))
}

func parseInitialization(data []byte) ([]commonprovider.ModelInfo, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), int(commonprovider.MaxInspectionOutput))
	state := claudeInitializationState{seenIDs: make(map[string]struct{})}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		decoded, err := decodeClaudeLine(scanner.Bytes(), "claude model output")
		if err != nil {
			return state.models, fmt.Errorf("claude model output line %d: %w", lineNumber, err)
		}
		if decoded.telemetry {
			continue
		}
		if err := state.accept(decoded.response); err != nil {
			return state.models, err
		}
	}
	if err := scanner.Err(); err != nil {
		return state.models, fmt.Errorf("read claude model output: %w", err)
	}
	if !state.seenResponse {
		return state.models, errors.New("claude model output has no initialize response")
	}
	if state.controlFailure {
		return state.models, errClaudeControlFailure
	}
	return state.models, nil
}

type claudeInitializationState struct {
	models         []commonprovider.ModelInfo
	seenIDs        map[string]struct{}
	seenResponse   bool
	controlFailure bool
}

func (s *claudeInitializationState) accept(response claudeControlResponse) error {
	if response.RequestID != modelRequestID {
		return errors.New("claude model output has the wrong request id")
	}
	if s.seenResponse {
		return errors.New("claude model output contains multiple initialize responses")
	}
	s.seenResponse = true
	switch response.Subtype {
	case "error":
		if !hasControlError(response) {
			return errors.New("claude control error has no error field")
		}
		s.controlFailure = true
		return nil
	case "success":
		return s.addModels(response.Response)
	default:
		return errors.New("claude model initialize returned an unknown control response")
	}
}

func (s *claudeInitializationState) addModels(data []byte) error {
	rows, err := decodeInitializationModels(data)
	if err != nil {
		return err
	}
	for _, model := range rows {
		if _, exists := s.seenIDs[model.ID]; exists {
			return fmt.Errorf("duplicate claude model id %q", model.ID)
		}
		s.seenIDs[model.ID] = struct{}{}
		s.models = append(s.models, model)
	}
	if len(s.models) > 4096 {
		return errors.New("claude model list exceeds model bound")
	}
	return nil
}

type claudeDecodedLine struct {
	telemetry bool
	response  claudeControlResponse
}

type claudeEnvelope struct {
	Type     string          `json:"type"`
	Subtype  string          `json:"subtype"`
	Response json.RawMessage `json:"response"`
}

type claudeControlResponse struct {
	RequestID string          `json:"request_id"`
	Subtype   string          `json:"subtype"`
	Response  json.RawMessage `json:"response"`
	Error     json.RawMessage `json:"error"`
}

func decodeClaudeLine(data []byte, label string) (claudeDecodedLine, error) {
	if err := task.ValidateJSONStructure(data); err != nil {
		return claudeDecodedLine{}, fmt.Errorf("%s is malformed: %w", label, err)
	}
	var envelope claudeEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return claudeDecodedLine{}, fmt.Errorf("%s is not an object: %w", label, err)
	}
	if envelope.Type != "control_response" {
		if knownTelemetry(envelope) {
			return claudeDecodedLine{telemetry: true}, nil
		}
		return claudeDecodedLine{}, errors.New("unexpected claude model protocol message")
	}
	if !isJSONObject(envelope.Response) {
		return claudeDecodedLine{}, errors.New("claude control response has no response object")
	}
	var response claudeControlResponse
	if err := json.Unmarshal(envelope.Response, &response); err != nil {
		return claudeDecodedLine{}, fmt.Errorf("claude control response is malformed: %w", err)
	}
	return claudeDecodedLine{response: response}, nil
}

func knownTelemetry(envelope claudeEnvelope) bool {
	if envelope.Type == "rate_limit_event" || envelope.Type == "hook_started" || envelope.Type == "hook_progress" || envelope.Type == "hook_response" {
		return true
	}
	if envelope.Type != "system" {
		return false
	}
	for _, subtype := range []string{
		"init", "hook_started", "hook_progress", "hook_response", "status",
		"compact_boundary", "files_persisted", "rate_limit_event", "api_error",
	} {
		if envelope.Subtype == subtype {
			return true
		}
	}
	return false
}

func hasControlError(response claudeControlResponse) bool {
	return len(response.Error) != 0 && !bytes.Equal(bytes.TrimSpace(response.Error), []byte("null"))
}

type claudeInitPayload struct {
	Models json.RawMessage `json:"models"`
}

func decodeInitializationModels(data []byte) ([]commonprovider.ModelInfo, error) {
	if !isJSONObject(data) {
		return nil, errors.New("claude initialize response has no result object")
	}
	var payload claudeInitPayload
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Models) == 0 || bytes.Equal(bytes.TrimSpace(payload.Models), []byte("null")) {
		return nil, errors.New("claude initialize response has no models array")
	}
	var rows []claudeModelMetadata
	if err := json.Unmarshal(payload.Models, &rows); err != nil {
		return nil, fmt.Errorf("claude initialize models is not an array: %w", err)
	}
	models := make([]commonprovider.ModelInfo, 0, len(rows))
	for index, row := range rows {
		model, err := decodeModel(row)
		if err != nil {
			return models, fmt.Errorf("claude model %d: %w", index, err)
		}
		models = append(models, model)
	}
	return models, nil
}

type claudeModelMetadata struct {
	Value                 string          `json:"value"`
	DisplayName           string          `json:"displayName"`
	SupportedEffortLevels json.RawMessage `json:"supportedEffortLevels"`
	SupportsEffort        *bool           `json:"supportsEffort"`
	DefaultEffort         json.RawMessage `json:"defaultEffort"`
	DefaultEffortSnake    json.RawMessage `json:"default_effort"`
}

func decodeModel(row claudeModelMetadata) (commonprovider.ModelInfo, error) {
	if row.Value == "" {
		return commonprovider.ModelInfo{}, errors.New("claude model value is missing or empty")
	}
	model := commonprovider.ModelInfo{ID: row.Value, Name: row.DisplayName}
	if len(row.SupportedEffortLevels) != 0 && !bytes.Equal(bytes.TrimSpace(row.SupportedEffortLevels), []byte("null")) {
		if err := json.Unmarshal(row.SupportedEffortLevels, &model.Efforts); err != nil {
			return commonprovider.ModelInfo{}, errors.New("claude supported effort levels are not an array of strings")
		}
	} else if row.SupportsEffort != nil && !*row.SupportsEffort {
		model.Efforts = []string{}
	}
	defaultValue := row.DefaultEffort
	if len(defaultValue) == 0 {
		defaultValue = row.DefaultEffortSnake
	}
	if len(defaultValue) != 0 {
		if err := json.Unmarshal(defaultValue, &model.DefaultEffort); err != nil {
			return commonprovider.ModelInfo{}, errors.New("claude default effort is not a string")
		}
	}
	return model, nil
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 1 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func baseCatalog(status, reason string, efforts []string, listed bool) commonprovider.ModelCatalog {
	catalog := commonprovider.ModelCatalog{
		Status:     status,
		ReasonCode: reason,
		Source:     modelDiscoverySource,
		Models:     []commonprovider.ModelInfo{},
	}
	if listed {
		catalog.Efforts = efforts
	}
	return catalog
}

func finishCatalog(catalog commonprovider.ModelCatalog) (commonprovider.ModelCatalog, error) {
	if err := commonprovider.ValidateModelCatalog(catalog); err != nil {
		return commonprovider.ModelCatalog{}, err
	}
	return catalog, nil
}
