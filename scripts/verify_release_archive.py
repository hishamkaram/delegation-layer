#!/usr/bin/env python3
"""Verify an exact release archive and atomically record sanitized evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import selectors
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.bounded_files import (
    BoundedReadError, read_bounded_regular_file,
)

from scripts.delegate_acceptance.directories.operations import DirectoryOwner as DirectoryOwner
from scripts.delegate_acceptance.directories.journal import (
    acquire_exclusive_file_lock, cleanup_journal_digest, prepare_directory_cleanup,
    read_directory_cleanup, release_exclusive_file_lock, resume_directory_cleanup,
)
from scripts.delegate_acceptance.native_evidence.contract import EvidenceError as EvidenceError
from scripts.delegate_acceptance.native_evidence.validation import validate_aggregate_receipt as validate_aggregate_receipt

from scripts.delegate_acceptance.release_verification.contract import (
    MAX_RECEIPT as MAX_RECEIPT,
    MAX_OUTPUT as MAX_OUTPUT,
    MAX_ARCHIVE as MAX_ARCHIVE,
    MAX_BINARY as MAX_BINARY,
    MAX_SOURCE_FILE as MAX_SOURCE_FILE,
    MAX_TREE_ENTRIES as MAX_TREE_ENTRIES,
    BINARIES as BINARIES,
    TAG_RE as TAG_RE,
    CHECKSUM_RE as CHECKSUM_RE,
    REVISION_RE as REVISION_RE,
    SOURCE_FILES as SOURCE_FILES,
    VerificationError as VerificationError,
    expected_archive as expected_archive,
    binary_version as binary_version,
    new_receipt as new_receipt,
    set_failure as set_failure,
    _release_cleanup_authorization as _release_cleanup_authorization,
    _release_cleanup_transaction as _release_cleanup_transaction,
)

from scripts.delegate_acceptance.release_verification.filesystem import (
    _fsync_directory as _fsync_directory,
    _bounded_file_bytes as _bounded_file_bytes,
    bounded_digest as bounded_digest,
    _reject_parent_components as _reject_parent_components,
    _reject_symlink_components as _reject_symlink_components,
    _bounded_children as _bounded_children,
    write_receipt as write_receipt,
    _acquire_receipt_lock as _acquire_receipt_lock,
    read_receipt as read_receipt,
    exit_code as exit_code,
)

from scripts.delegate_acceptance.release_verification.commands import (
    command as command,
)

from scripts.delegate_acceptance.release_verification.source import (
    source_fingerprint as source_fingerprint,
)

from scripts.delegate_acceptance.release_verification.assets import (
    platform_identity as platform_identity,
    release_paths as release_paths,
    resolve_release_identity as resolve_release_identity,
    gh_environment as gh_environment,
    remember_directory as remember_directory,
    verify_checksum as verify_checksum,
    extract_archive as extract_archive,
    inspect_binaries as inspect_binaries,
    exercise_default_path as exercise_default_path,
)

from scripts.delegate_acceptance.release_verification.cleanup import (
    _replay_prepared_cleanup as _replay_prepared_cleanup,
    finish_gate as finish_gate,
)

from scripts.delegate_acceptance.release_verification.verification import (
    verify as verify,
    _verify_locked as _verify_locked,
    finalize_native as finalize_native,
    _finalize_native_locked as _finalize_native_locked,
)

from scripts.delegate_acceptance.release_verification.cli import (
    main as main,
)


if __name__ == "__main__":
    raise SystemExit(main())
