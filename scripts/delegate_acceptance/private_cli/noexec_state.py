"""Noexec Delegate state proof: execution refusal probe and recorded custody placement policy.

The caller owns the noexec parent; the harness owns only the container it creates
there. Each lifetime registration is the sole authority for the custody pool its
scope used. This module only checks those published records against the exact
execution temporary root and the pool identity marker on disk; it never derives a
pool location itself.
"""

import json
from pathlib import Path
import stat

from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.lifetime.policy import validate_custody_placement
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command


EXECUTION_PROBE_NAME = ".execution-probe"
EXECUTION_PROBE_SCRIPT = "#!/bin/sh\nexit 0\n"
EXECUTION_PROBE_ENVIRONMENT = {"PATH": "/usr/bin:/bin"}
EXECUTION_PROBE_TIMEOUT_SECONDS = 10
POOL_IDENTITY_NAME = ".identity"
POOL_IDENTITY_VERSION = 1
MAX_POOL_IDENTITY_BYTES = 256
CUSTODY_NAMESPACE_KINDS = ("tasks", "inspections")


def require_execution_refused(noexec_directory, execution_directory):
    """Block unless an identical script runs in the execution directory and is refused in the other."""
    if _probe_execution(Path(execution_directory)) != "executed":
        raise AcceptanceBlocked("execution-probe-unavailable")
    outcome = _probe_execution(Path(noexec_directory))
    if outcome == "executed":
        raise AcceptanceBlocked("noexec-state-parent-executable")
    if outcome != "refused":
        raise AcceptanceBlocked("noexec-state-probe-inconclusive")
    return {"control": "executed", "state_container": "execution-refused"}


def _probe_execution(directory):
    probe = directory / EXECUTION_PROBE_NAME
    try:
        probe.write_text(EXECUTION_PROBE_SCRIPT)
        probe.chmod(0o700)
    except OSError as error:
        raise AcceptanceBlocked("execution-probe-unavailable") from error
    try:
        result = run_bounded_command(
            [str(probe)], EXECUTION_PROBE_ENVIRONMENT, max_output=0, timeout=EXECUTION_PROBE_TIMEOUT_SECONDS
        )
    except BoundedCommandError as error:
        # The interpreter and script bytes are identical in the control run, so a refused
        # start is attributable to the filesystem holding the probe.
        return "refused" if error.reason is CommandFailure.START_FAILED else "inconclusive"
    finally:
        probe.unlink()
    return "executed" if result.exit_code == 0 else "inconclusive"


def require_alternate_exec_custody(closures, *, state_root, execution_root):
    """Prove every registered task and inspection recorded its own pool beneath the execution root."""
    require(isinstance(closures, dict), "managed lifetime closure is invalid")
    evidence = {}
    for kind in CUSTODY_NAMESPACE_KINDS:
        namespaces = closures.get(kind)
        require(isinstance(namespaces, dict), "managed lifetime closure omitted " + kind)
        placements = {}
        for identity, closure in sorted(namespaces.items()):
            label = "%s %s" % (kind[:-1], identity)
            require(isinstance(closure, dict), label + " lifetime closure is invalid")
            registration = closure.get("registration")
            if registration is not None:
                placements[identity] = _recorded_pool(label, registration, Path(state_root), Path(execution_root))
        require(placements, "no %s lifetime registration recorded a custody placement" % kind)
        evidence[kind] = placements
    return evidence


def _recorded_pool(label, registration, state_root, execution_root):
    require(isinstance(registration, dict), label + " lifetime registration is invalid")
    require("custody_placement" in registration, label + " lifetime registration omitted its custody placement")
    placement = validate_custody_placement(registration["custody_placement"], label + " custody placement")
    pool_id = placement["pool_id"]
    pool_root_text = placement["pool_root"]
    pool_root = Path(pool_root_text)
    canonical, state, execution = _resolve_existing(label, pool_root, state_root, execution_root)
    require(canonical == pool_root, label + " custody pool root is not canonical")
    require(
        canonical != state and state not in canonical.parents,
        label + " custody pool root is inside the noexec state root",
    )
    require(execution in canonical.parents, label + " custody pool root is not beneath the execution temporary root")
    marker_id = _pool_marker_id(label, pool_root)
    require(marker_id == pool_id, label + " custody pool marker does not match its registration")
    return {"pool_root": pool_root_text, "pool_id": pool_id, "marker": "matched"}


def _resolve_existing(label, *paths):
    try:
        return tuple(path.resolve(strict=True) for path in paths)
    except OSError as error:
        raise RuntimeError(label + " custody pool or its comparison root is unreadable: " + str(error)) from error


def _pool_marker_id(label, pool_root):
    marker = pool_root / POOL_IDENTITY_NAME
    try:
        info = marker.lstat()
        require(
            stat.S_ISREG(info.st_mode) and info.st_size <= MAX_POOL_IDENTITY_BYTES,
            label + " custody pool identity marker is not a bounded regular file",
        )
        record = json.loads(marker.read_bytes())
    except OSError as error:
        raise RuntimeError(label + " custody pool identity marker is unreadable: " + str(error)) from error
    except ValueError as error:
        raise RuntimeError(label + " custody pool identity marker is not JSON") from error
    require(
        isinstance(record, dict)
        and set(record) == {"version", "pool_id"}
        and type(record["version"]) is int
        and record["version"] == POOL_IDENTITY_VERSION,
        label + " custody pool identity marker is not a version 1 record",
    )
    return record["pool_id"]
