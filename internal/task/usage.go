package task

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	UsageScopeTask                   = "task"
	UsageScopeConversationCumulative = "conversation-cumulative"
	UsageScopeMainAgent              = "main-agent"
	UsageScopeWholeTree              = "whole-tree"
	UsageScopeUnknown                = "unknown"

	UsageSourceProviderEnvelope = "provider-envelope"
	UsageSourceProviderEvent    = "provider-event"

	UsageReliabilityReported   = "reported"
	UsageReliabilityUnreliable = "unreliable"
	UsageReliabilityUnknown    = "unknown"
)

var usageNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// UsageMetadata preserves one provider accounting view without deriving task
// deltas or summing counters that may overlap. Nil counters are unavailable,
// not zero. A record's scope identifies the population represented by every
// counter in that record.
type UsageMetadata struct {
	Scope               string  `json:"scope"`
	Source              string  `json:"source"`
	Reliability         string  `json:"reliability"`
	InputTokens         *int64  `json:"input_tokens,omitempty"`
	OutputTokens        *int64  `json:"output_tokens,omitempty"`
	ThinkingTokens      *int64  `json:"thinking_tokens,omitempty"`
	TotalTokens         *int64  `json:"total_tokens,omitempty"`
	CacheReadTokens     *int64  `json:"cache_read_tokens,omitempty"`
	NumTurns            *int64  `json:"num_turns,omitempty"`
	DurationSeconds     *string `json:"duration_seconds,omitempty"`
	Model               *string `json:"model,omitempty"`
	CacheCreationTokens *int64  `json:"cache_creation_tokens,omitempty"`
	EstimatedCostUSD    *string `json:"estimated_cost_usd,omitempty"`
}

// ValidateUsageMetadata accepts absent accounting and bounds every present
// field. It never treats provider accounting as a task budget or completion
// fact.
func ValidateUsageMetadata(v *UsageMetadata) error {
	if v == nil {
		return nil
	}
	if !slices.Contains([]string{
		UsageScopeTask,
		UsageScopeConversationCumulative,
		UsageScopeMainAgent,
		UsageScopeWholeTree,
		UsageScopeUnknown,
	}, v.Scope) {
		return errors.New("invalid usage scope")
	}
	if !slices.Contains([]string{UsageSourceProviderEnvelope, UsageSourceProviderEvent}, v.Source) {
		return errors.New("invalid usage source")
	}
	if !slices.Contains([]string{UsageReliabilityReported, UsageReliabilityUnreliable, UsageReliabilityUnknown}, v.Reliability) {
		return errors.New("invalid usage reliability")
	}
	if v.Model != nil && (strings.TrimSpace(*v.Model) == "" || strings.TrimSpace(*v.Model) != *v.Model || strings.ContainsRune(*v.Model, '\x00') || !utf8.ValidString(*v.Model)) {
		return errors.New("invalid usage model")
	}
	for _, field := range []struct {
		name  string
		value *int64
	}{
		{name: "input_tokens", value: v.InputTokens},
		{name: "output_tokens", value: v.OutputTokens},
		{name: "thinking_tokens", value: v.ThinkingTokens},
		{name: "total_tokens", value: v.TotalTokens},
		{name: "cache_read_tokens", value: v.CacheReadTokens},
		{name: "cache_creation_tokens", value: v.CacheCreationTokens},
		{name: "num_turns", value: v.NumTurns},
	} {
		if field.value != nil && *field.value < 0 {
			return fmt.Errorf("negative usage counter: %s", field.name)
		}
	}
	if err := validateUsageNumber("duration", v.DurationSeconds); err != nil {
		return err
	}
	return validateUsageNumber("estimated cost", v.EstimatedCostUSD)
}

// ValidateUsageMetadataList validates each independent accounting scope in a
// record. An absent or empty list is valid and is preserved as no accounting.
func ValidateUsageMetadataList(values []UsageMetadata) error {
	for i := range values {
		if err := ValidateUsageMetadata(&values[i]); err != nil {
			return fmt.Errorf("usage[%d]: %w", i, err)
		}
	}
	return nil
}

func validateUsageNumber(name string, value *string) error {
	if value == nil {
		return nil
	}
	text := *value
	if len(text) == 0 || len(text) > 128 || !usageNumber.MatchString(text) {
		return fmt.Errorf("invalid usage %s", name)
	}
	if strings.HasPrefix(text, "-") && usageNumberHasNonzeroMagnitude(text) {
		return fmt.Errorf("usage %s must be finite and nonnegative", name)
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || parsed < 0 || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return fmt.Errorf("usage %s must be finite and nonnegative", name)
	}
	return nil
}

func usageNumberHasNonzeroMagnitude(text string) bool {
	mantissa := strings.TrimPrefix(text, "-")
	if exponent := strings.IndexAny(mantissa, "eE"); exponent >= 0 {
		mantissa = mantissa[:exponent]
	}
	for _, digit := range mantissa {
		if digit != '0' && digit != '.' {
			return true
		}
	}
	return false
}

// CloneUsageMetadata removes aliases to an interpreter's returned counters
// and optional values before an outcome is persisted.
func CloneUsageMetadata(values []UsageMetadata) []UsageMetadata {
	if values == nil {
		return nil
	}
	copied := make([]UsageMetadata, len(values))
	for i := range values {
		copied[i] = values[i]
		copied[i].Model = copyUsageValue(values[i].Model)
		copied[i].InputTokens = copyUsageValue(values[i].InputTokens)
		copied[i].OutputTokens = copyUsageValue(values[i].OutputTokens)
		copied[i].ThinkingTokens = copyUsageValue(values[i].ThinkingTokens)
		copied[i].TotalTokens = copyUsageValue(values[i].TotalTokens)
		copied[i].CacheReadTokens = copyUsageValue(values[i].CacheReadTokens)
		copied[i].CacheCreationTokens = copyUsageValue(values[i].CacheCreationTokens)
		copied[i].NumTurns = copyUsageValue(values[i].NumTurns)
		copied[i].DurationSeconds = copyUsageValue(values[i].DurationSeconds)
		copied[i].EstimatedCostUSD = copyUsageValue(values[i].EstimatedCostUSD)
	}
	return copied
}

func copyUsageValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
