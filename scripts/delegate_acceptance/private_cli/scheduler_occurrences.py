"""Read-only occurrence discovery; task files remain completion authority."""

from contextlib import closing
from dataclasses import dataclass
from pathlib import Path
import re
import sqlite3
import stat

from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp, _timestamp_unix_ns


MINUTE_NS = 60_000_000_000
IDENTITY = re.compile(r"[0-9a-f]{32}")


def identity(value: object) -> str:
    if not isinstance(value, str) or IDENTITY.fullmatch(value) is None:
        raise RuntimeError("scheduler acceptance received an invalid identity")
    return value


def json_object(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise RuntimeError("scheduler acceptance expected a JSON object")
    return dict(value)


def timestamp_ns(value: object) -> int:
    return _timestamp_unix_ns(_canonical_timestamp(value, "scheduler timestamp"))


@dataclass(frozen=True)
class Occurrence:
    task_id: str
    schedule_id: str
    due_ns: int


def read_occurrences(state: Path, root_id: str) -> tuple[Occurrence, ...]:
    """Only read identities/due times from the production store's current schema.

    The path and columns are defined by internal/scheduler/internal/sqlitestore.
    No cached observation, release, or queue row authorizes task completion.
    mode=ro refuses a missing database; opening never initializes or migrates it.
    """
    database = state / ".scheduler" / "schedules.sqlite"
    info = database.lstat()
    if not stat.S_ISREG(info.st_mode) or database.resolve(strict=True) != database:
        raise RuntimeError("scheduler occurrence database is unsafe")
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=1)) as connection:
        bound = connection.execute("SELECT root_id FROM scheduler_metadata WHERE singleton = 1").fetchone()
        if bound != (identity(root_id),):
            raise RuntimeError("scheduler occurrence database belongs to a different root")
        rows = connection.execute(
            "SELECT task_id, schedule_id, due_unix_nanos FROM occurrences ORDER BY due_unix_nanos, task_id LIMIT 257"
        ).fetchall()
    if len(rows) > 256:
        raise RuntimeError("bounded scheduler acceptance exceeded its occurrence inventory")
    result = []
    for task_id, schedule_id, due in rows:
        if type(due) is not int or due <= 0 or due % MINUTE_NS:
            raise RuntimeError("minute schedule produced an invalid due time")
        result.append(Occurrence(identity(task_id), identity(schedule_id), due))
    return tuple(result)


def require_occurrences(rows: tuple[Occurrence, ...], schedule_id: str, due_times: tuple[int, ...]) -> None:
    if tuple(row.due_ns for row in rows) != due_times or any(row.schedule_id != schedule_id for row in rows):
        raise RuntimeError("scheduler occurrence inventory differs from the actual minute ticks")
    if len({row.task_id for row in rows}) != len(rows):
        raise RuntimeError("scheduler reused a task ID for independent occurrences")
