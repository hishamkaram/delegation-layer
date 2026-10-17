"""Bounded, redacted projections of native acceptance diagnostics."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import stat

from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.native.constants import (
    FAILURE_DIAGNOSTICS_BYTES, FAILURE_DIAGNOSTICS_MAX_ENTRIES,
    FAILURE_DIAGNOSTICS_MAX_OMISSIONS, FAILURE_DIAGNOSTICS_TEXT_BYTES,
    MAX_CONTROL_BYTES,
)


def _diagnostic_text_descriptor(value: object) -> dict[str, object]:
    """Describe text without persisting the text itself."""
    if not isinstance(value, str):
        return {"present": value is not None, "type": type(value).__name__}
    data = value.encode("utf-8", errors="replace")
    return {"present": True, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def _delegate_status_summary(value: object) -> str:
    """Summarize Delegate's bounded public status response for a failed runner."""
    if not isinstance(value, dict):
        return "response=malformed"
    fields: list[str] = []
    for key in ("admission", "liveness", "publication", "status"):
        candidate = value.get(key)
        if isinstance(candidate, str) and candidate:
            fields.append(f"{key}={candidate[:FAILURE_DIAGNOSTICS_TEXT_BYTES]!r}")
    failure = value.get("failure")
    if isinstance(failure, dict):
        for key in ("code", "stage", "next_action"):
            candidate = failure.get(key)
            if isinstance(candidate, str) and candidate:
                fields.append(f"failure.{key}={candidate[:FAILURE_DIAGNOSTICS_TEXT_BYTES]!r}")
    error = value.get("error")
    if isinstance(error, str) and error:
        bounded = " ".join(error.split())[:FAILURE_DIAGNOSTICS_TEXT_BYTES]
        fields.append(f"error={bounded!r}")
    return ", ".join(fields) if fields else "response=no-actionable-fields"


def _diagnostic_regular_file(path: Path) -> tuple[dict[str, object] | None, str | None]:
    """Return metadata for one regular file without following unsafe entries."""
    try:
        metadata = path.lstat()
    except FileNotFoundError:
        return None, "missing"
    except OSError as error:
        return None, type(error).__name__
    if stat.S_ISLNK(metadata.st_mode):
        return None, "symlink"
    if not stat.S_ISREG(metadata.st_mode):
        return None, "special"
    try:
        return {
            "type": "file", "mode": stat.S_IMODE(metadata.st_mode),
            "bytes": metadata.st_size, "sha256": digest(path),
        }, None
    except OSError as error:
        return None, type(error).__name__


def _diagnostic_relative_path(value: object) -> str | None:
    if not isinstance(value, str) or not value or len(value) > FAILURE_DIAGNOSTICS_TEXT_BYTES:
        return None
    if any(ord(character) < 32 for character in value):
        return None
    path = Path(value)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        return None
    return str(path)


def _diagnostic_payload(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    basename = value.get("basename")
    if isinstance(basename, str) and basename and Path(basename).name == basename and \
            len(basename) <= FAILURE_DIAGNOSTICS_TEXT_BYTES:
        result["basename"] = basename
    length = value.get("length")
    if type(length) is int and length >= 0:
        result["length"] = length
    checksum = value.get("sha256")
    if isinstance(checksum, str) and len(checksum) == 64 and \
            all(character in "0123456789abcdef" for character in checksum):
        result["sha256"] = checksum
    return result or None


def _diagnostic_predicate(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    for key in ("adapter", "mode", "version"):
        candidate = value.get(key)
        if isinstance(candidate, str) and len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                not any(ord(character) < 32 for character in candidate):
            result[key] = candidate
    checksum = value.get("sha256")
    if isinstance(checksum, str) and len(checksum) == 64 and \
            all(character in "0123456789abcdef" for character in checksum):
        result["sha256"] = checksum
    return result or None


def _diagnostic_output_diagnostics(value: object) -> list[dict[str, object]]:
    if not isinstance(value, list):
        return []
    result: list[dict[str, object]] = []
    for item in value[:FAILURE_DIAGNOSTICS_MAX_ENTRIES]:
        if not isinstance(item, dict):
            continue
        diagnostic: dict[str, object] = {}
        for key in ("code", "message", "field"):
            candidate = item.get(key)
            if isinstance(candidate, str) and candidate and \
                    len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                    not any(ord(character) < 32 for character in candidate):
                diagnostic[key] = candidate
        for key in ("event", "offset"):
            candidate = item.get(key)
            if type(candidate) is int and candidate >= 0:
                diagnostic[key] = candidate
        if "code" in diagnostic and "message" in diagnostic:
            result.append(diagnostic)
    return result


def _diagnostic_outcome(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    verdict = value.get("verdict")
    if isinstance(verdict, str) and len(verdict) <= FAILURE_DIAGNOSTICS_TEXT_BYTES:
        result["verdict"] = verdict
    predicate = _diagnostic_predicate(value.get("predicate"))
    if predicate is not None:
        result["predicate"] = predicate
    payload = _diagnostic_payload(value.get("payload"))
    if payload is not None:
        result["payload"] = payload
    evidence = value.get("evidence_sha256")
    if isinstance(evidence, str) and len(evidence) == 64 and \
            all(character in "0123456789abcdef" for character in evidence):
        result["evidence_sha256"] = evidence
    diagnostics = _diagnostic_output_diagnostics(value.get("diagnostics"))
    if diagnostics:
        result["diagnostics"] = diagnostics
    return result or None


def _rejection_diagnostic_summary(outcome: dict[str, object]) -> str:
    diagnostics = _diagnostic_output_diagnostics(outcome.get("diagnostics"))
    if not diagnostics:
        return "verified rejection has no structured diagnostic"
    first = diagnostics[0]
    fields = [f"code={first['code']!r}"]
    if "field" in first:
        fields.append(f"field={first['field']!r}")
    fields.append(f"message={first['message']!r}")
    return ", ".join(fields)


def _diagnostic_provider_exit(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    for key in ("invocation_state", "exit_code"):
        candidate = value.get(key)
        if (key == "invocation_state" and isinstance(candidate, str) and
                len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES) or \
                (key == "exit_code" and type(candidate) is int):
            result[key] = candidate
    if "error" in value:
        result["error"] = _diagnostic_text_descriptor(value.get("error"))
    manifest = value.get("raw_manifest")
    if isinstance(manifest, list):
        result["raw_manifest_count"] = len(manifest)
        entries: list[dict[str, object]] = []
        for item in manifest[:FAILURE_DIAGNOSTICS_MAX_ENTRIES]:
            if not isinstance(item, dict):
                continue
            entry: dict[str, object] = {}
            relative = _diagnostic_relative_path(item.get("path"))
            if relative is not None:
                entry["path"] = relative
            for key in ("bytes", "length"):
                candidate = item.get(key)
                if type(candidate) is int and candidate >= 0:
                    entry[key] = candidate
            checksum = item.get("sha256")
            if isinstance(checksum, str) and len(checksum) == 64 and \
                    all(character in "0123456789abcdef" for character in checksum):
                entry["sha256"] = checksum
            if entry:
                entries.append(entry)
        result["raw_manifest"] = entries
        if len(manifest) > FAILURE_DIAGNOSTICS_MAX_ENTRIES:
            result["raw_manifest_truncated"] = True
    manifest_checksum = value.get("manifest_sha256")
    if isinstance(manifest_checksum, str) and len(manifest_checksum) == 64 and \
            all(character in "0123456789abcdef" for character in manifest_checksum):
        result["manifest_sha256"] = manifest_checksum
    return result or None


def _diagnostic_workspace(directory: Path) -> dict[str, object]:
    """Capture only metadata and hashes from the test-owned workspace."""
    result: dict[str, object] = {"entries": {}, "omitted": [], "truncated": False}
    omitted: list[str] = []
    entries: dict[str, dict[str, object]] = {}
    try:
        root = directory.lstat()
    except OSError as error:
        result["omitted"] = [type(error).__name__]
        return result
    if stat.S_ISLNK(root.st_mode) or not stat.S_ISDIR(root.st_mode):
        result["omitted"] = ["workspace-root"]
        return result
    try:
        paths = sorted(directory.rglob("*"))
    except OSError as error:
        result["omitted"] = [type(error).__name__]
        return result
    for path in paths:
        relative = str(path.relative_to(directory))
        if len(entries) >= FAILURE_DIAGNOSTICS_MAX_ENTRIES:
            result["truncated"] = True
            break
        try:
            metadata = path.lstat()
        except OSError as error:
            omitted.append(relative + ":" + type(error).__name__)
            continue
        if stat.S_ISLNK(metadata.st_mode):
            omitted.append(relative + ":symlink")
            continue
        if stat.S_ISDIR(metadata.st_mode):
            entries[relative] = {"type": "directory", "mode": stat.S_IMODE(metadata.st_mode)}
            continue
        if not stat.S_ISREG(metadata.st_mode):
            omitted.append(relative + ":special")
            continue
        try:
            entries[relative] = {
                "type": "file", "mode": stat.S_IMODE(metadata.st_mode),
                "bytes": metadata.st_size, "sha256": digest(path),
            }
        except OSError as error:
            omitted.append(relative + ":" + type(error).__name__)
    result["entries"] = entries
    result["omitted"] = omitted[:FAILURE_DIAGNOSTICS_MAX_OMISSIONS]
    result["omitted_count"] = len(omitted)
    if len(omitted) > FAILURE_DIAGNOSTICS_MAX_OMISSIONS:
        result["omitted_truncated"] = True
    return result


def _diagnostic_json_bytes(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def _fit_failure_diagnostics(value: dict[str, object]) -> dict[str, object]:
    """Keep the persisted diagnostic bounded while retaining structure."""
    status = value["status"]
    require(isinstance(status, dict), "failure diagnostic status is malformed")

    def mark_truncated(reason: str) -> None:
        status["partial"] = True
        truncated = status.setdefault("truncated", [])
        require(isinstance(truncated, list), "failure diagnostic truncation state is malformed")
        if reason not in truncated:
            truncated.append(reason)

    if len(_diagnostic_json_bytes(value)) <= FAILURE_DIAGNOSTICS_BYTES:
        return value

    workspace = value.get("workspace")
    entries = workspace.get("entries") if isinstance(workspace, dict) else None
    if isinstance(entries, dict):
        while entries and len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
            keep = max(0, len(entries) // 2)
            workspace["entries"] = dict(list(entries.items())[:keep])
            entries = workspace["entries"]
        mark_truncated("workspace.entries")

    if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
        tasks = value.get("tasks")
        if isinstance(tasks, list):
            for task in tasks:
                if not isinstance(task, dict):
                    continue
                provider_exit = task.get("provider_exit")
                if isinstance(provider_exit, dict) and provider_exit.get("raw_manifest"):
                    provider_exit["raw_manifest"] = []
                    provider_exit["raw_manifest_omitted"] = True
                    mark_truncated("provider_exit.raw_manifest")
                if "files" in task:
                    task.pop("files")
                    task["files_omitted"] = True
            mark_truncated("task.files")

    if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES and isinstance(workspace, dict):
        workspace["entries"] = {}
        mark_truncated("workspace.entries.all")

    if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
        tasks = value.get("tasks")
        if isinstance(tasks, list):
            value["tasks"] = [
                {"name": task.get("name"), "task_id": task.get("task_id")}
                for task in tasks if isinstance(task, dict)
            ]
            mark_truncated("task.details")

    if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
        task_ids = value.get("task_ids")
        if isinstance(task_ids, list):
            while task_ids and len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
                task_ids[:] = task_ids[:max(0, len(task_ids) // 2)]
            mark_truncated("task_ids")

    require(len(_diagnostic_json_bytes(value)) <= FAILURE_DIAGNOSTICS_BYTES,
            "failure diagnostic could not be bounded")
    status["partial"] = True
    return value


def _diagnostic_task(state: Path, task_id: str, omitted: list[str]) -> dict[str, object]:
    task_directory = state / "tasks" / task_id
    result: dict[str, object] = {"task_id": task_id, "files": {}}
    try:
        state_metadata = state.lstat()
        tasks_metadata = (state / "tasks").lstat()
        metadata = task_directory.lstat()
    except OSError as error:
        omitted.append(f"task:{task_id}:{type(error).__name__}")
        return result
    if (stat.S_ISLNK(state_metadata.st_mode) or not stat.S_ISDIR(state_metadata.st_mode) or
            stat.S_ISLNK(tasks_metadata.st_mode) or not stat.S_ISDIR(tasks_metadata.st_mode) or
            stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode)):
        omitted.append(f"task:{task_id}:unsafe-directory")
        return result

    files: dict[str, dict[str, object]] = {}
    decoded: dict[str, object] = {}
    for name in ("task.json", "meta.json", "provider.exit", "outcome.json",
                 "provider.ref.json", "result.txt", "publish.reject"):
        descriptor, reason = _diagnostic_regular_file(task_directory / name)
        if descriptor is not None:
            files[name] = descriptor
        elif reason not in (None, "missing"):
            omitted.append(f"task:{task_id}:{name}:{reason}")
            continue
        if name in {"provider.exit", "outcome.json"} and descriptor is not None:
            try:
                decoded[name] = read_json(task_directory / name, bound=MAX_CONTROL_BYTES)
            except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
                omitted.append(f"task:{task_id}:{name}:unreadable")

    outcome = _diagnostic_outcome(decoded.get("outcome.json"))
    if outcome is not None:
        result["outcome"] = outcome
    provider_exit = _diagnostic_provider_exit(decoded.get("provider.exit"))
    if provider_exit is not None:
        result["provider_exit"] = provider_exit
    result["files"] = files
    return result
