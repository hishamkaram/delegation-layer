"""Pure lifetime owner identities, record names, and launch sequence rules."""

from __future__ import annotations

import re
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.evidence.validation import _hex_digest, require


MAX_LIFETIME_LAUNCHES = 64


_WORKSPACE_LIFETIME_RECORDS = {
    "lifetime-intent.json", "lifetime-registration.json", "lifetime-worker.json",
    "lifetime-worker-wait.json", "lifetime-termination.json", "lifetime-unlaunched.json",
}


def _lifetime_token(value: object) -> bool:
    return (isinstance(value, str) and bool(value) and value == value.strip() and
            "\x00" not in value and len(value.encode("utf-8")) <= MAX_CONTROL_BYTES)


def _lifetime_record_name(owner_kind: str, record: str, sequence: int | None = None) -> str:
    """Return the persisted filename used by one lifetime record owner."""
    if owner_kind in {"inspection", "workspace"}:
        names = {
            "intent": "lifetime-intent.json",
            "registration": "lifetime-registration.json",
            "worker": "lifetime-worker.json",
            "worker-wait": "lifetime-worker-wait.json",
            "termination": "lifetime-termination.json",
        }
        if record == "launch":
            require(sequence is not None, "inspection lifetime launch sequence is absent")
            return f"lifetime-launch-{sequence:020d}.json"
    else:
        require(owner_kind == "task", "lifetime owner kind is invalid")
        names = {
            "intent": "intent.json",
            "registration": "registration.json",
            "worker": "worker.json",
            "worker-wait": "worker-wait.json",
            "termination": "terminated.json",
        }
        if record == "launch":
            require(sequence is not None, "ordinary task lifetime launch sequence is absent")
            return f"{sequence:020d}.launch.json"
    require(record in names, "lifetime record kind is invalid")
    return names[record]


def _lifetime_launch_sequence(owner_kind: str, name: str) -> int | None:
    if owner_kind in {"inspection", "workspace"}:
        match = re.fullmatch(r"lifetime-launch-([0-9]{20})\.json", name)
    else:
        require(owner_kind == "task", "lifetime owner kind is invalid")
        match = re.fullmatch(r"([0-9]{20})\.launch\.json", name)
    if match is None:
        return None
    sequence = int(match.group(1))
    return sequence if sequence > 0 else None


def ordinary_lifetime_owner_digest(spec_digest: str, meta_digest: str) -> str:
    """Match task.LifetimeTaskOwnerSHA256's domain-separated owner identity."""
    require(_hex_digest(spec_digest) and _hex_digest(meta_digest),
            "ordinary task lifetime owner inputs are invalid")
    material = ("delegation-layer:lifetime-owner:task:v1\n" + spec_digest +
                "\n" + meta_digest).encode("ascii")
    return sha(material)
