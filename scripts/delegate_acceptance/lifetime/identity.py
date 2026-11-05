"""Pure lifetime owner identities, record names, and launch sequence rules."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, _hex_digest, require
from typing import Iterable


# Projections of task.MaxLifetimeLaunches and task.MaxLifetimeLaunchBytes. The
# byte budget bounds the canonical launch set independently of its count.
MAX_LIFETIME_LAUNCHES = 4096
MAX_LIFETIME_LAUNCH_BYTES = 64 * MAX_CONTROL_BYTES


# Projection of taskdir's lifetimeLaunchNaming: each store's launch file name is
# prefix, a zero-padded 20-digit uint64 sequence, then suffix.
_LIFETIME_LAUNCH_NAMING = {
    "task": ("", ".launch.json"),
    "inspection": ("lifetime-launch-", ".json"),
    "workspace": ("lifetime-launch-", ".json"),
}
_LIFETIME_LAUNCH_SEQUENCE_DIGITS = 20
_MAX_UINT64 = (1 << 64) - 1


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
            if sequence is None:
                raise AcceptanceFailure("inspection lifetime launch sequence is absent")
            return _lifetime_launch_name(owner_kind, sequence)
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
            if sequence is None:
                raise AcceptanceFailure("ordinary task lifetime launch sequence is absent")
            return _lifetime_launch_name(owner_kind, sequence)
    require(record in names, "lifetime record kind is invalid")
    return names[record]


def _lifetime_launch_name(owner_kind: str, sequence: int) -> str:
    prefix, suffix = _LIFETIME_LAUNCH_NAMING[owner_kind]
    return f"{prefix}{sequence:0{_LIFETIME_LAUNCH_SEQUENCE_DIGITS}d}{suffix}"


def _lifetime_launch_sequence(owner_kind: str, name: str) -> int | None:
    """Return a launch name's sequence, or None for a name outside the launch namespace.

    Like taskdir's lifetimeLaunchNaming.parse, a launch-shaped name whose sequence
    is noncanonical, overflows uint64, or lies outside 1..MAX_LIFETIME_LAUNCHES is
    refused rather than ignored, so it can never read as launch absence.
    """
    require(owner_kind in _LIFETIME_LAUNCH_NAMING, "lifetime owner kind is invalid")
    prefix, suffix = _LIFETIME_LAUNCH_NAMING[owner_kind]
    if (not name.startswith(prefix) or not name.endswith(suffix) or
            len(name) < len(prefix) + len(suffix)):
        return None
    digits = name[len(prefix):len(name) - len(suffix)]
    require(len(digits) == _LIFETIME_LAUNCH_SEQUENCE_DIGITS and digits.isascii() and
            digits.isdigit() and int(digits) <= _MAX_UINT64,
            f"malformed lifetime launch record name {name!r}")
    sequence = int(digits)
    require(0 < sequence <= MAX_LIFETIME_LAUNCHES,
            f"lifetime launch sequence is outside 1..{MAX_LIFETIME_LAUNCHES}: {name!r}")
    return sequence


def _lifetime_launch_sequences(owner_kind: str, names: Iterable[str], *,
                               limit: int, label: str) -> list[int]:
    """Return the contiguous launch sequences, refusing excess while iterating."""
    sequences: list[int] = []
    for name in names:
        try:
            sequence = _lifetime_launch_sequence(owner_kind, name)
        except AcceptanceFailure as error:
            raise AcceptanceFailure(f"{label}: {error}") from error
        if sequence is None:
            continue
        require(len(sequences) < limit, label)
        sequences.append(sequence)
    sequences.sort()
    require(sequences == list(range(1, len(sequences) + 1)), label)
    return sequences


def ordinary_lifetime_owner_digest(spec_digest: str, meta_digest: str) -> str:
    """Match task.LifetimeTaskOwnerSHA256's domain-separated owner identity."""
    require(_hex_digest(spec_digest) and _hex_digest(meta_digest),
            "ordinary task lifetime owner inputs are invalid")
    material = ("delegation-layer:lifetime-owner:task:v1\n" + spec_digest +
                "\n" + meta_digest).encode("ascii")
    return sha(material)
