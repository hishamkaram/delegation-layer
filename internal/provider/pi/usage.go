package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// usageTotals mirrors one completed assistant message's provider accounting
// that Pi exposes on JSON events. Missing counters remain nil; no total is
// synthesized. A run can contain several assistant messages when tools are
// used, so the parser retains one row per completed message.
type usageTotals struct {
	present       bool
	input         *int64
	output        *int64
	thinking      *int64
	total         *int64
	cacheRead     *int64
	cacheCreation *int64
}

func parseUsage(fields map[string]json.RawMessage) (*usageTotals, error) {
	raw, ok := fields["usage"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var usageFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &usageFields); err != nil || usageFields == nil {
		return nil, errors.New("pi usage is not an object")
	}
	usage := &usageTotals{present: true}
	var err error
	if usage.input, err = optionalCounter(usageFields, "input_tokens", "inputTokens", "input"); err != nil {
		return nil, fmt.Errorf("pi usage input: %w", err)
	}
	if usage.output, err = optionalCounter(usageFields, "output_tokens", "outputTokens", "output"); err != nil {
		return nil, fmt.Errorf("pi usage output: %w", err)
	}
	if usage.thinking, err = optionalCounter(usageFields, "thinking_tokens", "thinkingTokens", "thinking"); err != nil {
		return nil, fmt.Errorf("pi usage thinking: %w", err)
	}
	if usage.total, err = optionalCounter(usageFields, "total_tokens", "totalTokens", "total"); err != nil {
		return nil, fmt.Errorf("pi usage total: %w", err)
	}
	if usage.cacheRead, err = optionalCounter(usageFields, "cache_read_input_tokens", "cacheReadInputTokens", "cacheRead"); err != nil {
		return nil, fmt.Errorf("pi usage cache read: %w", err)
	}
	if usage.cacheCreation, err = optionalCounter(usageFields, "cache_creation_input_tokens", "cacheCreationInputTokens", "cacheWrite"); err != nil {
		return nil, fmt.Errorf("pi usage cache creation: %w", err)
	}
	return usage, nil
}

func optionalCounter(fields map[string]json.RawMessage, names ...string) (*int64, error) {
	var raw json.RawMessage
	for _, name := range names {
		if candidate, ok := fields[name]; ok {
			raw = candidate
			break
		}
	}
	if raw == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return nil, errors.New("counter is not a nonnegative integer")
	}
	return &value, nil
}

func usageFromState(state eventState, reliability string) []task.UsageMetadata {
	if len(state.usage) == 0 {
		return nil
	}
	result := make([]task.UsageMetadata, 0, len(state.usage))
	for _, usage := range state.usage {
		if usage == nil || !usage.present {
			continue
		}
		result = append(result, task.UsageMetadata{
			// Pi reports usage per assistant message, not a conversation
			// total. Keep that distinction explicit rather than synthesizing
			// a sum across tool turns or resumed sessions.
			Scope:               task.UsageScopeUnknown,
			Source:              task.UsageSourceProviderEvent,
			Reliability:         reliability,
			InputTokens:         usage.input,
			OutputTokens:        usage.output,
			ThinkingTokens:      usage.thinking,
			TotalTokens:         usage.total,
			CacheReadTokens:     usage.cacheRead,
			CacheCreationTokens: usage.cacheCreation,
		})
	}
	return result
}
