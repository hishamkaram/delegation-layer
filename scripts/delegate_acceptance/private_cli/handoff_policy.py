"""Pure handoff binding and supplied process-registry identity decisions."""

from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_closure_evidence
import json


def require_handoff_task_binding(metadata_bytes, expected_binding):
    """Compare task metadata to its authoritative inspection journal binding."""
    require(isinstance(expected_binding, dict) and expected_binding, "inspection journal supervisor binding is missing")
    metadata = json.loads(metadata_bytes)
    require(isinstance(metadata, dict), "handoff task metadata is not an object")
    require(
        metadata.get("supervisor_config") == expected_binding,
        "handoff task metadata did not bind the inspection journal supervisor",
    )


def external_handoff_closure_evidence(closure, task_id):
    """Require normal completion to retain the same lifetime proof as stop tests."""
    require(isinstance(closure, dict), "lifetime closure is invalid: " + task_id)
    worker_wait = closure.get("worker_wait")
    termination = closure.get("termination")
    require(
        isinstance(worker_wait, dict) and isinstance(termination, dict),
        "lifetime closure omitted required evidence: " + task_id,
    )
    cause = termination.get("cause")
    require(isinstance(cause, str) and cause, "lifetime closure omitted its termination cause: " + task_id)
    return default_stop_closure_evidence(closure, task_id, cause)
