package sqlitestore

import (
	"database/sql"
	"strings"
	"testing"
)

const (
	testOutcomeSHA256 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testClosureSHA256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// schemaSQL returns the exact production text of one schema object.
func schemaSQL(t *testing.T, name string) string {
	t.Helper()
	for _, object := range schemaObjects() {
		if object.name == name {
			return object.sql
		}
	}
	t.Fatalf("no schema object %q", name)
	return ""
}

// bypassTrigger runs statements with one trigger dropped, then recreates it
// with its exact production text, so the schema guard still admits the store
// and only the stored data is tampered.
func bypassTrigger(t *testing.T, root, trigger string, statements ...string) {
	t.Helper()
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, append(append([]string{"DROP TRIGGER " + trigger}, statements...), schemaSQL(t, trigger))...)
	closeRaw(t, conn)
}

// bypassChecks runs statements with CHECK constraints ignored on one raw
// connection, to store rows the schema would refuse.
func bypassChecks(t *testing.T, root string, statements ...string) {
	t.Helper()
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, append([]string{"PRAGMA ignore_check_constraints = ON"}, statements...)...)
	closeRaw(t, conn)
}

// enforcingConn is an independent connection with foreign keys enforced, as
// every production connection has.
func enforcingConn(t *testing.T, root string) *sql.Conn {
	t.Helper()
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "PRAGMA foreign_keys = ON")
	return conn
}

func TestSchemaRefusesInvalidRowsDirectly(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	conn := enforcingConn(t, root)
	occurrence := "INSERT INTO occurrences (task_id, schedule_id, due_unix_nanos, armed_at_unix_nanos"
	cases := map[string]struct{ statement, refusal string }{
		"foreign key":            {occurrence + ") VALUES ('" + otherTaskID + "', '" + otherSchedule + "', 1, 1)", "FOREIGN KEY constraint failed"},
		"strict type":            {occurrence + ") VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 'soon', 1)", "cannot store TEXT value in INTEGER column"},
		"task id check":          {occurrence + ") VALUES ('ABC', '" + testScheduleID + "', 1, 1)", "CHECK constraint failed"},
		"one active reservation": {occurrence + ") VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 3000000000000, 1)", "UNIQUE constraint failed: occurrences.schedule_id"},
		"duplicate due":          {occurrence + ", released_at_unix_nanos, release_kind, release_ref) VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 2000000000000, 1, 1, 'closed', '" + testClosureSHA256 + "')", "UNIQUE constraint failed: occurrences.schedule_id, occurrences.due_unix_nanos"},
		"partial observation":    {occurrence + ", observed_at_unix_nanos) VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 1, 1, 1)", "CHECK constraint failed"},
		"outcome without commit": {occurrence + ", observed_at_unix_nanos, admission, liveness, publication, outcome_sha256, released_at_unix_nanos, release_kind, release_ref) VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 1, 1, 1, 2, 2, 1, '" + testOutcomeSHA256 + "', 1, 'closed', '" + testClosureSHA256 + "')", "CHECK constraint failed"},
		"partial release":        {occurrence + ", released_at_unix_nanos) VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 1, 1, 1)", "CHECK constraint failed"},
		"release reference":      {occurrence + ", released_at_unix_nanos, release_kind, release_ref) VALUES ('" + otherTaskID + "', '" + testScheduleID + "', 1, 1, 1, 'closed', 'not_a_digest')", "CHECK constraint failed"},
		"empty span":             {"INSERT INTO skip_spans VALUES ('" + testScheduleID + "', 5, 5, 'paused')", "CHECK constraint failed"},
		"span reason":            {"INSERT INTO skip_spans VALUES ('" + testScheduleID + "', 5, 6, 'bored')", "CHECK constraint failed"},
		"second metadata row":    {"INSERT INTO scheduler_metadata VALUES (2, '" + otherRootID + "')", "CHECK constraint failed"},
		"definition immutable":   {"UPDATE schedules SET cron = '* * * * *'", "schedule definition is immutable"},
		"cursor only advances":   {"UPDATE schedules SET cursor_unix_nanos = cursor_unix_nanos", "schedule cursor only advances"},
		"schedules permanent":    {"DELETE FROM schedules", "schedules are permanent"},
		"identity immutable":     {"UPDATE occurrences SET due_unix_nanos = 9", "occurrence identity is immutable"},
		"occurrences permanent":  {"DELETE FROM occurrences", "occurrences are permanent"},
		"metadata immutable":     {"UPDATE scheduler_metadata SET root_id = '" + otherRootID + "'", "scheduler metadata is immutable"},
		"metadata permanent":     {"DELETE FROM scheduler_metadata", "scheduler metadata is permanent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			requireStatementRefused(t, conn, tc.statement, tc.refusal)
		})
	}
	if count := rawInt(t, conn, "SELECT count(*) FROM occurrences"); count != 1 {
		t.Fatalf("%d occurrences after refused writes", count)
	}
}

func TestTriggersProtectReleasedAndObservedOccurrences(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	if err := store.RecordObservation(testContext(t), testObservation(testTaskID, 3_000)); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(testContext(t), testRelease(testTaskID, 4_000)); err != nil {
		t.Fatal(err)
	}
	conn := enforcingConn(t, root)
	requireStatementRefused(t, conn, "UPDATE occurrences SET observed_at_unix_nanos = observed_at_unix_nanos", "occurrence observation only advances")
	requireStatementRefused(t, conn, "UPDATE occurrences SET release_ref = '"+testOutcomeSHA256+"'", "occurrence release is immutable")
	rawExec(t, conn, "INSERT INTO skip_spans VALUES ('"+testScheduleID+"', 2000000000000, 2500000000000, 'missed_capacity')")
	requireStatementRefused(t, conn, "UPDATE skip_spans SET reason = 'paused'", "skip spans are immutable")
	requireStatementRefused(t, conn, "DELETE FROM skip_spans", "skip spans are permanent")
	rawExec(t, conn, "UPDATE schedules SET state = 'deleted'")
	requireStatementRefused(t, conn, "UPDATE schedules SET state = 'enabled'", "deleted schedule state is terminal")
}

func requireStatementRefused(t *testing.T, conn *sql.Conn, statement, refusal string) {
	t.Helper()
	_, err := conn.ExecContext(testContext(t), statement)
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("%s: err=%v, want %q", statement, err, refusal)
	}
}
