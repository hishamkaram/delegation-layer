"""Exercise private supervisor reuse across two installed CLI layouts."""

import errno as errno
import fcntl as fcntl
import os as os
from pathlib import Path
import platform as platform
import shutil as shutil
import socket as socket
import sys
import tempfile as tempfile
import time as time

# Both module invocation and direct-file execution use namespaced shared owners.
if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.acceptance_provider_common import (
    AcceptanceFailure as AcceptanceFailure,
    canonical_go_json as canonical_go_json,
    ordinary_lifetime_owner_digest as ordinary_lifetime_owner_digest,
)
from scripts.acceptance_supervisor_common import (
    _open_parent_directory as _open_parent_directory,
    config_for as config_for,
    DirectoryOwner as DirectoryOwner,
    remove_exact_owned_fifo as remove_exact_owned_fifo,
    rollback_owned_directories as rollback_owned_directories,
)


# Leave time for the two-minute task budget and supervisor stop observation.
# Capacity fixtures remain gated while the entire cohort is admitted and
# observed through the upgraded installation. That orchestration can exceed
# ten minutes on a loaded host; it is not the task-timeout acceptance case.


from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked as AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt as AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TASK_BUDGET as CAPACITY_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.capture import CAPTURE_START_LOCK as CAPTURE_START_LOCK
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH as COLLECT_WATCH
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_BUDGET as DEFAULT_STOP_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO as DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import (
    DEFAULT_STOP_TASK_BUDGET as DEFAULT_STOP_TASK_BUDGET,
)
from scripts.delegate_acceptance.private_cli.configuration import (
    DEFAULT_STOP_TASK_COUNT as DEFAULT_STOP_TASK_COUNT,
)
from scripts.delegate_acceptance.private_cli.configuration import (
    EXTERNAL_INSPECTION_HANDOFF_SCENARIO as EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
)
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLease as LiveFIFOLease
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort as LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate as LockedReleaseGate
from scripts.delegate_acceptance.private_cli.configuration import (
    PRIVATE_ACCEPTANCE_PREFIX as PRIVATE_ACCEPTANCE_PREFIX,
)
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET as TASK_BUDGET
from scripts.delegate_acceptance.private_cli.capture import _open_capture_writer as _open_capture_writer
from scripts.delegate_acceptance.private_cli.supervisor import (
    authorize_managed_lifetime_closure as authorize_managed_lifetime_closure,
)
from scripts.delegate_acceptance.private_cli.admission import await_created_task_ended as await_created_task_ended
from scripts.delegate_acceptance.private_cli.supervisor import (
    await_managed_lifetime_closure as await_managed_lifetime_closure,
)
from scripts.delegate_acceptance.private_cli.handoff_policy import (
    external_handoff_closure_evidence as external_handoff_closure_evidence,
)
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint as file_fingerprint
from scripts.delegate_acceptance.private_cli.installation import (
    install_adjacent_package as install_adjacent_package,
)
from scripts.delegate_acceptance.private_cli.fixtures import install_pi_fixture as install_pi_fixture
from scripts.delegate_acceptance.private_cli.wrappers import install_wrapper as install_wrapper
from scripts.delegate_acceptance.private_cli.capacity_ownership import (
    lease_candidate_specifications as lease_candidate_specifications,
)
from scripts.delegate_acceptance.private_cli.lease_policy import lease_fifo_path as lease_fifo_path
from scripts.delegate_acceptance.private_cli.admission import (
    observe_created_admission as observe_created_admission,
)
from scripts.delegate_acceptance.private_cli.capture import output_tail as output_tail
from scripts.delegate_acceptance.private_cli.installation import package_layout as package_layout
from scripts.delegate_acceptance.private_cli.supervisor import (
    private_daemon_process_gone as private_daemon_process_gone,
)
from scripts.delegate_acceptance.private_cli.supervisor import (
    private_shutdown_endpoint_closed as private_shutdown_endpoint_closed,
)
from scripts.delegate_acceptance.private_cli.supervisor import (
    private_shutdown_evidence as private_shutdown_evidence,
)
from scripts.delegate_acceptance.private_cli.capacity_ownership import (
    prove_lifetime_owner_cohort as prove_lifetime_owner_cohort,
)
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command as pueue_command
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot as pueue_snapshot
from scripts.delegate_acceptance.private_cli.admission import read_created_task_status as read_created_task_status
from scripts.delegate_acceptance.private_cli.handoff_journal import (
    read_external_inspection_handoff_journal as read_external_inspection_handoff_journal,
)
from scripts.delegate_acceptance.private_cli.capture import reap_captured_process as reap_captured_process
from scripts.delegate_acceptance.private_cli.handoff_supervisor import (
    registered_process_handle as registered_process_handle,
)
from scripts.delegate_acceptance.private_cli.root_cleanup import (
    remove_private_acceptance_root as remove_private_acceptance_root,
)
from scripts.delegate_acceptance.private_cli.handoff_policy import (
    require_handoff_task_binding as require_handoff_task_binding,
)
from scripts.delegate_acceptance.private_cli.wrapper_policy import (
    require_no_startup_control_calls as require_no_startup_control_calls,
)
from scripts.delegate_acceptance.private_cli.capture import run_captured_command as run_captured_command
from scripts.delegate_acceptance.private_cli.commands import run_json as run_json
from scripts.delegate_acceptance.private_cli.supervisor import (
    shutdown_private_supervisor as shutdown_private_supervisor,
)
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_remaining as shutdown_remaining
from scripts.delegate_acceptance.private_cli.queue_policy import task_row_by_label as task_row_by_label
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records as wrapper_call_records


from scripts.delegate_acceptance.private_cli.packaged_defaults import (
    packaged_default_commands as packaged_default_commands,
)
from scripts.delegate_acceptance.private_cli.packaged_defaults import (
    isolated_default_state_path as isolated_default_state_path,
)
from scripts.delegate_acceptance.private_cli.capacity_records import (
    capacity_lease_specifications as capacity_lease_specifications,
)
from scripts.delegate_acceptance.private_cli.capacity import (
    verify_current_installation_upgrade as verify_current_installation_upgrade,
)
from scripts.delegate_acceptance.private_cli.leases import (
    require_empty_fixture_waiters as require_empty_fixture_waiters,
)
from scripts.delegate_acceptance.private_cli.stop import (
    run_default_stop_acceptance as run_default_stop_acceptance,
)
from scripts.delegate_acceptance.private_cli.handoff_cleanup import (
    prepare_external_supervisor_directories as prepare_external_supervisor_directories,
)
from scripts.delegate_acceptance.private_cli.handoff_cleanup import (
    cleanup_external_handoff_cohorts as cleanup_external_handoff_cohorts,
)
from scripts.delegate_acceptance.private_cli.upgrade import (
    observe_installed_package as observe_installed_package,
)


from scripts.delegate_acceptance.private_cli.application import main


if __name__ == "__main__":
    main()
