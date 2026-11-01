"""One real-minute success/failure/restart flow through public history commands."""

from pathlib import Path

from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.private_cli.scheduler_cases import SchedulerCase
from scripts.delegate_acceptance.private_cli.scheduler_occurrences import (
    MINUTE_NS, Occurrence, json_object, require_occurrences, timestamp_ns,
)
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.tasks.publication import task_snapshot, verify_collected_outcome


def history_occurrences(response: dict[str, object]) -> tuple[dict[str, object], ...]:
    history = json_object(response.get("schedule_history"))
    entries = history.get("entries")
    if not isinstance(entries, list):
        raise RuntimeError("public history has no entries array")
    occurrences = []
    for item in entries:
        row = json_object(item)
        if row.get("kind") == "occurrence":
            occurrences.append(json_object(row.get("occurrence")))
        elif row.get("kind") == "skip":
            skipped = json_object(row.get("skip"))
            require(timestamp_ns(skipped.get("from")) < timestamp_ns(skipped.get("to")),
                    "public history returned an invalid skip interval")
            require(skipped.get("reason") in ("overlap_active", "blocked_uncertain", "missed_unavailable",
                                              "missed_capacity", "paused"), "public history returned an unknown skip reason")
        else:
            raise RuntimeError("public history returned an unknown entry kind")
    return tuple(occurrences)


def execution_evidence(response: dict[str, object], row: Occurrence) -> dict[str, object]:
    execution = json_object(response.get("schedule_execution"))
    occurrence = json_object(execution.get("occurrence"))
    require(occurrence.get("task_id") == row.task_id and occurrence.get("schedule_id") == row.schedule_id,
            "public execution identity changed")
    require(execution.get("complete") is True, "public execution inspection is incomplete")
    evidence = json_object(execution.get("local_evidence"))
    require(evidence.get("provenance") == "validated_local", "execution evidence lost its provenance")
    return evidence


def require_public_result(case: SchedulerCase, row: Occurrence, verdict: str) -> None:
    response = case.command("execution-" + row.task_id, ["schedule", "execution", row.task_id])
    evidence = execution_evidence(response, row)
    require(evidence.get("publication") == verdict and evidence.get("verdict") == verdict,
            "public execution returned the wrong publication")
    outcome = json_object(read_json(case.fixture.state / "tasks" / row.task_id / "outcome.json"))
    require(evidence.get("payload") == outcome.get("payload"), "public execution payload descriptor changed")
    require_public_logs(evidence, case.fixture.state / "tasks" / row.task_id)


def require_public_logs(evidence: dict[str, object], directory: Path) -> None:
    raw = evidence.get("raw")
    manifest = json_object(read_json(directory / "provider.exit")).get("raw_manifest")
    if not isinstance(raw, list) or not isinstance(manifest, list):
        raise RuntimeError("public execution omitted log descriptors")
    require(len(raw) == len(manifest) and len(raw) >= 2, "public execution changed sealed log inventory")
    expected = {}
    for item in manifest:
        entry = json_object(item)
        relative = entry.get("path")
        if not isinstance(relative, str) or Path(relative).is_absolute() or ".." in Path(relative).parts:
            raise RuntimeError("sealed log path is unsafe")
        expected[directory / relative] = entry
    actual = set()
    for descriptor in raw:
        stream = json_object(descriptor)
        location = stream.get("path")
        if not isinstance(location, str):
            raise RuntimeError("public log path is absent")
        path = Path(location)
        require(path in expected and path not in actual, "public execution changed a sealed log reference")
        actual.add(path)
        require(stream.get("sealed") is True and stream.get("available") is True,
                "public execution presented unavailable or unsealed output as final")
        require(stream.get("size") == expected[path].get("size") == path.stat().st_size
                and stream.get("sha256") == expected[path].get("sha256") == digest(path),
                "public execution log descriptor does not match its bytes")


def await_release(case: SchedulerCase, row: Occurrence) -> None:
    def released() -> bool:
        response = case.command("history-release", ["schedule", "history", case.schedule_id])
        history = history_occurrences(response)
        matching = [item for item in history if item.get("task_id") == row.task_id]
        require(len(matching) == 1, "public history lost or duplicated an occurrence")
        return matching[0].get("reservation") == "released"

    wait_until(released, timeout=case.remaining(), description="public reservation release")


def collect_failed(case: SchedulerCase, row: Occurrence) -> None:
    directory = case.fixture.state / "tasks" / row.task_id
    wait_until(lambda: (directory / "provider.exit").is_file(), timeout=case.remaining(),
               description="controlled provider exit seal")

    def collected():
        result = case.command("collect-failed", ["collect", row.task_id, "--watch", "2s"], (0, 3, 4))
        require(result.get("root_id") == case.root_id and result.get("task_id") == row.task_id,
                "failed collection identity changed")
        return result if result.get("publication") == "rejected" else None

    result = wait_until(collected, timeout=case.remaining(), description="controlled failure publication")
    verify_collected_outcome(result, directory, "rejected")
    seal = json_object(read_json(directory / "provider.exit"))
    require(seal.get("invocation_state") == "started" and seal.get("exit_code") == 17,
            "controlled provider failure was replaced with another failure")
    await_managed_lifetime_closure(case.fixture.state, (row.task_id,), timeout=min(30, case.remaining()))
    case.receipt.increment("collected_task_count")


def failure_marker(case: SchedulerCase) -> Path:
    if case.lifetime.gate is None:
        raise RuntimeError("scheduler controlled fixture lost its gate path")
    return case.lifetime.gate.with_name(case.lifetime.gate.name + ".scheduler-fail")


def require_overlap_history(case: SchedulerCase) -> None:
    response = case.command("history-overlap-final", ["schedule", "history", case.schedule_id])
    history = json_object(response.get("schedule_history"))
    require(history.get("schedule_id") == case.schedule_id, "overlap history changed schedule identity")
    occurrences = history_occurrences(response)
    require(len(occurrences) == 1 and timestamp_ns(occurrences[0].get("due")) == case.first_due_ns,
            "overlap history changed its admitted minute inventory")
    entries = history.get("entries")
    if not isinstance(entries, list):
        raise RuntimeError("overlap history has no entries array")
    due = case.first_due_ns + MINUTE_NS
    for entry in entries:
        row = json_object(entry)
        if row.get("kind") == "skip":
            skipped = json_object(row.get("skip"))
            # contract.SkipSpan owns (From, To] coverage semantics.
            if (skipped.get("reason") == "overlap_active"
                    and timestamp_ns(skipped.get("from")) < due <= timestamp_ns(skipped.get("to"))):
                return
    raise RuntimeError("public history omitted the actual overlap skip")


def run_successive(case: SchedulerCase) -> None:
    case.create(held=False, controlled_failure=True)
    first, = case.occurrences(1)
    require_occurrences((first,), case.schedule_id, (case.first_due_ns,))
    case.collect(first)
    await_release(case, first)
    require_public_result(case, first, "committed")
    first_snapshot = task_snapshot(case.fixture.state, first.task_id)

    marker = failure_marker(case)
    with marker.open("xb") as stream:
        stream.write(b"fail the next scheduled fixture invocation\n")
    marker.chmod(0o600)
    rows = case.occurrences(2)
    require_occurrences(rows, case.schedule_id, (case.first_due_ns, case.first_due_ns + MINUTE_NS))
    failed = rows[1]
    collect_failed(case, failed)
    await_release(case, failed)
    require_public_result(case, failed, "rejected")
    failed_snapshot = task_snapshot(case.fixture.state, failed.task_id)
    marker.unlink()

    case.receipt.phase("restart_scheduler_after_failure")
    case.restart()
    rows = case.occurrences(3)
    require_occurrences(rows, case.schedule_id, tuple(case.first_due_ns + i * MINUTE_NS for i in range(3)))
    case.collect(rows[2])
    await_release(case, rows[2])
    case.service.stop()
    require_public_result(case, rows[2], "committed")
    require(task_snapshot(case.fixture.state, first.task_id) == first_snapshot,
            "restart changed the first task's immutable evidence")
    require(task_snapshot(case.fixture.state, failed.task_id) == failed_snapshot,
            "restart changed the failed task's immutable evidence")
    public = history_occurrences(case.command("history-final", ["schedule", "history", case.schedule_id]))
    require(tuple(item.get("task_id") for item in public) == tuple(row.task_id for row in reversed(rows)),
            "public history lost descending independent occurrence identities")
    require(tuple(timestamp_ns(item.get("due")) for item in public) == tuple(row.due_ns for row in reversed(rows)),
            "public history changed the real-minute occurrence times")
    case.receipt.add_details(actual_minute_occurrences=3, successful_occurrences=2,
                             controlled_failure_task=failed.task_id, resumed_after_restart=True,
                             independent_task_ids=[row.task_id for row in rows])
