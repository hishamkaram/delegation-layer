"""Compatibility exports for the canonical shared acceptance foundation.

Patch the defining module when replacing implementation dependencies in tests.
"""

# Preserve the facade's published standard-library exports for compatibility.
from copy import deepcopy as deepcopy
from datetime import (
    datetime as datetime,
    timedelta as timedelta,
    timezone as timezone,
)
from pathlib import Path
from typing import (
    Callable as Callable,
    Iterable as Iterable,
)
import errno as errno
import fcntl as fcntl
import json as json
import os as os
import pwd as pwd
import re as re
import shlex as shlex
import stat as stat
import sys
import time as time
import unicodedata as unicodedata

# Direct script execution and legacy flat imports place scripts/ on sys.path.
# Add its parent so every implementation loads under one canonical namespace.
if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.delegate_acceptance.evidence.encoding import (
    canonical_go_json as canonical_go_json,
)
from scripts.delegate_acceptance.evidence.json_decoder import (
    unique_object as unique_object,
)
from scripts.delegate_acceptance.evidence.files import (
    digest as digest,
    read_json as read_json,
    write_json as write_json,
)
from scripts.delegate_acceptance.evidence.digests import (
    sha as sha,
)
from scripts.delegate_acceptance.evidence.limits import (
    MAX_CONTROL_BYTES as MAX_CONTROL_BYTES,
)
from scripts.delegate_acceptance.evidence.records import (
    _read_control_record as _read_control_record,
    bounded_text as bounded_text,
    parse_json_output as parse_json_output,
    status_jobs as status_jobs,
)
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure as AcceptanceFailure,
    require as require,
)
from scripts.delegate_acceptance.inspections.binding import (
    _inspection_group as _inspection_group,
    _inspection_label as _inspection_label,
    _validate_inspection_binding as _validate_inspection_binding,
)
from scripts.delegate_acceptance.lifetime.closure import (
    _ordinary_lifetime_control_started as _ordinary_lifetime_control_started,
    validate_managed_lifetime_closure as validate_managed_lifetime_closure,
)
from scripts.delegate_acceptance.lifetime.identity import (
    ordinary_lifetime_owner_digest as ordinary_lifetime_owner_digest,
)
from scripts.delegate_acceptance.paths.operations import (
    ensure_private_directory as ensure_private_directory,
    no_prompt_argv as no_prompt_argv,
    reject_tmp as reject_tmp,
    write_bytes as write_bytes,
)
from scripts.delegate_acceptance.paths.policy import (
    clean_absolute as clean_absolute,
)
from scripts.delegate_acceptance.supervisor.binding import (
    _validate_supervisor_binding as _validate_supervisor_binding,
    observed_pueue_version as observed_pueue_version,
)
from scripts.delegate_acceptance.supervisor.configuration import (
    supervisor_binding as supervisor_binding,
)
from scripts.delegate_acceptance.supervisor.config_policy import (
    resolved_supervisor_config_digest as resolved_supervisor_config_digest,
)
from scripts.delegate_acceptance.supervisor.observation import (
    wait_runner_done as wait_runner_done,
    wait_runner_terminal as wait_runner_terminal,
)
from scripts.delegate_acceptance.supervisor.queue_policy import (
    _validate_pueue_status as _validate_pueue_status,
    canonical_queue as canonical_queue,
    done_result as done_result,
    row_state as row_state,
)
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps as NativeTaskOps
from scripts.delegate_acceptance.tasks.publication import (
    read_outcome_payload as read_outcome_payload,
    snapshot as snapshot,
    task_snapshot as task_snapshot,
    verify_collected_outcome as verify_collected_outcome,
)
from scripts.delegate_acceptance.tasks.runtime_metadata import (
    MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES as MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES,
    runtime_capability_sha as runtime_capability_sha,
    validate_provider_runtime_metadata as validate_provider_runtime_metadata,
)
from scripts.delegate_acceptance.tasks.specification import (
    _canonical_task_bytes as _canonical_task_bytes,
    _validate_task_record as _validate_task_record,
)
from scripts.delegate_acceptance.workspaces.admission_evidence import (
    _inspection_request_identity as _inspection_request_identity,
    _validate_historical_workspace_attempt as _validate_historical_workspace_attempt,
    _workspace_admission_attempt as _workspace_admission_attempt,
)
from scripts.delegate_acceptance.workspaces.specification import (
    _canonical_operation_request_bytes as _canonical_operation_request_bytes,
    _canonical_workspace_resource_request_bytes as _canonical_workspace_resource_request_bytes,
)
