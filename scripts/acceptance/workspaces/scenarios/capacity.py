"""Capacity dispatch, shared lease observation, and per-task closure proof."""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor, as_completed
import time

import scripts.acceptance.private_cli.leases as private_cli_leases
import scripts.acceptance.private_cli.capacity.ownership as private_cli_capacity_ownership
import scripts.acceptance.private_cli.capacity.records as private_cli_capacity_records
from scripts.acceptance.directories.policy import require
from scripts.acceptance.workspaces.scenarios.context import WorkspaceContext
from scripts.acceptance.workspaces.scenarios.model import TASK_ID_RE
from scripts.acceptance.workspaces.scenarios.model import WorkspaceTask
from scripts.acceptance.workspaces.scenarios.model import CAPACITY_TASK_BUDGET
from scripts.acceptance.workspaces.scenarios.model import WAIT_SECONDS
from scripts.acceptance.workspaces.scenarios.operations import _adopt_workspace_container
from scripts.acceptance.workspaces.scenarios.operations import _await_task
from scripts.acceptance.workspaces.scenarios.operations import _collect_task
from scripts.acceptance.workspaces.scenarios.operations import _dispatch
from scripts.acceptance.workspaces.scenarios.operations import _prove_lifetimes_closed
from scripts.acceptance.workspaces.scenarios.policy import _status_value
from scripts.acceptance.workspaces.scenarios.policy import _validate_hex
from scripts.acceptance.workspaces.scenarios.policy import validate_capacity_lifetime_proof
from scripts.acceptance.workspaces.scenarios.policy import validate_capacity_overlap_proof
from scripts.acceptance.workspaces.scenarios.policy import validate_capacity_task_closure_records
from scripts.acceptance.workspaces.scenarios.workspace_operations import _finish_cohort
from scripts.acceptance.workspaces.scenarios.workspace_operations import _forget_workspace
from scripts.acceptance.workspaces.scenarios.workspace_operations import _release_and_forget



def _dispatch_capacity_one(context: WorkspaceContext) -> WorkspaceTask:
    return _dispatch(
        context, context.brief_capacity, task_budget=CAPACITY_TASK_BUDGET, fresh=True,
    )


def _settle_capacity_one(context: WorkspaceContext, task: WorkspaceTask) -> dict[str, object]:
    status = _await_task(context, task, WAIT_SECONDS)
    collected = _collect_task(context, task, WAIT_SECONDS)
    require(_status_value(status) != "timed_out", "capacity task timed out")
    outcome = collected.get("outcome")
    if not (isinstance(outcome, dict) and outcome.get("verdict") == "committed"):
        raise RuntimeError("capacity task provider outcome was not committed")
    return {
        "task_id": task["task_id"],
        "workspace_id": task["workspace_id"],
        "status": status.get("status"),
        "collection_status": collected.get("status"),
    }


def _release_forget_capacity_one(context: WorkspaceContext, task: WorkspaceTask) -> dict[str, object]:
    release = _release_and_forget(context, task)
    forget = _forget_workspace(context, task)
    return {"task_id": task["task_id"], "release": release, "forget": forget}


def _dispatch_capacity(context: WorkspaceContext, count: int, workers: int) -> tuple[list[WorkspaceTask], float]:
    started = time.monotonic()
    tasks: list[WorkspaceTask] = []
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-dispatch") as pool:
        futures = [pool.submit(_dispatch_capacity_one, context) for _ in range(count)]
        for future in as_completed(futures):
            tasks.append(future.result())
    burst_seconds = max(0.0, time.monotonic() - started)
    tasks.sort(key=lambda item: item["task_id"])
    require(len(tasks) == count, "capacity dispatch did not return every task")
    for task in tasks:
        _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    return tasks, burst_seconds


def _observe_capacity_cohort(
    context: WorkspaceContext, tasks: list[WorkspaceTask], count: int,
) -> tuple[
    private_cli_leases.LiveFIFOLeaseCohort, dict[str, object], tuple[str, str], dict[str, object],
]:
    deadline = time.monotonic() + WAIT_SECONDS
    specifications: list[dict[str, object]] = []
    while len(specifications) < count:
        specifications = private_cli_capacity_ownership.lease_candidate_specifications(private_cli_capacity_records.capacity_lease_specifications(
            context.state, tasks, context.waiter_directory, context.lease_nonce,
        ))
        if len(specifications) == count:
            break
        if time.monotonic() >= deadline:
            raise TimeoutError("capacity cohort did not reach all native leases")
        time.sleep(0.1)
    cohort = private_cli_leases.LiveFIFOLeaseCohort.acquire(specifications, context.lease_nonce)
    context.cohorts.append(cohort)
    cohort.assert_live()
    started_unix_ns = time.time_ns()
    started_monotonic_ns = time.monotonic_ns()
    proof = private_cli_capacity_ownership.prove_lifetime_owner_cohort(specifications, cohort)
    cohort.assert_live()
    ended_monotonic_ns = time.monotonic_ns()
    ended_unix_ns = time.time_ns()
    counted_records = proof.get("counted_tasks")
    if not (isinstance(counted_records, list)
            and all(isinstance(item, dict) for item in counted_records)):
        raise RuntimeError("capacity owner proof omitted per-task lease records")
    scope_values: set[tuple[str, str]] = set()
    for item in specifications:
        kind, assurance = item["scope_kind"], item["assurance"]
        if not isinstance(kind, str) or not isinstance(assurance, str):
            raise RuntimeError("capacity cohort selected mixed native scope assurance")
        scope_values.add((kind, assurance))
    scope_pairs = sorted(scope_values)
    require(len(scope_pairs) == 1, "capacity cohort selected mixed native scope assurance")
    task_ids = sorted(task["task_id"] for task in tasks)
    overlap = {
        "started_unix_ns": started_unix_ns,
        "ended_unix_ns": ended_unix_ns,
        "duration_ns": max(0, ended_monotonic_ns - started_monotonic_ns),
        "task_ids": task_ids,
        "lease_cohort_asserted_live_before_and_after": True,
        "live_lease_members": [
            {"task_id": item["task_id"], "pid": item["pid"]}
            for item in sorted(specifications, key=lambda value: _validate_hex(value["task_id"], TASK_ID_RE, "capacity task ID"))
        ],
        "owner_lock_probe_task_ids": [item["task_id"] for item in counted_records],
    }
    return cohort, proof, scope_pairs[0], overlap


def _settle_capacity(
    context: WorkspaceContext, tasks: list[WorkspaceTask], workers: int,
) -> list[dict[str, object]]:
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-settle") as pool:
        futures = [pool.submit(_settle_capacity_one, context, task) for task in tasks]
        settled = [future.result() for future in as_completed(futures)]
    return sorted(settled, key=lambda item: _validate_hex(item["task_id"], TASK_ID_RE, "capacity task ID"))


def _release_capacity(
    context: WorkspaceContext, tasks: list[WorkspaceTask], workers: int,
) -> list[dict[str, object]]:
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-release") as pool:
        futures = [pool.submit(_release_forget_capacity_one, context, task) for task in tasks]
        released = [future.result() for future in as_completed(futures)]
    return sorted(released, key=lambda item: _validate_hex(item["task_id"], TASK_ID_RE, "capacity task ID"))


def _run_capacity(context: WorkspaceContext, count: int, workers: int) -> dict[str, object]:
    gate = private_cli_leases.LockedReleaseGate.acquire(context.gate_path)
    context.gates.append(gate)
    tasks, burst_seconds = _dispatch_capacity(context, count, workers)
    task_ids = [task["task_id"] for task in tasks]
    cohort, proof, scope_pair, overlap = _observe_capacity_cohort(context, tasks, count)
    validate_capacity_overlap_proof(task_ids, proof, overlap)
    capacity_evidence: dict[str, object] = {
        "scenario": "capacity",
        "backend": scope_pair[0],
        "assurance": scope_pair[1],
        "counted_task_count": proof["counted_task_count"],
        "lease_probe": proof["lease_probe"],
        "lifetime_owner_proof": proof,
        "shared_live_overlap": overlap,
    }
    context.native_proofs.append(capacity_evidence)
    context.release_gates()
    settled = _settle_capacity(context, tasks, workers)
    capacity_evidence["settled_tasks"] = settled
    capacity_closure = _prove_lifetimes_closed(context, task_ids)
    capacity_evidence["lifetime_closure"] = capacity_closure
    validate_capacity_task_closure_records(task_ids, capacity_closure)
    fifo_cleanup = _finish_cohort(context, cohort)
    capacity_evidence["fifo_cleanup"] = fifo_cleanup
    released = _release_capacity(context, tasks, workers)
    capacity_evidence["released_workspaces"] = released
    validate_capacity_lifetime_proof(task_ids, proof, overlap, capacity_closure, settled, released)
    return {
        "requested_task_count": count,
        "dispatch_workers": workers,
        "dispatch_burst_seconds": round(burst_seconds, 6),
        "dispatch_latency_target": "none",
        "counted_task_count": proof["counted_task_count"],
        "native_scope": {
            "backend": scope_pair[0],
            "assurance": scope_pair[1],
        },
        "capacity_lifetime_proof": {
            "counted_task_count": proof["counted_task_count"],
            "lease_probe": proof["lease_probe"],
            "lifetime_owner_proof": proof,
            "shared_live_overlap": overlap,
            "lifetime_closure": capacity_closure,
        },
        "settled_tasks": settled,
        "released_workspaces": released,
        "fifo_cleanup": fifo_cleanup,
        "settled_task_count": len(settled),
        "released_workspace_count": len(released),
    }
