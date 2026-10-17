"""Observe auxiliary no-launch fences and probe their existing owner leases."""

from __future__ import annotations

import errno
import fcntl
import os
import stat
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.records import _read_canonical_control_record
from scripts.delegate_acceptance.evidence.timestamps import (
    _canonical_timestamp,
    _timestamp_at_or_after,
)
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, _exact_keys, require
from scripts.delegate_acceptance.lifetime.identity import (
    _WORKSPACE_LIFETIME_RECORDS,
    _lifetime_launch_sequence,
)
from pathlib import Path




def _operation_lifetime_owner_live(directory: Path, label: str) -> bool:
    """Probe the workspace ControlLifetime owner lock without taking ownership."""
    path = directory / ".lifetime-owner.lock"
    try:
        metadata = path.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise AcceptanceFailure(f"cannot inspect {label} lease") from error
    require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode),
            f"{label} lease file is not a regular file")
    flags = os.O_RDWR | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise AcceptanceFailure(f"cannot open {label} lease") from error
    try:
        opened = os.fstat(descriptor)
        require((opened.st_dev, opened.st_ino) == (metadata.st_dev, metadata.st_ino),
                f"{label} lease file changed while probing")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as error:
            if error.errno in {errno.EACCES, errno.EAGAIN}:
                return True
            raise AcceptanceFailure(f"cannot probe {label} lease") from error
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        return False
    finally:
        os.close(descriptor)


def _auxiliary_lifetime_unlaunched(
        directory: Path, root: str, operation: str, request_digest: str,
        deadline: str, names: set[str], owner_kind: str,
        ) -> dict[str, object]:
    """Validate the typed no-native-start fence without inventing closure records."""
    require(owner_kind in {"workspace", "inspection"}, "invalid auxiliary lifetime owner")
    label = "workspace operation" if owner_kind == "workspace" else "inspection"
    require("lifetime-unlaunched.json" in names,
            f"{label} unlaunched fence is missing for {operation}")
    for name in _WORKSPACE_LIFETIME_RECORDS - {"lifetime-unlaunched.json"}:
        require(name not in names,
                f"{label} unlaunched fence conflicts with {operation}/{name}")
    require(not any(_lifetime_launch_sequence(owner_kind, name) is not None
                    for name in names),
            f"{label} unlaunched fence conflicts with native launches for {operation}")
    require(not _operation_lifetime_owner_live(
        directory, f"{label} {operation}"),
        f"{label} lifetime owner is still active: {operation}")
    fence, fence_digest = _read_canonical_control_record(
        directory / "lifetime-unlaunched.json",
        f"{label} unlaunched fence for {operation}",
        lambda value: _canonical_unlaunched_bytes(
            value, root, operation, request_digest, deadline,
            f"{label} unlaunched fence for {operation}", owner_kind=owner_kind),
    )
    return {
        "mode": "unlaunched",
        "request_sha256": request_digest,
        "unlaunched": fence,
        "unlaunched_sha256": fence_digest,
    }

from scripts.delegate_acceptance.lifetime.policy import (
    _canonical_unlaunched_bytes,
)
