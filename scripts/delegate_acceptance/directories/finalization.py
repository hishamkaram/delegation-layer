"""Acceptance artifact finalization from caller-owned directory tokens."""

from __future__ import annotations

import math
import os
import stat
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import (
    OwnedDirectory,
    _check_cleanup_deadline,
    _is_ancestor_entry,
    release_owned_directory,
    remove_owned_directory,
)
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.processes.observation import failure_cleanup_deadline
from pathlib import Path


def _validate_retained_owned_directories(
        entries: tuple[OwnedDirectory, ...],
        retained_entries: tuple[OwnedDirectory, ...],
) -> set[int]:
    require(type(retained_entries) is tuple,
            "retained owned directories must be an identity-token tuple")
    retained_ids = set()
    for entry in retained_entries:
        require(isinstance(entry, OwnedDirectory),
                "retained owned directory identity token is invalid")
        require(id(entry) not in retained_ids,
                "retained owned directory identity token is duplicated")
        require(any(candidate is entry for candidate in entries),
                "retained owned directory token is not in the ownership sequence")
        entry.assert_live()
        retained_ids.add(id(entry))
    for entry in retained_entries:
        for ancestor in entries:
            if _is_ancestor_entry(ancestor, entry):
                require(id(ancestor) in retained_ids,
                        "retained child requires every owned ancestor to be retained")
    return retained_ids


def finalize_acceptance_artifacts(
        output: Path,
        supervisor_closed: bool,
        owned_directories: tuple[OwnedDirectory, ...],
        success: bool,
        remove_failed_process_captures: bool = False,
        raw_process_captures_token: OwnedDirectory | None = None,
        timeout: float = 30,
        *,
        retained_owned_directories: tuple[OwnedDirectory, ...] = ()) -> bool:
    """Clean one completed acceptance run without crossing an ownership boundary.

    ``owned_directories`` is the invocation's explicit creation-order sequence
    of identity tokens. When raw captures exist, ``raw_process_captures_token``
    must be that exact token and must already be present in the sequence. A
    private supervisor must have been positively observed closed before any
    token is removed. The compact evidence directory remains; a successful run
    drops only its large raw process captures. Failed runs may opt in to the
    same removal after a caller has persisted an independent, bounded
    diagnostic snapshot. On a genuine failed run, selected identity tokens may
    be retained while all other owned entries follow the normal cleanup path.
    Every retained child must include all of its owned ancestors, and the
    cleanup receipt records the selected creation identities without contents.
    """
    output = Path(output)
    require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0,
            "acceptance finalization timeout must be finite and positive")
    require(type(remove_failed_process_captures) is bool,
            "failed process capture cleanup mode is invalid")
    if raw_process_captures_token is not None:
        require(isinstance(raw_process_captures_token, OwnedDirectory),
                "raw process capture identity token is invalid")
    require(not remove_failed_process_captures or not success,
            "failed process capture cleanup is only valid for failed runs")
    try:
        entries, retained_entry_ids, entry_identities = _finalization_authorities(
            output, owned_directories, raw_process_captures_token, retained_owned_directories,
            success,
        )
    except (OSError, RuntimeError):
        # Cleanup is never allowed to replace the result it is preserving.
        # Callers turn this false result into a failed successful run, while a
        # failure path retains its original diagnostic and leaves all state.
        return False
    receipt = {
        "supervisor_shutdown_observed": bool(supervisor_closed),
        "completion": "passed" if success else "failed",
        "removed": [],
        "retained": [],
        "retained_owned_directories": [],
        "raw_process_captures": "retained",
    }
    if not supervisor_closed:
        receipt["status"] = "retained"
        receipt["reason"] = "private supervisor shutdown was not positively observed"
        receipt["retained"] = [entry.label for entry in entries]
        receipt["retained_owned_directories"] = [entry_identities[id(entry)]
                                                  for entry in entries]
        write_json(output / "cleanup.json", receipt, replace=(output / "cleanup.json").exists())
        return False

    failures, raw_removed = _finalize_owned_entries(
        entries, entry_identities, raw_process_captures_token, receipt,
        remove_failed_process_captures, retained_entry_ids, success, timeout,
    )
    if raw_process_captures_token is not None and raw_removed:
        receipt["raw_process_captures"] = "removed"
    if failures:
        receipt["status"] = "partial"
        receipt["failures"] = failures
    else:
        receipt["status"] = "removed"
    write_json(output / "cleanup.json", receipt, replace=(output / "cleanup.json").exists())
    return not failures


def _finalization_authorities(
        output: Path,
        owned_directories: tuple[OwnedDirectory, ...],
        raw_process_captures_token: OwnedDirectory | None,
        retained_owned_directories: tuple[OwnedDirectory, ...],
        success: bool,
) -> tuple[tuple[OwnedDirectory, ...], set[int], dict[int, dict[str, object]]]:
    retained_entries = retained_owned_directories
    require(not success or not retained_entries,
            "retained owned directories are only valid for failed runs")
    output_mode = output.lstat().st_mode
    require(stat.S_ISDIR(output_mode) and not stat.S_ISLNK(output_mode),
            "acceptance evidence directory is unsafe: " + str(output))
    entries = tuple(owned_directories)
    require(all(isinstance(entry, OwnedDirectory) for entry in entries),
            "acceptance cleanup requires identity tokens")
    retained_entry_ids = _validate_retained_owned_directories(entries, retained_entries)
    entry_identities = {
        id(entry): directory_identity_record(entry) for entry in entries
    }
    if raw_process_captures_token is not None:
        require(any(entry is raw_process_captures_token for entry in entries),
                "raw process capture token is not in the ownership sequence")
    paths = [entry.directory for entry in entries]
    require(len(paths) == len(set(paths)), "acceptance cleanup has duplicate owned directories")
    require(all(directory != output and directory not in output.parents for directory in paths),
            "acceptance cleanup cannot remove its evidence directory")
    raw_path = output / "processes"
    if (raw_path.exists() or raw_path.is_symlink()) and raw_process_captures_token is None:
        raise RuntimeError("raw process capture identity token is required")
    if raw_process_captures_token is not None:
        require(raw_process_captures_token.directory == raw_path,
                "raw process capture token does not identify output/processes")
        require(raw_process_captures_token.parent == output,
                "raw process capture token parent does not identify output")
        output_identity = os.stat(output, follow_symlinks=False)
        require((output_identity.st_dev, output_identity.st_ino) ==
                raw_process_captures_token.parent_identity,
                "raw process capture token parent identity does not match output")
    return entries, retained_entry_ids, entry_identities


def _finalize_owned_entries(
        entries: tuple[OwnedDirectory, ...],
        entry_identities: dict[int, dict[str, object]],
        raw_process_captures_token: OwnedDirectory | None,
        receipt: dict[str, object],
        remove_failed_process_captures: bool,
        retained_entry_ids: set[int],
        success: bool,
        timeout: float,
) -> tuple[list[dict[str, str]], bool]:
    deadline = failure_cleanup_deadline(timeout)
    failures = []
    failed_entries = []
    raw_removed = False
    raw_authorized = success or remove_failed_process_captures
    ordered_entries = tuple(reversed(entries))

    def record_retained(entry):
        receipt["retained"].append(entry.label)
        receipt["retained_owned_directories"].append(entry_identities[id(entry)])

    for index, entry in enumerate(ordered_entries):
        try:
            _check_cleanup_deadline(deadline)
        except RuntimeError as error:
            failures.append({"label": entry.label, "error": str(error)})
            for retained_entry in ordered_entries[index:]:
                record_retained(retained_entry)
                try:
                    retained_entry.consume()
                except RuntimeError as consume_error:
                    failures.append({"label": retained_entry.label,
                                     "error": str(consume_error)})
            break
        if any(_is_ancestor_entry(entry, failed) for failed in failed_entries):
            try:
                release_owned_directory(entry, deadline=deadline)
            except (OSError, RuntimeError) as error:
                failures.append({
                    "label": entry.label,
                    "error": "ancestor retained after descendant cleanup failure: " + str(error),
                })
            record_retained(entry)
            continue
        if id(entry) in retained_entry_ids:
            try:
                release_owned_directory(entry, deadline=deadline)
            except (OSError, RuntimeError) as error:
                failures.append({"label": entry.label, "error": str(error)})
            record_retained(entry)
            continue
        if entry is raw_process_captures_token and not raw_authorized:
            try:
                release_owned_directory(entry, deadline=deadline)
            except (OSError, RuntimeError) as error:
                failures.append({"label": entry.label, "error": str(error)})
            record_retained(entry)
            continue
        try:
            remove_owned_directory(entry, deadline=deadline)
            receipt["removed"].append(entry.label)
            if entry is raw_process_captures_token:
                raw_removed = True
        except (OSError, RuntimeError) as error:
            failures.append({"label": entry.label, "error": str(error)})
            failed_entries.append(entry)
    return failures, raw_removed
