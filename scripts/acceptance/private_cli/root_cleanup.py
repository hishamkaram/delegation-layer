"""Private CLI root cleanup boundary."""

from pathlib import Path
import os

from scripts.acceptance.directories.operations import OwnedDirectory
from scripts.acceptance.directories.operations import rollback_owned_directories
from scripts.acceptance.directories.policy import require
from scripts.acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.acceptance.private_cli.noexec_state import custody_release_pending
from scripts.acceptance.private_cli.noexec_state import describe_pending_custody
from scripts.acceptance.private_cli.supervisor import authorize_managed_lifetime_closure
from scripts.acceptance.private_cli.supervisor import read_managed_closure


def remove_private_acceptance_root(
    root_token: OwnedDirectory,
    owned_directories: tuple[OwnedDirectory, ...],
    managed_state_roots=(),
    *,
    disposal=None,
    deadline=None,
    reader=None,
):
    """Authorize managed closure and owned custody release, then remove the exact owned root.

    A managed root that still has a private supervisor is removed only under the
    ``disposal`` owner's held lease, taken here within ``deadline`` unless an
    earlier shutdown already holds it; closure and custody are rechecked under
    it. Canonical closure and empty custody alone do not prove that elected
    maintenance finished. A late write or deletion fault retains and raises.
    """
    if not isinstance(root_token, OwnedDirectory):
        raise RuntimeError("private acceptance root identity token is required")
    if root_token.directory.parent != root_token.parent or not root_token.directory.name.startswith(
        PRIVATE_ACCEPTANCE_PREFIX
    ):
        raise RuntimeError(
            "private acceptance root is outside its exact owned parent child: " + str(root_token.directory)
        )
    entries = tuple(owned_directories)
    if not any(entry is root_token for entry in entries):
        raise RuntimeError("private acceptance root token is absent from ownership sequence")
    managed = tuple(
        entry if isinstance(entry, tuple) and len(entry) == 2 else (entry, ()) for entry in managed_state_roots
    )
    owned_roots = tuple(entry.directory for entry in entries)
    # A refusal before the lease leaves every root byte unchanged; the lease then
    # excludes elected maintenance and the same facts are rechecked under it.
    _require_disposal_ready(managed, owned_roots, reader=reader)
    for state in sorted({Path(state) for state, _task_ids in managed}, key=str):
        if os.path.lexists(state / ".supervisor"):
            if disposal is None:
                raise RuntimeError("private supervisor root has no disposal lease: " + str(state))
            require(
                disposal.holds(state) or deadline is not None,
                "private supervisor disposal lease has no deadline: " + str(state),
            )
            disposal.hold(state, deadline)
    _require_disposal_ready(managed, owned_roots, reader=reader)
    rollback_owned_directories(entries)
    if root_token.directory.exists() or root_token.directory.is_symlink():
        raise RuntimeError("private acceptance root deletion was not verified: " + str(root_token.directory))


def _require_disposal_ready(managed, owned_roots, *, reader=None):
    for state, task_ids in managed:
        closure = (authorize_managed_lifetime_closure(state, tuple(task_ids)) if reader is None
                   else read_managed_closure(state, tuple(task_ids), reader))
        pending = custody_release_pending(closure, owned_roots)
        require(not pending, "custody release remained pending before root removal: " + describe_pending_custody(pending))
