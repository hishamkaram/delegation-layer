"""Antigravity native response validation."""

from __future__ import annotations

import json

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require

def validate_l1_write_denial(stdout: bytes, stderr: bytes, conversation: str) -> None:
    """Validate a controlled denial, never authorize result publication."""
    require(len(stdout) <= 2 * 1024 * 1024, "L1 denial envelope exceeds observation bound")
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "L1 denial envelope has duplicate keys")
            result[key] = value
        return result
    def reject_constant(value):
        raise ValueError("nonfinite JSON")
    try:
        value = json.loads(stdout.decode("utf-8"), object_pairs_hook=unique_object,
                           parse_constant=reject_constant)
    except (ValueError, UnicodeError) as error:
        raise AcceptanceFailure("L1 denial envelope is not strict JSON") from error
    require(isinstance(value, dict), "L1 denial envelope is not an object")
    require(set(value) <= {"conversation_id", "status", "response", "duration_seconds", "num_turns", "usage", "denied_actions"},
            "L1 denial has unknown fields")
    require(isinstance(conversation, str) and conversation and
            value.get("conversation_id") == conversation and value.get("status") == "SUCCESS" and
            isinstance(value.get("response"), str), "L1 denial identity/status/response mismatch")
    actions = value.get("denied_actions")
    require(isinstance(actions, list) and len(actions) == 1 and isinstance(actions[0], dict) and
            set(actions[0]) <= {"action", "display_name"} and actions[0].get("action") == "write_file" and
            isinstance(actions[0].get("display_name"), str) and actions[0]["display_name"],
            "L1 did not record exactly one write_file denial")
    require(b'"write_file" permission' in stderr and b"auto-denied" in stderr and
            b"[agy] print timeout" not in stderr,
            "L1 lacks positive headless write-denial diagnostic")
