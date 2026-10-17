"""Compatibility exports for the canonical shared acceptance foundation.

Patch the defining module when replacing implementation dependencies in tests.
"""

from __future__ import annotations

from copy import deepcopy
from datetime import datetime
from datetime import timedelta
from datetime import timezone
from pathlib import Path
from typing import Callable
from typing import Iterable
import errno
import fcntl
import json
import os
import pwd
import re
import shlex
import stat
import sys
import time
import unicodedata

# Direct script execution and legacy flat imports place scripts/ on sys.path.
# Add its parent so every implementation loads under one canonical namespace.
if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.delegate_acceptance.evidence.encoding import (
    canonical_go_json,
)
from scripts.delegate_acceptance.evidence.json_decoder import (
    _SIMPLE_FOLD_TABLE,
    _SIMPLE_FOLD_TRANSLATION,
    _load_simple_fold_table,
    unique_object,
)
from scripts.delegate_acceptance.evidence.files import (
    digest,
    read_json,
    write_json,
)
from scripts.delegate_acceptance.evidence.digests import (
    sha,
)
from scripts.delegate_acceptance.evidence.limits import (
    MAX_CONTROL_BYTES,
    MAX_PROVIDER_BYTES,
)
from scripts.delegate_acceptance.evidence.records import (
    _read_canonical_control_record,
    _read_control_record,
    _read_optional_control_record,
    _read_strict_json,
    _state_root_identity,
    bounded_text,
    parse_json_output,
    status_jobs,
)
from scripts.delegate_acceptance.evidence.timestamps import (
    _TIMESTAMP_PATTERN,
    _canonical_timestamp,
    _timestamp_at_or_after,
    _timestamp_before,
)
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    _required_keys,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.inspections.authority import (
    _inspection_exit_authority,
    _inspection_lifetime_control_started,
    _inspection_managed_authority_started,
    _validate_unsubmitted_inspection_records,
)
from scripts.delegate_acceptance.inspections.binding import (
    ADMISSION_INSPECTION_TIMEOUT_SECONDS,
    INSPECTION_GROUP_PREFIX,
    MODEL_DISCOVERY_TIMEOUT_SECONDS,
    _INSPECTION_BINDING_KEYS,
    _INSPECTION_BINDING_OPTIONAL_KEYS,
    _RUNTIME_INSPECTION_REVISION,
    _inspection_group,
    _inspection_group_parallelism,
    _inspection_label,
    _is_private_supervisor_config,
    _uses_concurrent_inspection_group,
    _validate_claim_record,
    _validate_inspection_binding,
)
from scripts.delegate_acceptance.lifetime.absence_policy import (
    _stable_lifetime_boot_identity,
    _validate_lifetime_absence_completion,
)
from scripts.delegate_acceptance.lifetime.closure import (
    _ordinary_lifetime_control_started,
    _ordinary_supervisor_handoff_started,
    _scan_managed_inspection_lifetime_closure,
    _scan_managed_ordinary_lifetime_closure,
    validate_managed_lifetime_closure,
)
from scripts.delegate_acceptance.lifetime.evidence import (
    _validate_intent_only_lifetime_absence,
    _validate_managed_inspection_lifetime,
    _validate_managed_lifetime,
    _validate_managed_ordinary_lifetime,
)
from scripts.delegate_acceptance.lifetime.identity import (
    MAX_LIFETIME_LAUNCHES,
    _WORKSPACE_LIFETIME_RECORDS,
    _lifetime_launch_sequence,
    _lifetime_record_name,
    _lifetime_token,
    ordinary_lifetime_owner_digest,
)
from scripts.delegate_acceptance.lifetime.unlaunched import (
    _auxiliary_lifetime_unlaunched,
    _operation_lifetime_owner_live,
)
from scripts.delegate_acceptance.lifetime.policy import (
    _canonical_unlaunched_bytes,
)
from scripts.delegate_acceptance.paths.operations import (
    ensure_private_directory,
    no_prompt_argv,
    path_is_within,
    reject_tmp,
    write_bytes,
)
from scripts.delegate_acceptance.paths.policy import (
    _raw_clean_absolute_path,
    clean_absolute,
)
from scripts.delegate_acceptance.processes.observation import (
    failure_cleanup_deadline,
    wait_for_failure_cleanup,
)
from scripts.delegate_acceptance.supervisor.binding import (
    _SUPERVISOR_BINDING_KEYS,
    _SUPERVISOR_BINDING_OPTIONAL_KEYS,
    _SUPERVISOR_JSON_ORDER,
    _SUPERVISOR_RESOLUTION_KEYS,
    _canonical_supervisor_ref,
    _validate_supervisor_binding,
    observed_pueue_version,
)
from scripts.delegate_acceptance.supervisor.configuration import (
    _absolute_environment_or,
    pueue_command,
    supervisor_binding,
    supervisor_resolution,
)
from scripts.delegate_acceptance.supervisor.config_policy import (
    config_for,
    resolved_supervisor_config,
    resolved_supervisor_config_digest,
)
from scripts.delegate_acceptance.supervisor.observation import (
    wait_runner_done,
    wait_runner_terminal,
)
from scripts.delegate_acceptance.supervisor.queue_policy import (
    PUEUE_STATE_VARIANTS,
    _PUEUE_GROUP_KEYS,
    _PUEUE_ROW_KEYS,
    _PUEUE_STATUS_KEYS,
    _validate_pueue_state,
    _validate_pueue_status,
    _validate_pueue_timestamp,
    canonical_queue,
    done_result,
    failure_queue_finished,
    row_state,
    validate_queue_row,
)
from scripts.delegate_acceptance.tasks.metadata import (
    _EFFECTIVE_CONFIG_KEYS,
    _META_OPTIONAL_KEYS,
    _META_REQUIRED_KEYS,
    _PREDICATE_KEYS,
)
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.tasks.refusal_policy import (
    AUTHENTICATION_REFUSAL_ERROR,
    INSPECTION_FAILURE_RESULTS,
    STATIC_REFUSAL_ERROR,
    _STATIC_REFUSAL_RESPONSE_KEYS,
)
from scripts.delegate_acceptance.tasks.publication import (
    read_outcome_payload,
    snapshot,
    task_snapshot,
    verify_collected_outcome,
)
from scripts.delegate_acceptance.tasks.runtime_metadata import (
    MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES,
    _provider_runtime_identity,
    runtime_capability_sha,
    validate_provider_runtime_metadata,
)
from scripts.delegate_acceptance.tasks.specification import (
    _MAX_INT64,
    _MAX_UINT64,
    _PRIOR_SESSION_KEYS,
    _TASK_CONFIG_KEYS,
    _TASK_OPTIONAL_KEYS,
    _TASK_REQUIRED_KEYS,
    _WORKSPACE_BINDING_KEYS,
    _canonical_review_target,
    _canonical_task_bytes,
    _canonical_workspace_binding,
    _validate_task_record,
)
from scripts.delegate_acceptance.workspaces.admission_evidence import (
    _inspection_request_identity,
    _validate_historical_workspace_attempt,
    _workspace_admission_attempt,
)
from scripts.delegate_acceptance.workspaces.admission_identity import (
    _ADMISSION_ATTEMPT_DOMAIN,
    _admission_attempt_operation_id,
)
from scripts.delegate_acceptance.workspaces.closure import (
    _WORKSPACE_OPERATION_GUARDS,
    _WORKSPACE_OPERATION_LOCKS,
    _scan_managed_workspace_lifetime_closure,
    _validate_workspace_operation_records,
    _workspace_operation_entries,
    _workspace_operation_native_closure,
    _workspace_operation_unlaunched,
    validate_managed_workspace_operation_closure,
)
from scripts.delegate_acceptance.workspaces.specification import (
    _OPERATION_KINDS,
    _OPERATION_REQUEST_OPTIONAL_KEYS,
    _OPERATION_REQUEST_REQUIRED_KEYS,
    _OPERATION_SUBJECT_KEYS,
    _RESOURCE_REQUEST_KEYS,
    _WORKSPACE_CLOSURE_KEYS,
    _WORKSPACE_CLOSURE_OPTIONAL_KEYS,
    _WORKSPACE_RESERVATION_KEYS,
    _WORKSPACE_SUBJECT_KEYS,
    _canonical_operation_guard_bytes,
    _canonical_operation_receipt_bytes,
    _canonical_operation_request_bytes,
    _canonical_operation_result_bytes,
    _canonical_operation_subject,
    _canonical_workspace_artifact,
    _canonical_workspace_closure_bytes,
    _canonical_workspace_reservation_bytes,
    _canonical_workspace_resource_request_bytes,
    _canonical_workspace_subject,
)
