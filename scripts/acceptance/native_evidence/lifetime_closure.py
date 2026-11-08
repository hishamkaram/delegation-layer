"""Pure contract for the compact native lifetime-closure proof of one case.

The native producer projects the canonical managed-lifetime scanner into this
compact form during success-only shutdown, before any owned state is removed.
The producer and the release validators apply this one positive rule, so a
PASS never rests on daemon shutdown alone.
"""

from __future__ import annotations

import re
from typing import Iterable

LIFETIME_CLOSURE_KIND = "native-lifetime-closure"
LIFETIME_CLOSURE_SCHEMA_VERSION = 1

# Only a worker Wait observed by its own guardian proves natural completion;
# recovered absence, Wait errors, and an unstarted worker do not.
POSITIVE_WAIT_RESULTS = frozenset({"success", "exit-error"})

_PROOF_KEYS = frozenset({"schema_version", "kind", "ordinary", "model_discovery"})
_ROOT_KEYS = frozenset({"root_id", "launch", "tasks", "inspections"})
_LIFETIME_FACT_KEYS = frozenset({
    "cause", "scope_empty", "wait_result", "recovered", "launch_count",
    "registration_sha256", "termination_sha256", "wait_sha256",
})
TASK_FACT_KEYS = _LIFETIME_FACT_KEYS | {"spec_sha256", "meta_sha256"}
INSPECTION_FACT_KEYS = _LIFETIME_FACT_KEYS | {"request_sha256"}

_HEX_DIGEST = re.compile(r"[0-9a-f]{64}")
_TASK_ID = re.compile(r"[0-9a-f]{32}")


def _identity(value: object) -> bool:
    return isinstance(value, str) and _TASK_ID.fullmatch(value) is not None


def _digest(value: object) -> bool:
    return isinstance(value, str) and _HEX_DIGEST.fullmatch(value) is not None


def _fact_error(fact: object, keys: frozenset[str], *, budget_allowed: bool) -> str | None:
    if not isinstance(fact, dict) or set(fact) != keys:
        return "lifetime_closure_invalid"
    if not all(_digest(fact[key]) for key in keys if key.endswith("_sha256")):
        return "lifetime_closure_invalid"
    launch_count = fact["launch_count"]
    causes = {"completed", "budget"} if budget_allowed else {"completed"}
    if (fact["recovered"] is not False or fact["scope_empty"] is not True
            or not isinstance(fact["wait_result"], str)
            or fact["wait_result"] not in POSITIVE_WAIT_RESULTS
            or type(launch_count) is not int or launch_count < 1
            or not isinstance(fact["cause"], str) or fact["cause"] not in causes):
        return "lifetime_closure_unproven"
    return None


def _facts_error(facts: dict[object, object], keys: frozenset[str],
                 budget_ids: frozenset[str]) -> str | None:
    for identity, fact in sorted(facts.items(), key=lambda item: str(item[0])):
        if not _identity(identity):
            return "lifetime_closure_invalid"
        error = _fact_error(fact, keys, budget_allowed=identity in budget_ids)
        if error is not None:
            return error
    return None


def _root_error(root: object, *, task_ids: frozenset[str], budget_ids: frozenset[str],
                inspection_count: int | None) -> str | None:
    if not isinstance(root, dict) or set(root) != _ROOT_KEYS:
        return "lifetime_closure_invalid"
    tasks, inspections = root["tasks"], root["inspections"]
    if not isinstance(tasks, dict) or not isinstance(inspections, dict):
        return "lifetime_closure_invalid"
    if root["launch"] == "none":
        # A known no-launch root admitted nothing, so it has no identity.
        if root["root_id"] is not None or tasks or inspections:
            return "lifetime_closure_invalid"
        return "lifetime_closure_incomplete" if task_ids or inspection_count else None
    if root["launch"] != "admitted" or not _identity(root["root_id"]):
        return "lifetime_closure_invalid"
    if set(tasks) != task_ids or (inspection_count is not None and len(inspections) != inspection_count):
        return "lifetime_closure_incomplete"
    if inspection_count is None and not tasks:
        # An admitted ordinary root always owns at least one ordinary task.
        return "lifetime_closure_incomplete"
    return (_facts_error(tasks, TASK_FACT_KEYS, budget_ids)
            or _facts_error(inspections, INSPECTION_FACT_KEYS, frozenset()))


def lifetime_closure_error(
        proof: object, *, task_ids: Iterable[object], budget_task_ids: Iterable[object] = (),
        model_discovery_required: bool | None = None) -> str | None:
    """Return why a compact closure cannot support a PASS, or None.

    ``task_ids`` are the exact ordinary tasks the case admitted; only
    ``budget_task_ids`` may close by budget. ``model_discovery_required`` is
    None when the caller cannot observe whether discovery ran; a present
    discovery root is then still validated.
    """
    expected = tuple(task_ids)
    budget = tuple(budget_task_ids)
    if not all(_identity(task) for task in (*expected, *budget)) or len(set(expected)) != len(expected):
        return "lifetime_closure_identity_invalid"
    expected_ids = frozenset(str(task) for task in expected)
    budget_ids = frozenset(str(task) for task in budget)
    if not budget_ids <= expected_ids:
        return "lifetime_closure_identity_invalid"
    if proof is None:
        return "lifetime_closure_missing"
    if (not isinstance(proof, dict) or set(proof) != _PROOF_KEYS
            or type(proof["schema_version"]) is not int
            or proof["schema_version"] != LIFETIME_CLOSURE_SCHEMA_VERSION
            or proof["kind"] != LIFETIME_CLOSURE_KIND):
        return "lifetime_closure_invalid"
    discovery = proof["model_discovery"]
    if model_discovery_required is not None and (discovery is not None) != model_discovery_required:
        return "lifetime_closure_incomplete"
    ordinary_error = _root_error(proof["ordinary"], task_ids=expected_ids, budget_ids=budget_ids,
                                 inspection_count=None)
    if ordinary_error is not None or discovery is None:
        return ordinary_error
    # Model discovery owns one separate root with exactly its one inspection.
    return _root_error(discovery, task_ids=frozenset(), budget_ids=frozenset(), inspection_count=1)


def resource_closure_error(proof: object, *, task_ids: Iterable[object],
                           discovery_inspection_id: object | None) -> str | None:
    """Return why a compact closure cannot prove one case's owned resources closed, or None.

    This is not a PASS rule and leaves the case verdict unchanged: any of the
    exact known ordinary ``task_ids`` may have closed by its own budget.
    ``discovery_inspection_id`` is the case's one model discovery inspection,
    or None when the case admitted no discovery. Every other guard is the
    canonical ``lifetime_closure_error``. Requested, recovered or
    previous-boot terminations and budget-terminated inspections stay
    unproven.
    """
    expected = tuple(task_ids)
    if discovery_inspection_id is not None and not _identity(discovery_inspection_id):
        return "lifetime_closure_identity_invalid"
    error = lifetime_closure_error(proof, task_ids=expected, budget_task_ids=expected,
                                   model_discovery_required=discovery_inspection_id is not None)
    if error is not None or discovery_inspection_id is None:
        return error
    discovery = proof.get("model_discovery") if isinstance(proof, dict) else None
    inspections = discovery.get("inspections") if isinstance(discovery, dict) else None
    if not isinstance(inspections, dict) or set(inspections) != {discovery_inspection_id}:
        return "lifetime_closure_incomplete"
    return None


def case_lifetime_closed(*, status: object, cleanup_method: object, proof_state: object,
                         lifecycle_state: object, supervisor_shutdown_observed: object) -> bool:
    """Return whether one compact case's owned lifetime is positively closed.

    The producer binds every admitted task's managed lifetime, including model
    discovery, into a complete lifecycle state with ``lifetime_closure_error``;
    a reader that sees only the compact case consumes that result here. A PASS
    needs that complete lifecycle, and every case needs observed supervisor
    shutdown unless it was blocked before any runtime started.
    """
    if status == "PASS" and lifecycle_state != "complete":
        return False
    blocked_before_runtime = (status == "BLOCKED" and cleanup_method == "blocked-before-runtime-cleanup"
                              and proof_state == "blocked-before-runtime")
    return supervisor_shutdown_observed is True or blocked_before_runtime
