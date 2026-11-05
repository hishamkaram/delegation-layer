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
    require as require,
)
from scripts.delegate_acceptance.directories.finalization import (
    finalize_acceptance_artifacts as finalize_acceptance_artifacts,
)
from scripts.delegate_acceptance.directories.journal import (
    _cleanup_empty as _cleanup_empty,
    directory_identity_record as directory_identity_record,
    prepare_directory_cleanup as prepare_directory_cleanup,
    read_directory_cleanup as read_directory_cleanup,
    resume_directory_cleanup as resume_directory_cleanup,
)
from scripts.delegate_acceptance.directories.operations import (
    DirectoryOwner as DirectoryOwner,
    OwnedDirectory as OwnedDirectory,
    _close_parent_descriptor as _close_parent_descriptor,
    _open_parent_directory as _open_parent_directory,
    _rename_noreplace as _rename_noreplace,
    clear_owned_directory as clear_owned_directory,
    release_owned_directory as release_owned_directory,
    remove_exact_owned_fifo as remove_exact_owned_fifo,
    remove_owned_directory as remove_owned_directory,
    rollback_owned_directories as rollback_owned_directories,
)
from scripts.delegate_acceptance.evidence.files import (
    digest as digest,
    read_json as read_json,
    write_json as write_json,
)
from scripts.delegate_acceptance.evidence.digests import (
    sha as sha,
)
from scripts.delegate_acceptance.processes.contracts import (
    OutputCapture as OutputCapture,
)
from scripts.delegate_acceptance.processes.observation import (
    inherited_environment as inherited_environment,
)
from scripts.delegate_acceptance.processes.observation import (
    failure_cleanup_deadline as failure_cleanup_deadline,
    wait_for_failure_cleanup as wait_for_failure_cleanup,
    wait_until as wait_until,
)
from scripts.delegate_acceptance.processes.operations import (
    Process as Process,
    Processes as Processes,
)
from scripts.delegate_acceptance.supervisor.config_policy import (
    config_for as config_for,
)
