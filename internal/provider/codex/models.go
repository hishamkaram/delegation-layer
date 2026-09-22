package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	modelDiscoverySource = "codex app-server"
	initializeRequestID  = 1
	firstListRequestID   = 2
	maxModelPages        = 4096
	maxModelRows         = 4096
)

var errCodexControlFailure = errors.New("codex model discovery control request failed")

var errCodexPaginationIncomplete = errors.New("codex model discovery pagination incomplete")

// ModelDiscovery describes Codex's app-server stdio model exchange. The
// shared inspection runner owns the process and writes the returned requests.
func ModelDiscovery() commonprovider.ModelDiscoveryDefinition {
	return commonprovider.ModelDiscoveryDefinition{
		Arguments:   []string{"app-server"},
		Source:      modelDiscoverySource,
		Capability:  commonprovider.RuntimeCapability{HelpArgs: []string{"app-server"}},
		NewExchange: func() commonprovider.ModelExchange { return &modelExchange{} },
		Project:     projectModels,
	}
}

type modelExchange struct {
	started       bool
	finished      bool
	initialized   bool
	pendingID     int
	pageCount     int
	modelCount    int
	nextRequestID int
}

func (e *modelExchange) Start() ([][]byte, error) {
	if e == nil || e.started {
		return nil, errors.New("codex model exchange already started")
	}
	e.started = true
	e.pendingID = initializeRequestID
	message := map[string]any{
		"id":     initializeRequestID,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "delegation-layer",
				"title":   "Delegation Layer",
				"version": commonprovider.ModelsRevision,
			},
		},
	}
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode codex model initialize: %w", err)
	}
	return [][]byte{data}, nil
}

func (e *modelExchange) Accept(line []byte) ([][]byte, bool, error) {
	if e == nil || !e.started || e.finished {
		return nil, false, errors.New("codex model exchange is not accepting replies")
	}
	message, err := decodeRPCMessage(line, "codex model response")
	if err != nil {
		return nil, false, err
	}
	if len(message.ID) == 0 {
		if message.Method == "" {
			return nil, false, errors.New("codex model notification has no method")
		}
		return nil, false, nil
	}
	id, err := requiredInt(message.ID, "id")
	if err != nil || id != e.pendingID {
		return nil, false, errors.New("codex model response has the wrong request id")
	}
	if hasRPCError(message) {
		if err := validateRPCError(message.Error); err != nil {
			return nil, false, err
		}
		e.finished = true
		return nil, true, nil
	}
	if len(message.Result) == 0 {
		return nil, false, errors.New("codex model response has no result")
	}
	if !e.initialized {
		return e.acceptInitialize(message.Result)
	}
	return e.acceptPage(message.Result)
}

func (e *modelExchange) acceptInitialize(result []byte) ([][]byte, bool, error) {
	if !isJSONObject(result) {
		return nil, false, errors.New("codex initialize result is not an object")
	}
	e.initialized = true
	e.nextRequestID = firstListRequestID
	e.pendingID = firstListRequestID
	request, err := modelListRequest(firstListRequestID, "")
	if err != nil {
		return nil, false, err
	}
	return [][]byte{[]byte(`{"method":"initialized","params":{}}`), request}, false, nil
}

func (e *modelExchange) acceptPage(result []byte) ([][]byte, bool, error) {
	rows, cursor, err := decodePage(result)
	if err != nil {
		return nil, false, err
	}
	e.pageCount++
	e.modelCount += len(rows)
	if e.pageCount > maxModelPages || e.modelCount > maxModelRows {
		return nil, false, errors.New("codex model list exceeds bound")
	}
	if cursor == "" {
		e.finished = true
		return nil, true, nil
	}
	e.nextRequestID++
	e.pendingID = e.nextRequestID
	request, err := modelListRequest(e.nextRequestID, cursor)
	if err != nil {
		return nil, false, err
	}
	return [][]byte{request}, false, nil
}

func modelListRequest(id int, cursor string) ([]byte, error) {
	params := map[string]any{}
	if cursor != "" {
		params["cursor"] = cursor
	}
	data, err := json.Marshal(map[string]any{"id": id, "method": "model/list", "params": params})
	if err != nil {
		return nil, fmt.Errorf("encode codex model/list request: %w", err)
	}
	return data, nil
}

func projectModels(stdout, stderr, help []byte, success bool) (commonprovider.ModelCatalog, error) {
	efforts, effortsListed := commonprovider.ParseListedModelEfforts(help, "--effort")
	if !success {
		return nativeFailure(stdout, stderr, efforts, effortsListed)
	}
	models, err := parseModelOutput(stdout)
	if errors.Is(err, errCodexPaginationIncomplete) {
		if len(models) == 0 {
			return finishCatalog(baseCatalog("failed", "pagination_incomplete", efforts, effortsListed))
		}
		catalog := baseCatalog("partial", "pagination_incomplete", efforts, effortsListed)
		catalog.Models = models
		return finishCatalog(catalog)
	}
	if errors.Is(err, errCodexControlFailure) {
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

type modelOutputState struct {
	initialized bool
	seenList    bool
	finalPage   bool
	terminal    bool
	expectedID  int
	pageCount   int
	models      []commonprovider.ModelInfo
	seenIDs     map[string]struct{}
}

func parseModelOutput(data []byte) ([]commonprovider.ModelInfo, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), int(commonprovider.MaxInspectionOutput))
	state := modelOutputState{expectedID: initializeRequestID, seenIDs: make(map[string]struct{})}
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		message, err := decodeRPCMessage(scanner.Bytes(), "codex model output")
		if err != nil {
			return state.models, fmt.Errorf("codex model output line %d: %w", lineNumber, err)
		}
		if err := state.accept(message); err != nil {
			return state.models, err
		}
	}
	if err := scanner.Err(); err != nil {
		return state.models, fmt.Errorf("read codex model output: %w", err)
	}
	if state.terminal {
		return state.models, errCodexControlFailure
	}
	if !state.initialized || !state.seenList {
		return state.models, errors.New("codex model output is missing initialize or model/list response")
	}
	if !state.finalPage {
		return state.models, errCodexPaginationIncomplete
	}
	return state.models, nil
}

func (s *modelOutputState) accept(message rpcMessage) error {
	if len(message.ID) == 0 {
		if message.Method == "" {
			return errors.New("codex model output notification has no method")
		}
		return nil
	}
	if s.finalPage || s.terminal {
		return errors.New("codex model output has a response after completion")
	}
	id, err := requiredInt(message.ID, "id")
	if err != nil || id != s.expectedID {
		return errors.New("codex model output has an unexpected request id")
	}
	if hasRPCError(message) {
		return s.acceptError(message.Error)
	}
	if len(message.Result) == 0 {
		return errors.New("codex model output response has no result")
	}
	if !s.initialized {
		return s.acceptInitialize(id, message.Result)
	}
	return s.acceptPage(id, message.Result)
}

func (s *modelOutputState) acceptError(data []byte) error {
	if err := validateRPCError(data); err != nil {
		return err
	}
	if s.terminal {
		return errors.New("codex model output has a duplicate terminal response")
	}
	s.terminal = true
	return nil
}

func (s *modelOutputState) acceptInitialize(id int, result []byte) error {
	if id != initializeRequestID || s.terminal || !isJSONObject(result) {
		return errors.New("codex model output has an invalid initialize response")
	}
	s.initialized = true
	s.expectedID = firstListRequestID
	return nil
}

func (s *modelOutputState) acceptPage(id int, result []byte) error {
	if s.terminal || !s.initialized || id != s.expectedID {
		return errors.New("codex model output has an unexpected model/list response")
	}
	rows, cursor, err := decodePage(result)
	if err != nil {
		return err
	}
	s.pageCount++
	if s.pageCount > maxModelPages {
		return errors.New("codex model output exceeds page bound")
	}
	for _, model := range rows {
		if _, exists := s.seenIDs[model.ID]; exists {
			return fmt.Errorf("duplicate codex model id %q", model.ID)
		}
		s.seenIDs[model.ID] = struct{}{}
		s.models = append(s.models, model)
	}
	if len(s.models) > maxModelRows {
		return errors.New("codex model output exceeds model bound")
	}
	s.seenList = true
	if cursor == "" {
		s.finalPage = true
	} else {
		s.finalPage = false
		s.expectedID++
	}
	return nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func decodeRPCMessage(data []byte, label string) (rpcMessage, error) {
	if err := task.ValidateJSONStructure(data); err != nil {
		return rpcMessage{}, fmt.Errorf("%s is malformed: %w", label, err)
	}
	var message rpcMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return rpcMessage{}, fmt.Errorf("%s is not an object: %w", label, err)
	}
	return message, nil
}

func hasRPCError(message rpcMessage) bool {
	return len(message.Error) != 0 && !bytes.Equal(bytes.TrimSpace(message.Error), []byte("null"))
}

func validateRPCError(data []byte) error {
	if !isJSONObject(data) {
		return errors.New("codex model error is not an object")
	}
	var rpcError struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(data, &rpcError); err != nil || (len(rpcError.Code) == 0 && rpcError.Message == "") {
		return errors.New("codex model error has no code or message")
	}
	return nil
}

type modelListResult struct {
	Data       json.RawMessage `json:"data"`
	NextCursor json.RawMessage `json:"nextCursor"`
}

func decodePage(data []byte) ([]commonprovider.ModelInfo, string, error) {
	if !isJSONObject(data) {
		return nil, "", errors.New("codex model/list result is not an object")
	}
	var page modelListResult
	if err := json.Unmarshal(data, &page); err != nil || len(page.Data) == 0 || bytes.Equal(bytes.TrimSpace(page.Data), []byte("null")) {
		return nil, "", errors.New("codex model/list response has no data array")
	}
	var rows []codexModelMetadata
	if err := json.Unmarshal(page.Data, &rows); err != nil {
		return nil, "", fmt.Errorf("codex model/list data is not an array: %w", err)
	}
	models := make([]commonprovider.ModelInfo, 0, len(rows))
	for index, row := range rows {
		model, err := decodeModel(row)
		if err != nil {
			return models, "", fmt.Errorf("codex model %d: %w", index, err)
		}
		models = append(models, model)
	}
	cursor, err := decodeCursor(page.NextCursor)
	return models, cursor, err
}

type codexModelMetadata struct {
	ID                        *string         `json:"id"`
	Model                     *string         `json:"model"`
	DisplayName               string          `json:"displayName"`
	Name                      string          `json:"name"`
	SupportedReasoningEfforts json.RawMessage `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort    json.RawMessage `json:"defaultReasoningEffort"`
	DefaultEffort             json.RawMessage `json:"defaultEffort"`
}

func decodeModel(row codexModelMetadata) (commonprovider.ModelInfo, error) {
	id := ""
	if row.Model != nil {
		id = *row.Model
	} else if row.ID != nil {
		id = *row.ID
	}
	if id == "" {
		return commonprovider.ModelInfo{}, errors.New("codex model has no id or model value")
	}
	model := commonprovider.ModelInfo{ID: id, Name: row.DisplayName}
	if model.Name == "" {
		model.Name = row.Name
	}
	if len(row.SupportedReasoningEfforts) != 0 {
		var err error
		model.Efforts, err = decodeEfforts(row.SupportedReasoningEfforts)
		if err != nil {
			return commonprovider.ModelInfo{}, err
		}
	}
	defaultValue := row.DefaultReasoningEffort
	if len(defaultValue) == 0 {
		defaultValue = row.DefaultEffort
	}
	if len(defaultValue) != 0 {
		var err error
		model.DefaultEffort, err = decodeEffortValue(defaultValue)
		if err != nil {
			return commonprovider.ModelInfo{}, err
		}
	}
	return model, nil
}

func decodeEfforts(data []byte) ([]string, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, errors.New("codex supported reasoning efforts are not an array")
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		effort, err := decodeEffortValue(value)
		if err != nil || effort == "" {
			return nil, errors.New("codex reasoning effort is invalid")
		}
		result = append(result, effort)
	}
	return result, nil
}

func decodeEffortValue(data []byte) (string, error) {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		return value, nil
	}
	var object struct {
		ReasoningEffort string `json:"reasoningEffort"`
		Effort          string `json:"effort"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return "", errors.New("codex reasoning effort is not a string or object")
	}
	if object.ReasoningEffort != "" {
		return object.ReasoningEffort, nil
	}
	return object.Effort, nil
}

func decodeCursor(data []byte) (string, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return "", nil
	}
	var cursor string
	if err := json.Unmarshal(data, &cursor); err != nil {
		return "", errors.New("codex next cursor is not a string")
	}
	return cursor, nil
}

func requiredInt(data []byte, key string) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, fmt.Errorf("codex response %s is not an integer", key)
	}
	value, err := strconv.Atoi(string(number))
	if err != nil {
		return 0, fmt.Errorf("codex response %s is not an integer", key)
	}
	return value, nil
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
