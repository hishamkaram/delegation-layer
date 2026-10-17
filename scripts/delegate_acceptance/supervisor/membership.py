"""Exact queue membership using supplied ledgers, journals, and row validators."""

from __future__ import annotations

import shlex
from scripts.delegate_acceptance.evidence.validation import _required_keys, require
from scripts.delegate_acceptance.supervisor.queue_policy import (
    _PUEUE_GROUP_KEYS,
    done_result,
    row_state,
)
from scripts.delegate_acceptance.tasks.refusal_policy import INSPECTION_FAILURE_RESULTS
from pathlib import Path
from typing import Callable


def validate_queue_membership(*, status: dict[str, object], allow_failure: bool,
        tasks_by_name: dict[str, str], labels_by_name: dict[str, str],
        numbers_by_name: dict[str, int], expected_results: dict[str, str],
        dispatch_attempts: set[str], expected_binding_required: bool,
        inspection_journals: Callable[..., dict[str, dict[str, object]]],
        durable_admission: Callable[..., dict[str, object] | None],
        validate_groups: Callable[..., None], validate_ordinary_row: Callable[..., None],
        validate_inspection_row: Callable[..., None]):
    tasks = status.get("tasks")
    require(isinstance(tasks, dict), "private queue tasks object is absent")
    require(dispatch_attempts, "no supervisor dispatch attempt was recorded")
    journals = inspection_journals(
        allow_failure, expected_binding_required)
    ordinary = _ordinary_queue_admissions(
        allow_failure=allow_failure, dispatch_attempts=dispatch_attempts, durable_admission=durable_admission,
        expected_results=expected_results, journals=journals,
        labels_by_name=labels_by_name, numbers_by_name=numbers_by_name, tasks_by_name=tasks_by_name,
    )
    ordinary_task_ids = set(ordinary)
    require(ordinary_task_ids <= dispatch_attempts,
            "ordinary task response has no matching dispatch attempt")
    inspection_task_ids = set(journals)
    require(ordinary_task_ids | inspection_task_ids == dispatch_attempts,
            "a dispatch attempt has no exact ordinary or inspection record")
    if not allow_failure:
        require(ordinary_task_ids == dispatch_attempts,
                "a successful dispatch attempt has no exact ordinary task response")
    historical_journals = _inspection_membership_groups(journals, status, validate_groups)
    ordinary_labels = {value["label"] for value in ordinary.values()}
    queue_journals = [journal for journal in journals.values()
                      if journal["inspection_exit_authority"]["mode"] in
                      {"queue", "guardian-unstarted"}]
    guardian_journals = [journal for journal in journals.values()
                         if journal["inspection_exit_authority"]["mode"] == "guardian"]
    guardian_unlaunched_journals = [journal for journal in journals.values()
                                   if journal["inspection_exit_authority"]["mode"] ==
                                   "guardian-unlaunched"]
    historical_queue_journals = [journal for journal in historical_journals
                                 if journal["inspection_exit_authority"]["mode"] in
                                 {"queue", "guardian-unstarted"}]
    historical_guardian_journals = [journal for journal in historical_journals
                                   if journal["inspection_exit_authority"]["mode"] == "guardian"]
    historical_guardian_unlaunched_journals = [
        journal for journal in historical_journals
        if journal["inspection_exit_authority"]["mode"] == "guardian-unlaunched"
    ]
    required_labels = ordinary_labels | {journal["label"] for journal in queue_journals}
    optional_labels = ({journal["label"] for journal in guardian_journals} |
                       {journal["label"] for journal in guardian_unlaunched_journals} |
                       {journal["label"] for journal in historical_queue_journals} |
                       {journal["label"] for journal in historical_guardian_journals} |
                       {journal["label"] for journal in historical_guardian_unlaunched_journals})
    allowed_labels = required_labels | optional_labels
    rows = _validate_ordinary_membership(
        allow_failure, allowed_labels, ordinary, required_labels, tasks, validate_ordinary_row,
    )
    _validate_inspection_membership_rows(allow_failure=allow_failure, guardian_journals=guardian_journals,
        guardian_unlaunched_journals=guardian_unlaunched_journals, historical_guardian_journals=historical_guardian_journals,
        historical_guardian_unlaunched_journals=historical_guardian_unlaunched_journals, historical_queue_journals=historical_queue_journals,
        queue_journals=queue_journals, rows=rows, validate_inspection_row=validate_inspection_row)
    return journals


def validate_group_snapshot(groups: object, expected: set[str]):
    require(isinstance(groups, dict), "private queue groups object is absent")
    require(set(groups) == expected, "private queue contains an unknown supervisor group")
    for name in expected:
        group = groups[name]
        require(isinstance(group, dict), f"private queue group {name} is malformed")
        _required_keys(group, _PUEUE_GROUP_KEYS, label=f"private queue group {name}")
        require(group.get("status") in {"Running", "Paused", "Reset"} and
                type(group.get("parallel_tasks")) is int and
                group["parallel_tasks"] >= 0,
                f"private queue group {name} is malformed")


def validate_inspection_queue_row(*, row: dict[str, object], journal: dict[str, object], allow_failure: bool,
        state_parent: Path, pueue_config: Path | None, runner: Path, state_root: Path,
        terminal_required: bool = True):
    task = journal["task_id"]
    label = journal["label"]
    require(row.get("label") == label and row.get("group") == journal["group"],
            f"inspection queue identity mismatch for {task}")
    require(type(row.get("id")) is int and
            row.get("id") == journal["numeric_task_id"],
            f"inspection queue numeric identity mismatch for {task}")
    row_path = row.get("path")
    require(pueue_config is not None,
            "inspection supervisor config is unavailable")
    # A fresh client uses the invocation directory. A client reconstructed
    # from its durable binding intentionally uses the config directory so
    # recovery cannot depend on an ambient directory that may be gone.
    expected_paths = {state_parent, pueue_config.parent}
    require(isinstance(row_path, str) and Path(row_path) in expected_paths,
            f"inspection queue working directory mismatch for {task}")
    require(type(terminal_required) is bool, "inspection queue terminal mode is invalid")
    state = row_state(row)
    if terminal_required:
        require(state == "Done", f"inspection worker is not Done for {task}")
    result = done_result(row)
    inspection_result = journal["result"]
    successful_result = (isinstance(inspection_result, dict) and
                         inspection_result.get("reason") == "eligible")
    if terminal_required:
        if journal.get("historical_aborted") is True:
            require(result == "Success" or result in INSPECTION_FAILURE_RESULTS,
                    f"historical inspection worker has no terminal observation for {task}")
        elif not allow_failure or successful_result:
            require(result == "Success", f"inspection worker did not succeed for {task}")
        else:
            require(result in INSPECTION_FAILURE_RESULTS,
                    f"inspection worker failure was not positively observed for {task}")
    expected = [str(runner), "--inspection", "--guardian", "--supervised",
                "--root", str(state_root), str(journal["operation_id"])]
    commands = []
    for key in ("original_command", "command"):
        value = row.get(key)
        require(isinstance(value, str), f"inspection queue row missing {key} for {task}")
        try:
            commands.append(shlex.split(value))
        except ValueError:
            commands.append([])
    require(all(command == expected for command in commands),
            f"inspection queue argv mismatch for {task}")


def _ordinary_queue_admissions(
        *, allow_failure: bool,
        dispatch_attempts: set[str],
        durable_admission: Callable[..., dict[str, object] | None],
        expected_results: dict[str, str],
        journals: dict[str, dict[str, object]],
        labels_by_name: dict[str, str],
        numbers_by_name: dict[str, int],
        tasks_by_name: dict[str, str],
) -> dict[str, dict[str, object]]:
    observed: dict[str, dict[str, object]] = {}
    for name, task in tasks_by_name.items():
        require(name in labels_by_name and name in numbers_by_name,
                f"ordinary task response is incomplete for {task}")
        observed[task] = {
            "task_id": task,
            "label": labels_by_name[name],
            "numeric_task_id": numbers_by_name[name],
            "expected_result": expected_results.get(name, "Success"),
        }
    if allow_failure:
        ordinary: dict[str, dict[str, object]] = {}
        for task in dispatch_attempts:
            durable = durable_admission(task, journals.get(task))
            if durable is None:
                continue
            if task in observed:
                require(all(observed[task][key] == durable[key]
                            for key in ("task_id", "label", "numeric_task_id")),
                        f"ordinary task response disagrees with durable admission for {task}")
            ordinary[durable["task_id"]] = durable
    else:
        ordinary = observed
    return ordinary


def _inspection_membership_groups(
        journals: dict[str, dict[str, object]],
        status: dict[str, object],
        validate_groups: Callable[..., None],
) -> list[dict[str, object]]:
    groups = status.get("groups")
    historical_journals = [history
                           for journal in journals.values()
                           for history in journal["historical_attempts"]]
    all_inspection_journals = list(journals.values()) + historical_journals
    expected_groups = {"default"} | {
        journal["group"] for journal in all_inspection_journals
    }
    validate_groups(groups, expected_groups)
    for journal in all_inspection_journals:
        group = groups.get(journal["group"])
        require(isinstance(group, dict) and group.get("status") == "Running" and
                isinstance(group.get("parallel_tasks"), int) and
                not isinstance(group.get("parallel_tasks"), bool) and
                group.get("parallel_tasks") == journal["group_parallelism"],
                f"inspection group has unexpected parallelism for {journal['task_id']}")
    return historical_journals


def _validate_ordinary_membership(
        allow_failure: bool,
        allowed_labels: set[str],
        ordinary: dict[str, dict[str, object]],
        required_labels: set[str],
        tasks: dict[str, object],
        validate_ordinary_row: Callable[..., None],
) -> list[dict[str, object]]:
    rows = [row for row in tasks.values() if isinstance(row, dict)]
    require(len(rows) == len(tasks), "private queue contains a malformed row")
    labels = [row.get("label") for row in rows]
    require(all(isinstance(label, str) for label in labels) and len(set(labels)) == len(labels) and
            set(labels) <= allowed_labels and required_labels <= set(labels),
            "private queue labels are not exact")
    for key, row in tasks.items():
        require(isinstance(key, str) and key.isdecimal() and str(int(key)) == key and
                type(row.get("id")) is int and row["id"] == int(key),
                "private queue task numeric identity is not exact")
    for task, admission in ordinary.items():
        row = next((candidate for candidate in rows
                    if candidate.get("label") == admission["label"]), None)
        require(isinstance(row, dict), f"private queue lost ordinary task {task}")
        validate_ordinary_row(
            row, admission["label"], task,
            admission["numeric_task_id"], "default")
        expected_result = admission["expected_result"]
        require(row_state(row) == "Done" and
                (not allow_failure or done_result(row) is not None) and
                (allow_failure or done_result(row) == expected_result),
                f"ordinary task is not in an acceptable terminal state: {task}")
    return rows


def _validate_inspection_membership_rows(
        *, allow_failure: bool,
        guardian_journals: list[dict[str, object]],
        guardian_unlaunched_journals: list[dict[str, object]],
        historical_guardian_journals: list[dict[str, object]],
        historical_guardian_unlaunched_journals: list[dict[str, object]],
        historical_queue_journals: list[dict[str, object]],
        queue_journals: list[dict[str, object]],
        rows: list[dict[str, object]],
        validate_inspection_row: Callable[..., None],
) -> None:
    for journal in queue_journals:
        row = next((candidate for candidate in rows if candidate.get(
            "label",
        ) == journal["label"]), None)
        require(isinstance(row, dict), f"private queue lost inspection task {journal['task_id']}")
        validate_inspection_row(row, journal, allow_failure)
    for journal in guardian_journals:
        row = next((candidate for candidate in rows if candidate.get(
            "label",
        ) == journal["label"]), None)
        if row is not None:
            validate_inspection_row(
                row, journal, allow_failure, terminal_required=False)
    for journal in historical_queue_journals:
        row = next((candidate for candidate in rows if candidate.get(
            "label",
        ) == journal["label"]), None)
        require(isinstance(row, dict),
                f"private queue lost historical inspection task {journal['task_id']}")
        validate_inspection_row(row, journal, allow_failure)
    for journal in historical_guardian_journals:
        row = next((candidate for candidate in rows if candidate.get(
            "label",
        ) == journal["label"]), None)
        if row is not None:
            validate_inspection_row(
                row, journal, allow_failure, terminal_required=False)
    for journal in guardian_unlaunched_journals + historical_guardian_unlaunched_journals:
        row = next((candidate for candidate in rows if candidate.get(
            "label",
        ) == journal["label"]), None)
        if row is not None:
            validate_inspection_row(
                row, journal, allow_failure, terminal_required=False)
