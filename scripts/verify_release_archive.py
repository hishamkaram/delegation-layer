#!/usr/bin/env python3
"""Verify an exact release archive and atomically record sanitized evidence."""

from __future__ import annotations

from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))


from scripts.acceptance.directories.operations import DirectoryOwner as DirectoryOwner
from scripts.acceptance.native_evidence.contract import EvidenceError as EvidenceError
from scripts.acceptance.native_evidence.validation import validate_aggregate_receipt as validate_aggregate_receipt

from scripts.release_verification.contract import MAX_RECEIPT as MAX_RECEIPT
from scripts.acceptance.evidence.source import MAX_OUTPUT as MAX_OUTPUT
from scripts.release_verification.contract import MAX_ARCHIVE as MAX_ARCHIVE
from scripts.release_verification.contract import MAX_BINARY as MAX_BINARY
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE as MAX_SOURCE_FILE
from scripts.release_verification.contract import MAX_TREE_ENTRIES as MAX_TREE_ENTRIES
from scripts.release_verification.contract import BINARIES as BINARIES
from scripts.release_verification.contract import TAG_RE as TAG_RE
from scripts.release_verification.contract import CHECKSUM_RE as CHECKSUM_RE
from scripts.acceptance.evidence.source import REVISION_RE as REVISION_RE
from scripts.release_verification.contract import SOURCE_FILES as SOURCE_FILES
from scripts.acceptance.evidence.source import VerificationError as VerificationError
from scripts.release_verification.contract import expected_archive as expected_archive
from scripts.release_verification.contract import binary_version as binary_version
from scripts.release_verification.contract import new_receipt as new_receipt
from scripts.release_verification.contract import set_failure as set_failure
from scripts.release_verification.contract import _release_cleanup_authorization as _release_cleanup_authorization
from scripts.release_verification.contract import _release_cleanup_transaction as _release_cleanup_transaction

from scripts.release_verification.filesystem import _fsync_directory as _fsync_directory
from scripts.acceptance.evidence.source import _bounded_file_bytes as _bounded_file_bytes
from scripts.release_verification.filesystem import bounded_digest as bounded_digest
from scripts.acceptance.evidence.source import _reject_parent_components as _reject_parent_components
from scripts.acceptance.evidence.source import _reject_symlink_components as _reject_symlink_components
from scripts.release_verification.filesystem import _bounded_children as _bounded_children
from scripts.release_verification.filesystem import write_receipt as write_receipt
from scripts.release_verification.filesystem import _acquire_receipt_lock as _acquire_receipt_lock
from scripts.release_verification.filesystem import read_receipt as read_receipt
from scripts.release_verification.filesystem import exit_code as exit_code

from scripts.acceptance.evidence.source import command as command

from scripts.release_verification.source import source_fingerprint as source_fingerprint

from scripts.release_verification.assets import platform_identity as platform_identity
from scripts.release_verification.assets import release_paths as release_paths
from scripts.release_verification.assets import resolve_release_identity as resolve_release_identity
from scripts.release_verification.assets import gh_environment as gh_environment
from scripts.release_verification.assets import remember_directory as remember_directory
from scripts.release_verification.assets import verify_checksum as verify_checksum
from scripts.release_verification.assets import extract_archive as extract_archive
from scripts.release_verification.assets import inspect_binaries as inspect_binaries
from scripts.release_verification.assets import exercise_default_path as exercise_default_path

from scripts.release_verification.cleanup import _replay_prepared_cleanup as _replay_prepared_cleanup
from scripts.release_verification.cleanup import finish_gate as finish_gate

from scripts.release_verification.verification import verify as verify
from scripts.release_verification.verification import _verify_locked as _verify_locked
from scripts.release_verification.verification import finalize_native as finalize_native
from scripts.release_verification.verification import _finalize_native_locked as _finalize_native_locked

from scripts.release_verification.cli import main as main


if __name__ == "__main__":
    raise SystemExit(main())
