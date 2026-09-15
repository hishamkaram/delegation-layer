package codex

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// usageTotals mirrors the five counters copied by the observed Codex event
// processor from ThreadTokenUsage.total. Pointers preserve a missing counter;
// no task delta or synthetic total is calculated.
type usageTotals struct {
	present     bool
	input       *int64
	cachedInput *int64
	cacheWrite  *int64
	output      *int64
	reasoning   *int64
}

func parseUsage(fields map[string]json.RawMessage) (*usageTotals, error) {
	rawUsage, ok := fields["usage"]
	if !ok {
		return nil, nil
	}
	var usageFields map[string]json.RawMessage
	if err := json.Unmarshal(rawUsage, &usageFields); err != nil || usageFields == nil {
		return nil, errors.New("turn.completed usage is not an object")
	}
	usage := &usageTotals{present: true}
	var err error
	if usage.input, err = optionalNonnegativeInt(usageFields, "input_tokens"); err != nil {
		return nil, err
	}
	if usage.cachedInput, err = optionalNonnegativeInt(usageFields, "cached_input_tokens"); err != nil {
		return nil, err
	}
	if usage.cacheWrite, err = optionalNonnegativeInt(usageFields, "cache_write_input_tokens"); err != nil {
		return nil, err
	}
	if usage.output, err = optionalNonnegativeInt(usageFields, "output_tokens"); err != nil {
		return nil, err
	}
	if usage.reasoning, err = optionalNonnegativeInt(usageFields, "reasoning_output_tokens"); err != nil {
		return nil, err
	}
	return usage, nil
}

func optionalNonnegativeInt(fields map[string]json.RawMessage, name string) (*int64, error) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("usage %s is not an integer", name)
	}
	if value < 0 {
		return nil, fmt.Errorf("usage %s is negative", name)
	}
	return &value, nil
}

// usageFromLastTotal converts the provider's cumulative thread totals into
// the shared nullable usage record. Reliability is selected by the predicate
// after the event stream has been classified.
func usageFromLastTotal(usage *usageTotals, reliability string) []task.UsageMetadata {
	if usage == nil || !usage.present {
		return nil
	}
	return []task.UsageMetadata{{
		Scope:               task.UsageScopeConversationCumulative,
		Source:              task.UsageSourceProviderEvent,
		Reliability:         reliability,
		InputTokens:         usage.input,
		OutputTokens:        usage.output,
		ThinkingTokens:      usage.reasoning,
		CacheReadTokens:     usage.cachedInput,
		CacheCreationTokens: usage.cacheWrite,
	}}
}
