"""Read-only cross-schema preservation oracle, taken only after writers join."""

from contextlib import closing
from dataclasses import dataclass
from pathlib import Path
import sqlite3
import stat

from scripts.acceptance.evidence.validation import require
from scripts.acceptance.evidence.files import digest
from scripts.acceptance.private_cli.scheduler.occurrences import identity
from scripts.acceptance.tasks.publication import task_snapshot
from scripts.acceptance.tasks.publication import snapshot as evidence_snapshot
from scripts.acceptance.lifetime.closure import validate_managed_lifetime_closure


PRESERVED_TABLES = ("scheduler_metadata", "schedules", "occurrences", "skip_spans")
RELIABILITY_TABLES = ("schedule_hooks", "occurrence_attempts", "occurrence_results",
                      "schedule_policy", "schedule_holds", "notification_attempts")
TABLE_KEYS = dict(zip(PRESERVED_TABLES + RELIABILITY_TABLES,
                     ("singleton", "schedule_id", "task_id", "schedule_id,to_unix_nanos",
                      "schedule_id", "task_id", "task_id", "schedule_id", "schedule_id", "event_id")))
MAX_ROWS = 256
MAX_VALUE_BYTES = 8 << 20


@dataclass(frozen=True)
class StoreSnapshot:
    version: int
    root_id: str
    tables: tuple[tuple[str, tuple[str, ...], tuple[tuple[object, ...], ...]], ...]
    tasks: tuple[tuple[str, object], ...]
    closure: tuple[tuple[str, object], ...] = ()

    def rows(self, table: str) -> tuple[tuple[object, ...], ...]:
        for name, _columns, rows in self.tables:
            if name == table:
                return rows
        raise RuntimeError("snapshot table is unavailable")


def snapshot(state: Path, root_id: str) -> StoreSnapshot:
    database = state / ".scheduler" / "schedules.sqlite"
    require(stat.S_ISREG(database.lstat().st_mode) and database.resolve(strict=True) == database,
            "upgrade snapshot database is unsafe")
    tables = []
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=1)) as connection:
        version, = connection.execute("PRAGMA user_version").fetchone()
        require(version in (1, 2), "upgrade snapshot has an unsupported schema")
        require(connection.execute("SELECT root_id FROM scheduler_metadata WHERE singleton=1").fetchone()
                == (identity(root_id),), "upgrade snapshot root changed")
        for name in PRESERVED_TABLES + (RELIABILITY_TABLES if version == 2 else ()):
            # Names are fixed harness constants, never input from a record.
            cursor = connection.execute("SELECT * FROM " + name + " ORDER BY " +
                                        TABLE_KEYS[name]
                                        + " LIMIT 257")
            rows = tuple(cursor.fetchall())
            require(len(rows) <= MAX_ROWS, "upgrade snapshot exceeded bounded inventory")
            require(all(not isinstance(value, (bytes, str)) or len(value) <= MAX_VALUE_BYTES
                        for row in rows for value in row), "upgrade snapshot value exceeded bound")
            tables.append((name, tuple(column[0] for column in cursor.description), rows))
    occurrences = next(rows for name, _columns, rows in tables if name == "occurrences")
    tasks = tuple((identity(row[0]), task_snapshot(state, identity(row[0])))
                  for row in occurrences if (state / "tasks" / identity(row[0])).is_dir())
    closure = ()
    if tasks:
        checked = validate_managed_lifetime_closure(state, tuple(task_id for task_id, _ in tasks))
        require(checked.get("root_id") == root_id, "upgrade closure root changed")
        canonical = checked.get("tasks")
        if not isinstance(canonical, dict):
            raise RuntimeError("upgrade canonical task closure is unavailable")
        closure = tuple((task_id, {"canonical": canonical[task_id],
                                  "files": evidence_snapshot(state / "tasks" / task_id / "lifetime", ignored_prefixes=())})
                        for task_id, _ in tasks)
    return StoreSnapshot(version, root_id, tuple(tables), tasks, closure)


def require_preserved(before: StoreSnapshot, after: StoreSnapshot) -> None:
    require(after.version == 2, "candidate did not migrate to scheduler schema2")
    previous = {name: (columns, rows) for name, columns, rows in before.tables}
    current = {name: (columns, rows) for name, columns, rows in after.tables}
    require(before.root_id == after.root_id and all(current.get(name) == rows for name, rows in previous.items()),
            "upgrade changed definitions, providers, cadence, state, reservations or historical skip spans")
    if before.version == 1:
        require(all(not current[name][1] for name in RELIABILITY_TABLES),
                "v1 migration invented historical reliability facts")
    require(before.tasks == after.tasks and before.closure == after.closure, "upgrade changed immutable task/result/closure evidence")


def require_runtime_preserved(before: StoreSnapshot, after: StoreSnapshot, paused: str) -> None:
    """Compare immutable old evidence after every candidate writer has joined."""
    require(before.root_id == after.root_id and after.version == 2, "runtime changed scheduler identity")
    for prior, current in ((before.tasks, after.tasks), (before.closure, after.closure)):
        indexed = dict(current)
        require(all(indexed.get(task_id) == evidence for task_id, evidence in prior),
                "candidate runtime changed old immutable task/result/closure evidence")
    for table, immutable, key in (
            ("schedules", ("schedule_id", "created_at_unix_nanos", "cron", "timezone", "request_json", "brief",
                           "brief_sha256", "definition_sha256"), "schedule_id"),
            ("occurrences", ("task_id", "schedule_id", "due_unix_nanos", "armed_at_unix_nanos"), "task_id")):
        def records(value: StoreSnapshot):
            columns, rows = next((columns, rows) for name, columns, rows in value.tables if name == table)
            return {row[columns.index(key)]: dict(zip(columns, row)) for row in rows}
        old, new = records(before), records(after)
        for record_id, record in old.items():
            require(record_id in new and all(new[record_id][field] == record[field] for field in immutable),
                    "candidate runtime changed immutable definition or occurrence identity")
            if table == "schedules" and record_id == paused:
                require(record["state"] == new[record_id]["state"] == "paused", "candidate resumed untouched paused definition")
            if table == "occurrences" and record["released_at_unix_nanos"] is not None:
                require(all(new[record_id][field] == record[field] for field in
                            ("released_at_unix_nanos", "release_kind", "release_ref")), "candidate changed an old release fact")
    require(set(before.rows("skip_spans")) <= set(after.rows("skip_spans")), "candidate changed old immutable skip history")
    if before.version == 2:
        for name in ("occurrence_attempts", "schedule_holds", "notification_attempts"):
            if not before.rows(name):
                require(not after.rows(name), "healthy candidate invented failure attempts, holds or notifications")
        for name, columns, rows in before.tables:
            if name not in ("schedule_hooks", "occurrence_attempts", "schedule_policy", "occurrence_results"):
                continue
            current_columns, current_rows = next((columns, rows) for table, columns, rows in after.tables if table == name)
            require(columns == current_columns, "candidate runtime changed reliability columns")
            preserved = rows
            if name == "schedule_policy":
                preserved = tuple(row for row in rows if row[columns.index("schedule_id")] == paused)
            if name == "occurrence_results":
                # RecordResult retains committed facts exactly. The old held
                # task's unknown projection may advance after canonical closure.
                preserved = tuple(row for row in rows if row[columns.index("verdict")] == "committed")
            require(set(preserved) <= set(current_rows), "candidate runtime changed old immutable reliability evidence")


def scheduler_files(state: Path) -> dict[str, tuple[int, int, str]]:
    """Bounded evidence bytes and complete regular-file/sidecar inventory, never executables."""
    directory = state / ".scheduler"
    entries = tuple(directory.iterdir())
    require(len(entries) <= 64, "scheduler passive evidence inventory exceeded bound")
    result = {}
    for path in entries:
        info = path.lstat()
        require(not stat.S_ISLNK(info.st_mode), "scheduler passive evidence contains a symlink")
        require(not stat.S_ISREG(info.st_mode) or info.st_size <= 32 << 20, "scheduler passive evidence exceeded byte bound")
        result[path.name] = (info.st_mode, info.st_size, digest(path) if stat.S_ISREG(info.st_mode) else "")
    return result
