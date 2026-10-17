"""Private CLI root cleanup boundary."""

from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.directories.operations import rollback_owned_directories
from scripts.delegate_acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.delegate_acceptance.private_cli.supervisor import authorize_managed_lifetime_closure


def remove_private_acceptance_root(
    root_token: OwnedDirectory, owned_directories: tuple[OwnedDirectory, ...], managed_state_roots=()
):
    """Authorize managed task closure, then remove the exact owned root."""
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
    for entry in managed_state_roots:
        if isinstance(entry, tuple) and len(entry) == 2:
            state, task_ids = entry
        else:
            state, task_ids = entry, ()
        authorize_managed_lifetime_closure(state, tuple(task_ids))
    rollback_owned_directories(entries)
    if root_token.directory.exists() or root_token.directory.is_symlink():
        raise RuntimeError("private acceptance root deletion was not verified: " + str(root_token.directory))
