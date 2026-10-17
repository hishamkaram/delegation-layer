"""Pure recovered-absence rules that never invent native Wait or success."""

from __future__ import annotations

import re
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import _exact_keys, require


def _stable_lifetime_boot_identity(value: object) -> tuple[str, str] | None:
    if not isinstance(value, str):
        return None
    match = re.fullmatch(
        r"(linux:|darwin:boot-session:)([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-"
        r"[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})", value)
    return (match.group(1), match.group(2).lower()) if match is not None else None


def _validate_lifetime_absence_completion(
        *, wait: dict[str, object], root: str, task: str, recorded_boot: str,
        registration_digest: str, intent_digest: str,
        worker: dict[str, object] | None, worker_digest: str,
        termination: dict[str, object] | None) -> None:
    """An absence disposition proves closure, never native Wait or success."""
    _exact_keys(wait, {
        "schema_version", "root_id", "task_id", "registration_sha256",
        "child_pid", "result", "observed_at", "absence_proof", "observed_boot_id",
    }, {"worker_sha256", "intent_sha256"}, label=f"lifetime absence for {task}")
    require(type(wait.get("schema_version")) is int and wait["schema_version"] == 1 and
            wait.get("root_id") == root and wait.get("task_id") == task and
            wait.get("result") == "absent" and
            wait.get("registration_sha256") == registration_digest and
            wait.get("intent_sha256", "") == intent_digest and
            type(wait.get("child_pid")) is int,
            f"lifetime absence binding mismatch for {task}")
    _canonical_timestamp(wait.get("observed_at"), f"lifetime absence observed_at for {task}")
    recorded = _stable_lifetime_boot_identity(recorded_boot)
    observed = _stable_lifetime_boot_identity(wait.get("observed_boot_id"))
    require(recorded is not None and observed is not None and recorded[0] == observed[0],
            f"lifetime absence lacks stable comparable boot identities for {task}")
    same_boot = recorded == observed
    exact_same_boot = same_boot and wait.get("observed_boot_id") == recorded_boot
    if worker is None:
        require(wait.get("worker_sha256", "") == "" and wait["child_pid"] == 0,
                f"lifetime absence invents worker identity for {task}")
    else:
        require(wait.get("worker_sha256") == worker_digest and
                wait["child_pid"] == worker.get("pid"),
                f"lifetime absence worker mismatch for {task}")
    proof = wait.get("absence_proof")
    if proof == "exact-worker":
        valid = bool(registration_digest) and worker is not None and exact_same_boot and (
            termination is not None and termination.get("cause") != "previous-boot")
    elif proof == "previous-boot":
        valid = not same_boot and (termination is not None if registration_digest else worker is None)
    elif proof == "unregistered-disposed":
        valid = bool(intent_digest) and not registration_digest and worker is None and exact_same_boot
    else:
        valid = False
    require(valid, f"lifetime absence witness mismatch for {task}")
