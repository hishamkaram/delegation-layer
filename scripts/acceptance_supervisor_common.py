"""Compatibility exports for the canonical shared acceptance foundation.

Patch the defining module when replacing implementation dependencies in tests.
"""

# Preserve the facade's published standard-library exports for compatibility.
from dataclasses import (
    dataclass as dataclass,
    field as field,
)
from pathlib import Path
from typing import (
    BinaryIO as BinaryIO,
    Callable as Callable,
    Protocol as Protocol,
)
import ctypes as ctypes
import fcntl as fcntl
import hashlib as hashlib
import json as json
import math as math
import os as os
import secrets as secrets
import stat as stat
import subprocess as subprocess
import sys
import tempfile as tempfile
import time as time

# Direct script execution and legacy flat imports place scripts/ on sys.path.
# Add its parent so every implementation loads under one canonical namespace.
if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.delegate_acceptance.directories.policy import (
    require,
)
from scripts.delegate_acceptance.directories.finalization import (
    _validate_retained_owned_directories,
    finalize_acceptance_artifacts,
)
from scripts.delegate_acceptance.directories.journal import (
    _CLEANUP_HEX,
    _CLEANUP_JOURNAL_BOUND,
    _CLEANUP_JOURNAL_KIND,
    _CLEANUP_JOURNAL_VERSION,
    _CLEANUP_STATES,
    _cleanup_clear_contents,
    _cleanup_empty,
    _cleanup_isolate,
    _cleanup_lock,
    _cleanup_open_directory,
    _cleanup_open_parent,
    _cleanup_path_state,
    _cleanup_read_journal,
    _cleanup_remove_empty,
    _cleanup_unlock,
    _cleanup_validate_journal_path,
    _sync_cleanup_journal,
    acquire_exclusive_file_lock,
    cleanup_journal_digest,
    directory_identity_record,
    prepare_directory_cleanup,
    read_directory_cleanup,
    release_exclusive_file_lock,
    resume_directory_cleanup,
)
from scripts.delegate_acceptance.directories.policy import (
    _cleanup_identity,
    _cleanup_identity_matches_token,
)
from scripts.delegate_acceptance.directories.operations import (
    DirectoryOwner,
    OwnedDirectory,
    _DIRECTORY_OPEN_FLAGS,
    _capture_created_directory,
    _check_cleanup_deadline,
    _close_parent_descriptor,
    _create_cleanup_quarantine,
    _create_one_directory,
    _directory_name,
    _is_ancestor_entry,
    _open_parent_directory,
    _remove_empty_cleanup_quarantine,
    _remove_entry_at,
    _rename_noreplace,
    _require_fd_cleanup_support,
    _retain_after_descendant_failure,
    _transfer_child_ownership,
    _validate_entry_parent,
    clear_owned_directory,
    release_owned_directory,
    remove_exact_owned_fifo,
    remove_owned_directory,
    rollback_owned_directories,
)
from scripts.delegate_acceptance.evidence.files import (
    digest,
    read_json,
    write_json,
)
from scripts.delegate_acceptance.evidence.digests import (
    sha,
)
from scripts.delegate_acceptance.processes.contracts import (
    OutputCapture,
)
from scripts.delegate_acceptance.processes.observation import (
    inherited_environment,
)
from scripts.delegate_acceptance.processes.observation import (
    failure_cleanup_deadline,
    wait_for_failure_cleanup,
    wait_until,
)
from scripts.delegate_acceptance.processes.operations import (
    Process,
    Processes,
)
from scripts.delegate_acceptance.supervisor.config_policy import (
    config_for,
)
