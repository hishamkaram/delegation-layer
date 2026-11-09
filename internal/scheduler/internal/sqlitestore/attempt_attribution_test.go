package sqlitestore

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestAttemptAndProjectionArrivalOrderPreservesFailureAttribution(t *testing.T) {
	for _, verdict := range []string{"rejected", "quota_rejected", "not_created", "closed_without_outcome", "committed", "unknown"} {
		for _, attemptFirst := range []bool{false, true} {
			name := verdict + "/projection_first"
			if attemptFirst {
				name = verdict + "/attempt_first"
			}
			t.Run(name, func(t *testing.T) {
				assertAttemptProjectionOrder(t, verdict, attemptFirst)
			})
		}
	}
}

func assertAttemptProjectionOrder(t *testing.T, verdict string, attemptFirst bool) {
	t.Helper()
	s, root := initializedStore(t)
	createSchedule(t, s, testScheduleID)
	ctx := testContext(t)
	if _, err := s.ClaimAndArm(ctx, testClaim(testScheduleID, testTaskID, 1000, 1100)); err != nil {
		t.Fatal(err)
	}
	attempt := contract.AttemptFailure{Code: "supervisor_upgrade_required", Stage: "start-supervisor", NextAction: "run_supervisor_migrate", Message: "Supervisor migration is required."}
	fact := arrivalOrderFact(verdict)
	if attemptFirst {
		requireReliabilityWrite(t, s.RecordAttempt(ctx, testTaskID, attempt))
	}
	requireReliabilityWrite(t, s.RecordResult(ctx, fact))
	if !attemptFirst {
		requireReliabilityWrite(t, s.RecordAttempt(ctx, testTaskID, attempt))
	}
	// A repeated passive result may have captured its old classification before
	// the attempt was saved. Replaying that result must not undo attribution.
	requireReliabilityWrite(t, s.RecordResult(ctx, fact))
	assertPersistedAttribution(t, rawConn(t, DatabasePath(root)), fact, verdict)
}

func arrivalOrderFact(verdict string) contract.ResultFact {
	fact := contract.ResultFact{TaskID: testTaskID, ObservedAt: at(1200), Verdict: verdict}
	switch verdict {
	case "quota_rejected":
		fact.Verdict, fact.Code = "rejected", "provider_quota_exhausted"
		fact.QuotaNotBefore = at(1700)
		fact.OutcomeSHA256 = strings.Repeat("a", 64)
	case "rejected", "committed":
		fact.OutcomeSHA256 = strings.Repeat("a", 64)
		if verdict == "rejected" {
			fact.Code = "provider_output_rejected"
		}
	case "not_created", "closed_without_outcome":
		fact.Code = "admission_failed"
	case "unknown":
	}
	return fact
}

func assertPersistedAttribution(t *testing.T, conn *sql.Conn, want contract.ResultFact, scenario string) {
	t.Helper()
	var verdict string
	var code, reference sql.NullString
	var observed int64
	var quota sql.NullInt64
	err := conn.QueryRowContext(testContext(t), `SELECT verdict,code,outcome_sha256,observed_at,quota_until
 FROM occurrence_results WHERE task_id=?`, testTaskID).Scan(&verdict, &code, &reference, &observed, &quota)
	if err != nil {
		t.Fatal(err)
	}
	wantCode := "supervisor_upgrade_required"
	switch scenario {
	case "quota_rejected":
		wantCode = "provider_quota_exhausted"
	case "committed", "unknown":
		wantCode = ""
	}
	if code.String != wantCode || verdict != want.Verdict || reference.String != want.OutcomeSHA256 || observed != want.ObservedAt.UnixNano() {
		t.Fatalf("arrival changed authority or lost attribution: verdict=%s code=%s ref=%s observed=%d", verdict, code.String, reference.String, observed)
	}
	if quota.Valid != !want.QuotaNotBefore.IsZero() {
		t.Fatal("quota advice presence changed")
	}
	if quota.Valid && quota.Int64 != want.QuotaNotBefore.UnixNano() {
		t.Fatal("quota reset changed")
	}
}
