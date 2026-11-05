"""Workspace response, task verdict, cleanup, and capacity proof rules."""

from __future__ import annotations

import hashlib
import re
from typing import Mapping

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from .model import SHA256_RE, TASK_ID_RE



def _status_value(response: Mapping[str, object]) -> str:
    task, job = response.get("task"), response.get("job")
    for candidate in (
        response.get("status"),
        response.get("state"),
        task.get("status") if isinstance(task, dict) else None,
        job.get("status") if isinstance(job, dict) else None,
    ):
        if isinstance(candidate, str):
            return candidate.lower()
        if isinstance(candidate, dict):
            for key in ("state", "status", "result"):
                nested = candidate.get(key)
                if isinstance(nested, str):
                    return nested.lower()
    return ""


def classify_control_response(exit_code: int, response: Mapping[str, object]) -> str:
    """Classify the public exit-3 protocol without treating it as success."""
    workspace = response.get("workspace")
    if not isinstance(workspace, dict):
        return "ready" if exit_code == 0 else "failed"
    if workspace.get("reason_code") == "requested_action_not_authorized":
        if (exit_code == 3
                and workspace.get("next_action") in {"retry_exec", "retry_release", "retry_forget"}
                and workspace.get("operation_id") in {None, ""}
                and workspace.get("operation") is None):
            return "not-authorized"
        return "failed"
    pending = (
        workspace.get("next_action") == "watch_workspace"
        or workspace.get("state") in {"provisioning", "busy", "cleanup_pending", "forgetting"}
        or (isinstance(workspace.get("operation"), dict)
            and workspace["operation"].get("status") == "pending")
    )
    if exit_code == 3 and pending:
        return "pending"
    if exit_code == 0 and not pending:
        return "ready"
    return "failed"


def classify_task_verdict(
    task_status: Mapping[str, object],
    outcome: dict[str, object] | None,
    scenario: str,
    *,
    lifetime_closed: bool,
    capture_status: object = None,
) -> dict[str, object]:
    """Apply provider-authority predicates separately from workspace evidence."""
    verdict = outcome.get("verdict") if outcome is not None else None
    if not lifetime_closed:
        return {"verdict": "failed", "reason": "native-lifetime-not-closed"}
    if scenario == "success":
        if not isinstance(verdict, str):
            return {"verdict": "failed", "reason": "outcome-verdict-missing"}
        if verdict != "committed":
            return {"verdict": "failed", "reason": "provider-outcome-not-committed"}
        if capture_status != "captured":
            return {"verdict": "failed", "reason": "workspace-capture-not-captured"}
        return {"verdict": "passed", "reason": "provider-outcome-committed"}
    if scenario == "timeout":
        if _status_value(task_status) != "timed_out":
            return {"verdict": "failed", "reason": "task-was-not-authoritatively-timed-out"}
        if outcome is None:
            continuation = task_status.get("continuation")
            if not (isinstance(continuation, dict)
                    and continuation.get("resumable") is True
                    and continuation.get("mode") == "native"
                    and _task_id_matches_continuation(task_status, continuation)):
                return {"verdict": "failed", "reason": "timeout-continuation-not-proven"}
        elif verdict not in {"committed", "rejected"}:
            return {"verdict": "failed", "reason": "provider-outcome-unsealed"}
        if capture_status != "captured":
            return {"verdict": "failed", "reason": "timeout-capture-not-captured"}
        return {
            "verdict": "passed",
            "reason": ("native-timeout-and-capture-proven-without-provider-publication"
                       if outcome is None else
                       "provider-outcome-retained-and-timeout-separated-from-capture"),
        }
    return {"verdict": "failed", "reason": "unknown-task-scenario"}


def _task_id_matches_continuation(
    status: Mapping[str, object], continuation: Mapping[str, object],
) -> bool:
    task_id = status.get("task_id")
    return (isinstance(task_id, str) and TASK_ID_RE.fullmatch(task_id) is not None
            and continuation.get("predecessor_task_id") == task_id)


def cleanup_decision(
    verdict: str,
    *,
    lifetime_closed: bool,
    daemon_closed: bool,
    task_count: int,
    pending_process_count: int,
) -> tuple[bool, str]:
    """Return whether exact owned-directory cleanup is authorized."""
    if pending_process_count:
        return False, "process-observation-pending"
    if verdict == "passed" and lifetime_closed and daemon_closed:
        return True, "passed-with-closed-managed-lifetimes"
    if verdict == "BLOCKED" and task_count == 0 and daemon_closed:
        return True, "blocked-before-task-or-daemon"
    if not lifetime_closed:
        return False, "managed-lifetime-closure-unproven"
    if not daemon_closed:
        return False, "private-daemon-closure-unproven"
    return False, "failure-evidence-retained"


_FACT_TOKEN_RE = re.compile(r"^[a-z][a-z0-9_]{0,63}$")


def _fact(value: object, pattern: re.Pattern[str]) -> str:
    """Render an absent value, or a public-response value only if it is a bounded identifier."""
    if value is None:
        return "none"
    return value if isinstance(value, str) and pattern.fullmatch(value) else "invalid"


def _validate_hex(value: object, pattern: re.Pattern[str], label: str) -> str:
    if not (isinstance(value, str) and pattern.fullmatch(value) is not None):
        raise AcceptanceFailure(label + " is invalid")
    return value


def _contains_depth_limit(value: object) -> bool:
    if value == "<depth-limit>":
        return True
    if isinstance(value, dict):
        return any(_contains_depth_limit(item) for item in value.values())
    if isinstance(value, (list, tuple)):
        return any(_contains_depth_limit(item) for item in value)
    return False


def _capacity_records(records: object, label: str) -> tuple[list[str], list[dict[str, object]]]:
    if not (isinstance(records, list)):
        raise AcceptanceFailure(label + " must be a list")
    ids: list[str] = []
    validated: list[dict[str, object]] = []
    for record in records:
        if not (isinstance(record, dict)):
            raise AcceptanceFailure(label + " contains a non-object record")
        task_id = record.get("task_id")
        if not (isinstance(task_id, str) and TASK_ID_RE.fullmatch(task_id) is not None):
            raise AcceptanceFailure(label + " contains an invalid task ID")
        ids.append(task_id)
        validated.append(record)
    require(len(ids) == len(set(ids)), label + " contains duplicate task IDs")
    return ids, validated


def _capacity_record_ids(records: object, label: str) -> list[str]:
    return _capacity_records(records, label)[0]


def _validate_capacity_task_closures(
    task_ids: list[str], closure: Mapping[str, object],
) -> None:
    tasks = closure.get("tasks")
    if not (isinstance(tasks, dict)):
        raise AcceptanceFailure("capacity closure omitted task records")
    require(set(tasks) == set(task_ids), "capacity closure task set is incomplete")
    root_id = closure.get("root_id")
    if not (isinstance(root_id, str) and TASK_ID_RE.fullmatch(root_id) is not None):
        raise AcceptanceFailure("capacity closure omitted root identity")
    for task_id in task_ids:
        record = tasks.get(task_id)
        if not (isinstance(record, dict)):
            raise AcceptanceFailure("capacity task closure is incomplete: " + task_id)
        _validate_lifetime_records(task_id, root_id, record, "capacity task")
    workspace = closure.get("workspace")
    if not (isinstance(workspace, dict) and bool(workspace)):
        raise AcceptanceFailure("capacity closure omitted workspace operation records")
    for operation_id, operation in workspace.items():
        if not (isinstance(operation_id, str) and TASK_ID_RE.fullmatch(operation_id) is not None
                and isinstance(operation, dict)):
            raise AcceptanceFailure("capacity workspace operation closure is malformed")
        mode = operation.get("mode")
        require(mode in {"terminated", "recovered", "unsubmitted"},
                "capacity workspace operation closure is incomplete")
        if mode in {"terminated", "recovered"}:
            lifetime = operation.get("managed_lifetime")
            if not (isinstance(lifetime, dict)):
                raise AcceptanceFailure("capacity workspace operation omitted native closure")
            _validate_lifetime_records(operation_id, root_id, lifetime, "capacity workspace")
    require(not _contains_depth_limit(closure), "capacity closure contains compacted evidence")


def _validate_lifetime_records(
    identity: str, root_id: str, records: Mapping[str, object], label: str,
) -> None:
    validated: dict[str, dict[str, object]] = {}
    for key in ("intent", "registration", "worker", "worker_wait", "termination"):
        record = records.get(key)
        if not (isinstance(record, dict)):
            raise AcceptanceFailure(label + " closure omitted " + key + ": " + identity)
        validated[key] = record
    launches = records.get("launches")
    if not (isinstance(launches, list) and bool(launches)):
        raise AcceptanceFailure(label + " closure omitted launch evidence: " + identity)
    intent = validated["intent"]
    registration = validated["registration"]
    termination = validated["termination"]
    require(intent.get("task_id") == identity and intent.get("root_id") == root_id
            and registration.get("task_id") == identity
            and registration.get("root_id") == root_id
            and termination.get("task_id") == identity
            and termination.get("root_id") == root_id,
            label + " closure identity changed: " + identity)


def _validate_capacity_overlap(
    task_ids: list[str], lease_proof: Mapping[str, object], overlap: Mapping[str, object],
) -> None:
    lease_ids, lease_records = _capacity_records(lease_proof.get("counted_tasks"), "capacity lease proof")
    require(lease_ids == task_ids, "capacity lease proof task set is incomplete")
    require(lease_proof.get("counted_task_count") == len(task_ids),
            "capacity lease proof count is incomplete")
    require(lease_proof.get("lease_probe") == "exclusive-flock-contended",
            "capacity lease proof lacks owner-lock contention")
    closure_start = lease_proof.get("started_unix_ns")
    closure_end = lease_proof.get("ended_unix_ns")
    closure_duration = lease_proof.get("duration_ns")
    if not (type(closure_start) is int and type(closure_end) is int
            and closure_start > 0 and closure_end > closure_start
            and type(closure_duration) is int and closure_duration > 0):
        raise AcceptanceFailure("capacity owner proof omitted its observation interval")
    scopes: set[tuple[str, str]] = set()
    for record in lease_records:
        require(record.get("root_id") == lease_records[0].get("root_id"),
                "capacity lease proof mixed root identities")
        owner_lock = record.get("owner_lock")
        if not (isinstance(owner_lock, dict)
                and type(owner_lock.get("device")) is int
                and type(owner_lock.get("inode")) is int
                and owner_lock["inode"] > 0):
            raise AcceptanceFailure("capacity lease proof omitted owner-lock identity")
        for field in ("owner_sha256", "registration_sha256", "scope_plan_sha256", "scope_id_sha256"):
            value = record.get(field)
            if not (isinstance(value, str)
                    and SHA256_RE.fullmatch(value) is not None):
                raise AcceptanceFailure("capacity lease proof omitted " + field)
        scope_kind = record.get("scope_kind")
        assurance = record.get("assurance")
        if not (isinstance(scope_kind, str) and isinstance(assurance, str) and (scope_kind, assurance) in {
            ("cgroup-v2", "exact"), ("process-group", "cooperative"),
        }):
            raise AcceptanceFailure("capacity lease proof omitted native scope assurance")
        scopes.add((scope_kind, assurance))
    require(len(scopes) == 1, "capacity lease proof mixed native scope assurance")
    member_ids, members = _capacity_records(overlap.get("live_lease_members"), "capacity live lease cohort")
    require(member_ids == task_ids, "capacity live lease cohort is incomplete")
    pids = [member.get("pid") for member in members]
    if not (all(type(pid) is int and pid > 0 for pid in pids)
            and len(pids) == len(set(pids))):
        raise AcceptanceFailure("capacity live lease identities are incomplete")
    require(overlap.get("task_ids") == task_ids
            and overlap.get("lease_cohort_asserted_live_before_and_after") is True,
            "capacity shared interval lacks live cohort assertions")
    require(overlap.get("owner_lock_probe_task_ids") == task_ids,
            "capacity overlap omitted task owner-lock probes")
    started = overlap.get("started_unix_ns")
    ended = overlap.get("ended_unix_ns")
    duration = overlap.get("duration_ns")
    if not (type(started) is int and type(ended) is int and ended > started
            and type(duration) is int and duration > 0
            and started <= closure_start <= closure_end <= ended):
        raise AcceptanceFailure("capacity shared live-overlap interval is incomplete")
    require(not _contains_depth_limit(overlap), "capacity overlap contains compacted evidence")


def validate_capacity_lifetime_proof(
    task_ids: list[str], lease_proof: Mapping[str, object], overlap: Mapping[str, object],
    closure: Mapping[str, object], settled: list[dict[str, object]],
    released: list[dict[str, object]],
) -> None:
    """Reject a capacity receipt unless every task and shared interval is retained."""
    expected = sorted(task_ids)
    require(expected and len(expected) == len(set(expected)),
            "capacity proof task identities are empty or duplicated")
    validate_capacity_overlap_proof(expected, lease_proof, overlap)
    validate_capacity_task_closure_records(expected, closure)
    _validate_capacity_owner_bindings(lease_proof, closure)
    settled_ids = _capacity_record_ids(settled, "capacity settled tasks")
    require(sorted(settled_ids) == expected,
            "capacity settled task set is incomplete")
    if not (all(isinstance(item.get("status"), str) and item.get("status")
                and isinstance(item.get("collection_status"), str)
                and item.get("collection_status") for item in settled)):
        raise AcceptanceFailure("capacity settled task evidence is incomplete")
    released_ids = _capacity_record_ids(released, "capacity released workspaces")
    require(sorted(released_ids) == expected,
            "capacity released workspace set is incomplete")
    for item in released:
        release = item.get("release")
        forget = item.get("forget")
        if not (isinstance(release, dict) and isinstance(forget, dict)
                and forget.get("forget_state") == "forgotten"):
            raise AcceptanceFailure("capacity workspace release evidence is incomplete")


def _validate_capacity_owner_bindings(
    lease_proof: Mapping[str, object], closure: Mapping[str, object],
) -> None:
    lease_records = lease_proof.get("counted_tasks")
    tasks = closure.get("tasks")
    if not (isinstance(lease_records, list) and isinstance(tasks, dict)):
        raise AcceptanceFailure("capacity owner binding evidence is incomplete")
    require(lease_records and lease_records[0].get("root_id") == closure.get("root_id"),
            "capacity owner proof root does not match closure")
    for owner in lease_records:
        task_id = owner["task_id"]
        task = tasks[task_id]
        intent = task["intent"]
        registration = task["registration"]
        worker = task["worker"]
        scope_plan = intent.get("scope_plan")
        scope_id = registration.get("scope_id")
        if not (owner.get("owner_sha256") == intent.get("owner_sha256")
                and owner.get("owner_sha256") == registration.get("owner_sha256")
                and owner.get("registration_sha256") == worker.get("registration_sha256")
                and owner.get("scope_kind") == registration.get("scope_kind")
                and owner.get("assurance") == registration.get("assurance")
                and isinstance(scope_plan, str) and scope_plan
                and owner.get("scope_plan_sha256") == hashlib.sha256(
                    scope_plan.encode("utf-8")).hexdigest()
                and isinstance(scope_id, str) and scope_id
                and owner.get("scope_id_sha256") == hashlib.sha256(
                    scope_id.encode("utf-8")).hexdigest()):
            raise AcceptanceFailure("capacity live owner differs from its closed task proof: " + task_id)


def validate_capacity_overlap_proof(
    task_ids: list[str], lease_proof: Mapping[str, object], overlap: Mapping[str, object],
) -> None:
    """Validate simultaneous native lease and per-task owner-lock observations."""
    expected = sorted(task_ids)
    require(expected and len(expected) == len(set(expected)),
            "capacity overlap identities are empty or duplicated")
    _validate_capacity_overlap(expected, lease_proof, overlap)


def validate_capacity_task_closure_records(
    task_ids: list[str], closure: Mapping[str, object],
) -> None:
    """Require canonical closure evidence for every counted task and operation."""
    expected = sorted(task_ids)
    require(expected and len(expected) == len(set(expected)),
            "capacity closure identities are empty or duplicated")
    _validate_capacity_task_closures(expected, closure)
