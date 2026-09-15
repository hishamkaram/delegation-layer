package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// usageEnvelope mirrors provider-reported main-loop counters. Every pointer
// remains nil when Claude omits a counter or reports JSON null; no total is
// calculated from the other counters.
type usageEnvelope struct {
	input         *int64
	output        *int64
	thinking      *int64
	total         *int64
	cacheRead     *int64
	cacheCreation *int64
}

type modelUsageRecord struct {
	model         string
	input         *int64
	output        *int64
	thinking      *int64
	total         *int64
	cacheRead     *int64
	cacheCreation *int64
	costUSD       *string
}

func parseUsageEnvelope(fields map[string]json.RawMessage) (*usageEnvelope, error) {
	raw, ok := fields["usage"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var usageFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &usageFields); err != nil || usageFields == nil {
		return nil, errors.New("result usage is not an object")
	}
	usage := &usageEnvelope{}
	var err error
	if usage.input, err = optionalNonnegativeIntField(usageFields, "input_tokens"); err != nil {
		return nil, err
	}
	if usage.output, err = optionalNonnegativeIntField(usageFields, "output_tokens"); err != nil {
		return nil, err
	}
	if usage.thinking, err = optionalNonnegativeIntField(usageFields, "thinking_tokens"); err != nil {
		return nil, err
	}
	if usage.total, err = optionalNonnegativeIntField(usageFields, "total_tokens"); err != nil {
		return nil, err
	}
	if usage.cacheRead, err = optionalNonnegativeIntField(usageFields, "cache_read_input_tokens"); err != nil {
		return nil, err
	}
	if usage.cacheCreation, err = optionalNonnegativeIntField(usageFields, "cache_creation_input_tokens"); err != nil {
		return nil, err
	}
	return usage, nil
}

func parseModelUsage(fields map[string]json.RawMessage) ([]modelUsageRecord, error) {
	raw, ok := fields["modelUsage"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(raw, &models); err != nil || models == nil {
		return nil, errors.New("result modelUsage is not an object")
	}
	if len(models) > maxModelRecords {
		return nil, fmt.Errorf("result modelUsage exceeds bounded record count %d", maxModelRecords)
	}
	keys := make([]string, 0, len(models))
	for model := range models {
		if !validModelName(model) || len(model) > maxModelNameBytes {
			return nil, errors.New("result modelUsage contains an invalid model name")
		}
		keys = append(keys, model)
	}
	// Sorting makes interpretation deterministic even though JSON object map
	// iteration has deliberately unspecified order.
	slices.Sort(keys)
	result := make([]modelUsageRecord, 0, len(keys))
	for _, model := range keys {
		var modelFields map[string]json.RawMessage
		if err := json.Unmarshal(models[model], &modelFields); err != nil || modelFields == nil {
			return nil, fmt.Errorf("result modelUsage[%q] is not an object", model)
		}
		record, err := parseModelUsageRecord(model, modelFields)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

func parseModelUsageRecord(model string, fields map[string]json.RawMessage) (modelUsageRecord, error) {
	record := modelUsageRecord{model: model}
	var err error
	if record.input, err = optionalNonnegativeIntField(fields, "inputTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] inputTokens: %w", model, err)
	}
	if record.output, err = optionalNonnegativeIntField(fields, "outputTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] outputTokens: %w", model, err)
	}
	if record.thinking, err = optionalNonnegativeIntField(fields, "thinkingTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] thinkingTokens: %w", model, err)
	}
	if record.total, err = optionalNonnegativeIntField(fields, "totalTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] totalTokens: %w", model, err)
	}
	if record.cacheRead, err = optionalNonnegativeIntField(fields, "cacheReadInputTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] cacheReadInputTokens: %w", model, err)
	}
	if record.cacheCreation, err = optionalNonnegativeIntField(fields, "cacheCreationInputTokens"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] cacheCreationInputTokens: %w", model, err)
	}
	if record.costUSD, err = optionalDecimalField(fields, "costUSD"); err != nil {
		return modelUsageRecord{}, fmt.Errorf("modelUsage[%q] costUSD: %w", model, err)
	}
	return record, nil
}

// validModelName matches the task usage contract while preserving the native
// spelling exactly. In particular, surrounding whitespace is not a harmless
// formatting difference: it would make the emitted UsageMetadata invalid.
func validModelName(value string) bool {
	return validText(value) && strings.TrimSpace(value) == value
}

func optionalNonnegativeIntField(fields map[string]json.RawMessage, name string) (*int64, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s is not an integer", name)
	}
	if value < 0 {
		return nil, fmt.Errorf("%s is negative", name)
	}
	return &value, nil
}

func optionalDecimalField(fields map[string]json.RawMessage, name string) (*string, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return nil, fmt.Errorf("%s is not a JSON number", name)
	}
	value := string(trimmed)
	if err := validateDecimal(value); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &value, nil
}

func validateDecimal(value string) error {
	var parsed float64
	if err := json.Unmarshal([]byte(value), &parsed); err != nil || parsed < 0 {
		return errors.New("must be a finite nonnegative number")
	}
	if err := task.ValidateUsageMetadata(&task.UsageMetadata{
		Scope: task.UsageScopeWholeTree, Source: task.UsageSourceProviderEnvelope,
		Reliability: task.UsageReliabilityReported, EstimatedCostUSD: &value,
	}); err != nil {
		return err
	}
	return nil
}

// usageFromResult preserves independent native accounting views. Aggregate
// total_cost_usd and per-model modelUsage records intentionally remain separate
// records; callers must not add overlapping whole-tree records together.
func usageFromResult(state eventState, reliability string) []task.UsageMetadata {
	if !state.resultSeen {
		return nil
	}
	result := state.result
	usage := make([]task.UsageMetadata, 0, 2+len(result.modelUsage))
	if result.usage != nil || result.numTurns != nil || result.durationMS != nil {
		main := task.UsageMetadata{
			Scope: task.UsageScopeMainAgent, Source: task.UsageSourceProviderEnvelope,
			Reliability: reliability, NumTurns: result.numTurns,
			DurationSeconds: durationSeconds(result.durationMS),
		}
		if result.usage != nil {
			main.InputTokens = result.usage.input
			main.OutputTokens = result.usage.output
			main.ThinkingTokens = result.usage.thinking
			main.TotalTokens = result.usage.total
			main.CacheReadTokens = result.usage.cacheRead
			main.CacheCreationTokens = result.usage.cacheCreation
		}
		if state.init.model != "" {
			model := state.init.model
			main.Model = &model
		}
		usage = append(usage, main)
	}
	if result.totalCostUSD != nil {
		cost := *result.totalCostUSD
		usage = append(usage, task.UsageMetadata{
			Scope: task.UsageScopeWholeTree, Source: task.UsageSourceProviderEnvelope,
			Reliability: reliability, EstimatedCostUSD: &cost,
		})
	}
	for _, modelRecord := range result.modelUsage {
		model := modelRecord.model
		usage = append(usage, task.UsageMetadata{
			Scope: task.UsageScopeWholeTree, Source: task.UsageSourceProviderEnvelope,
			Reliability: reliability, Model: &model,
			InputTokens: modelRecord.input, OutputTokens: modelRecord.output,
			ThinkingTokens: modelRecord.thinking, TotalTokens: modelRecord.total,
			CacheReadTokens: modelRecord.cacheRead, CacheCreationTokens: modelRecord.cacheCreation,
			EstimatedCostUSD: modelRecord.costUSD,
		})
	}
	return usage
}

func durationSeconds(milliseconds *int64) *string {
	if milliseconds == nil {
		return nil
	}
	seconds := strconv.FormatInt(*milliseconds/1000, 10)
	remainder := *milliseconds % 1000
	if remainder == 0 {
		return &seconds
	}
	value := fmt.Sprintf("%s.%03d", seconds, remainder)
	value = strings.TrimRight(value, "0")
	return &value
}
