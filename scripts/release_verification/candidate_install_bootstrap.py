"""The installed default fixture turn behind the candidate install BootstrapProbe port.

The install proof application supplies this probe to the channel observers. One call
owns one short private root below ``parent`` through its own DirectoryOwner, installs
the existing shell pi fixture, resolves ``delegate`` through the target's PATH, records
that entry, and runs the private CLI's shared installed-default core with it: dispatch,
status, collect, the dispatched task's own managed lifetime closure and a verified
daemon and endpoint shutdown. Its children run through that core's identity-pinned
file capture, not ConsumerPorts.run.

One local DirectoryOwner lifetime covers the root's creation, setup, the turn, failure
diagnostics and its single disposal. One DisposalLeases owner spans the same lifetime: the
shared core's authorized shutdown takes the state root's disposal lease through it, and the
lease stays held through the canonical owned-root removal, so no elected maintenance owner or
late callback helper can recreate the deleted root through a provisioning path. The lease is
released only after that removal. The intended root path is recorded before creation;
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
import time
from scripts.acceptance.directories.journal import directory_identity_record
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.directories.operations import OwnedDirectory
from scripts.acceptance.directories.operations import release_owned_directories
from scripts.acceptance.private_cli.fixtures import install_pi_fixture
from scripts.acceptance.private_cli.leases import DisposalLeases
from scripts.acceptance.private_cli.fixture_lifetime import SupervisorCleanup
from scripts.acceptance.private_cli.packaged_defaults import INSTALLED_BINARIES
from scripts.acceptance.private_cli.packaged_defaults import InstalledBootstrap
from scripts.acceptance.private_cli.packaged_defaults import InstalledDefaults
from scripts.acceptance.private_cli.packaged_defaults import isolated_default_state_path
from scripts.acceptance.private_cli.packaged_defaults import prove_installed_default_bootstrap
from scripts.acceptance.private_cli.root_cleanup import remove_private_acceptance_root
from scripts.acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.acceptance.private_cli.supervisor import shutdown_pending_private_supervisor
from scripts.acceptance.private_cli.supervisor import terminal_cleanup_evidence
from scripts.release_verification.candidate_install_contract import BOOTSTRAP_SHUTDOWN
from scripts.release_verification.candidate_install_contract import BootstrapObservation
from scripts.release_verification.candidate_install_contract import BootstrapTarget
from scripts.release_verification.contract import InstalledExecutables
from scripts.acceptance.evidence.source import VerificationError


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
        disposal = DisposalLeases()
        root = self.parent / f"{ROOT_PREFIX}{secrets.token_hex(4)}"
        # The intent is recorded before the fallible creation; it grants no authority.
        facts: dict[str, object] = {"status": "partial", "parent": str(self.parent), "root": str(root)}
        self.receipt["candidate_install"]["bootstrap"] = facts
        self.locations["bootstrap_root"] = root
        ledger: list[SupervisorCleanup] = []
        token = None
        capture = None
        observation = None
        managed_roots: tuple[tuple[Path, tuple[str, ...]], ...] = ()
        try:
            try:
                token = owner.create("candidate bootstrap root", root, self.parent)
                facts["root_identity"] = directory_identity_record(token)
                capture = owner.create("candidate bootstrap captures", root / "c", root)
                state, closed = _run(owner, capture, root=root, target=target, ledger=ledger, facts=facts,
                                     disposal=disposal)
                observation = _observation(closed)
                managed_roots = ((state, (closed.task_id,)),)
            except Exception as primary:
                refusal = _retained_failure(primary, capture=capture, ledger=ledger, facts=facts,
                                            disposal=disposal)
                if refusal is None:
                    raise
                raise refusal from primary
            except BaseException:
                # No shutdown is attempted for an interruption; every pending supervisor stays recorded.
                facts["status"] = "interrupted"
                _publish_unresolved(ledger, facts)
                raise
        finally:
            _dispose(owner, token, disposal=disposal, managed_roots=managed_roots,
                     verified=observation is not None, facts=facts)
        facts.update({"status": "verified", "root_id": observation.root_id, "task_id": observation.task_id})
        return observation


def _observation(result: InstalledBootstrap) -> BootstrapObservation:
    return BootstrapObservation(
        root_id=result.root_id, task_id=result.task_id,
        binaries=InstalledExecutables(delegate=result.delegate, delegate_run=result.runner,
                                       pueue=result.client, pueued=result.daemon),
        outcome=result.outcome, worker_result=result.worker_result,
        termination_cause=result.termination_cause, scope_empty=result.scope_empty,
        shutdown=BOOTSTRAP_SHUTDOWN)


def _dispose(owner: DirectoryOwner, root_token: OwnedDirectory | None, *, disposal: DisposalLeases,
             managed_roots: tuple[tuple[Path, tuple[str, ...]], ...], verified: bool,
             facts: dict[str, object]) -> None:
    """The local owner's one disposal: only a verified turn removes the root; anything else retains it.

    The disposal owner's lease is released after this removal, never before it. A lease release
    fault is recorded; it fails a verified turn as a cleanup fault and never replaces a propagating one.
    """
    entries = owner.transfer()
    try:
        _remove_or_retain(entries, root_token, disposal=disposal, managed_roots=managed_roots,
                          verified=verified, facts=facts)
    finally:
        lease_released = _release_leases(disposal, facts)
    if not lease_released and verified:
        facts["status"] = "cleanup-failed"
        raise VerificationError("candidate_bootstrap_cleanup_failed", STAGE) from None


def _remove_or_retain(entries: tuple[OwnedDirectory, ...], root_token: OwnedDirectory | None, *,
                      disposal: DisposalLeases, managed_roots: tuple[tuple[Path, tuple[str, ...]], ...],
                      verified: bool, facts: dict[str, object]) -> None:
    """Remove the exact owned root of a verified turn under the held lease; otherwise retain every directory.

    Every token leaves the owner first, so no later step consumes one twice; after a partial
    removal only the still-unconsumed pins are released. A release fault is recorded and never
    replaces the propagating fault; an interrupted disposal is recorded and keeps propagating.
    """
    if not verified:
        facts["root_disposition"] = "retained"
        _release_unconsumed(entries, facts)
        return
    try:
        if root_token is None:
            raise RuntimeError("private acceptance root identity token is required")
        # The shutdown already holds the lease; a root not yet held gets one attempt per lock, no wait.
        remove_private_acceptance_root(root_token, entries, managed_roots, disposal=disposal,
                                       deadline=time.monotonic())
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


def _release_leases(disposal: DisposalLeases, facts: dict[str, object]) -> bool:
    """Release every held root lease; record a fault and return False, keep propagating an interrupt."""
    try:
        disposal.release_all()
    except OSError as error:
        facts["lease_release_failure"] = _fault(error)
        return False
    except BaseException as interrupted:
        facts["lease_release_failure"] = {"type": type(interrupted).__name__}
        raise
    return True


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
         ledger: list[SupervisorCleanup], facts: dict[str, object],
         disposal: DisposalLeases) -> tuple[Path, InstalledBootstrap]:
    """Run the shared core for one installed root; return its state root with the closed turn."""
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
    paths = target.binaries.as_mapping()
    installed = InstalledDefaults(
        delegate=Path(found), supervisor_directory=target.supervisor_directory,
        executables=tuple((name, paths[name]) for name in INSTALLED_BINARIES), env=env,
        state=isolated_default_state_path(home), workspace=workspace, brief=brief, response_directory=responses)
    facts["state"] = str(installed.state)
    closed = prove_installed_default_bootstrap(
        installed, supervisor_cleanups=ledger, capture_root=capture, disposal=disposal)
    return installed.state, closed


def _fault(error: BaseException) -> dict[str, object]:
    """The private receipt's record of one original fault; a verifier fault is named by its code."""
    if isinstance(error, VerificationError):
        return {"type": type(error).__name__, "code": error.code}
    return {"type": type(error).__name__, "message": str(error)[-2000:]}


def _retained_failure(primary: Exception, *, capture: OwnedDirectory | None,
                      ledger: list[SupervisorCleanup], facts: dict[str, object],
                      disposal: DisposalLeases) -> VerificationError | None:
    """Record the original fault and shut down each pending supervisor once; the root's disposal is the caller's.

    Each shutdown runs under the verification's ``disposal`` owner: a lease the failed shutdown already
    took is reused, never reopened, since a second open of a held flock conflicts with this process's own.

    Returns the refusal to raise, or None when ``primary`` is itself the refusal. An
    interrupted shutdown is recorded with the ledger and keeps propagating.
    """
    facts.update({"status": "failed", "failure": _fault(primary)})
    cleanup_faults: list[dict[str, object]] = []
    facts["cleanup_failures"] = cleanup_faults
    for pending in tuple(ledger):
        try:
            if capture is None:
                cleanup_faults.append({"type": "SupervisorUnverified", "message": "no shutdown was verified"})
            else:
                shutdown_pending_private_supervisor(pending, ledger, capture_root=capture, disposal=disposal)
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


def _publish_unresolved(ledger: list[SupervisorCleanup], facts: dict[str, object]) -> list[dict[str, object]]:
    """Record every pending supervisor, then keep each one after an attempted dispatch, with its evidence.

    The local ledger stays authoritative. Its entries are copied into the private facts before any
    fallible observation, so a faulted filter or diagnostic keeps every known entry recorded and
    propagates. Only a completed observation drops an entry whose dispatch never started: without
    a task ID or a config no delegate child ever ran.
    """
    pending = tuple(ledger)
    records: list[dict[str, object]] = [
        {"config": str(entry["config"]), "state": str(entry["state"]), "task_ids": list(entry["task_ids"])}
        for entry in pending]
    facts["unresolved_ownership"] = records
    unresolved: list[dict[str, object]] = []
    for entry, record in zip(pending, records):
        record.update(terminal_cleanup_evidence(entry))
        if entry["task_ids"] or entry["config"].exists() or "owner" in entry:
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
