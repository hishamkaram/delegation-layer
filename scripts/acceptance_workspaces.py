#!/usr/bin/env python3
"""Native managed-workspace acceptance through the shipped Delegate CLI.

This driver is deliberately fixture-only.  It uses the rebuilt Delegate and
runner supplied by the caller, plus the real pueue pair, while the provider is
the existing finite private fixture.  Process ownership, command capture,
lease observation, and directory rollback remain in the shared acceptance
helpers.
"""

from pathlib import Path
import sys
import time as time
import uuid as uuid

if not __package__:
    repository_root = Path(__file__).resolve().parents[1]
    if str(repository_root) not in sys.path:
        sys.path.insert(0, str(repository_root))

import scripts.acceptance_private_cli as _private_cli
from scripts.acceptance_supervisor_common import (
    DirectoryOwner as DirectoryOwner,
    OwnedDirectory as OwnedDirectory,
    Processes as Processes,
)
from scripts.delegate_acceptance.workspaces.scenarios.model import (
    AcceptanceBlocked as AcceptanceBlocked,
    MAX_RECEIPT_BYTES as MAX_RECEIPT_BYTES,
    TIMEOUT_CONTINUE_TASK_BUDGET as TIMEOUT_CONTINUE_TASK_BUDGET,
    WAIT_SECONDS as WAIT_SECONDS,
    FIXTURE_SESSION as FIXTURE_SESSION,
    FIXTURE_FILE as FIXTURE_FILE,
)
from scripts.delegate_acceptance.workspaces.scenarios.context import (
    AcceptanceSetupFailure as AcceptanceSetupFailure,
    WorkspaceContext as WorkspaceContext,
)
from scripts.delegate_acceptance.workspaces.scenarios.evidence import (
    ReceiptFinalizer as ReceiptFinalizer,
)
from scripts.delegate_acceptance.workspaces.scenarios.policy import (
    classify_control_response as classify_control_response,
    classify_task_verdict as classify_task_verdict,
    cleanup_decision as cleanup_decision,
)
from scripts.delegate_acceptance.workspaces.scenarios.source import (
    _git_executable as _git_executable,
    _install_workspace_fixture as _install_workspace_fixture,
)
from scripts.delegate_acceptance.workspaces.scenarios.operations import (
    _wait_workspace as _wait_workspace,
    _dispatch as _dispatch,
    _continue as _continue,
    _prove_lifetimes_closed as _prove_lifetimes_closed,
    _immutable_task_snapshot as _immutable_task_snapshot,
    _assert_immutable_task_snapshot as _assert_immutable_task_snapshot,
    _validate_continuation_binding as _validate_continuation_binding,
)
from scripts.delegate_acceptance.workspaces.scenarios.workspace_operations import (
    _capture_roundtrip as _capture_roundtrip,
    _workspace_exec as _workspace_exec,
    _workspace_mutation as _workspace_mutation,
    _verify_ref as _verify_ref,
    _forget_workspace as _forget_workspace,
    verify_capture_receipt as verify_capture_receipt,
)
from scripts.delegate_acceptance.workspaces.scenarios.lifecycle import (
    _run_success as _run_success,
)
from scripts.delegate_acceptance.workspaces.scenarios.capacity import (
    _run_capacity as _run_capacity,
)
from scripts.delegate_acceptance.workspaces.scenarios.application import (
    _run as _run,
    main,
)

# Workspace callers use this compatibility module export.
private_cli = _private_cli


if __name__ == "__main__":
    raise SystemExit(main())
