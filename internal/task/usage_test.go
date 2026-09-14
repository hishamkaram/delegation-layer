package task

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func usageOutcome() *OutcomeRecord {
	return &OutcomeRecord{
		SchemaVersion:  1,
		RootID:         "0123456789abcdef0123456789abcdef",
		TaskID:         "abcdef0123456789abcdef0123456789",
		SpecSHA256:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		MetaSHA256:     "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Verdict:        VerdictCommitted,
		EvidenceSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Predicate:      FixturePredicateRef(),
		Payload: PayloadDescriptor{
			Basename: "result.txt",
			Length:   2,
			SHA256:   ComputeSHA256([]byte("ok")),
		},
	}
}

func TestUsageMetadataPreservesMissingZeroAndExactNumbers(t *testing.T) {
	zero, output, thinking, total, cacheRead, cacheCreate, turns := int64(0), int64(25), int64(7), int64(32), int64(0), int64(3), int64(2)
	duration, cost, model := "1.250e+0", "0.000100", "provider-model"
	values := []UsageMetadata{{
		Scope:               UsageScopeConversationCumulative,
		Source:              UsageSourceProviderEnvelope,
		Reliability:         UsageReliabilityReported,
		Model:               &model,
		InputTokens:         &zero,
		OutputTokens:        &output,
		ThinkingTokens:      &thinking,
		TotalTokens:         &total,
		CacheReadTokens:     &cacheRead,
		CacheCreationTokens: &cacheCreate,
		NumTurns:            &turns,
		DurationSeconds:     &duration,
		EstimatedCostUSD:    &cost,
	}}
	if err := ValidateUsageMetadataList(values); err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalCanonical(values)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"input_tokens":0`)) || !bytes.Contains(encoded, []byte(`"duration_seconds":"1.250e+0"`)) || !bytes.Contains(encoded, []byte(`"estimated_cost_usd":"0.000100"`)) {
		t.Fatalf("usage lost explicit zero or exact number spelling: %s", encoded)
	}
	if bytes.Contains(encoded, []byte(`"unreported_counter"`)) {
		t.Fatalf("unexpected usage field: %s", encoded)
	}
	var decoded []UsageMetadata
	if err = DecodeStrict(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, decoded) {
		t.Fatalf("usage changed in strict round trip: %s", encoded)
	}
}

func TestUsageMetadataAcceptsExplicitNulls(t *testing.T) {
	data := []byte(`{"scope":"task","source":"provider-event","reliability":"reported","model":null,"input_tokens":null,"output_tokens":null,"thinking_tokens":null,"total_tokens":null,"cache_read_tokens":null,"cache_creation_tokens":null,"num_turns":null,"duration_seconds":null,"estimated_cost_usd":null}`)
	var value UsageMetadata
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUsageMetadata(&value); err != nil {
		t.Fatal(err)
	}
	if value.Model != nil || value.InputTokens != nil || value.DurationSeconds != nil {
		t.Fatal("explicit nulls were converted to available values")
	}
}

func TestUsageMetadataRejectsInvalidValues(t *testing.T) {
	negative := int64(-1)
	invalidModel := " model "
	invalidUTF8Model := string([]byte{'m', 'o', 0xff, 'd', 'e', 'l'})
	cases := []struct {
		name   string
		change func(*UsageMetadata)
	}{
		{"scope", func(v *UsageMetadata) { v.Scope = "assumed-task" }},
		{"source", func(v *UsageMetadata) { v.Source = "estimated" }},
		{"reliability", func(v *UsageMetadata) { v.Reliability = "certain" }},
		{"counter", func(v *UsageMetadata) { v.InputTokens = &negative }},
		{"cache creation", func(v *UsageMetadata) { v.CacheCreationTokens = &negative }},
		{"model", func(v *UsageMetadata) { v.Model = &invalidModel }},
		{"invalid utf8 model", func(v *UsageMetadata) { v.Model = &invalidUTF8Model }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := UsageMetadata{Scope: UsageScopeUnknown, Source: UsageSourceProviderEnvelope, Reliability: UsageReliabilityUnknown}
			tc.change(&value)
			if err := ValidateUsageMetadata(&value); err == nil {
				t.Fatal("invalid usage metadata was accepted")
			}
			if err := ValidateInterpretation(Interpretation{Verdict: VerdictCommitted, Usage: []UsageMetadata{value}}); err == nil {
				t.Fatal("invalid interpretation accounting was accepted")
			}
		})
	}
	for _, text := range []string{"", " 1", "01", "NaN", "Infinity", "-1", "-1e-999", "1e999", "1,2", "0x1", "1."} {
		t.Run("duration="+text, func(t *testing.T) {
			value := UsageMetadata{Scope: UsageScopeTask, Source: UsageSourceProviderEvent, Reliability: UsageReliabilityReported, DurationSeconds: &text}
			if err := ValidateUsageMetadata(&value); err == nil {
				t.Fatal("invalid duration was accepted")
			}
		})
	}
	for _, text := range []string{"-0", "-0.0", "-0e999"} {
		t.Run("negative-zero="+text, func(t *testing.T) {
			value := UsageMetadata{Scope: UsageScopeTask, Source: UsageSourceProviderEvent, Reliability: UsageReliabilityReported, DurationSeconds: &text}
			if err := ValidateUsageMetadata(&value); err != nil {
				t.Fatalf("negative zero should remain an accepted exact zero: %v", err)
			}
		})
	}
	if ValidateUsageMetadata(nil) != nil {
		t.Fatal("absent usage was rejected")
	}
}

func TestUsageScopesRemainSeparateAcrossCumulativeReset(t *testing.T) {
	first, reset, mainAgent, wholeTree := int64(120), int64(3), int64(9), int64(15)
	values := []UsageMetadata{
		{Scope: UsageScopeConversationCumulative, Source: UsageSourceProviderEnvelope, Reliability: UsageReliabilityReported, TotalTokens: &first},
		{Scope: UsageScopeConversationCumulative, Source: UsageSourceProviderEnvelope, Reliability: UsageReliabilityReported, TotalTokens: &reset},
		{Scope: UsageScopeMainAgent, Source: UsageSourceProviderEvent, Reliability: UsageReliabilityReported, TotalTokens: &mainAgent},
		{Scope: UsageScopeWholeTree, Source: UsageSourceProviderEvent, Reliability: UsageReliabilityReported, TotalTokens: &wholeTree},
	}
	if err := ValidateUsageMetadataList(values); err != nil {
		t.Fatal(err)
	}
	if *values[0].TotalTokens != 120 || *values[1].TotalTokens != 3 || len(values) != 4 {
		t.Fatal("usage records were treated as a delta or merged total")
	}
	other := usageOutcome()
	other.Usage = CloneUsageMetadata(values)
	if !CompareOutcomes(&OutcomeRecord{Usage: nil}, &OutcomeRecord{Usage: []UsageMetadata{}}) {
		t.Fatal("nil and empty usage did not compare as equivalent")
	}
	if !CompareOutcomes(&OutcomeRecord{Usage: values}, &OutcomeRecord{Usage: other.Usage}) {
		t.Fatal("equal independent usage records compared unequal")
	}
	*other.Usage[1].TotalTokens = 4
	if CompareOutcomes(&OutcomeRecord{Usage: values}, &OutcomeRecord{Usage: other.Usage}) {
		t.Fatal("usage value change was ignored by semantic equality")
	}
}

func TestOldOutcomesOmitUsageAndRemainStrictlyReadable(t *testing.T) {
	outcome := usageOutcome()
	if err := ValidateOutcomeRecord(outcome); err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalCanonical(outcome)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"usage"`)) {
		t.Fatalf("old outcome acquired an empty usage field: %s", encoded)
	}
	var decoded OutcomeRecord
	if err = DecodeStrict(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage != nil || !CompareOutcomes(outcome, &decoded) {
		t.Fatal("old outcome did not retain nil usage semantics")
	}
	empty := *outcome
	empty.Usage = []UsageMetadata{}
	if !CompareOutcomes(outcome, &empty) {
		t.Fatal("empty usage was not equivalent to omitted usage")
	}
}

func TestCloneUsageMetadataDeepCopiesEveryPointer(t *testing.T) {
	model, input, output, thinking, total, read, creation, turns := "model", int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7)
	duration, cost := "2.5", "0.5"
	original := []UsageMetadata{{Scope: UsageScopeTask, Source: UsageSourceProviderEnvelope, Reliability: UsageReliabilityReported, Model: &model, InputTokens: &input, OutputTokens: &output, ThinkingTokens: &thinking, TotalTokens: &total, CacheReadTokens: &read, CacheCreationTokens: &creation, NumTurns: &turns, DurationSeconds: &duration, EstimatedCostUSD: &cost}}
	copy := CloneUsageMetadata(original)
	if !reflect.DeepEqual(original, copy) {
		t.Fatal("cloned usage changed values")
	}
	model = "changed"
	input, output, thinking, total, read, creation, turns = 11, 12, 13, 14, 15, 16, 17
	duration, cost = "3.5", "1.5"
	if *copy[0].Model != "model" || *copy[0].InputTokens != 1 || *copy[0].OutputTokens != 2 || *copy[0].ThinkingTokens != 3 || *copy[0].TotalTokens != 4 || *copy[0].CacheReadTokens != 5 || *copy[0].CacheCreationTokens != 6 || *copy[0].NumTurns != 7 || *copy[0].DurationSeconds != "2.5" || *copy[0].EstimatedCostUSD != "0.5" {
		t.Fatal("cloned usage retained pointer aliases")
	}
}
