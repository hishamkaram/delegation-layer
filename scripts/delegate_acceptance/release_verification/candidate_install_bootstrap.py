"""The installed default fixture turn behind the candidate install BootstrapProbe port.

The install proof application supplies this probe to the channel observers. One call
owns one short private root below ``parent`` through its own DirectoryOwner, installs
the existing shell pi fixture, resolves ``delegate`` through the target's PATH, records
that entry, and runs the private CLI's shared installed-default core with it: dispatch,
status, collect, the dispatched task's own managed lifetime closure and a verified
daemon and endpoint shutdown. Its children run through that core's identity-pinned
file capture, not ConsumerPorts.run.

One local DirectoryOwner lifetime covers the root's creation, setup, the turn, failure
diagnostics and its single disposal. The intended root path is recorded before creation;
the creation token's own identity record is copied into the private facts right after
creation, so a later pathname replacement cannot change it. Only a verified turn removes
the root. Any failure keeps it: the original fault, any cleanup or release fault, the
private paths and every supervisor still pending in the ledger are recorded in the
private receipt, and the root's path is listed for ``finish_gate`` to report as retained.
That path and identity record are observations, not cleanup authority, and the root stays
outside the proof gate's workspace cleanup. A pending supervisor of an attempted dispatch,
including one whose config never appeared, is unresolved ownership. The witness carries
only the compact observation.
"""

from __future__ import annotations

from pathlib import Path
import os
import secrets
import shutil
from ..directories.journal import directory_identity_record
from ..directories.operations import (
    DirectoryOwner, OwnedDirectory, release_owned_directories, rollback_owned_directories,
)
from ..private_cli.fixtures import install_pi_fixture
from ..private_cli.packaged_defaults import (
    INSTALLED_BINARIES, InstalledBootstrap, InstalledDefaults, isolated_default_state_path,
    prove_installed_default_bootstrap,
)
from ..private_cli.supervisor import private_shutdown_evidence, shutdown_pending_private_supervisor
from .candidate_install_contract import BOOTSTRAP_SHUTDOWN, BootstrapObservation, BootstrapTarget
from .contract import BinaryDigests, VerificationError


STAGE = "candidate-bootstrap"


# The fixture's finite branch answers any prompt without its wait marker.
BRIEF = "Reply with one short acknowledgement for the installed bootstrap check.\n"


# Kept as short as the private CLI acceptance roots: on Darwin the default state root below
# HOME adds 59 characters before the generated short endpoint must fit in 103.
ROOT_PREFIX = ".dl-i"


class InstalledBootstrapProbe:
    """One invocation's BootstrapProbe; ``receipt`` and ``locations`` are the proof gate's own.

    ``parent`` must be a short, symlink-free directory: the isolated HOME below it must
    leave room for the default state root's private supervisor endpoint.
    """

    def __init__(self, *, receipt: dict, locations: dict[str, Path], parent: Path):
        self.receipt = receipt
        self.locations = locations
        self.parent = parent

    def __call__(self, target: BootstrapTarget) -> BootstrapObservation:
        owner = DirectoryOwner()
        root = self.parent / f"{ROOT_PREFIX}{secrets.token_hex(4)}"
        # The intent is recorded before the fallible creation; it grants no authority.
        facts: dict[str, object] = {"status": "partial", "parent": str(self.parent), "root": str(root)}
        self.receipt["candidate_install"]["bootstrap"] = facts
        self.locations["bootstrap_root"] = root
        ledger: list[dict] = []
        capture = None
        observation = None
        try:
            try:
                token = owner.create("candidate bootstrap root", root, self.parent)
                facts["root_identity"] = directory_identity_record(token)
                capture = owner.create("candidate bootstrap captures", root / "c", root)
                observation = _observation(_run(owner, capture, root=root, target=target, ledger=ledger,
                                                facts=facts))
            except Exception as primary:
                refusal = _retained_failure(primary, capture=capture, ledger=ledger, facts=facts)
                if refusal is None:
                    raise
                raise refusal from primary
            except BaseException:
                # No shutdown is attempted for an interruption; every pending supervisor stays recorded.
                facts["status"] = "interrupted"
                _publish_unresolved(ledger, facts)
                raise
        finally:
            _dispose(owner, verified=observation is not None, facts=facts)
        facts.update({"status": "verified", "root_id": observation.root_id, "task_id": observation.task_id})
        return observation


def _observation(result: InstalledBootstrap) -> BootstrapObservation:
    return BootstrapObservation(
        root_id=result.root_id, task_id=result.task_id,
        binaries=BinaryDigests(delegate=result.delegate_sha256, delegate_run=result.runner_sha256,
                               pueue=result.client_sha256, pueued=result.daemon_sha256),
        outcome=result.outcome, worker_result=result.worker_result,
        termination_cause=result.termination_cause, scope_empty=result.scope_empty,
        shutdown=BOOTSTRAP_SHUTDOWN)


def _dispose(owner: DirectoryOwner, *, verified: bool, facts: dict[str, object]) -> None:
    """The local owner's one disposal: only a verified turn removes the root; anything else retains it.

    Every token leaves the owner first, so no later step consumes one twice; after a partial
    removal only the still-unconsumed pins are released. A release fault is recorded and never
    replaces the propagating fault; an interrupted disposal is recorded and keeps propagating.
    """
    entries = owner.transfer()
    if not verified:
        facts["root_disposition"] = "retained"
        _release_unconsumed(entries, facts)
        return
    try:
        rollback_owned_directories(entries)
    except (OSError, RuntimeError) as error:
        facts.update({"status": "cleanup-failed", "root_disposition": "retained", "disposal_failure": _fault(error)})
        _release_unconsumed(entries, facts)
        raise VerificationError("candidate_bootstrap_cleanup_failed", STAGE) from None
    except BaseException as interrupted:
        facts.update({"status": "cleanup-interrupted", "root_disposition": "incomplete",
                      "disposal_failure": {"type": type(interrupted).__name__}})
        _release_unconsumed(entries, facts)
        raise
    facts["root_disposition"] = "removed"


def _release_unconsumed(entries: tuple[OwnedDirectory, ...], facts: dict[str, object]) -> None:
    """Consume the pins no earlier step consumed, retaining every directory."""
    remaining = tuple(entry for entry in entries if not entry.consumed)
    try:
        release_owned_directories(remaining)
    except (OSError, RuntimeError) as error:
        facts["release_failure"] = _fault(error)
    except BaseException as interrupted:
        facts["release_failure"] = {"type": type(interrupted).__name__,
                                    "unconsumed": sum(not entry.consumed for entry in remaining)}
        raise


def _run(owner: DirectoryOwner, capture: OwnedDirectory, *, root: Path, target: BootstrapTarget,
         ledger: list[dict], facts: dict[str, object]) -> InstalledBootstrap:
    home, providers, workspace, responses = (
        owner.create(label, root / name, root).directory for label, name in (
            ("candidate bootstrap home", "h"), ("candidate bootstrap providers", "p"),
            ("candidate bootstrap workspace", "w"), ("candidate bootstrap responses", "r")))
    install_pi_fixture(providers, root / "fixture-gate", root / "fixture-waiters", secrets.token_hex(8))
    brief = root / "brief.md"
    brief.write_text(BRIEF, encoding="utf-8")
    env = {"HOME": str(home), "PATH": os.pathsep.join((str(target.command_directory), str(providers))),
           "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}
    found = shutil.which("delegate", path=env["PATH"])
    facts.update({"path": env["PATH"], "delegate": found, "supervisor_directory": str(target.supervisor_directory)})
    if found is None or Path(found).resolve() != target.delegate:
        raise VerificationError("candidate_bootstrap_delegate_mismatch", STAGE)
    digests = target.binaries.as_mapping()
    installed = InstalledDefaults(
        delegate=Path(found), supervisor_directory=target.supervisor_directory,
        digests=tuple((name, digests[name]) for name in INSTALLED_BINARIES), env=env,
        state=isolated_default_state_path(home), workspace=workspace, brief=brief, response_directory=responses)
    facts["state"] = str(installed.state)
    return prove_installed_default_bootstrap(installed, supervisor_cleanups=ledger, capture_root=capture)


def _fault(error: BaseException) -> dict[str, str]:
    """The private receipt's record of one original fault; a verifier fault is named by its code."""
    if isinstance(error, VerificationError):
        return {"type": type(error).__name__, "code": error.code}
    return {"type": type(error).__name__, "message": str(error)[-2000:]}


def _retained_failure(primary: Exception, *, capture: OwnedDirectory | None,
                      ledger: list[dict], facts: dict[str, object]) -> VerificationError | None:
    """Record the original fault and shut down each pending supervisor once; the root's disposal is the caller's.

    Returns the refusal to raise, or None when ``primary`` is itself the refusal. An
    interrupted shutdown is recorded with the ledger and keeps propagating.
    """
    facts.update({"status": "failed", "failure": _fault(primary)})
    cleanup_faults: list[dict[str, object]] = []
    facts["cleanup_failures"] = cleanup_faults
    for pending in tuple(ledger):
        try:
            if capture is None or shutdown_pending_private_supervisor(pending, ledger, capture_root=capture) is None:
                cleanup_faults.append({"type": "SupervisorUnverified", "message": "no shutdown was verified"})
        except Exception as cleanup_error:
            cleanup_faults.append(_fault(cleanup_error))
        except BaseException as interrupted:
            cleanup_faults.append({"type": type(interrupted).__name__, "interrupted": True})
            _publish_unresolved(ledger, facts)
            raise
    if not cleanup_faults:
        del facts["cleanup_failures"]
    if _publish_unresolved(ledger, facts):
        return VerificationError("candidate_bootstrap_unresolved", STAGE)
    del facts["unresolved_ownership"]
    if isinstance(primary, VerificationError):
        return None
    return VerificationError("candidate_bootstrap_failed", STAGE)


def _publish_unresolved(ledger: list[dict], facts: dict[str, object]) -> list[dict[str, object]]:
    """Record every pending supervisor, then keep each one after an attempted dispatch, with its evidence.

    The local ledger stays authoritative. Its entries are copied into the private facts before any
    fallible observation, so a faulted filter or diagnostic keeps every known entry recorded and
    propagates. Only a completed observation drops an entry whose dispatch never started: without
    a task ID or a config no delegate child ever ran.
    """
    pending = tuple(ledger)
    records = [{"config": str(entry["config"]), "state": str(entry["state"]), "task_ids": list(entry["task_ids"])}
               for entry in pending]
    facts["unresolved_ownership"] = records
    unresolved = []
    for entry, record in zip(pending, records):
        if entry["task_ids"] or entry["config"].exists():
            _add_shutdown_evidence(record, entry["config"])
            unresolved.append(record)
    facts["unresolved_ownership"] = unresolved
    return unresolved


def _add_shutdown_evidence(record: dict[str, object], config: Path) -> None:
    """Add the private PID and endpoint of one supervisor whose closure no cleanup proved."""
    try:
        pid, endpoint, _ = private_shutdown_evidence(config)
        record.update({"pid": pid, "endpoint": str(endpoint)})
    except (OSError, RuntimeError, ValueError) as error:
        record["evidence"] = _fault(error)
