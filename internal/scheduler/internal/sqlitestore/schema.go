package sqlitestore

import (
	"fmt"
	"strings"
)

// schemaVersion is the PRAGMA user_version this code reads and writes. Only
// the 0 to 1 initialization exists; there is no shipped later version.
const schemaVersion = 2

// Every timestamp column holds caller-supplied UTC Unix nanoseconds. Text
// shapes repeat the contract's stored-value rules so that a write bypassing the
// contract still cannot store an invalid fact; reads validate again.
const (
	hex32Check = "length(%[1]s) = 32 AND %[1]s NOT GLOB '*[^0-9a-f]*'"
	hex64Check = "length(%[1]s) = 64 AND %[1]s NOT GLOB '*[^0-9a-f]*'"
	codeCheck  = "length(%[1]s) BETWEEN 1 AND 64 AND %[1]s NOT GLOB '*[^a-z_]*'"
)

type schemaObject struct {
	kind string
	name string
	sql  string
}

// schemaObjects returns the exact schema objects of version 1 in creation
// order. The guard compares sqlite_schema against them in every transaction.
func legacySchemaObjects() []schemaObject {
	objects := metadataObjects()
	objects = append(objects, scheduleObjects()...)
	objects = append(objects, occurrenceObjects()...)
	return append(objects, legacySkipSpanObjects()...)
}

func check(format, column string) string { return fmt.Sprintf(format, column) }

func trigger(name, event, message string) schemaObject {
	return schemaObject{kind: "trigger", name: name, sql: "CREATE TRIGGER " + name + "\n" + event +
		"\nBEGIN SELECT RAISE(ABORT, '" + message + "'); END"}
}

// metadataObjects holds the one immutable root binding row.
func metadataObjects() []schemaObject {
	return []schemaObject{
		{kind: "table", name: "scheduler_metadata", sql: `CREATE TABLE scheduler_metadata (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	root_id TEXT NOT NULL CHECK (` + check(hex32Check, "root_id") + `)
) STRICT`},
		trigger("scheduler_metadata_immutable", "BEFORE UPDATE ON scheduler_metadata", "scheduler metadata is immutable"),
		trigger("scheduler_metadata_permanent", "BEFORE DELETE ON scheduler_metadata", "scheduler metadata is permanent"),
	}
}

// scheduleObjects holds immutable definitions beside their mutable state and
// cursor. Deleted is terminal and the cursor only advances.
func scheduleObjects() []schemaObject {
	return []schemaObject{
		{kind: "table", name: "schedules", sql: `CREATE TABLE schedules (
	schedule_id TEXT PRIMARY KEY CHECK (` + check(hex32Check, "schedule_id") + `),
	created_at_unix_nanos INTEGER NOT NULL,
	cron TEXT NOT NULL CHECK (length(CAST(cron AS BLOB)) BETWEEN 1 AND 256),
	timezone TEXT NOT NULL CHECK (length(CAST(timezone AS BLOB)) BETWEEN 1 AND 256),
	request_json BLOB NOT NULL CHECK (length(request_json) BETWEEN 1 AND 1048576),
	brief BLOB NOT NULL CHECK (length(brief) BETWEEN 1 AND 8388608),
	brief_sha256 TEXT NOT NULL CHECK (` + check(hex64Check, "brief_sha256") + `),
	definition_sha256 TEXT NOT NULL CHECK (` + check(hex64Check, "definition_sha256") + `),
	state TEXT NOT NULL CHECK (state IN ('enabled', 'paused', 'deleted')),
	cursor_unix_nanos INTEGER NOT NULL CHECK (cursor_unix_nanos >= created_at_unix_nanos)
) STRICT`},
		trigger("schedules_definition_immutable",
			"BEFORE UPDATE OF schedule_id, created_at_unix_nanos, cron, timezone, request_json, brief, brief_sha256, definition_sha256 ON schedules",
			"schedule definition is immutable"),
		trigger("schedules_deleted_terminal", "BEFORE UPDATE OF state ON schedules WHEN OLD.state = 'deleted'",
			"deleted schedule state is terminal"),
		trigger("schedules_cursor_forward",
			"BEFORE UPDATE OF cursor_unix_nanos ON schedules WHEN NEW.cursor_unix_nanos <= OLD.cursor_unix_nanos",
			"schedule cursor only advances"),
		trigger("schedules_permanent", "BEFORE DELETE ON schedules", "schedules are permanent"),
	}
}

// occurrenceObjects holds reservations keyed by task ID. The partial unique
// index is the only authority for which occurrence of a schedule is active.
// Observation and release columns are each one all-or-nothing tuple.
func occurrenceObjects() []schemaObject {
	return []schemaObject{
		{kind: "table", name: "occurrences", sql: `CREATE TABLE occurrences (
	task_id TEXT PRIMARY KEY CHECK (` + check(hex32Check, "task_id") + `),
	schedule_id TEXT NOT NULL REFERENCES schedules (schedule_id),
	due_unix_nanos INTEGER NOT NULL,
	armed_at_unix_nanos INTEGER NOT NULL,
	observed_at_unix_nanos INTEGER,
	admission INTEGER CHECK (admission BETWEEN 0 AND 2),
	liveness INTEGER CHECK (liveness BETWEEN 0 AND 2),
	publication INTEGER CHECK (publication BETWEEN 0 AND 3),
	outcome_sha256 TEXT CHECK (outcome_sha256 IS NULL OR (` + check(hex64Check, "outcome_sha256") + `)),
	fault_code TEXT CHECK (fault_code IS NULL OR (` + check(codeCheck, "fault_code") + `)),
	released_at_unix_nanos INTEGER,
	release_kind TEXT CHECK (release_kind IN ('not_created', 'closed')),
	release_ref TEXT,
	UNIQUE (schedule_id, due_unix_nanos),
	CHECK ((observed_at_unix_nanos IS NULL) = (admission IS NULL)
		AND (observed_at_unix_nanos IS NULL) = (liveness IS NULL)
		AND (observed_at_unix_nanos IS NULL) = (publication IS NULL)),
	CHECK (observed_at_unix_nanos IS NOT NULL OR (outcome_sha256 IS NULL AND fault_code IS NULL)),
	CHECK (observed_at_unix_nanos IS NULL OR (publication IN (2, 3)) = (outcome_sha256 IS NOT NULL)),
	CHECK ((released_at_unix_nanos IS NULL) = (release_kind IS NULL)
		AND (released_at_unix_nanos IS NULL) = (release_ref IS NULL)),
	CHECK (release_kind IS NULL
		OR (release_kind = 'not_created' AND ` + check(codeCheck, "release_ref") + `)
		OR (release_kind = 'closed' AND ` + check(hex64Check, "release_ref") + `))
) STRICT`},
		{kind: "index", name: "occurrences_one_active", sql: `CREATE UNIQUE INDEX occurrences_one_active
ON occurrences (schedule_id) WHERE released_at_unix_nanos IS NULL`},
		trigger("occurrences_identity_immutable",
			"BEFORE UPDATE OF task_id, schedule_id, due_unix_nanos, armed_at_unix_nanos ON occurrences",
			"occurrence identity is immutable"),
		trigger("occurrences_release_immutable",
			"BEFORE UPDATE OF released_at_unix_nanos, release_kind, release_ref ON occurrences WHEN OLD.released_at_unix_nanos IS NOT NULL",
			"occurrence release is immutable"),
		trigger("occurrences_observation_newer",
			"BEFORE UPDATE OF observed_at_unix_nanos ON occurrences WHEN OLD.observed_at_unix_nanos IS NOT NULL"+
				" AND (NEW.observed_at_unix_nanos IS NULL OR NEW.observed_at_unix_nanos <= OLD.observed_at_unix_nanos)",
			"occurrence observation only advances"),
		trigger("occurrences_permanent", "BEFORE DELETE ON occurrences", "occurrences are permanent"),
	}
}

// skipSpanObjects holds immutable (from, to] spans keyed by their endpoint.
func legacySkipSpanObjects() []schemaObject {
	return []schemaObject{
		{kind: "table", name: "skip_spans", sql: `CREATE TABLE skip_spans (
	schedule_id TEXT NOT NULL REFERENCES schedules (schedule_id),
	from_unix_nanos INTEGER NOT NULL,
	to_unix_nanos INTEGER NOT NULL CHECK (to_unix_nanos > from_unix_nanos),
	reason TEXT NOT NULL CHECK (reason IN ('overlap_active', 'blocked_uncertain', 'missed_unavailable', 'missed_capacity', 'paused')),
	PRIMARY KEY (schedule_id, to_unix_nanos)
) STRICT`},
		trigger("skip_spans_immutable", "BEFORE UPDATE ON skip_spans", "skip spans are immutable"),
		trigger("skip_spans_permanent", "BEFORE DELETE ON skip_spans", "skip spans are permanent"),
	}
}

func schemaObjects() []schemaObject {
	objects := legacySchemaObjects()
	for i := range objects {
		if objects[i].name == "skip_spans" {
			objects[i].sql = strings.Replace(objects[i].sql, "'paused'))", "'paused', 'provider_quota_exhausted'))", 1)
		}
	}
	return append(objects, reliabilityObjects()...)
}

func reliabilityObjects() []schemaObject {
	var objects []schemaObject
	for _, table := range []struct {
		name, columns string
		immutable     bool
	}{
		{"schedule_hooks", `schedule_id TEXT PRIMARY KEY REFERENCES schedules (schedule_id), hook_json BLOB NOT NULL CHECK (length(hook_json) BETWEEN 1 AND 8192)`, true},
		{"occurrence_attempts", `task_id TEXT PRIMARY KEY REFERENCES occurrences (task_id), attempt_json BLOB NOT NULL CHECK (length(attempt_json) BETWEEN 1 AND 2048)`, true},
		{"occurrence_results", `task_id TEXT PRIMARY KEY REFERENCES occurrences (task_id), observed_at INTEGER NOT NULL, verdict TEXT NOT NULL CHECK (verdict IN ('unknown','committed','rejected','not_created','closed_without_outcome')), code TEXT, outcome_sha256 TEXT, quota_until INTEGER`, false},
		{"schedule_policy", `schedule_id TEXT PRIMARY KEY REFERENCES schedules (schedule_id), enabled INTEGER NOT NULL CHECK (enabled IN (0,1)), max_hold INTEGER NOT NULL CHECK (max_hold > 0 AND max_hold <= 604800000000000), revision INTEGER NOT NULL CHECK (revision > 0), source_floor INTEGER NOT NULL`, false},
		{"schedule_holds", `schedule_id TEXT PRIMARY KEY REFERENCES schedules (schedule_id), source_task_id TEXT NOT NULL REFERENCES occurrences (task_id), created_at INTEGER NOT NULL, until_at INTEGER NOT NULL CHECK (until_at > created_at), capped INTEGER NOT NULL CHECK (capped IN (0,1))`, false},
		{"notification_attempts", `event_id TEXT PRIMARY KEY CHECK (` + check(hex32Check, "event_id") + `), schedule_id TEXT NOT NULL REFERENCES schedules (schedule_id), kind TEXT NOT NULL CHECK (kind IN ('failure','recovery')), anchor_task_id TEXT NOT NULL REFERENCES occurrences (task_id), last_task_id TEXT NOT NULL REFERENCES occurrences (task_id), streak_count INTEGER NOT NULL CHECK (streak_count >= 0), failure_code TEXT NOT NULL, created_at INTEGER NOT NULL, result TEXT NOT NULL CHECK (result IN ('claimed','succeeded','failed','timed_out','unavailable','uncertain'))`, false},
	} {
		objects = append(objects, schemaObject{kind: "table", name: table.name, sql: "CREATE TABLE " + table.name + " (\n" + table.columns + "\n) STRICT"})
		if table.immutable {
			objects = append(objects, trigger(table.name+"_immutable", "BEFORE UPDATE ON "+table.name, "immutable scheduler fact"), trigger(table.name+"_permanent", "BEFORE DELETE ON "+table.name, "permanent scheduler fact"))
		}
	}
	return objects
}
