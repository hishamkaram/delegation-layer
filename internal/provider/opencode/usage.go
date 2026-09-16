package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type usageTotals struct {
	present    bool
	input      *int64
	output     *int64
	reasoning  *int64
	total      *int64
	cacheRead  *int64
	cacheWrite *int64
	cost       *string
}

func parseUsage(part map[string]json.RawMessage) (*usageTotals, error) {
	usage := &usageTotals{}
	if err := parseTokenUsage(part, usage); err != nil {
		return nil, err
	}
	if err := parseCost(part, usage); err != nil {
		return nil, err
	}
	if !usage.present {
		return nil, nil
	}
	return usage, nil
}

func parseTokenUsage(part map[string]json.RawMessage, usage *usageTotals) error {
	rawTokens, present := part["tokens"]
	if !present || bytes.Equal(bytes.TrimSpace(rawTokens), []byte("null")) {
		return nil
	}
	fields, err := decodeObject(rawTokens, "step_finish tokens")
	if err != nil {
		return err
	}
	usage.present = true
	for _, counter := range []struct {
		name   string
		target **int64
	}{
		{name: "input", target: &usage.input},
		{name: "output", target: &usage.output},
		{name: "reasoning", target: &usage.reasoning},
		{name: "total", target: &usage.total},
	} {
		value, counterErr := optionalNonnegativeInt(fields, counter.name)
		if counterErr != nil {
			return counterErr
		}
		*counter.target = value
	}
	return parseCacheUsage(fields, usage)
}

func parseCacheUsage(fields map[string]json.RawMessage, usage *usageTotals) error {
	rawCache, present := fields["cache"]
	if !present || bytes.Equal(bytes.TrimSpace(rawCache), []byte("null")) {
		return nil
	}
	cache, err := decodeObject(rawCache, "step_finish tokens.cache")
	if err != nil {
		return err
	}
	for _, counter := range []struct {
		name   string
		target **int64
	}{
		{name: "read", target: &usage.cacheRead},
		{name: "write", target: &usage.cacheWrite},
	} {
		value, counterErr := optionalNonnegativeInt(cache, counter.name)
		if counterErr != nil {
			return counterErr
		}
		*counter.target = value
	}
	return nil
}

func parseCost(part map[string]json.RawMessage, usage *usageTotals) error {
	rawCost, present := part["cost"]
	if !present || bytes.Equal(bytes.TrimSpace(rawCost), []byte("null")) {
		return nil
	}
	cost, err := decimalNumber(rawCost)
	if err != nil {
		return err
	}
	usage.cost = &cost
	usage.present = true
	return nil
}

func decodeObject(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New(name + " is not an object")
	}
	return fields, nil
}

func optionalNonnegativeInt(fields map[string]json.RawMessage, name string) (*int64, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return nil, fmt.Errorf("usage %s is not a nonnegative integer", name)
	}
	return &value, nil
}

func decimalNumber(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return "", errors.New("step_finish cost is not a JSON number")
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil || number.String() == "" {
		return "", errors.New("step_finish cost is not a number")
	}
	value := number.String()
	if err := task.ValidateUsageMetadata(&task.UsageMetadata{
		Scope: task.UsageScopeConversationCumulative, Source: task.UsageSourceProviderEvent,
		Reliability: task.UsageReliabilityReported, EstimatedCostUSD: &value,
	}); err != nil {
		return "", fmt.Errorf("step_finish cost: %w", err)
	}
	return value, nil
}

func usageFromTotals(usages []*usageTotals, reliability string) []task.UsageMetadata {
	if len(usages) == 0 {
		return nil
	}
	result := make([]task.UsageMetadata, 0, len(usages))
	for _, usage := range usages {
		if usage == nil || !usage.present {
			continue
		}
		result = append(result, task.UsageMetadata{
			// OpenCode reports one step's accounting here. It is not a
			// conversation total, and the adapter intentionally does not add
			// overlapping values across steps or resumed invocations.
			Scope:               task.UsageScopeUnknown,
			Source:              task.UsageSourceProviderEvent,
			Reliability:         reliability,
			InputTokens:         usage.input,
			OutputTokens:        usage.output,
			ThinkingTokens:      usage.reasoning,
			TotalTokens:         usage.total,
			CacheReadTokens:     usage.cacheRead,
			CacheCreationTokens: usage.cacheWrite,
			EstimatedCostUSD:    usage.cost,
		})
	}
	return result
}
