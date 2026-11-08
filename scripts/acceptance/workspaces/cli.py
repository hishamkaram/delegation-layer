#!/usr/bin/env python3
"""Native managed-workspace acceptance through the shipped Delegate CLI.

This driver is deliberately fixture-only. It builds Delegate and its runner
from the selected checkout and installs the verified pinned Pueue archive, while the provider is
the existing finite private fixture.  Process ownership, command capture,
lease observation, and directory rollback remain in the shared acceptance
helpers.
"""

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import time as time
import uuid as uuid




from scripts.acceptance.directories.operations import DirectoryOwner as DirectoryOwner
from scripts.acceptance.directories.operations import OwnedDirectory as OwnedDirectory
from scripts.acceptance.processes.operations import Processes as Processes
from scripts.acceptance.workspaces.scenarios.model import AcceptanceBlocked as AcceptanceBlocked
from scripts.acceptance.workspaces.scenarios.model import MAX_RECEIPT_BYTES as MAX_RECEIPT_BYTES
from scripts.acceptance.workspaces.scenarios.model import TIMEOUT_CONTINUE_TASK_BUDGET as TIMEOUT_CONTINUE_TASK_BUDGET
from scripts.acceptance.workspaces.scenarios.model import WAIT_SECONDS as WAIT_SECONDS
from scripts.acceptance.workspaces.scenarios.model import FIXTURE_SESSION as FIXTURE_SESSION
from scripts.acceptance.workspaces.scenarios.model import FIXTURE_FILE as FIXTURE_FILE
from scripts.acceptance.workspaces.scenarios.context import AcceptanceSetupFailure as AcceptanceSetupFailure
from scripts.acceptance.workspaces.scenarios.context import WorkspaceContext as WorkspaceContext
from scripts.acceptance.workspaces.scenarios.evidence import ReceiptFinalizer as ReceiptFinalizer
from scripts.acceptance.workspaces.scenarios.policy import classify_control_response as classify_control_response
from scripts.acceptance.workspaces.scenarios.policy import classify_task_verdict as classify_task_verdict
from scripts.acceptance.workspaces.scenarios.policy import cleanup_decision as cleanup_decision
from scripts.acceptance.workspaces.scenarios.source import _git_executable as _git_executable
from scripts.acceptance.workspaces.scenarios.source import _install_workspace_fixture as _install_workspace_fixture
from scripts.acceptance.workspaces.scenarios.operations import _wait_workspace as _wait_workspace
from scripts.acceptance.workspaces.scenarios.operations import _dispatch as _dispatch
from scripts.acceptance.workspaces.scenarios.operations import _continue as _continue
from scripts.acceptance.workspaces.scenarios.operations import _prove_lifetimes_closed as _prove_lifetimes_closed
from scripts.acceptance.workspaces.scenarios.operations import _immutable_task_snapshot as _immutable_task_snapshot
from scripts.acceptance.workspaces.scenarios.operations import _assert_immutable_task_snapshot as _assert_immutable_task_snapshot
from scripts.acceptance.workspaces.scenarios.operations import _validate_continuation_binding as _validate_continuation_binding
from scripts.acceptance.workspaces.scenarios.workspace_operations import _capture_roundtrip as _capture_roundtrip
from scripts.acceptance.workspaces.scenarios.workspace_operations import _workspace_exec as _workspace_exec
from scripts.acceptance.workspaces.scenarios.workspace_operations import _workspace_mutation as _workspace_mutation
from scripts.acceptance.workspaces.scenarios.workspace_operations import _verify_ref as _verify_ref
from scripts.acceptance.workspaces.scenarios.workspace_operations import _forget_workspace as _forget_workspace
from scripts.acceptance.workspaces.scenarios.workspace_operations import verify_capture_receipt as verify_capture_receipt
from scripts.acceptance.workspaces.scenarios.lifecycle import _run_success as _run_success
from scripts.acceptance.workspaces.scenarios.capacity import _run_capacity as _run_capacity
from scripts.acceptance.workspaces.scenarios.application import _run as _run
from scripts.acceptance.workspaces.scenarios.application import main


if __name__ == "__main__":
    raise SystemExit(main())
