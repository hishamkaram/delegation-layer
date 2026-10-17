"""Finite timeout, native closure, and exact continuation scenario."""

from __future__ import annotations

from dataclasses import dataclass
import time

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
import scripts.delegate_acceptance.private_cli.admission as private_cli_admission
import scripts.delegate_acceptance.private_cli.stop_policy as private_cli_stop_policy
import scripts.delegate_acceptance.private_cli.stop as private_cli_stop
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.directories.policy import require
from .context import WorkspaceContext
from .model import (
    TIMEOUT_CONTINUE_TASK_BUDGET, TIMEOUT_TASK_BUDGET, TIMEOUT_WAIT_SECONDS, WAIT_SECONDS,
)
from .operations import (
    _adopt_workspace_container, _assert_immutable_task_snapshot, _await_task, _collect_task,
    _continue, _dispatch, _fixture_session_marker, _immutable_task_snapshot,
    _prove_lifetimes_closed, _provider_session, _validate_continuation_binding,
    _wait_capture, _workspace_from,
)
from .policy import (
    _status_value, classify_task_verdict,
)
from .workspace_operations import (
    _capture_roundtrip, _finish_cohort, _forget_workspace, _release_and_forget, _verify_ref,
    verify_capture_receipt,
)



def _wait_for_timeout(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    deadline = time.monotonic() + TIMEOUT_WAIT_SECONDS
    latest: dict[str, object] = {}
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("fixture task did not reach timed_out status")
        receipt = context.command_receipt(task["task_id"], "timeout-status")
        latest = private_cli_admission.read_created_task_status(
            context.delegate,
            context.state,
            context.environment,
            task["task_id"],
            receipt,
            phase="timeout-observation",
            timeout=min(10, remaining),
            capture_root=context.capture_root,
        )
        if _status_value(latest) == "timed_out":
            return latest
        if _status_value(latest) in {"done", "finished", "completed"}:
            raise RuntimeError("timeout fixture ended before authoritative timeout")
        time.sleep(min(0.1, remaining))


@dataclass(frozen=True)
class TimeoutPredecessor:
    task: dict[str, object]
    status: dict[str, object]
    outcome: dict[str, object] | None
    collected: dict[str, object] | None
    closure: dict[str, object]
    lease_specification: dict[str, object]
    lease_observation: dict[str, object]
    lease_cleanup: list[dict[str, object]]
    capture: dict[str, object]
    session: dict[str, object]
    marker: dict[str, object]
    capture_evidence: dict[str, object]
    subject: dict[str, object]
    snapshot: dict[str, object]


@dataclass(frozen=True)
class TimeoutContinuation:
    task: dict[str, object]
    status: dict[str, object]
    collected: dict[str, object]
    workspace: dict[str, object]
    outcome: dict[str, object]
    capture: dict[str, object]
    closure: dict[str, object]
    binding: dict[str, object]
    session: dict[str, object]
    marker: dict[str, object]
    capture_evidence: dict[str, object]
    subject: dict[str, object]


def _start_timeout(context: WorkspaceContext) -> TimeoutPredecessor:
    gate = private_cli_leases.LockedReleaseGate.acquire(context.gate_path)
    context.gates.append(gate)
    task = _dispatch(context, context.brief_timeout, task_budget=TIMEOUT_TASK_BUDGET, fresh=True)
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    specification, cohort, lease_observation = private_cli_stop.default_stop_live_lease(
        context.pueue, context.private_config, context.environment, context.state, task,
        context.waiter_directory, context.lease_nonce, capture_root=context.capture_root,
    )
    context.cohorts.append(cohort)
    context.native_proofs.append({
        "scenario": "timeout",
        "scope_kind": specification["scope_kind"],
        "assurance": specification["assurance"],
        "counted_task_count": 1,
        "lease": private_cli_stop_policy.default_stop_lease_evidence(specification, lease_observation),
    })
    _wait_for_timeout(context, task)
    context.release_gates()
    closure = private_cli_supervisor.await_managed_lifetime_closure(
        context.state, (task["task_id"],), timeout=TIMEOUT_WAIT_SECONDS,
    )
    lease_cleanup = _finish_cohort(context, cohort)
    _workspace, capture = _wait_capture(context, task)
    status = _wait_for_timeout(context, task)
    outcome_path = context.state / "tasks" / task["task_id"] / "outcome.json"
    collected = (
        _collect_task(context, task, TIMEOUT_WAIT_SECONDS)
        if outcome_path.exists() or outcome_path.is_symlink() else None
    )
    outcome = collected.get("outcome") if collected is not None else None
    classification = classify_task_verdict(
        status, outcome, "timeout", lifetime_closed=bool(closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    session = _provider_session(context, task["task_id"])
    session_id = session.get("conversation_id")
    require(isinstance(session_id, str) and session_id,
            "timeout fixture session identity is unavailable")
    marker = _fixture_session_marker(context, resumed=False)
    capture_evidence = _capture_roundtrip(context, capture)
    subject = capture_evidence["subject"]
    require(isinstance(subject, dict) and subject.get("id") == task["task_id"],
            "timeout capture is bound to a different predecessor")
    snapshot = _immutable_task_snapshot(context, task["task_id"])
    return TimeoutPredecessor(
        task, status, outcome, collected, closure, specification, lease_observation,
        lease_cleanup, capture, session, marker, capture_evidence, subject, snapshot,
    )


def _continue_timeout(
    context: WorkspaceContext, predecessor: TimeoutPredecessor,
) -> TimeoutContinuation:
    task = _continue(
        context, predecessor.task, brief=context.brief_timeout_resume,
        task_budget=TIMEOUT_CONTINUE_TASK_BUDGET,
    )
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    status = _await_task(context, task, WAIT_SECONDS)
    collected = _collect_task(context, task, WAIT_SECONDS)
    workspace, capture = _wait_capture(context, task)
    outcome = read_json(context.state / "tasks" / task["task_id"] / "outcome.json")
    capture_evidence = verify_capture_receipt(context.state, capture)
    subject = capture_evidence["subject"]
    require(isinstance(subject, dict) and subject.get("id") == task["task_id"],
            "continuation capture is bound to a different successor")
    closure = _prove_lifetimes_closed(context, [task["task_id"]])
    capture_evidence = _capture_roundtrip(
        context, capture, label="timeout-continuation-capture-roundtrip",
    )
    classification = classify_task_verdict(
        status, outcome, "success", lifetime_closed=bool(closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    binding = _validate_continuation_binding(
        context, predecessor.task, task, str(predecessor.session["conversation_id"]),
    )
    _assert_immutable_task_snapshot(
        context, predecessor.task["task_id"], predecessor.snapshot,
    )
    session = _provider_session(context, task["task_id"])
    marker = _fixture_session_marker(context, resumed=True)
    require(predecessor.capture_evidence["operation_id"] != capture_evidence["operation_id"],
            "timeout predecessor and successor share a capture operation")
    require(predecessor.capture_evidence["operation_id"] != predecessor.task["task_id"]
            and capture_evidence["operation_id"] != task["task_id"],
            "capture operation was attributed to a task lifetime")
    return TimeoutContinuation(
        task, status, collected, workspace, outcome, capture, closure, binding,
        session, marker, capture_evidence, subject,
    )


def _finish_timeout(
    context: WorkspaceContext, predecessor: TimeoutPredecessor,
    continuation: TimeoutContinuation,
) -> dict[str, object]:
    capture_evidence = predecessor.capture_evidence
    continuation_capture = continuation.capture_evidence
    ref = capture_evidence["snapshot_ref"]
    _verify_ref(context, ref, "timeout-capture-ref-before-release", (0,))
    continuation_ref = continuation_capture["snapshot_ref"]
    _verify_ref(context, continuation_ref, "timeout-continuation-ref-before-release", (0,))
    release = _release_and_forget(context, continuation.task)
    _verify_ref(context, ref, "timeout-capture-ref-after-release", (0,))
    _verify_ref(context, continuation_ref, "timeout-continuation-ref-after-release", (0,))
    forget = _forget_workspace(context, continuation.task)
    require(_verify_ref(context, ref, "timeout-capture-ref-after-forget", (1,)) == 1,
            "timeout forget did not remove capture ref")
    require(_verify_ref(context, continuation_ref,
                        "timeout-continuation-ref-after-forget", (1,)) == 1,
            "timeout continuation forget did not remove capture ref")
    return {
        "task_id": predecessor.task["task_id"],
        "workspace_id": predecessor.task["workspace_id"],
        "continuation_task_id": continuation.task["task_id"],
        "task_status": predecessor.status.get("status"),
        "ended_status": predecessor.status.get("status"),
        "provider_outcome_authority": (predecessor.outcome.get("verdict")
                                        if predecessor.outcome is not None else None),
        "predecessor_publication": predecessor.status.get("publication"),
        "lifetime_closure": predecessor.closure,
        "native_session_proof": {
            "claim": "fixture-only",
            "predecessor": predecessor.session,
            "continuation": continuation.session,
            "marker_before_gate": predecessor.marker,
            "marker_after_continue": continuation.marker,
        },
        "capture": capture_evidence,
        "continuation_capture": continuation_capture,
        "continuation_lifetime_closure": continuation.closure,
        "continuation_binding": continuation.binding,
        "predecessor_outcome_immutable": True,
        "capture_scopes": {
            "predecessor": {
                "operation_id": capture_evidence["operation_id"],
                "subject_id": predecessor.subject.get("id"),
            },
            "continuation": {
                "operation_id": continuation_capture["operation_id"],
                "subject_id": continuation.subject.get("id"),
            },
        },
        "lease": private_cli_stop_policy.default_stop_lease_evidence(
            predecessor.lease_specification, predecessor.lease_observation,
        ),
        "lease_cleanup": predecessor.lease_cleanup,
        "capture_ref_after_release": "resolves",
        "capture_ref_after_forget": "absent",
        "release": release,
        "forget": forget,
        "continuation_workspace_state": _workspace_from(continuation.workspace).get("state"),
        "collection_status": (predecessor.collected.get("status")
                               if predecessor.collected is not None
                               else "not_requested_without_outcome"),
        "continuation_collection_status": continuation.collected.get("status"),
    }


def _run_timeout(context: WorkspaceContext) -> dict[str, object]:
    predecessor = _start_timeout(context)
    continuation = _continue_timeout(context, predecessor)
    return _finish_timeout(context, predecessor, continuation)
