"""Pure oracles over the public checkpoint command family's JSON and exits.

A mutation result and a metadata observation are different facts. Creation and
removal acknowledge accepted work; only a later qualified observation says the
checkpoint is usable or the selected removal is done. These rules never infer
pending or unavailable state from an omitted description.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.workspaces.checkpoint_records import checkpoint_ref
from scripts.acceptance.workspaces.scenarios.model import OID_RE
from scripts.acceptance.workspaces.scenarios.model import TASK_ID_RE

WATCH = "watch_checkpoint"
STOP_AND_REPORT = "stop_and_report"
NOT_AUTHORIZED = "requested_action_not_authorized"
PENDING = "pending"
CHECKPOINTED = "checkpointed"
UNAVAILABLE = "unavailable"
REMOVED = "removed"
CREATION_STATES = frozenset({PENDING, CHECKPOINTED, UNAVAILABLE})
REMOVAL_STATES = frozenset({PENDING, REMOVED, "nothing_to_remove", UNAVAILABLE})
SETTLED_REMOVAL_STATES = frozenset({REMOVED, "nothing_to_remove"})
# A checkpoint response never borrows provider-task or workspace authority.
FOREIGN_RESPONSE_KEYS = frozenset({
    "task_id", "task_record", "parent_task_id", "outcome", "payload", "workspace", "workspaces",
})
_BLOCK_KEYS = frozenset({"id", "operation_id", "current", "operation", "reason_code", "next_action"})


@dataclass(frozen=True)
class Acknowledgement:
    """One mutation result: refused carries no ID; accepted carries exactly what it acknowledged."""

    accepted: bool
    checkpoint_id: str = ""
    operation_id: str = ""
    settled: bool = False


@dataclass(frozen=True)
class CheckpointFacts:
    """A qualified, usable checkpoint's public identity."""

    checkpoint_id: str
    commit_oid: str
    snapshot_ref: str


def _canonical_id(value: object, label: str) -> str:
    if not (isinstance(value, str) and TASK_ID_RE.fullmatch(value) is not None):
        raise AcceptanceFailure(label + " is invalid")
    return value


def _failure(label: str, code: int, block: Mapping[str, object], response: Mapping[str, object]) -> AcceptanceFailure:
    return AcceptanceFailure(
        f"{label}: exit {code} checkpoint {block!r} error {response.get('error', '')!r}")


def checkpoint_block(response: Mapping[str, object], root_id: str, *, listing: bool = False) -> dict[str, object]:
    """Validate the shared response frame and return its checkpoint projection."""
    require(response.get("schema_version") == 1 and response.get("command") == "checkpoint",
            "checkpoint response frame is invalid")
    require(response.get("root_id") == root_id, "checkpoint response names a different state root")
    borrowed = sorted(FOREIGN_RESPONSE_KEYS & set(response))
    require(not borrowed, "checkpoint response borrowed foreign authority: " + ", ".join(borrowed))
    if listing:
        require("checkpoint" not in response, "checkpoint list carried a command projection")
        return {}
    require("checkpoints" not in response and "checkpoint_cursor" not in response,
            "checkpoint command carried a list page")
    block = response.get("checkpoint")
    if not (isinstance(block, dict)):
        raise AcceptanceFailure("checkpoint response omitted its checkpoint projection")
    require(set(block) <= _BLOCK_KEYS, "checkpoint projection has unknown fields")
    return block


def _refusal(code: int, block: Mapping[str, object], action: str) -> bool:
    return code == 3 and block == {"reason_code": NOT_AUTHORIZED, "next_action": "retry_" + action}


def classify_creation(code: int, response: Mapping[str, object], root_id: str) -> Acknowledgement:
    """Classify `checkpoint create`; a refusal has no acknowledged checkpoint."""
    block = checkpoint_block(response, root_id)
    if _refusal(code, block, "create"):
        return Acknowledgement(accepted=False)
    plain = "operation_id" not in block and "operation" not in block and "reason_code" not in block
    if code == 3 and plain and block.get("next_action") == WATCH and "current" not in block:
        return Acknowledgement(accepted=True, checkpoint_id=_canonical_id(block.get("id"), "created checkpoint ID"))
    current = block.get("current")
    if code == 0 and plain and "next_action" not in block and isinstance(current, dict):
        checkpoint_id = _canonical_id(block.get("id"), "created checkpoint ID")
        require(current.get("id") == checkpoint_id and current.get("state") == CHECKPOINTED,
                "settled creation did not describe its own checkpointed receipt")
        return Acknowledgement(accepted=True, checkpoint_id=checkpoint_id, settled=True)
    raise _failure("checkpoint creation", code, block, response)


def classify_removal(code: int, response: Mapping[str, object], root_id: str, checkpoint_id: str) -> Acknowledgement:
    """Classify `checkpoint remove C`; only an accepted removal names its exact attempt R."""
    block = checkpoint_block(response, root_id)
    if _refusal(code, block, "remove"):
        return Acknowledgement(accepted=False)
    if block.get("id") != checkpoint_id or "reason_code" in block:
        raise _failure("checkpoint removal", code, block, response)
    operation_id = _canonical_id(block.get("operation_id"), "removal operation ID")
    require(operation_id != checkpoint_id, "removal acknowledged the creation ID as its attempt")
    if code == 3 and block.get("next_action") == WATCH and "operation" not in block:
        return Acknowledgement(accepted=True, checkpoint_id=checkpoint_id, operation_id=operation_id)
    operation = block.get("operation")
    if code == 0 and "next_action" not in block and isinstance(operation, dict):
        require(operation.get("operation_id") == operation_id and
                operation.get("state") in SETTLED_REMOVAL_STATES,
                "settled removal did not describe its own acknowledged attempt")
        return Acknowledgement(accepted=True, checkpoint_id=checkpoint_id, operation_id=operation_id,
                               settled=True)
    raise _failure("checkpoint removal", code, block, response)


def _described_states(current: Mapping[str, object]) -> tuple[str, str | None]:
    state = current.get("state")
    if not (state in CREATION_STATES and isinstance(current.get("usable"), bool)):
        raise AcceptanceFailure("checkpoint description state is outside the delivered vocabulary")
    removal = current.get("removal")
    if removal is None:
        return str(state), None
    if not (isinstance(removal, dict) and removal.get("state") in REMOVAL_STATES):
        raise AcceptanceFailure("latest removal state is outside the delivered vocabulary")
    _canonical_id(removal.get("operation_id"), "latest removal operation ID")
    return str(state), str(removal["state"])


def classify_status(
        code: int, response: Mapping[str, object], root_id: str, checkpoint_id: str) -> dict[str, object] | None:
    """Classify plain `checkpoint status C`: None while pending, else the observed description.

    Exit 0 is a successful observation, not usability: unavailable history is
    returned with stop_and_report so the caller inspects state and `usable`.
    """
    block = checkpoint_block(response, root_id)
    current = block.get("current")
    if code == 3 and block.get("next_action") == WATCH and "operation" not in block:
        if current is not None:
            if not (isinstance(current, dict) and current.get("id") == checkpoint_id and
                    PENDING in _described_states(current)):
                raise AcceptanceFailure("pending status described settled state")
        return None
    if code != 0 or not isinstance(current, dict) or "operation" in block:
        raise _failure("checkpoint status " + checkpoint_id, code, block, response)
    require(block.get("id") == checkpoint_id and current.get("id") == checkpoint_id,
            "checkpoint status described another checkpoint")
    creation, removal = _described_states(current)
    require(PENDING not in (creation, removal), "settled status described pending state")
    history_failed = UNAVAILABLE in (creation, removal)
    require(block.get("next_action") == (STOP_AND_REPORT if history_failed else None),
            "checkpoint status guidance does not match its described history")
    require(current["usable"] == (creation == CHECKPOINTED and removal is None),
            "checkpoint usability contradicts its described history")
    return current


def classify_removal_status(
        code: int, response: Mapping[str, object], root_id: str, *, checkpoint_id: str,
        operation_id: str) -> dict[str, object] | None:
    """Classify `status C --operation R`: only R's own state chooses the result, never the latest head."""
    block = checkpoint_block(response, root_id)
    operation = block.get("operation")
    if operation is not None:
        if not (isinstance(operation, dict) and operation.get("operation_id") == operation_id and
                operation.get("state") in REMOVAL_STATES):
            raise AcceptanceFailure("selected removal described another attempt")
    if code == 3 and block.get("next_action") == WATCH:
        require(operation is None or operation["state"] == PENDING,
                "pending removal status described a settled attempt")
        require(block.get("operation_id") in {None, operation_id}, "pending status switched attempts")
        return None
    if code != 0 or operation is None or "next_action" in block:
        raise _failure("checkpoint removal status " + operation_id, code, block, response)
    require(block.get("id") == checkpoint_id and block.get("operation_id") == operation_id,
            "settled removal status lost its acknowledged identities")
    require(operation["state"] in SETTLED_REMOVAL_STATES, "selected removal did not settle successfully")
    return operation


def qualified_checkpoint(
        current: Mapping[str, object], *, root_id: str, selection: str, cwd: str | None = None,
        workspace_id: str | None = None) -> CheckpointFacts:
    """Require a usable checkpointed description bound to its exact target and owned ref."""
    checkpoint_id = _canonical_id(current.get("id"), "checkpoint ID")
    require(current.get("state") == CHECKPOINTED and current.get("usable") is True and
            "removal" not in current and "reason_code" not in current,
            "checkpoint is not a usable qualified checkpoint")
    require(current.get("selection") == selection and current.get("cwd") == cwd and
            current.get("workspace_id") == workspace_id,
            "checkpoint does not describe its requested target")
    commit = current.get("commit_oid")
    if not (isinstance(commit, str) and OID_RE.fullmatch(commit) is not None):
        raise AcceptanceFailure("checkpoint commit is invalid")
    require(current.get("snapshot_ref") == checkpoint_ref(root_id, checkpoint_id),
            "checkpoint does not name its owned ref")
    return CheckpointFacts(checkpoint_id=checkpoint_id, commit_oid=commit,
                           snapshot_ref=str(current["snapshot_ref"]))


def removed_history(current: Mapping[str, object], facts: CheckpointFacts, operation_id: str) -> None:
    """A removed checkpoint keeps its qualified creation, is no longer usable, and names R as latest."""
    require(current.get("id") == facts.checkpoint_id and current.get("state") == CHECKPOINTED and
            current.get("usable") is False and current.get("commit_oid") == facts.commit_oid and
            current.get("snapshot_ref") == facts.snapshot_ref,
            "removed checkpoint lost its qualified creation history")
    require(current.get("removal") == {"operation_id": operation_id, "attempts": 1, "state": REMOVED},
            "removed checkpoint does not name its exact removal as the only attempt")


def list_page(
        response: Mapping[str, object], root_id: str, *, limit: int, after: str) -> tuple[list[dict[str, object]], str]:
    """Validate one metadata page: ascending IDs after the cursor and a cursor only for a full page."""
    checkpoint_block(response, root_id, listing=True)
    entries = response.get("checkpoints")
    if not (isinstance(entries, list) and len(entries) <= limit):
        raise AcceptanceFailure("checkpoint page is invalid")
    previous = after
    for entry in entries:
        if not (isinstance(entry, dict)):
            raise AcceptanceFailure("checkpoint page entry is not an object")
        identity = _canonical_id(entry.get("id"), "listed checkpoint ID")
        require(identity > previous, "checkpoint page is not in ascending ID order after its cursor")
        _described_states(entry)
        previous = identity
    cursor = response.get("checkpoint_cursor", "")
    if not (isinstance(cursor, str)):
        raise AcceptanceFailure("checkpoint cursor is invalid")
    if cursor:
        require(len(entries) == limit and cursor == entries[-1]["id"],
                "checkpoint cursor is not the last entry of a full page")
    return entries, cursor


def ref_observation(
        *, reference: str, inventory: bytes, inventory_stderr: bytes, symbolic_exit: int,
        symbolic_stdout: bytes, resolved_exit: int, resolved_stdout: bytes) -> str | None:
    """Return the commit a direct ref names, or None only after proving exact absence.

    The ref inventory silently skips a dangling symbolic ref, so absence also
    requires that the name is not symbolic and resolves to nothing.
    """
    require(symbolic_exit == 1 and not symbolic_stdout, "owned ref is symbolic: " + reference)
    listed = [line.split(b" ") for line in inventory.splitlines()]
    require(all(len(fields) == 2 and fields[0] == reference.encode() for fields in listed) and len(listed) <= 1,
            "ref inventory named another ref: " + reference)
    if not listed:
        require(not inventory_stderr and resolved_exit == 1 and not resolved_stdout,
                "ref absence is not proven: " + reference)
        return None
    commit = listed[0][1].decode("ascii")
    require(resolved_exit == 0 and resolved_stdout.strip() == listed[0][1] and OID_RE.fullmatch(commit),
            "listed ref does not resolve to its commit: " + reference)
    return commit


def closure_facts(closure: Mapping[str, object]) -> dict[str, object]:
    """Project a validated operation closure's mode and native scope facts for the receipt."""
    lifetime = closure.get("managed_lifetime")
    intent = lifetime.get("intent") if isinstance(lifetime, dict) else None
    facts: dict[str, object] = {"mode": closure.get("mode"), "request_sha256": closure.get("request_sha256")}
    if isinstance(intent, dict):
        facts.update({"scope_kind": intent.get("scope_kind"), "assurance": intent.get("assurance")})
    return facts


def unchanged_checkout(before: Mapping[str, object], after: Mapping[str, object], label: str) -> None:
    """HEAD, the real index's bytes and identity, status and worktree bytes stay exactly the same."""
    for field in sorted(before.keys() | after.keys()):
        require(before.get(field) == after.get(field), label + " changed its " + field)
