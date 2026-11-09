package sqlitestore

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestQuotaHoldDefersForLaterClosedTurnThatCanStillPublish(t *testing.T) {
	for _, laterVerdict := range []string{"committed", "rejected"} {
		t.Run(laterVerdict, func(t *testing.T) { assertQuotaWaitsForLaterPublication(t, laterVerdict) })
	}
}

func assertQuotaWaitsForLaterPublication(t *testing.T, laterVerdict string) {
	t.Helper()
	s, _ := initializedStore(t)
	createSchedule(t, s, testScheduleID)
	ctx := testContext(t)
	policy := contract.DefaultQuotaPolicy()
	policy.Enabled = true
	requireReliabilityWrite(t, s.SetQuotaPolicy(ctx, testScheduleID, policy))
	recordReliabilityOccurrence(t, s, testTaskID, 1000, 1100, "unknown", "")
	recordReliabilityOccurrence(t, s, otherTaskID, 1100, 1200, "closed_without_outcome", "closed_without_outcome")
	requireReliabilityWrite(t, s.RecordResult(ctx, contract.ResultFact{TaskID: testTaskID, ObservedAt: at(1300), Verdict: "rejected", Code: "provider_quota_exhausted", OutcomeSHA256: strings.Repeat("a", 64), QuotaNotBefore: at(2000)}))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, at(1301)))
	hold, err := s.QuotaHold(ctx, testScheduleID)
	if err != nil || hold != nil {
		t.Fatalf("inspection readiness became publication coverage: %+v %v", hold, err)
	}
	requireReliabilityWrite(t, s.RecordResult(ctx, contract.ResultFact{TaskID: otherTaskID, ObservedAt: at(1302), Verdict: laterVerdict, OutcomeSHA256: strings.Repeat("b", 64)}))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, at(1303)))
	hold, err = s.QuotaHold(ctx, testScheduleID)
	if err != nil {
		t.Fatal(err)
	}
	if (hold == nil) != (laterVerdict == "committed") {
		t.Fatalf("later publication classification lost: verdict=%s hold=%+v", laterVerdict, hold)
	}
}
