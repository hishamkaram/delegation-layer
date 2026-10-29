"""Noexec Delegate state proof, recorded custody placement policy, and custody release readiness.

The caller owns the noexec parent; the harness owns only the container it creates
there. Each lifetime registration is the sole authority for the custody pool its
scope used. This module only checks those published records against the exact
execution temporary root and the pool identity marker on disk; it never derives a
pool location itself. Go's verifiedexec Pool owns the root, its marker and its
views; the static checks here mirror Go's root, marker, views and owner-link
decisions and never relax them. They read by path once and claim nothing
against a concurrent replacement.
"""

import json
import os
from pathlib import Path
import stat

from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.directories.policy import require_private_directory
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import _hex_digest
from scripts.delegate_acceptance.lifetime.policy import validate_custody_placement
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command


EXECUTION_PROBE_NAME = ".execution-probe"
EXECUTION_PROBE_SCRIPT = "#!/bin/sh\nexit 0\n"
EXECUTION_PROBE_ENVIRONMENT = {"PATH": "/usr/bin:/bin"}
EXECUTION_PROBE_TIMEOUT_SECONDS = 10
POOL_IDENTITY_NAME = ".identity"
POOL_VIEWS_NAME = "views"
MAX_REPORTED_PENDING_NAMESPACES = 8
POOL_IDENTITY_VERSION = 1
MAX_POOL_IDENTITY_BYTES = 256
# The only mode taskdir.CheckPrivateStat accepts for a private regular file.
POOL_IDENTITY_MODE = 0o600
# The permission bits verifiedexec.requirePrivateInfo forbids on a pool root, views or owner directory.
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


def custody_release_pending(closures, owned_roots):
    """Return the owner namespaces still holding views in each recorded fixture-owned pool.

    Pools come only from validated registrations; registrations without a
    placement keep their existing contract. A pool outside every owned root is
    refused before anything beneath it is read. A missing, replaced or aliased
    pool root or marker, a views directory or owner entry that is not a private
    directory of this user, or conflicting identities for one pool, is a
    concrete fault and raises; it never reads as released. An empty result is ready.
    """
    owned = tuple(Path(root).resolve() for root in owned_roots)
    pools = {}
    for kind in CUSTODY_NAMESPACE_KINDS:
        for identity, closure in sorted(closures.get(kind, {}).items()):
            registration = closure.get("registration")
            if registration is None or "custody_placement" not in registration:
                continue
            label = "%s %s" % (kind[:-1], identity)
            placement = validate_custody_placement(registration["custody_placement"], label + " custody placement")
            recorded = pools.setdefault(placement["pool_root"], (label, placement["pool_id"]))
            require(
                recorded[1] == placement["pool_id"],
                "%s and %s record conflicting identities for custody pool %s"
                % (recorded[0], label, placement["pool_root"]),
            )
    pending = []
    for pool_root_text, (label, pool_id) in sorted(pools.items()):
        pool_root = Path(pool_root_text)
        require(
            any(root in pool_root.parents for root in owned),
            label + " custody pool is outside the fixture-owned roots: " + pool_root_text,
        )
        (canonical,) = _resolve_existing(label, pool_root)
        require(canonical == pool_root, label + " custody pool root is not canonical")
        require(_pool_marker_id(label, pool_root) == pool_id, label + " custody pool marker does not match its registration")
        pending.extend(pool_root_text + "/" + POOL_VIEWS_NAME + "/" + name for name in _pool_view_owners(label, pool_root))
    return tuple(pending)


def describe_pending_custody(pending):
    """Bound a pending-release diagnostic to a few owner namespace paths and the total count."""
    shown = ", ".join(pending[:MAX_REPORTED_PENDING_NAMESPACES])
    hidden = len(pending) - MAX_REPORTED_PENDING_NAMESPACES
    return "%d owner namespaces: %s%s" % (len(pending), shown, " and %d more" % hidden if hidden > 0 else "")


def _pool_view_owners(label, pool_root):
    """Return the owner namespaces present in a private views directory of the pool.

    ReleasePending observes only its requested namespace; this fixture reader
    conservatively checks every entry like that requested link of Go's
    observeDirectory. An entry released after the listing is absent; any other
    entry that is not a private directory of this user is a concrete fault.
    """
    views = pool_root / POOL_VIEWS_NAME
    try:
        info = views.lstat()
    except FileNotFoundError:
        info = None
    except OSError as error:
        raise RuntimeError(label + " custody pool views are unreadable: " + str(error)) from error
    # Fixture layout assertion, not the product contract: Prepare creates views
    # before a placement is recorded, so a prepared registered pool without it
    # is not this fixture's layout. ReleasePending itself accepts a missing views.
    require(info is not None, label + " fixture layout assertion failed: registered custody pool has no views directory")
    require_private_directory(
        info, label + " custody pool views is not a private directory of this user", owner_uid=os.geteuid()
    )
    try:
        names = sorted(entry.name for entry in views.iterdir())
    except OSError as error:
        raise RuntimeError(label + " custody pool views are unreadable: " + str(error)) from error
    return [name for name in names if _view_owner_present(label, views / name)]


def _view_owner_present(label, owner):
    try:
        info = owner.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise RuntimeError(label + " custody pool view owner " + owner.name + " is unreadable: " + str(error)) from error
    require_private_directory(
        info,
        label + " custody pool view owner " + owner.name + " is not a private directory of this user",
        owner_uid=os.geteuid(),
    )
    return True


def _resolve_existing(label, *paths):
    try:
        return tuple(path.resolve(strict=True) for path in paths)
    except OSError as error:
        raise RuntimeError(label + " custody pool or its comparison root is unreadable: " + str(error)) from error


def _pool_marker_id(label, pool_root):
    """Return the marker id of a private pool root; refuse a root or marker Go refuses statically.

    Mirrors proveIdentity's root check, requirePrivateInfo (a real directory
    owned by the effective user without group or world bits), then
    requireIdentityFile (a bounded regular file owned by the effective user
    with exactly mode 0600) and decodePoolIdentity (only the bytes Go's
    json.Marshal writes for a version 1 record with a 64-lowercase-hex id).
    """
    marker = pool_root / POOL_IDENTITY_NAME
    try:
        require_private_directory(
            pool_root.lstat(),
            label + " custody pool root is not a private directory of this user",
            owner_uid=os.geteuid(),
        )
        info = marker.lstat()
        require(
            stat.S_ISREG(info.st_mode) and info.st_size <= MAX_POOL_IDENTITY_BYTES,
            label + " custody pool identity marker is not a bounded regular file",
        )
        require(
            info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == POOL_IDENTITY_MODE,
            label + " custody pool identity marker has an unsafe owner or mode",
        )
        raw = marker.read_bytes()
        record = json.loads(raw)
    except OSError as error:
        raise RuntimeError(label + " custody pool identity marker is unreadable: " + str(error)) from error
    except ValueError as error:
        raise RuntimeError(label + " custody pool identity marker is not JSON") from error
    pool_id = record.get("pool_id") if isinstance(record, dict) else None
    require(
        _hex_digest(pool_id)
        and raw == canonical_go_json({"version": POOL_IDENTITY_VERSION, "pool_id": pool_id}, newline=False),
        label + " custody pool identity marker is not the canonical version 1 record",
    )
    return pool_id
