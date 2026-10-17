"""Exercise private supervisor reuse across two installed CLI layouts."""

import argparse
from contextlib import ExitStack, contextmanager
from concurrent.futures import ThreadPoolExecutor
import errno
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import resource
import shlex
import shutil
import socket
import stat
import sys
import tempfile
import threading
import time
import uuid

# Both module invocation and direct-file execution use namespaced shared owners.
if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.acceptance_provider_common import (
    AcceptanceFailure,
    canonical_go_json,
    ordinary_lifetime_owner_digest,
    snapshot,
    validate_managed_lifetime_closure,
)
from scripts.acceptance_supervisor_common import (
    _close_parent_descriptor,
    _open_parent_directory,
    config_for,
    DirectoryOwner,
    DirectoryOwner as SupervisorDirectoryOwner,
    OwnedDirectory,
    Processes,
    remove_exact_owned_fifo,
    remove_owned_directory,
    require,
    read_json,
    rollback_owned_directories,
    wait_until,
    write_json,
)


# Leave time for the two-minute task budget and supervisor stop observation.
# Capacity fixtures remain gated while the entire cohort is admitted and
# observed through the upgraded installation. That orchestration can exceed
# ten minutes on a loaded host; it is not the task-timeout acceptance case.


from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TARGET_TASKS
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.capture import CAPTURE_START_LOCK
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_DISPATCH_WORKERS
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import LEASE_HANDSHAKE_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLease
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate
from scripts.delegate_acceptance.private_cli.fixtures import PI_FIXTURE
from scripts.delegate_acceptance.private_cli.fixtures import PI_WAIT_PROGRAM
from scripts.delegate_acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.delegate_acceptance.private_cli.configuration import PROTECTED_CALLER_PHASES
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.configuration import STARTUP_CONTROL_COMMANDS
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET
from scripts.delegate_acceptance.private_cli.capture import _assert_capture_root
from scripts.delegate_acceptance.private_cli.capture import _capture_opener
from scripts.delegate_acceptance.private_cli.capture import _capture_path
from scripts.delegate_acceptance.private_cli.capture import _open_capture_writer
from scripts.delegate_acceptance.private_cli.capture import _read_capture
from scripts.delegate_acceptance.private_cli.capture import _reap_private_control_process
from scripts.delegate_acceptance.private_cli.capture import _retain_capture_failure
from scripts.delegate_acceptance.private_cli.leases import _validate_live_parent_pin
from scripts.delegate_acceptance.private_cli.capture import _wait_private_control_process
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.supervisor import authorize_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.admission import await_created_task_ended
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.wrapper_policy import compact_sequence_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_closure_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_dispatch_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_lease_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_status_row
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_stop_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import descriptor_evidence
from scripts.delegate_acceptance.private_cli.handoff_policy import external_handoff_closure_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
from scripts.delegate_acceptance.private_cli.installation import install_adjacent_package
from scripts.delegate_acceptance.private_cli.installation import install_fixture
from scripts.delegate_acceptance.private_cli.installation import install_package_layout
from scripts.delegate_acceptance.private_cli.fixtures import install_pi_fixture
from scripts.delegate_acceptance.private_cli.wrappers import install_wrapper
from scripts.delegate_acceptance.private_cli.capacity_ownership import lease_candidate_specifications
from scripts.delegate_acceptance.private_cli.lease_policy import lease_fifo_path
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
from scripts.delegate_acceptance.private_cli.capture import output_tail
from scripts.delegate_acceptance.private_cli.installation import package_layout
from scripts.delegate_acceptance.private_cli.environment import phase_environment
from scripts.delegate_acceptance.private_cli.supervisor import private_daemon_process_gone
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_endpoint_closed
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.delegate_acceptance.private_cli.wrapper_policy import protected_wrapper_calls
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.admission import read_created_task_status
from scripts.delegate_acceptance.private_cli.handoff_journal import read_external_inspection_handoff_journal
from scripts.delegate_acceptance.private_cli.capture import reap_captured_process
from scripts.delegate_acceptance.private_cli.commands import record_invocation_error
from scripts.delegate_acceptance.private_cli.handoff_supervisor import registered_process_handle
from scripts.delegate_acceptance.private_cli.root_cleanup import remove_private_acceptance_root
from scripts.delegate_acceptance.private_cli.handoff_policy import require_handoff_task_binding
from scripts.delegate_acceptance.private_cli.wrapper_policy import require_no_startup_control_calls
from scripts.delegate_acceptance.private_cli.capture import run_captured_command
from scripts.delegate_acceptance.private_cli.capture import run_captured_text_command
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_remaining
from scripts.delegate_acceptance.private_cli.queue_policy import task_row_by_label
from scripts.delegate_acceptance.private_cli.queue_policy import task_rows_by_label
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_pueue_state
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_queued_label_prefix
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_command_counts


from scripts.delegate_acceptance.private_cli.packaged_defaults import packaged_default_commands
from scripts.delegate_acceptance.private_cli.packaged_defaults import isolated_default_state_path
from scripts.delegate_acceptance.private_cli.packaged_defaults import verify_packaged_default_path
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from scripts.delegate_acceptance.private_cli.capacity import verify_current_installation_upgrade
from scripts.delegate_acceptance.private_cli.stop import default_stop_live_lease
from scripts.delegate_acceptance.private_cli.leases import require_empty_fixture_waiters
from scripts.delegate_acceptance.private_cli.stop import run_default_stop_acceptance
from scripts.delegate_acceptance.private_cli.handoff_cleanup import prepare_external_supervisor_directories
from scripts.delegate_acceptance.private_cli.handoff_cleanup import cleanup_external_handoff_cohorts
from scripts.delegate_acceptance.private_cli.handoff import run_external_inspection_handoff_acceptance
from scripts.delegate_acceptance.private_cli.upgrade import private_fixture_transition
from scripts.delegate_acceptance.private_cli.upgrade import seed_historical_private_paths
from scripts.delegate_acceptance.private_cli.upgrade import run_pending_package_upgrade
from scripts.delegate_acceptance.private_cli.upgrade import run_supervisor_upgrade_recovery


from scripts.delegate_acceptance.private_cli.application import main


if __name__ == "__main__":
    main()
