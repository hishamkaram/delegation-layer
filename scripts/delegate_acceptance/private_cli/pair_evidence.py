"""Observe installed pair selection from the canonical runtime binding."""

import json
import os
from pathlib import Path
import stat

from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, read_bounded_regular_file


MAX_BINDING_BYTES = 64 * 1024


def assert_state_supervisor_pair(state, bundled_directory):
    """Require the runtime's selected pair to belong to the expected installation.

    The runtime binding owns selection. This observation never reads executable
    contents and grants no process or directory cleanup authority.
    """
    record_path = state / ".supervisor" / "supervisor.identity.json"
    try:
        record = json.loads(read_bounded_regular_file(record_path, MAX_BINDING_BYTES))
    except (BoundedReadError, UnicodeError, json.JSONDecodeError) as error:
        raise RuntimeError("state-root supervisor binding is missing, unsafe or unreadable") from error
    binding = record.get("binding") if isinstance(record, dict) else None
    if not isinstance(binding, dict) or record.get("schema_version") != 1:
        raise RuntimeError("state-root supervisor binding has an unsupported representation")
    daemon_path = state / ".supervisor" / "daemon.identity.json"
    try:
        daemon = json.loads(read_bounded_regular_file(daemon_path, MAX_BINDING_BYTES))
    except (BoundedReadError, UnicodeError, json.JSONDecodeError) as error:
        raise RuntimeError("state-root daemon identity is missing, unsafe or unreadable") from error
    if not isinstance(daemon, dict):
        raise RuntimeError("state-root daemon identity is invalid")
    daemon_executable = daemon.get("executable")
    if "daemon_executable" in binding and binding["daemon_executable"] != daemon_executable:
        raise RuntimeError("state-root daemon selection disagrees with its canonical identity")
    expected = bundled_directory.resolve(strict=True)
    for name, value in (("pueue", binding.get("client_executable")), ("pueued", daemon_executable)):
        if (not isinstance(value, str) or not Path(value).is_absolute()
                or ".." in Path(value).parts or str(Path(value)) != value
                or any(ord(char) < 32 for char in value)):
            raise RuntimeError("state-root supervisor selection is invalid: " + name)
        selected = Path(value)
        try:
            metadata = selected.stat()
            valid = (stat.S_ISREG(metadata.st_mode) and os.access(selected, os.X_OK)
                     and selected.resolve(strict=True) == expected / name)
        except OSError as error:
            raise RuntimeError("state-root supervisor selection is unavailable: " + name) from error
        if not valid:
            raise RuntimeError("state-root selected an unexpected installed supervisor member " + name)
    return expected
