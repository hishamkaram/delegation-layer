"""Pure checkpoint journal record encoders mirroring the frozen Go contract."""

from __future__ import annotations

from dataclasses import dataclass

from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.paths.policy import _raw_clean_absolute_path
from scripts.delegate_acceptance.tasks.specification import _MAX_UINT64


# Projections of the journal's checkpoint record names.
CHECKPOINT_CANDIDATE_RECORD = "checkpoint-candidate.json"
CHECKPOINT_RECEIPT_RECORD = "checkpoint-receipt.json"
CHECKPOINT_CREATION_RECORDS = frozenset({CHECKPOINT_CANDIDATE_RECORD, CHECKPOINT_RECEIPT_RECORD})
_REMOVAL_NAMESPACE = "removal-"
_REMOVAL_PREFIXES = {"attempt": "removal-attempt-", "receipt": "removal-receipt-"}
_REMOVAL_SEQUENCE_DIGITS = 20

# Projections of contract.CheckpointCommitPayloadLimit and its trailers.
_COMMIT_PAYLOAD_LIMIT = 4096
_CHECKPOINT_TRAILER = "Delegate-Checkpoint: "
_CHECKPOINT_REQUEST_TRAILER = "Delegate-Checkpoint-Request: "

_SOURCE_KEYS = {"Root", "CommonDir", "GitDir", "RelativeCwd", "BaseOID", "ObjectFormat", "Dirty"}
_SOURCE_OPTIONAL_KEYS = {"CommonDirDevice", "CommonDirInode"}
_TARGET_KEYS = {"source", "checkout_root", "admin_dir"}
_CANDIDATE_KEYS = {
    "schema_version", "operation_id", "request_sha256", "target", "observed_head",
    "index_sha256", "tree_oid", "commit_oid", "snapshot_ref", "commit_payload",
}
_CLOSURE_KEYS = ("termination_sha256", "worker_wait_sha256", "unlaunched_sha256")
_RECEIPT_KEYS = {"schema_version", "operation_id", "request_sha256", "status"}
_RECEIPT_OPTIONAL_KEYS = {"lifetime_closure", "reason_code", "candidate", *_CLOSURE_KEYS}
_REFERENCE_KEYS = {"schema_version", "checkpoint_id", "sequence", "operation_id", "request_sha256"}
_REMOVAL_RECEIPT_KEYS = {"schema_version", "attempt", "status"}
_REMOVAL_RECEIPT_OPTIONAL_KEYS = {"lifetime_closure", "reason_code", *_CLOSURE_KEYS}

# Receipt closure kinds accepted by contract.ValidateClosureEvidence. Open and
# registered kinds are never recorded; the default native closure is absent.
_RECORDED_CLOSURE_KINDS = {"unregistered-recovered", "legacy-previous-boot"}


@dataclass(frozen=True)
class ClosureEvidence:
    """Projection of contract.ClosureEvidence; empty strings are absent fields."""

    kind: str = ""
    termination_sha256: str = ""
    worker_wait_sha256: str = ""
    unlaunched_sha256: str = ""


@dataclass(frozen=True)
class ClosureProof:
    """Projection of guardian.ClosureProof over validated lifetime records.

    kind is task.ResolveLifetimeClosure's classification, or empty for an
    unlaunched fence, which has no native lifetime closure. termination_cause
    is the validated termination record's cause, or empty without one.
    """

    kind: str
    termination_sha256: str = ""
    termination_cause: str = ""
    worker_wait_sha256: str = ""
    unlaunched_sha256: str = ""


def checkpoint_removal_record(name: str) -> tuple[str, int] | None:
    """Parse one removal-namespace name as Go's parseCheckpointRemovalName.

    Names outside the namespace return None; a malformed name inside it is an
    evidence fault and is refused rather than ignored.
    """
    if not name.startswith(_REMOVAL_NAMESPACE):
        return None
    for family, prefix in _REMOVAL_PREFIXES.items():
        if name.startswith(prefix):
            stem = name[len(prefix):]
            require(stem.endswith(".json") and len(stem) == _REMOVAL_SEQUENCE_DIGITS + 5 and
                    stem[:-5].isascii() and stem[:-5].isdigit(),
                    f"malformed checkpoint removal record {name!r}")
            sequence = int(stem[:-5])
            require(0 < sequence <= _MAX_UINT64 and checkpoint_removal_name(family, sequence) == name,
                    f"malformed checkpoint removal record {name!r}")
            return family, sequence
    raise AcceptanceFailure(f"unknown checkpoint removal record {name!r}")


def checkpoint_removal_name(family: str, sequence: int) -> str:
    """Return the canonical removal attempt or receipt record name."""
    return f"{_REMOVAL_PREFIXES[family]}{sequence:0{_REMOVAL_SEQUENCE_DIGITS}d}.json"


def checkpoint_ref(root: str, checkpoint: str) -> str:
    """Return contract.CheckpointRef, the one owned ref of a checkpoint."""
    require(_task_identity(root) and _task_identity(checkpoint), "checkpoint ref identity is invalid")
    return f"refs/delegate/{root}/checkpoints/{checkpoint}"


def _git_oid(value: object, object_format: str) -> bool:
    """Match contract.ValidateOID, which accepts either hex letter case."""
    width = {"sha1": 40, "sha256": 64}.get(object_format)
    return (width is not None and isinstance(value, str) and len(value) == width and
            all(character in "0123456789abcdefABCDEF" for character in value))


def _clean_absolute_path(value: object) -> bool:
    """Match contract.ValidateAbsoluteCleanPath."""
    return _raw_clean_absolute_path(value) and "\x00" not in value


def _clean_relative_cwd(value: object) -> bool:
    """Match contract.ValidateSourceRelativeCwd for Unix paths."""
    if not isinstance(value, str) or not value or "\x00" in value or value.startswith("/"):
        return False
    if value == ".." or value.startswith("../"):
        return False
    parts = value.split("/")
    return value == "." or all(part not in {"", ".", ".."} for part in parts)


def _uint64(value: object, *, positive: bool) -> bool:
    return type(value) is int and (1 if positive else 0) <= value <= _MAX_UINT64


def _canonical_source(value: object, label: str) -> dict[str, object]:
    """Validate a pinned contract.Source and return its Go field order."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _SOURCE_KEYS, _SOURCE_OPTIONAL_KEYS, label)
    object_format = value.get("ObjectFormat")
    require(object_format in {"sha1", "sha256"}, f"{label} object format is invalid")
    require(_git_oid(value.get("BaseOID"), object_format), f"{label} base OID is invalid")
    require(_clean_relative_cwd(value.get("RelativeCwd")), f"{label} relative cwd is invalid")
    for key in ("Root", "CommonDir", "GitDir"):
        require(_clean_absolute_path(value.get(key)), f"{label} {key} is invalid")
    require(isinstance(value.get("Dirty"), bool), f"{label} Dirty is invalid")
    for key in _SOURCE_OPTIONAL_KEYS:
        if key in value:
            require(_uint64(value[key], positive=True), f"{label} {key} is invalid")
    require("CommonDirInode" in value, f"{label} common directory is not pinned")
    canonical = {key: value[key] for key in (
        "Root", "CommonDir", "GitDir", "RelativeCwd", "BaseOID", "ObjectFormat", "Dirty")}
    canonical.update({key: value[key] for key in ("CommonDirDevice", "CommonDirInode") if key in value})
    return canonical


def _joined_cwd(root: str, relative: str) -> str:
    """Return filepath.Join for a clean absolute root and clean relative path."""
    return root if relative == "." else root.rstrip("/") + "/" + relative


def _canonical_target(value: object, selection: dict[str, object], label: str) -> dict[str, object]:
    """Validate contract.CheckpointTarget against the request's selection."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _TARGET_KEYS, label=label)
    source = _canonical_source(value.get("source"), f"{label} source")
    checkout_root = value.get("checkout_root")
    admin_dir = value.get("admin_dir")
    require(_clean_absolute_path(checkout_root) and _clean_absolute_path(admin_dir),
            f"{label} checkout coordinates are invalid")
    if selection["kind"] == "ordinary":
        require(checkout_root == source["Root"] and admin_dir == source["GitDir"] and
                _joined_cwd(source["Root"], source["RelativeCwd"]) == selection.get("cwd"),
                f"{label} does not resolve the ordinary selection")
    else:
        require(selection["kind"] == "managed" and checkout_root != source["Root"] and
                admin_dir != source["CommonDir"],
                f"{label} is not a managed checkout")
    return {"source": source, "checkout_root": checkout_root, "admin_dir": admin_dir}


def _validate_commit_payload(candidate: dict[str, object], label: str) -> None:
    """Match contract.validateCheckpointPayload without hashing the payload."""
    payload = candidate.get("commit_payload")
    require(isinstance(payload, str) and 0 < len(payload) <= _COMMIT_PAYLOAD_LIMIT and
            all(character == "\n" or " " <= character <= "~" for character in payload) and
            payload.endswith("\n"), f"{label} commit payload is invalid")
    header, separator, message = payload.partition("\n\n")
    lines = header.split("\n")
    require(separator and len(lines) == 4 and lines[0] == "tree " + candidate["tree_oid"] and
            lines[1] == "parent " + candidate["observed_head"] and
            _signature(lines[2], "author ") and _signature(lines[3], "committer "),
            f"{label} commit payload header is invalid")
    message_lines = message.removesuffix("\n").split("\n")
    require(len(message_lines) >= 2 and
            message_lines[-2] == _CHECKPOINT_TRAILER + candidate["operation_id"] and
            message_lines[-1] == _CHECKPOINT_REQUEST_TRAILER + candidate["request_sha256"] and
            not any(line.startswith(_CHECKPOINT_TRAILER.removesuffix(": "))
                    for line in message_lines[:-2]),
            f"{label} commit payload trailers are invalid")


def _signature(line: str, prefix: str) -> bool:
    return line.startswith(prefix) and bool(line[len(prefix):].strip(" "))


def canonical_checkpoint_candidate(
        value: object, request: dict[str, object], request_digest: str, label: str,
        ) -> dict[str, object]:
    """Validate contract.CheckpointCandidate for its exact creation request."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _CANDIDATE_KEYS, label=label)
    selection = request.get("checkpoint")
    require(request.get("kind") == "checkpoint" and isinstance(selection, dict),
            f"{label} does not belong to a checkpoint creation")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("operation_id") == request.get("operation_id") and
            value.get("request_sha256") == request_digest and _hex_digest(request_digest),
            f"{label} identity is invalid")
    require(_hex_digest(value.get("index_sha256")), f"{label} index_sha256 is invalid")
    target = _canonical_target(value.get("target"), selection, f"{label} target")
    require(value.get("snapshot_ref") == checkpoint_ref(request["root_id"], value["operation_id"]),
            f"{label} snapshot ref is not the owned checkpoint ref")
    object_format = target["source"]["ObjectFormat"]
    for key in ("observed_head", "tree_oid", "commit_oid"):
        require(_git_oid(value.get(key), object_format), f"{label} {key} is invalid")
    _validate_commit_payload(value, label)
    return {
        "schema_version": value["schema_version"],
        "operation_id": value["operation_id"],
        "request_sha256": value["request_sha256"],
        "target": target,
        **{key: value[key] for key in (
            "observed_head", "index_sha256", "tree_oid", "commit_oid", "snapshot_ref",
            "commit_payload")},
    }


def canonical_checkpoint_candidate_bytes(
        value: object, request: dict[str, object], request_digest: str, label: str) -> bytes:
    """Marshal one validated checkpoint candidate exactly as Go does."""
    return canonical_go_json(canonical_checkpoint_candidate(value, request, request_digest, label))


def _receipt_closure(value: dict[str, object], label: str) -> tuple[ClosureEvidence, bool]:
    """Match contract.ValidateClosureEvidence and report whether the worker may have run."""
    for key in ("lifetime_closure", *_CLOSURE_KEYS):
        if key in value:
            require(isinstance(value[key], str) and value[key], f"{label} {key} is invalid")
    closure = ClosureEvidence(
        kind=value.get("lifetime_closure", ""),
        termination_sha256=value.get("termination_sha256", ""),
        worker_wait_sha256=value.get("worker_wait_sha256", ""),
        unlaunched_sha256=value.get("unlaunched_sha256", ""))
    if closure.kind == "unregistered-recovered":
        require(not closure.termination_sha256 and not closure.unlaunched_sha256 and
                _hex_digest(closure.worker_wait_sha256), f"{label} closure is invalid")
        return closure, False
    if closure.kind == "legacy-previous-boot":
        require(not closure.worker_wait_sha256 and not closure.unlaunched_sha256 and
                _hex_digest(closure.termination_sha256), f"{label} closure is invalid")
        return closure, True
    require(closure.kind == "", f"{label} lifetime_closure is invalid")
    if not closure.unlaunched_sha256:
        require(_hex_digest(closure.termination_sha256) and _hex_digest(closure.worker_wait_sha256),
                f"{label} closure is invalid")
        return closure, True
    require(not closure.termination_sha256 and not closure.worker_wait_sha256 and
            _hex_digest(closure.unlaunched_sha256), f"{label} closure is invalid")
    return closure, False


def _validate_disposition(
        value: dict[str, object], closure: ClosureEvidence, worker_may_have_run: bool, *,
        successes: set[str], not_launched: str, run_reasons: set[str], label: str) -> bool:
    """Apply the shared success-encoding and unavailable-reason rules."""
    status = value.get("status")
    reason = value.get("reason_code", "")
    require(isinstance(reason, str) and ("reason_code" not in value or reason),
            f"{label} reason_code is invalid")
    if status in successes:
        require(not reason, f"{label} success has a reason code")
        require(closure.kind == "" and not closure.unlaunched_sha256 and
                closure.termination_sha256 and closure.worker_wait_sha256,
                f"{label} success lacks native closure encoding")
        return True
    require(status == "unavailable", f"{label} status is invalid")
    if worker_may_have_run:
        require(reason in run_reasons, f"{label} unavailable reason is invalid")
    else:
        require(reason == not_launched, f"{label} unavailable reason is invalid")
    return False


def _closure_fields(value: dict[str, object]) -> dict[str, object]:
    return {key: value[key] for key in _CLOSURE_KEYS if key in value}


def canonical_checkpoint_receipt(
        value: object, request: dict[str, object], request_digest: str, label: str,
        ) -> dict[str, object]:
    """Validate contract.CheckpointReceipt for its exact creation request."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _RECEIPT_KEYS, _RECEIPT_OPTIONAL_KEYS, label)
    require(request.get("kind") == "checkpoint" and
            type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("operation_id") == request.get("operation_id") and
            value.get("request_sha256") == request_digest and _hex_digest(request_digest),
            f"{label} identity is invalid")
    closure, worker_may_have_run = _receipt_closure(value, label)
    checkpointed = _validate_disposition(
        value, closure, worker_may_have_run, successes={"checkpointed"},
        not_launched="checkpoint_not_launched",
        run_reasons={"checkpoint_failed", "checkpoint_result_unavailable"}, label=label)
    require(("candidate" in value) == checkpointed, f"{label} candidate presence is invalid")
    canonical: dict[str, object] = {}
    if closure.kind:
        canonical["lifetime_closure"] = closure.kind
    canonical.update({key: value[key] for key in (
        "schema_version", "operation_id", "request_sha256", "status")})
    if "reason_code" in value:
        canonical["reason_code"] = value["reason_code"]
    if checkpointed:
        canonical["candidate"] = canonical_checkpoint_candidate(
            value["candidate"], request, request_digest, f"{label} candidate")
    canonical.update(_closure_fields(value))
    return canonical


def canonical_checkpoint_receipt_bytes(
        value: object, request: dict[str, object], request_digest: str, label: str) -> bytes:
    """Marshal one validated checkpoint creation receipt exactly as Go does."""
    return canonical_go_json(canonical_checkpoint_receipt(value, request, request_digest, label))


def canonical_checkpoint_removal_reference(
        value: object, checkpoint: str, sequence: int, label: str) -> dict[str, object]:
    """Validate one contract.CheckpointRemovalReference at its stored sequence."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _REFERENCE_KEYS, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            _task_identity(checkpoint) and value.get("checkpoint_id") == checkpoint and
            _uint64(value.get("sequence"), positive=True) and value["sequence"] == sequence,
            f"{label} identity is invalid")
    require(_task_identity(value.get("operation_id")) and _hex_digest(value.get("request_sha256")),
            f"{label} removal request is invalid")
    return {key: value[key] for key in (
        "schema_version", "checkpoint_id", "sequence", "operation_id", "request_sha256")}


def canonical_checkpoint_removal_reference_bytes(
        value: object, checkpoint: str, sequence: int, label: str) -> bytes:
    """Marshal one validated removal attempt reference exactly as Go does."""
    return canonical_go_json(canonical_checkpoint_removal_reference(value, checkpoint, sequence, label))


def canonical_checkpoint_removal_receipt(
        value: object, attempt: dict[str, object], label: str) -> dict[str, object]:
    """Validate contract.CheckpointRemovalReceipt for its registered attempt."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _REMOVAL_RECEIPT_KEYS, _REMOVAL_RECEIPT_OPTIONAL_KEYS, label)
    stored = canonical_checkpoint_removal_reference(
        value.get("attempt"), attempt["checkpoint_id"], attempt["sequence"], f"{label} attempt")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            stored == attempt, f"{label} does not close its registered attempt")
    closure, worker_may_have_run = _receipt_closure(value, label)
    _validate_disposition(
        value, closure, worker_may_have_run, successes={"removed", "nothing_to_remove"},
        not_launched="checkpoint_removal_not_launched",
        run_reasons={"checkpoint_removal_failed", "checkpoint_removal_result_unavailable"},
        label=label)
    canonical: dict[str, object] = {}
    if closure.kind:
        canonical["lifetime_closure"] = closure.kind
    canonical.update({"schema_version": value["schema_version"], "attempt": stored,
                      "status": value["status"]})
    if "reason_code" in value:
        canonical["reason_code"] = value["reason_code"]
    canonical.update(_closure_fields(value))
    return canonical


def canonical_checkpoint_removal_receipt_bytes(
        value: object, attempt: dict[str, object], label: str) -> bytes:
    """Marshal one validated removal receipt exactly as Go does."""
    return canonical_go_json(canonical_checkpoint_removal_receipt(value, attempt, label))


def resolved_closure_kind(*, registered: bool, worker_wait: dict[str, object] | None) -> str:
    """Mirror task.ResolveLifetimeClosure over records the lifetime validator closed."""
    if worker_wait is None:
        return "legacy-previous-boot"
    if worker_wait.get("result") == "absent":
        return "registered-recovered" if registered else "unregistered-recovered"
    return "registered-native"


def require_receipt_closure(
        receipt: dict[str, object], proof: ClosureProof, *, success: bool, label: str) -> None:
    """Bind a receipt's closure fields to the actual proof, as the coordinator records them.

    Only unregistered-recovered and legacy-previous-boot kinds are recorded. The
    stored receipt is authoritative, so a legacy receipt also survives a later
    previous-boot absence publication over the same termination. A success
    additionally requires a registered native worker Wait: recovered absence
    and legacy previous-boot termination can never qualify one.
    """
    recorded = ClosureEvidence(
        kind=proof.kind if proof.kind in _RECORDED_CLOSURE_KINDS else "",
        termination_sha256=proof.termination_sha256,
        worker_wait_sha256=proof.worker_wait_sha256,
        unlaunched_sha256=proof.unlaunched_sha256)
    stored, _ = _receipt_closure(receipt, label)
    require(stored == recorded or _legacy_receipt_before_absence(stored, proof),
            f"{label} closure does not match the actual lifetime records")
    require(not success or proof.kind == "registered-native",
            f"{label} success has no native worker Wait")


def _legacy_receipt_before_absence(stored: ClosureEvidence, proof: ClosureProof) -> bool:
    """Accept a legacy receipt recorded before a previous-boot absence was published.

    A legacy-previous-boot closure is a registered previous-boot termination
    without a worker Wait. Publishing the absence adds only the Wait record and
    re-resolves the same termination as registered-recovered; the receipt
    written earlier names exactly that termination and nothing else.
    """
    return (proof.kind == "registered-recovered" and proof.termination_cause == "previous-boot" and
            stored == ClosureEvidence(
                kind="legacy-previous-boot", termination_sha256=proof.termination_sha256))
