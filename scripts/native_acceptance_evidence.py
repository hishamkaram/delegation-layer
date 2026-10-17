#!/usr/bin/env python3
"""Compatibility entrypoint for native acceptance evidence tools."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.directories.operations import DirectoryOwner as DirectoryOwner
from scripts.delegate_acceptance.processes.contracts import OutputCapture as OutputCapture
from scripts.delegate_acceptance.directories.operations import OwnedDirectory as OwnedDirectory
from scripts.delegate_acceptance.processes.operations import Process as Process
from scripts.delegate_acceptance.processes.operations import Processes as Processes
from scripts.delegate_acceptance.directories.journal import _CLEANUP_HEX as _CLEANUP_HEX
from scripts.delegate_acceptance.directories.journal import _CLEANUP_JOURNAL_BOUND as _CLEANUP_JOURNAL_BOUND
from scripts.delegate_acceptance.directories.journal import _CLEANUP_JOURNAL_KIND as _CLEANUP_JOURNAL_KIND
from scripts.delegate_acceptance.directories.journal import _CLEANUP_JOURNAL_VERSION as _CLEANUP_JOURNAL_VERSION
from scripts.delegate_acceptance.directories.journal import _CLEANUP_STATES as _CLEANUP_STATES
from scripts.delegate_acceptance.directories.operations import _DIRECTORY_OPEN_FLAGS as _DIRECTORY_OPEN_FLAGS
from scripts.delegate_acceptance.directories.operations import _capture_created_directory as _capture_created_directory
from scripts.delegate_acceptance.directories.operations import _check_cleanup_deadline as _check_cleanup_deadline
from scripts.delegate_acceptance.directories.journal import _cleanup_clear_contents as _cleanup_clear_contents
from scripts.delegate_acceptance.directories.journal import _cleanup_empty as _cleanup_empty
from scripts.delegate_acceptance.directories.policy import _cleanup_identity as _cleanup_identity
from scripts.delegate_acceptance.directories.policy import _cleanup_identity_matches_token as _cleanup_identity_matches_token
from scripts.delegate_acceptance.directories.journal import _cleanup_isolate as _cleanup_isolate
from scripts.delegate_acceptance.directories.journal import _cleanup_lock as _cleanup_lock
from scripts.delegate_acceptance.directories.journal import _cleanup_open_directory as _cleanup_open_directory
from scripts.delegate_acceptance.directories.journal import _cleanup_open_parent as _cleanup_open_parent
from scripts.delegate_acceptance.directories.journal import _cleanup_path_state as _cleanup_path_state
from scripts.delegate_acceptance.directories.journal import _cleanup_read_journal as _cleanup_read_journal
from scripts.delegate_acceptance.directories.journal import _cleanup_remove_empty as _cleanup_remove_empty
from scripts.delegate_acceptance.directories.journal import _cleanup_unlock as _cleanup_unlock
from scripts.delegate_acceptance.directories.journal import _cleanup_validate_journal_path as _cleanup_validate_journal_path
from scripts.delegate_acceptance.directories.operations import _close_parent_descriptor as _close_parent_descriptor
from scripts.delegate_acceptance.directories.operations import _create_cleanup_quarantine as _create_cleanup_quarantine
from scripts.delegate_acceptance.directories.operations import _create_one_directory as _create_one_directory
from scripts.delegate_acceptance.directories.operations import _directory_name as _directory_name
from scripts.delegate_acceptance.directories.operations import _is_ancestor_entry as _is_ancestor_entry
from scripts.delegate_acceptance.directories.operations import _open_parent_directory as _open_parent_directory
from scripts.delegate_acceptance.directories.operations import _remove_empty_cleanup_quarantine as _remove_empty_cleanup_quarantine
from scripts.delegate_acceptance.directories.operations import _remove_entry_at as _remove_entry_at
from scripts.delegate_acceptance.directories.operations import _rename_noreplace as _rename_noreplace
from scripts.delegate_acceptance.directories.operations import _require_fd_cleanup_support as _require_fd_cleanup_support
from scripts.delegate_acceptance.directories.operations import _retain_after_descendant_failure as _retain_after_descendant_failure
from scripts.delegate_acceptance.directories.journal import _sync_cleanup_journal as _sync_cleanup_journal
from scripts.delegate_acceptance.directories.operations import _transfer_child_ownership as _transfer_child_ownership
from scripts.delegate_acceptance.directories.operations import _validate_entry_parent as _validate_entry_parent
from scripts.delegate_acceptance.directories.finalization import _validate_retained_owned_directories as _validate_retained_owned_directories
from scripts.delegate_acceptance.directories.journal import acquire_exclusive_file_lock as acquire_exclusive_file_lock
import argparse as argparse
from scripts.delegate_acceptance.directories.journal import cleanup_journal_digest as cleanup_journal_digest
from scripts.delegate_acceptance.directories.operations import clear_owned_directory as clear_owned_directory
from scripts.delegate_acceptance.supervisor.config_policy import config_for as config_for
from scripts.delegate_acceptance.evidence.files import digest as digest
from scripts.delegate_acceptance.directories.journal import directory_identity_record as directory_identity_record
from scripts.delegate_acceptance.processes.observation import failure_cleanup_deadline as failure_cleanup_deadline
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts as finalize_acceptance_artifacts
import hashlib as hashlib
from scripts.delegate_acceptance.processes.observation import inherited_environment as inherited_environment
import json as json
import os as os
from scripts.delegate_acceptance.directories.journal import prepare_directory_cleanup as prepare_directory_cleanup
import re as re
from scripts.delegate_acceptance.directories.journal import read_directory_cleanup as read_directory_cleanup
from scripts.delegate_acceptance.evidence.files import read_json as read_json
from scripts.delegate_acceptance.directories.journal import release_exclusive_file_lock as release_exclusive_file_lock
from scripts.delegate_acceptance.directories.operations import release_owned_directory as release_owned_directory
from scripts.delegate_acceptance.directories.operations import remove_exact_owned_fifo as remove_exact_owned_fifo
from scripts.delegate_acceptance.directories.operations import remove_owned_directory as remove_owned_directory
from scripts.delegate_acceptance.directories.policy import require as require
from scripts.delegate_acceptance.directories.journal import resume_directory_cleanup as resume_directory_cleanup
from scripts.delegate_acceptance.directories.operations import rollback_owned_directories as rollback_owned_directories
from scripts.delegate_acceptance.evidence.digests import sha as sha
import stat as stat
import sys as sys
import tempfile as tempfile
import time as time
from scripts.delegate_acceptance.processes.observation import wait_for_failure_cleanup as wait_for_failure_cleanup
from scripts.delegate_acceptance.processes.observation import wait_until as wait_until
from scripts.delegate_acceptance.evidence.files import write_json as write_json

from scripts.delegate_acceptance.native_evidence.contract import (
    AGGREGATE_KIND as AGGREGATE_KIND,
    AGGREGATE_RECEIPT_NAME as AGGREGATE_RECEIPT_NAME,
    CASE_CLEANUP_ROOT_NAME as CASE_CLEANUP_ROOT_NAME,
    CASE_MATRIX_NAME as CASE_MATRIX_NAME,
    CASE_OWNER_ROOT_NAME as CASE_OWNER_ROOT_NAME,
    CASE_ROOT_NAME as CASE_ROOT_NAME,
    COMPACT_RECEIPT_ROOT_NAME as COMPACT_RECEIPT_ROOT_NAME,
    CaseDefinition as CaseDefinition,
    DEFAULT_RETENTION_COUNT as DEFAULT_RETENTION_COUNT,
    EXPECTED_CASES as EXPECTED_CASES,
    EvidenceError as EvidenceError,
    HEX_DIGEST as HEX_DIGEST,
    MATRIX_HEADER as MATRIX_HEADER,
    MAX_AGGREGATE_BYTES as MAX_AGGREGATE_BYTES,
    MAX_INPUT_BYTES as MAX_INPUT_BYTES,
    MAX_MATRIX_BYTES as MAX_MATRIX_BYTES,
    MAX_RECEIPT_BYTES as MAX_RECEIPT_BYTES,
    MAX_RETENTION_COUNT as MAX_RETENTION_COUNT,
    MAX_WALK_BYTES as MAX_WALK_BYTES,
    MAX_WALK_ENTRIES as MAX_WALK_ENTRIES,
    MatrixRow as MatrixRow,
    RETENTION_CLEANUP_ROOT_NAME as RETENTION_CLEANUP_ROOT_NAME,
    RETENTION_PLAN_NAME as RETENTION_PLAN_NAME,
    SAFE_TEXT as SAFE_TEXT,
    SCHEMA_VERSION as SCHEMA_VERSION,
    _REQUESTED_OPTION_KEYS as _REQUESTED_OPTION_KEYS,
    _REQUESTED_OPTION_STATES as _REQUESTED_OPTION_STATES,
    _aggregate_exit as _aggregate_exit,
    _case_cleanup_authorization as _case_cleanup_authorization,
    _case_definition as _case_definition,
    _case_result_verdict as _case_result_verdict,
    _compact_provider_runtime_identities as _compact_provider_runtime_identities,
    _compact_provider_versions as _compact_provider_versions,
    _digest_bytes as _digest_bytes,
    _empty_summary as _empty_summary,
    _expected_task_aliases as _expected_task_aliases,
    _invalid_requested_options as _invalid_requested_options,
    _normalize_requested_options as _normalize_requested_options,
    _parse_optional_revision as _parse_optional_revision,
    _safe_proof_text as _safe_proof_text,
    _safe_runtime_executable as _safe_runtime_executable,
    _safe_source_status as _safe_source_status,
    _slug as _slug,
    _source_requested_options as _source_requested_options,
    _source_status_matches as _source_status_matches,
    _summary_with_removed as _summary_with_removed,
    _task_identifier_values as _task_identifier_values,
    _validate_requested_options as _validate_requested_options,
    _validate_summary as _validate_summary,
    canonical_json as canonical_json,
    expected_cases as expected_cases,
)
from scripts.delegate_acceptance.native_evidence.filesystem import (
    _bounded_children as _bounded_children,
    _bounded_file_bytes as _bounded_file_bytes,
    _digest_file as _digest_file,
    _directory_identity as _directory_identity,
    _ensure_directory as _ensure_directory,
    _fsync_directory as _fsync_directory,
    _identity_matches as _identity_matches,
    _identity_shape as _identity_shape,
    _inside as _inside,
    _read_json as _read_json,
    _reject_path_components as _reject_path_components,
    _tree_summary as _tree_summary,
    _write_atomic as _write_atomic,
    _write_bytes_atomic as _write_bytes_atomic,
)
from scripts.delegate_acceptance.native_evidence.directories import (
    _case_creation_identity as _case_creation_identity,
    _case_owner_identity as _case_owner_identity,
    prepare_case_directory as prepare_case_directory,
    validate_prepared_case_directory as validate_prepared_case_directory,
)
from scripts.delegate_acceptance.native_evidence.proof import (
    _binary_identities as _binary_identities,
    _cleanup_observation as _cleanup_observation,
    _read_optional_object as _read_optional_object,
    _source_proof as _source_proof,
    _task_session_bindings as _task_session_bindings,
)
from scripts.delegate_acceptance.native_evidence.cases import (
    _case_cleanup_journal as _case_cleanup_journal,
    _case_reference as _case_reference,
    _prepare_case_cleanup as _prepare_case_cleanup,
    _read_case_source as _read_case_source,
    _replay_prepared_case as _replay_prepared_case,
    _sanitize_case as _sanitize_case,
)
from scripts.delegate_acceptance.native_evidence.retention_records import (
    _aggregate_root_summary as _aggregate_root_summary,
    _current_retention_entry as _current_retention_entry,
    _retention_authorization as _retention_authorization,
    _retention_count as _retention_count,
    _retention_plan_result as _retention_plan_result,
    _validate_retention_plan as _validate_retention_plan,
)
from scripts.delegate_acceptance.native_evidence.retention import (
    _enforce_retention as _enforce_retention,
    _enforce_retention_locked as _enforce_retention_locked,
    _publish_completed_retention_receipt as _publish_completed_retention_receipt,
    _reconcile_pending_retention as _reconcile_pending_retention,
    _resolved_receipt as _resolved_receipt,
)
from scripts.delegate_acceptance.native_evidence.validation import (
    _validate_case_receipt as _validate_case_receipt,
    _validate_retention as _validate_retention,
    validate_aggregate_receipt as validate_aggregate_receipt,
)
from scripts.delegate_acceptance.native_evidence.aggregate import (
    _build_aggregate_locked as _build_aggregate_locked,
    _parse_matrix as _parse_matrix,
    build_aggregate as build_aggregate,
)
from scripts.delegate_acceptance.native_evidence.staging import (
    _stage_evidence_locked as _stage_evidence_locked,
    stage_evidence as stage_evidence,
)
from scripts.delegate_acceptance.native_evidence.cli import (
    _command as _command,
    main as main,
)


if __name__ == "__main__":
    raise SystemExit(main())
