"""Interpret passive scheduler proof without treating reservations as task creation."""

import re

from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.scheduler.occurrences import Occurrence
from scripts.acceptance.private_cli.scheduler.occurrences import json_object
from scripts.acceptance.private_cli.scheduler.occurrences import timestamp_ns


def not_created_failure(response: dict[str, object], root_id: str, row: Occurrence, *,
                        allow_in_flight: bool = False) -> dict[str, object] | None:
    """Return bounded context only for the exact occurrence's authoritative absence.

    The scheduler owns release evidence; validated local inspection owns task
    absence. Neither a missing path nor a cached observation proves no creation.
    """
    require(type(response.get("schema_version")) is int and response.get("schema_version") == 1
            and response.get("command") == "schedule"
            and response.get("root_id") == root_id and response.get("task_id") == row.task_id,
            "scheduler execution response identity changed")
    execution = json_object(response.get("schedule_execution"))
    occurrence = json_object(execution.get("occurrence"))
    require(occurrence.get("task_id") == row.task_id and occurrence.get("schedule_id") == row.schedule_id
            and timestamp_ns(occurrence.get("due")) == row.due_ns,
            "scheduler execution occurrence identity changed")
    query_error = response.get("schedule_query_error")
    if (allow_in_flight and execution.get("complete") is False and occurrence.get("reservation") == "active"
            and isinstance(query_error, dict) and query_error.get("code") == "invalid_evidence"
            and execution.get("local_evidence") is None):
        # Creation publishes its directory before all prepared records. This
        # typed incomplete read can remain pending, but never authorizes cleanup.
        return None
    require(not response.get("error") and not response.get("failure") and query_error is None,
            "scheduler execution inspection failed")
    if execution.get("complete") is not True or occurrence.get("reservation") != "released":
        return None
    release = json_object(occurrence.get("release"))
    evidence = json_object(execution.get("local_evidence"))
    if not (release.get("kind") == "not_created" and release.get("ref") == "task_absent"
            and evidence.get("provenance") == "validated_local" and evidence.get("task_presence") == "absent"
            and evidence.get("supervisor_receipt") is False):
        return None
    failure = occurrence.get("failure")
    diagnostic = ({"code": "admission_failed", "stage": "dispatch", "next_action": "inspect_schedule_execution"}
                  if failure is None else json_object(failure))
    context: dict[str, object] = {"task_id": row.task_id, "schedule_id": row.schedule_id,
                                  "due_unix_nanos": row.due_ns}
    for name in ("code", "stage", "next_action"):
        value = diagnostic.get(name)
        require(isinstance(value, str) and re.fullmatch(r"[a-z][a-z0-9_-]{0,127}", value) is not None,
                "scheduler admission diagnostic has an invalid field")
        context[name] = value
    return context
