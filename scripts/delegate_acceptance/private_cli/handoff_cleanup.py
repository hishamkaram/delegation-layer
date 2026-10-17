"""Private CLI handoff cleanup workflow."""

from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.directories.operations import DirectoryOwner as SupervisorDirectoryOwner
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.stop import require_empty_fixture_waiters


def prepare_external_supervisor_directories(ownership, external_root_token, case_label):
    """Create every configured external-supervisor parent under one owner."""
    require(isinstance(ownership, SupervisorDirectoryOwner), "external supervisor directory owner is required")
    require(isinstance(external_root_token, OwnedDirectory), "external supervisor root identity token is required")
    external_root_token.assert_live()
    external_root = external_root_token.directory
    ownership.create("external inspection supervisor state " + case_label, external_root / "state", external_root)
    ownership.create("external inspection supervisor runtime " + case_label, external_root / "run", external_root)


def cleanup_external_handoff_cohorts(
    state, task_ids, waiter_directory, cohorts, evidence, cleanup_errors, *, delegate_control_started=False
):
    """Remove fixture FIFOs only after the canonical lifetime proof succeeds."""
    active_cohorts = tuple(cohort for cohort in cohorts if cohort is not None)
    requested_task_ids = tuple(task_ids)
    closure_proven = False
    if not active_cohorts and not requested_task_ids and not delegate_control_started:
        # A post-start failure before any Delegate control begins has no
        # managed work or fixture lease to close. The external daemon is still
        # reaped by the caller after the waiter directory is checked.
        closure_proven = True
    else:
        try:
            await_managed_lifetime_closure(state, requested_task_ids, timeout=45)
        except BaseException as error:
            evidence["cleanup"]["fixture_waiter_entries"] = "retained-unproven-closure"
            cleanup_errors.append("managed lifetime closure failed: " + str(error))
        else:
            closure_proven = True
    for cohort in active_cohorts:
        if closure_proven:
            try:
                cohort.remove_closed_fifos()
            except BaseException as error:
                cleanup_errors.append("fixture FIFO cleanup failed: " + str(error))
        try:
            cohort.close()
        except BaseException as error:
            cleanup_errors.append("fixture lease close failed: " + str(error))
    if closure_proven:
        try:
            require_empty_fixture_waiters(waiter_directory)
        except BaseException as error:
            cleanup_errors.append("fixture waiter cleanup failed: " + str(error))
    return closure_proven
