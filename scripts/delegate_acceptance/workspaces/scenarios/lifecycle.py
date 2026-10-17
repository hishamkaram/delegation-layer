"""Successful managed-workspace lifecycle scenario."""

from __future__ import annotations

from dataclasses import dataclass

from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.directories.policy import require
from .context import WorkspaceContext
from .model import SUCCESS_TASK_BUDGET, WAIT_SECONDS
from .operations import (
    _adopt_workspace_container, _await_task, _collect_task, _dispatch,
    _prove_lifetimes_closed, _provider_session, _wait_capture, _workspace_from,
)
from .policy import classify_task_verdict
from .workspace_operations import (
    _capture_roundtrip, _forget_workspace, _release_and_forget, _verify_ref, _workspace_exec,
    verify_capture_receipt,
)



@dataclass(frozen=True)
class SuccessfulTaskEvidence:
    task: dict[str, object]
    status: dict[str, object]
    workspace: dict[str, object]
    capture: dict[str, object]
    outcome: dict[str, object]
    closure: dict[str, object]
    session: dict[str, object]
    capture_evidence: dict[str, object]


def _run_first_success(context: WorkspaceContext) -> SuccessfulTaskEvidence:
    task = _dispatch(context, context.brief_success, task_budget=SUCCESS_TASK_BUDGET, fresh=True)
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    status = _await_task(context, task, WAIT_SECONDS)
    _collect_task(context, task, WAIT_SECONDS)
    workspace, capture = _wait_capture(context, task)
    outcome = read_json(context.state / "tasks" / task["task_id"] / "outcome.json")
    closure = _prove_lifetimes_closed(context, [task["task_id"]])
    classification = classify_task_verdict(
        status, outcome, "success", lifetime_closed=bool(closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    session = _provider_session(context, task["task_id"])
    capture_evidence = _capture_roundtrip(context, capture)
    subject = capture_evidence["subject"]
    require(isinstance(subject, dict) and subject.get("id") == task["task_id"],
            "first task capture is bound to a different subject")
    return SuccessfulTaskEvidence(
        task, status, workspace, capture, outcome, closure, session, capture_evidence,
    )


def _run_success(context: WorkspaceContext) -> dict[str, object]:
    first = _run_first_success(context)
    ref = first.capture_evidence["snapshot_ref"]
    reused = _dispatch(
        context, context.brief_success_reuse, task_budget=SUCCESS_TASK_BUDGET,
        fresh=False, workspace_id=first.task["workspace_id"],
    )
    _adopt_workspace_container(context, reused["root_id"], reused["workspace_id"])
    reused_status = _await_task(context, reused, WAIT_SECONDS)
    reused_collect = _collect_task(context, reused, WAIT_SECONDS)
    reused_workspace, reused_capture = _wait_capture(context, reused)
    reused_outcome = read_json(context.state / "tasks" / reused["task_id"] / "outcome.json")
    reused_capture_evidence = verify_capture_receipt(context.state, reused_capture)
    reused_subject = reused_capture_evidence["subject"]
    require(isinstance(reused_subject, dict) and reused_subject.get("id") == reused["task_id"],
            "reused task capture is bound to a different subject")
    reused_closure = _prove_lifetimes_closed(context, [reused["task_id"]])
    reused_capture_evidence = _capture_roundtrip(
        context, reused_capture, label="reused-capture-roundtrip",
    )
    reused_session = _provider_session(context, reused["task_id"])
    all_closure = _prove_lifetimes_closed(
        context, [first.task["task_id"], reused["task_id"]],
    )
    reused_classification = classify_task_verdict(
        reused_status, reused_outcome, "success", lifetime_closed=bool(reused_closure),
        capture_status=reused_capture.get("status"),
    )
    require(reused_classification["verdict"] == "passed", str(reused_classification))
    exec_evidence = _workspace_exec(context, first.task)
    _verify_ref(context, ref, "capture-ref-before-release", (0,))
    reused_ref = reused_capture_evidence["snapshot_ref"]
    _verify_ref(context, reused_ref, "reused-capture-ref-before-release", (0,))
    release_evidence = _release_and_forget(context, first.task)
    _verify_ref(context, ref, "capture-ref-after-release", (0,))
    _verify_ref(context, reused_ref, "reused-capture-ref-after-release", (0,))
    forget_evidence = _forget_workspace(context, first.task)
    after_forget = _verify_ref(context, ref, "capture-ref-after-forget", (1,))
    require(after_forget == 1, "forget did not remove the capture ref")
    require(
        _verify_ref(context, reused_ref, "reused-capture-ref-after-forget", (1,)) == 1,
        "forget did not remove the reused capture ref",
    )
    return {
        "task_ids": [first.task["task_id"], reused["task_id"]],
        "workspace_id": first.task["workspace_id"],
        "provider_outcome_authority": first.outcome.get("verdict"),
        "lifetime_closure": first.closure,
        "all_lifetime_closure": all_closure,
        "provider_session": first.session,
        "capture": first.capture_evidence,
        "reused_capture": reused_capture_evidence,
        "reused_lifetime_closure": reused_closure,
        "reused_provider_session": reused_session,
        "source_untouched_before_cleanup": True,
        "workspace_exec": exec_evidence,
        "capture_ref_after_release": "resolves",
        "capture_ref_after_forget": "absent",
        "release": release_evidence,
        "forget": forget_evidence,
        "reused_workspace_dispatch": {
            "task_id": reused["task_id"],
            "outcome": reused_outcome.get("verdict"),
            "collection": reused_collect.get("status"),
            "capture": _workspace_from(reused_workspace).get("state"),
        },
        "workspace_response_state": _workspace_from(first.workspace).get("state"),
    }
