"""Pure identity, timing and backend oracles for nested survival."""

from scripts.acceptance.evidence.timestamps import _canonical_timestamp
from scripts.acceptance.evidence.timestamps import _timestamp_unix_ns
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.queue_policy import task_row_by_label
from scripts.acceptance.private_cli.queue_policy import pueue_state


LEADER_SECONDS = 180
WORKER_SECONDS = 600
SETUP_MARGIN_SECONDS = 30
DESCENDANT_WINDOW_SECONDS = 30
DESCENDANT_BACKSTOP_SECONDS = 60


def timestamp_ns(value):
    return _timestamp_unix_ns(_canonical_timestamp(value, "nested lifetime timestamp"))


def require_backend(specification, expected):
    assurance = {"cgroup-v2": "exact", "process-group": "cooperative"}[expected]
    require(specification.get("scope_kind") == expected and specification.get("assurance") == assurance,
            "nested actor selected a different native backend/assurance")


def require_setup_margin(deadline_ns, observed_ns):
    require(deadline_ns - observed_ns >= SETUP_MARGIN_SECONDS * 1_000_000_000,
            "setup_inconclusive: less than 30s remains on the leader deadline")


def require_challenge(reply, nonce, launch, termination_ns, worker_deadline_ns, *, same_identity):
    require(isinstance(reply, dict) and reply.get("nonce") == nonce
            and type(reply.get("pid")) is int and reply["pid"] == launch.get("pid")
            and same_identity is True,
            "nested nonce reply changed provider incarnation")
    observed = reply.get("observed_at_unix_ns")
    require(type(observed) is int and termination_ns < observed < worker_deadline_ns,
            "nested nonce reply lies outside the independent live window")


def require_worker_deadline(intent, resume_ns, terminated_ns, allowance_seconds=45):
    created, deadline = timestamp_ns(intent.get("created_at")), timestamp_ns(intent.get("deadline"))
    require(created >= max(resume_ns, terminated_ns), "queued lifetime began before resume/leader termination")
    remaining = deadline - created
    require((WORKER_SECONDS - allowance_seconds) * 1_000_000_000 <= remaining
            <= WORKER_SECONDS * 1_000_000_000, "queued worker lost its independent execution budget")


def require_descendant_absent(before, after, observed_ns, leader_deadline_ns, released, *, same_identity):
    require(type(same_identity) is bool, "native descendant identity verdict is missing")
    require(before.get("present") is True and type(before.get("pid")) is int
            and bool(before.get("process_start")), "ordinary descendant was not proven live")
    same_live = after.get("present") is True and same_identity is True
    require(after.get("pid") == before["pid"] and not same_live and not released
            and leader_deadline_ns <= observed_ns
            < leader_deadline_ns + DESCENDANT_WINDOW_SECONDS * 1_000_000_000,
            "ordinary descendant containment is unproven before its release/backstop")


def queue_ended_evidence(snapshot, receipt, closure):
    """A terminal callback may already have removed a canonically closed row."""
    require(closure.get("scope_disposed") is True and closure.get("owner_released") is True
            and closure.get("termination", {}).get("scope_empty") is True,
            "queue disappearance cannot replace canonical native closure")
    require(type(receipt.get("numeric_task_id")) is int and receipt["numeric_task_id"] >= 0
            and receipt.get("label") == "delegate:" + receipt["root_id"] + ":" + receipt["task_id"],
            "queue observation lacks immutable admission identity")
    row = task_row_by_label(snapshot, receipt["label"])
    if row is None:
        return {"state": "removed-after-canonical-native-closure", "label": receipt["label"]}
    require(row.get("id") == receipt["numeric_task_id"], "queue label changed numeric admission identity")
    if pueue_state(row) != "Done":
        return None
    return {"state": "Done", "label": receipt["label"], "numeric_task_id": receipt["numeric_task_id"]}
