#!/usr/bin/env python3
"""Native managed-workspace acceptance through the shipped Delegate CLI.

This driver is deliberately fixture-only.  It uses the rebuilt Delegate and
runner supplied by the caller, plus the real pueue pair, while the provider is
the existing finite private fixture.  Process ownership, command capture,
lease observation, and directory rollback remain in the shared acceptance
helpers.
"""

from pathlib import Path
import argparse
import json
import os
import platform
import re
import shlex
import shutil
import sys
import tempfile
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed

if not __package__:
    repository_root = Path(__file__).resolve().parents[1]
    if str(repository_root) not in sys.path:
        sys.path.insert(0, str(repository_root))

import scripts.acceptance_private_cli as private_cli
from scripts.acceptance_provider_common import validate_managed_workspace_operation_closure, verify_collected_outcome
from scripts.acceptance_supervisor_common import (
    DirectoryOwner, OwnedDirectory, Process, Processes, digest, inherited_environment,
    read_json, require, write_json,
)
from scripts.delegate_acceptance.workspaces.scenarios.model import (
    AcceptanceBlocked,
    DRIVER_DESCRIPTION,
    TASK_ID_RE,
    OID_RE,
    SHA256_RE,
    MAX_RECEIPT_BYTES,
    MAX_RESPONSE_BYTES,
    MAX_COMPACT_DEPTH,
    MAX_COMPACT_ITEMS,
    MAX_COMPACT_STRING,
    DEFAULT_WORKSPACE_BUDGET,
    DEFAULT_DISPATCH_WORKERS,
    ADMISSION_COMMAND_TIMEOUT_SECONDS,
    DEFAULT_CAPACITY_TASKS,
    SUCCESS_TASK_BUDGET,
    TIMEOUT_TASK_BUDGET,
    TIMEOUT_CONTINUE_TASK_BUDGET,
    CAPACITY_TASK_BUDGET,
    WORKSPACE_OPERATION_BUDGET,
    WAIT_SECONDS,
    TIMEOUT_WAIT_SECONDS,
    FIXTURE_SESSION,
    FIXTURE_FILE,
)
from scripts.delegate_acceptance.workspaces.scenarios.context import (
    AcceptanceSetupFailure,
    WorkspaceContext,
)
from scripts.delegate_acceptance.workspaces.scenarios.evidence import (
    _compact,
    file_fingerprint,
    ReceiptFinalizer,
)
from scripts.delegate_acceptance.workspaces.scenarios.policy import (
    _status_value,
    classify_control_response,
    classify_task_verdict,
    _task_id_matches_continuation,
    cleanup_decision,
    _validate_hex,
)
from scripts.delegate_acceptance.workspaces.scenarios.source import (
    REPOSITORY,
    ENTRYPOINT_PATH,
    _git_executable,
    _git_process,
    _require_executable,
    _git,
    _git_ok,
    _prepare_source,
    _assert_source_untouched,
    _git_input_paths,
    build_source_manifest,
    _install_workspace_fixture,
    _write_briefs,
)
from scripts.delegate_acceptance.workspaces.scenarios.operations import (
    _control,
    _workspace_from,
    _workspace_id,
    _operation_id,
    _wait_workspace,
    _adopt_workspace_container,
    _admit_created_task,
    _dispatch,
    _continue,
    _await_task,
    _collect_task,
    _prove_lifetimes_closed,
    _wait_capture,
    _provider_session,
    _fixture_session_marker,
    _task_record,
    _immutable_task_snapshot,
    _assert_immutable_task_snapshot,
    _validate_continuation_binding,
)
from scripts.delegate_acceptance.workspaces.scenarios.workspace_operations import (
    _capture_roundtrip,
    _workspace_exec,
    _workspace_mutation,
    _verify_ref,
    _release_and_forget,
    _forget_workspace,
    _finish_cohort,
    verify_capture_receipt,
)
from scripts.delegate_acceptance.workspaces.scenarios.lifecycle import (
    _run_success,
)
from scripts.delegate_acceptance.workspaces.scenarios.timeout import (
    _wait_for_timeout,
    _run_timeout,
)
from scripts.delegate_acceptance.workspaces.scenarios.capacity import (
    _dispatch_capacity_one,
    _settle_capacity_one,
    _release_forget_capacity_one,
    _run_capacity,
)
from scripts.delegate_acceptance.workspaces.scenarios.application import (
    _make_context,
    _make_context_unchecked,
    _run,
    _parse_args,
    main,
)


if __name__ == "__main__":
    raise SystemExit(main())
