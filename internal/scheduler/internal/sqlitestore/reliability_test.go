package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
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
	for _, fixture := range []struct {
		definition contract.Definition
		state      string
		cursor     int64
	}{
		{testDefinition(testScheduleID), "enabled", 1200},
		{pausedLegacyDefinition(), "paused", 1400},
	} {
		d := fixture.definition
		request, err := contract.MarshalRequest(d.Request)
		if err != nil {
			t.Fatal(err)
		}
		digests, err := contract.DefinitionDigests(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(testContext(t), `INSERT INTO schedules VALUES (?,?,?,?,?,?,?,?,?,?)`, d.ScheduleID, d.CreatedAt.UnixNano(), d.Cron, d.Timezone, request, d.Brief, digests.BriefSHA256, digests.DefinitionSHA256, fixture.state, at(fixture.cursor).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(testContext(t), `INSERT INTO occurrences VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, testTaskID, testScheduleID, at(1100).UnixNano(), at(1101).UnixNano(), at(1102).UnixNano(), 2, 2, 2, strings.Repeat("a", 64), nil, at(1103).UnixNano(), "closed", strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(testContext(t), `INSERT INTO occurrences(task_id,schedule_id,due_unix_nanos,armed_at_unix_nanos,observed_at_unix_nanos,admission,liveness,publication,fault_code) VALUES (?,?,?,?,?,?,?,?,?)`, otherTaskID, testScheduleID, at(1200).UnixNano(), at(1201).UnixNano(), at(1202).UnixNano(), 2, 1, 0, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	for _, span := range []struct {
		schedule string
		from, to int64
		reason   string
	}{
		{testScheduleID, 1100, 1200, "missed_unavailable"},
		{otherSchedule, 1200, 1400, "paused"},
	} {
		if _, err := conn.ExecContext(testContext(t), `INSERT INTO skip_spans VALUES (?,?,?,?)`, span.schedule, at(span.from).UnixNano(), at(span.to).UnixNano(), span.reason); err != nil {
			t.Fatal(err)
		}
	}
	rawExec(t, conn, "PRAGMA user_version = 1")
	return root
}

func pausedLegacyDefinition() contract.Definition {
	d := testDefinition(otherSchedule)
	d.Cron = "*/15 * * * *"
	d.Timezone = "UTC"
	d.Brief = []byte("preserve paused explicit provider request")
	d.Request.Auto = false
	d.Request.Provider = "codex:review"
	d.Request.Config.Model = "selected-model"
	d.Request.Config.Effort = "high"
	d.Request.Config.Budget = "2m"
	return d
}

// Snapshot every v1 column, including canonical request/blob bytes and nullable
// observation/release tuples. Schema is recorded independently of v2 objects.
type legacySnapshot struct {
	rows    map[string][]string
	schema  []string
	version int64
}

func snapshotLegacy(t *testing.T, conn *sql.Conn) legacySnapshot {
	t.Helper()
	snapshot := legacySnapshot{rows: make(map[string][]string), version: rawInt(t, conn, "PRAGMA user_version")}
	for _, table := range []struct{ name, columns, order string }{
		{"scheduler_metadata", "singleton,root_id", "singleton"},
		{"schedules", "schedule_id,created_at_unix_nanos,cron,timezone,request_json,brief,brief_sha256,definition_sha256,state,cursor_unix_nanos", "schedule_id"},
		{"occurrences", "task_id,schedule_id,due_unix_nanos,armed_at_unix_nanos,observed_at_unix_nanos,admission,liveness,publication,outcome_sha256,fault_code,released_at_unix_nanos,release_kind,release_ref", "task_id"},
		{"skip_spans", "schedule_id,from_unix_nanos,to_unix_nanos,reason", "schedule_id,to_unix_nanos"},
	} {
		columns := strings.Split(table.columns, ",")
		for i, column := range columns {
			columns[i] = "quote(" + column + ")"
		}
		snapshot.rows[table.name] = legacyQuery(t, conn, "SELECT "+strings.Join(columns, " || '|' || ")+" FROM "+table.name+" ORDER BY "+table.order)
	}
	snapshot.schema = legacyQuery(t, conn, "SELECT type || '|' || name || '|' || coalesce(sql,'') FROM sqlite_schema ORDER BY type,name")
	return snapshot
}

func legacyQuery(t *testing.T, conn *sql.Conn, query string) []string {
	t.Helper()
	rows, err := conn.QueryContext(testContext(t), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestV1MigrationPreservesChildrenAndMatchesFreshSchema(t *testing.T) {
	root := legacyDatabase(t)
	conn := rawConn(t, DatabasePath(root))
	before := snapshotLegacy(t, conn)
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, contract.ErrMigrationRequired) {
		t.Fatalf("legacy open repaired or misclassified: %v", err)
	}
	requireV1RootMismatchPreservesDatabase(t, root, conn)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatal(err)
	}
	after := snapshotLegacy(t, conn)
	if !reflect.DeepEqual(after.rows, before.rows) || after.version != schemaVersion {
		t.Fatalf("migration changed legacy fields: before=%+v after=%+v", before, after)
	}
	freshRoot := newRoot(t)
	if err := Initialize(testContext(t), freshRoot, testRootID); err != nil {
		t.Fatal(err)
	}
	fresh := snapshotLegacy(t, rawConn(t, DatabasePath(freshRoot)))
	if !reflect.DeepEqual(after.schema, fresh.schema) {
		t.Fatalf("migrated schema differs from fresh current schema: migrated=%v fresh=%v", after.schema, fresh.schema)
	}
	s := openStore(t, root)
	for _, id := range []string{testScheduleID, otherSchedule} {
		if _, err := s.GetSchedule(testContext(t), id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{testTaskID, otherTaskID} {
		if _, err := s.GetOccurrence(testContext(t), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.transact(testContext(t), s.currentGuard, func(ctx context.Context, tx *sql.Tx) error { return verifyObjectSet(ctx, tx, schemaObjects()) }); err != nil {
		t.Fatal(err)
	}
	requireEmptyV2History(t, conn)
	migrated := snapshotLegacy(t, conn)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("repeat migrated v1 initialize: %v", err)
	}
	if repeated := snapshotLegacy(t, conn); !reflect.DeepEqual(repeated, migrated) {
		t.Fatalf("repeating migrated v1 initialize changed database: before=%+v after=%+v", migrated, repeated)
	}
	requireEmptyV2History(t, conn)
}

func requireV1RootMismatchPreservesDatabase(t *testing.T, root string, conn *sql.Conn) {
	t.Helper()
	before := snapshotLegacy(t, conn)
	if err := Initialize(testContext(t), root, otherRootID); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("v1 migration accepted wrong root: %v", err)
	}
	if refused := snapshotLegacy(t, conn); !reflect.DeepEqual(refused, before) {
		t.Fatalf("wrong-root v1 migration changed legacy database: before=%+v after=%+v", before, refused)
	}
}

func requireEmptyV2History(t *testing.T, conn *sql.Conn) {
	t.Helper()
	// These facts first exist in v2; migration must not invent reliability history.
	for _, table := range []string{"schedule_hooks", "occurrence_attempts", "occurrence_results", "schedule_policy", "schedule_holds", "notification_attempts"} {
		if n := rawInt(t, conn, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("migration invented %s rows: %d", table, n)
		}
	}
}

func TestV1MigrationRollbackPreservesLegacyDatabase(t *testing.T) {
	root := legacyDatabase(t)
	path := DatabasePath(root)
	conn := rawConn(t, path)
	before := snapshotLegacy(t, conn)
	connector, err := sqlite.NewConnector(databaseURI(path, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	fault := &faultConnector{base: connector}
	fault.inject(faultCommitBeforeReal)
	if err := initializeWithConnector(testContext(t), fault, path, testRootID, runtime.GOOS, schemaObjects()); !errors.Is(err, errInjected) {
		t.Fatalf("missing injected failure %v", err)
	}
	if after := snapshotLegacy(t, conn); !reflect.DeepEqual(after, before) {
		t.Fatalf("rollback changed legacy schema/fields: before=%+v after=%+v", before, after)
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
