package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"modernc.org/sqlite"
)

func recordReliabilityOccurrence(t *testing.T, s *Store, id string, cursor, due int64, verdict, code string) {
	t.Helper()
	ctx := testContext(t)
	if result, err := s.ClaimAndArm(ctx, testClaim(testScheduleID, id, cursor, due)); err != nil || !result.AuthorizesInvocation() {
		t.Fatalf("claim: %+v %v", result, err)
	}
	f := contract.ResultFact{TaskID: id, ObservedAt: at(due + 1), Verdict: verdict, Code: code}
	if verdict == "committed" || verdict == "rejected" {
		f.OutcomeSHA256 = strings.Repeat("a", 64)
	}
	if err := s.RecordResult(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, contract.OccurrenceRelease{TaskID: id, Release: contract.Release{ReleasedAt: at(due + 1), Kind: contract.ReleaseNotCreated, Ref: "test_absent"}}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionAttemptSurvivesRestartAndCannotBeOverwritten(t *testing.T) {
	s, root := initializedStore(t)
	createSchedule(t, s, testScheduleID)
	if _, err := s.ClaimAndArm(testContext(t), testClaim(testScheduleID, testTaskID, 1000, 1100)); err != nil {
		t.Fatal(err)
	}
	f := contract.AttemptFailure{Code: "supervisor_upgrade_required", Stage: "start-supervisor", NextAction: "run_supervisor_migrate", Message: "Supervisor migration is required."}
	if err := s.RecordAttempt(testContext(t), testTaskID, f); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(testContext(t), testObservation(testTaskID, 1200)); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, root)
	got, err := reopened.GetOccurrence(testContext(t), testTaskID)
	if err != nil || got.Attempt == nil || *got.Attempt != f {
		t.Fatalf("lost concrete diagnostic: %+v %v", got, err)
	}
	f.Code = "dispatch_failed"
	if err := s.RecordAttempt(testContext(t), testTaskID, f); err == nil {
		t.Fatal("overwrote immutable diagnostic")
	}
}

func TestHealthTracksDueOrderUnknownGapsAndHookEpisode(t *testing.T) {
	s, _ := initializedStore(t)
	d := testDefinition(testScheduleID)
	d.Hook = &contract.FailureHook{Executable: "/installed/hook", Threshold: 2, Timeout: time.Second}
	if err := s.CreateSchedule(testContext(t), d); err != nil {
		t.Fatal(err)
	}
	requireStoredHook(t, s, d.Hook)

	recordReliabilityOccurrence(t, s, testTaskID, 1000, 1100, "unknown", "")
	recordReliabilityOccurrence(t, s, otherTaskID, 1100, 1200, "not_created", "admission_failed")
	h, err := s.Health(testContext(t), testScheduleID, at(2000))
	if err != nil || h.Complete || h.ConsecutiveNonCommitted != 1 {
		t.Fatalf("unknown gap hidden: %+v %v", h, err)
	}
	requireNoNotice(t, s, "c", 1300)
	requireReliabilityWrite(t, s.RecordResult(testContext(t), contract.ResultFact{TaskID: testTaskID, ObservedAt: at(1400), Verdict: "not_created", Code: "admission_failed"}))
	n, err := s.ClaimNotification(testContext(t), testScheduleID, strings.Repeat("c", 32), at(1401))
	if err != nil || n == nil || n.StreakCount != 2 {
		t.Fatalf("missing threshold notice %+v %v", n, err)
	}
	requireReliabilityWrite(t, s.FinishNotification(testContext(t), n.EventID, "failed"))
	requireNoNotice(t, s, "d", 1402)
	committed := strings.Repeat("e", 32)
	recordReliabilityOccurrence(t, s, committed, 1200, 1500, "committed", "")
	n, err = s.ClaimNotification(testContext(t), testScheduleID, strings.Repeat("f", 32), at(1502))
	if err != nil || n == nil || n.Kind != "recovery" {
		t.Fatalf("missing recovery %+v %v", n, err)
	}
	requireNoNotice(t, s, "1", 1503)
	// Older committed evidence never replaces the newest due committed occurrence.
	requireReliabilityWrite(t, s.RecordResult(testContext(t), contract.ResultFact{TaskID: testTaskID, ObservedAt: at(1600), Verdict: "committed", OutcomeSHA256: strings.Repeat("b", 64)}))
	h, err = s.Health(testContext(t), testScheduleID, at(2000))
	if err != nil || !h.Complete || h.LastCommitted.TaskID != committed || h.ConsecutiveNonCommitted != 0 {
		t.Fatalf("due order drift %+v %v", h, err)
	}
}

func TestQuotaHoldRequiresCompleteLaterCoverageAndResumeConsumesSource(t *testing.T) {
	s, _ := initializedStore(t)
	createSchedule(t, s, testScheduleID)
	ctx := testContext(t)
	p := contract.DefaultQuotaPolicy()
	p.Enabled = true
	p.MaxHold = 2 * time.Minute
	requireReliabilityWrite(t, s.SetQuotaPolicy(ctx, testScheduleID, p))
	recordReliabilityOccurrence(t, s, testTaskID, 1000, 1100, "unknown", "")
	recordReliabilityOccurrence(t, s, otherTaskID, 1100, 1200, "unknown", "")
	f := contract.ResultFact{TaskID: testTaskID, ObservedAt: at(1300), Verdict: "rejected", Code: "provider_quota_exhausted", OutcomeSHA256: strings.Repeat("a", 64), QuotaNotBefore: at(2000)}
	// Project quota advice together with the first validated terminal evidence.
	requireReliabilityWrite(t, s.RecordResult(ctx, f))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, at(1301)))
	if h, err := s.QuotaHold(ctx, testScheduleID); err != nil || h != nil {
		t.Fatalf("unknown later gap allowed obsolete hold %+v %v", h, err)
	}
	requireReliabilityWrite(t, s.RecordResult(ctx, contract.ResultFact{TaskID: otherTaskID, ObservedAt: at(1302), Verdict: "not_created", Code: "admission_failed"}))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, at(1303)))
	h, err := s.QuotaHold(ctx, testScheduleID)
	if err != nil || h == nil || h.Until != at(1423) || !h.Capped {
		t.Fatalf("hold not bounded %+v %v", h, err)
	}
	if _, err := s.ClaimAndArm(ctx, testClaim(testScheduleID, strings.Repeat("c", 32), 1200, 1400)); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("claim bypassed hold: %v", err)
	}
	requireReliabilityWrite(t, s.ResumeWithHold(ctx, testScheduleID, at(1000)))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, at(1304)))
	if h, err := s.QuotaHold(ctx, testScheduleID); err != nil || h != nil {
		t.Fatalf("late source recreated cleared hold %+v %v", h, err)
	}
}

// legacyDatabase exercises the real migration from the exact released DDL.
func legacyDatabase(t *testing.T) string {
	t.Helper()
	root := newRoot(t)
	path := DatabasePath(root)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	conn := rawConn(t, path)
	for _, object := range legacySchemaObjects() {
		rawExec(t, conn, object.sql)
	}
	if _, err := conn.ExecContext(testContext(t), "INSERT INTO scheduler_metadata VALUES (1,?)", testRootID); err != nil {
		t.Fatal(err)
	}
	d := testDefinition(testScheduleID)
	request, err := contract.MarshalRequest(d.Request)
	if err != nil {
		t.Fatal(err)
	}
	digests, err := contract.DefinitionDigests(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(testContext(t), `INSERT INTO schedules VALUES (?,?,?,?,?,?,?,?,?,?)`, d.ScheduleID, d.CreatedAt.UnixNano(), d.Cron, d.Timezone, request, d.Brief, digests.BriefSHA256, digests.DefinitionSHA256, "enabled", at(1200).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(testContext(t), `INSERT INTO occurrences(task_id,schedule_id,due_unix_nanos,armed_at_unix_nanos) VALUES (?,?,?,?)`, testTaskID, testScheduleID, at(1100).UnixNano(), at(1100).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(testContext(t), `INSERT INTO skip_spans VALUES (?,?,?,'missed_unavailable')`, testScheduleID, at(1100).UnixNano(), at(1200).UnixNano()); err != nil {
		t.Fatal(err)
	}
	rawExec(t, conn, "PRAGMA user_version = 1")
	return root
}

func TestV1MigrationPreservesChildrenAndMatchesFreshSchema(t *testing.T) {
	root := legacyDatabase(t)
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, contract.ErrMigrationRequired) {
		t.Fatalf("legacy open repaired or misclassified: %v", err)
	}
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, root)
	if _, err := s.GetSchedule(testContext(t), testScheduleID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOccurrence(testContext(t), testTaskID); err != nil {
		t.Fatal(err)
	}
	conn := rawConn(t, DatabasePath(root))
	if n := rawInt(t, conn, "SELECT count(*) FROM skip_spans"); n != 1 {
		t.Fatalf("lost skip %d", n)
	}
	if err := s.transact(testContext(t), s.currentGuard, func(ctx context.Context, tx *sql.Tx) error { return verifyObjectSet(ctx, tx, schemaObjects()) }); err != nil {
		t.Fatal(err)
	}
}

func TestV1MigrationRollbackPreservesLegacyDatabase(t *testing.T) {
	root := legacyDatabase(t)
	path := DatabasePath(root)
	connector, err := sqlite.NewConnector(databaseURI(path, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	fault := &faultConnector{base: connector}
	fault.inject(faultCommitBeforeReal)
	if err := initializeWithConnector(testContext(t), fault, path, testRootID, runtime.GOOS, schemaObjects()); !errors.Is(err, errInjected) {
		t.Fatalf("missing injected failure %v", err)
	}
	conn := rawConn(t, path)
	if v := rawInt(t, conn, "PRAGMA user_version"); v != 1 {
		t.Fatalf("partial migration version %d", v)
	}
	if n := rawInt(t, conn, "SELECT count(*) FROM skip_spans"); n != 1 {
		t.Fatalf("rollback lost children %d", n)
	}
}

func requireReliabilityWrite(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireNoNotice(t *testing.T, s *Store, event string, now int64) {
	t.Helper()
	n, err := s.ClaimNotification(testContext(t), testScheduleID, strings.Repeat(event, 32), at(now))
	if err != nil || n != nil {
		t.Fatalf("unexpected notice %+v %v", n, err)
	}
}

func requireStoredHook(t *testing.T, s *Store, want *contract.FailureHook) {
	t.Helper()
	got, err := s.GetSchedule(testContext(t), testScheduleID)
	if err != nil || got.Definition.Hook == nil || *got.Definition.Hook != *want {
		t.Fatalf("full hook not hydrated %+v %v", got, err)
	}
	page, err := s.ListSchedules(testContext(t), contract.SchedulePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Schedules[0].Header.Hook == nil || *page.Schedules[0].Header.Hook != *want {
		t.Fatal("summary hook not hydrated")
	}
}

func TestQuotaHoldCapDoesNotOverflowSupportedClock(t *testing.T) {
	s, _ := initializedStore(t)
	createSchedule(t, s, testScheduleID)
	ctx := testContext(t)
	p := contract.DefaultQuotaPolicy()
	p.Enabled = true
	requireReliabilityWrite(t, s.SetQuotaPolicy(ctx, testScheduleID, p))
	recordReliabilityOccurrence(t, s, testTaskID, 1000, 1100, "unknown", "")
	latest := time.Unix(0, 9223372036854775807).UTC()
	now := latest.Add(-time.Second)
	requireReliabilityWrite(t, s.RecordResult(ctx, contract.ResultFact{TaskID: testTaskID, ObservedAt: now, Verdict: "rejected", Code: "provider_quota_exhausted", OutcomeSHA256: strings.Repeat("a", 64), QuotaNotBefore: latest}))
	requireReliabilityWrite(t, s.ConsiderQuotaHold(ctx, testTaskID, now))
	h, err := s.QuotaHold(ctx, testScheduleID)
	if err != nil || h == nil || !h.Until.Equal(latest) || !h.Until.After(now) {
		t.Fatalf("cap overflow changed hold %+v %v", h, err)
	}
}
