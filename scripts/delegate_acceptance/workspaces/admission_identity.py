"""Pure workspace admission inspection identity derivation."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import _hex_digest, _task_identity, require
from scripts.delegate_acceptance.tasks.specification import _MAX_UINT64


_ADMISSION_ATTEMPT_DOMAIN = b"delegation-layer:workspace-admission-inspection:v1\n"


def _admission_attempt_operation_id(
        root: str, workspace: str, task: str, request_sha256: str,
        sequence: int, label: str) -> str:
    """Derive the effective inspection identity from the immutable projection."""
    require(_task_identity(root), f"{label} root_id is invalid")
    require(_task_identity(workspace), f"{label} workspace_id is invalid")
    require(_task_identity(task), f"{label} task_id is invalid")
    require(_hex_digest(request_sha256), f"{label} request_sha256 is invalid")
    require(type(sequence) is int and 0 < sequence <= _MAX_UINT64,
            f"{label} sequence is invalid")
    projection = {
        "root_id": root,
        "workspace_id": workspace,
        "task_id": task,
        "request_sha256": request_sha256,
        "sequence": sequence,
    }
    return sha(_ADMISSION_ATTEMPT_DOMAIN + canonical_go_json(projection))[:32]
