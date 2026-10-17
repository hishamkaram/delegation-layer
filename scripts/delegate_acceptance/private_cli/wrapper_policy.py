"""Private CLI wrapper policy boundary."""

from scripts.delegate_acceptance.private_cli.configuration import PROTECTED_CALLER_PHASES
from scripts.delegate_acceptance.private_cli.configuration import STARTUP_CONTROL_COMMANDS
import hashlib
import json


def protected_wrapper_calls(records):
    return [
        record
        for record in records
        if record["phase"] in PROTECTED_CALLER_PHASES
        and (record["role"] == "daemon" or record["command"] in STARTUP_CONTROL_COMMANDS)
    ]


def require_no_startup_control_calls(records):
    forbidden = protected_wrapper_calls(records)
    if forbidden:
        raise RuntimeError(
            "managed-runner or inspection startup control command observed: " + json.dumps(forbidden, sort_keys=True)
        )


def compact_sequence_evidence(values):
    encoded = json.dumps(values, sort_keys=True, separators=(",", ":")).encode()
    return {"count": len(values), "sha256": hashlib.sha256(encoded).hexdigest()}
