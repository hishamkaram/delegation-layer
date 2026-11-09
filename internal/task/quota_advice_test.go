package task

import "testing"

func TestCompareOutcomesIncludesQuotaAdvice(t *testing.T) {
	first := &OutcomeRecord{Quota: &QuotaAdvice{NotBefore: "2026-11-11T07:00:00Z"}}
	same := &OutcomeRecord{Quota: &QuotaAdvice{NotBefore: "2026-11-11T07:00:00Z"}}
	changed := &OutcomeRecord{Quota: &QuotaAdvice{NotBefore: "2026-11-10T02:00:00Z"}}
	absent := &OutcomeRecord{}
	if !CompareOutcomes(first, same) {
		t.Fatal("identical quota advice changed outcome equality")
	}
	if CompareOutcomes(first, changed) {
		t.Fatal("different quota advice accepted as the same immutable outcome")
	}
	if CompareOutcomes(first, absent) || CompareOutcomes(absent, first) {
		t.Fatal("missing quota advice accepted as equivalent")
	}
	if !CompareOutcomes(absent, &OutcomeRecord{}) {
		t.Fatal("legacy nil advice no longer equivalent")
	}
}
