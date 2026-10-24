"""Canonical source, artifact, and index-document policy for handoff evidence."""

from __future__ import annotations

from dataclasses import dataclass
import errno
import os
from pathlib import Path
import re
import subprocess
import unicodedata

from scripts.delegate_acceptance.evidence.bounded_files import (
    BoundedReadError, ReadFailure, fingerprint_bounded_regular_file,
)


SCHEMA_VERSION = 1
TOOL_VERSION = "1"
MAX_INDEX_BYTES = 8 * 1024 * 1024
MAX_GIT_OUTPUT_BYTES = 8 * 1024 * 1024
MAX_FILES = 4096
MAX_ITEMS = 512
MAX_FILE_BYTES = 512 * 1024 * 1024
MAX_TOTAL_BYTES = 2 * 1024 * 1024 * 1024
MAX_TEXT_BYTES = 1024
MAX_PATH_BYTES = 4096
CLAIM_CLASSES = ("fixture", "native", "package", "provider", "review")
ROW_ID = re.compile(r"^[A-Za-z][A-Za-z0-9_.:/-]{0,79}$")
ROLE = re.compile(r"^[a-z][a-z0-9._-]{0,63}$")
REVISION = re.compile(r"^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$")
# Inherited pathspec settings would add magic to every Git command (and conflict with the
# explicit --literal-pathspecs below), so run_git removes them from the child environment.
PATHSPEC_ENVIRONMENT = (
    "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
)
SECRET = re.compile(
    r"(?i)(?:\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|password|secret|authorization|credential)\s*[:=]\s*\S+|\bbearer\s+[A-Za-z0-9._~+/=-]{12,}|\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16})|-----BEGIN [A-Z ]*PRIVATE KEY-----)"
)



class EvidenceError(Exception):
    def __init__(self, code: str, stage: str, safe_to_write: bool = True):
        super().__init__(code)
        self.code = code
        self.stage = stage
        self.safe_to_write = safe_to_write


def safe_text(value: str, limit: int = MAX_TEXT_BYTES) -> str:
    try:
        size = len(value.encode("utf-8"))
    except UnicodeEncodeError:
        raise EvidenceError("invalid_text", "validate-input") from None
    if not value or size > limit or any(unicodedata.category(char) == "Cc" for char in value):
        raise EvidenceError("invalid_text", "validate-input")
    if SECRET.search(value):
        raise EvidenceError("sensitive_input", "validate-input")
    return value


def run_git(
        root: Path, *arguments: str, stage: str, allow_failure: bool = False,
        literal_pathspecs: bool = True) -> tuple[int, bytes]:
    """Run Git with a pathspec-free environment; literal_pathspecs=False is only for commands
    that take plain pathnames (git check-ignore rejects any pathspec magic)."""
    environment = {name: value for name, value in os.environ.items() if name not in PATHSPEC_ENVIRONMENT}
    try:
        result = subprocess.run(
            ["git", *(["--literal-pathspecs"] if literal_pathspecs else []), *arguments],
            cwd=root,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=30,
            check=False,
        )
    except subprocess.TimeoutExpired:
        raise EvidenceError("git_timeout", stage) from None
    except OSError:
        raise EvidenceError("git_unavailable", stage) from None
    if len(result.stdout) > MAX_GIT_OUTPUT_BYTES:
        raise EvidenceError("git_output_too_large", stage)
    if result.returncode and not allow_failure:
        raise EvidenceError("git_command_failed", stage)
    return result.returncode, result.stdout


def _git_text(root: Path, arguments: tuple[str, ...], stage: str, allow_failure: bool = False) -> str | None:
    code, output = run_git(root, *arguments, stage=stage, allow_failure=allow_failure)
    if code:
        return None
    try:
        return output.decode("utf-8").rstrip("\r\n")
    except UnicodeDecodeError:
        raise EvidenceError("git_output_invalid", stage) from None


def canonical_path(path: Path, *, strict: bool) -> Path:
    """Path.resolve with one failure type: Python 3.12 reports a symlink loop as RuntimeError."""
    try:
        return path.resolve(strict=strict)
    except RecursionError:
        raise
    except RuntimeError:
        raise OSError(errno.ELOOP, os.strerror(errno.ELOOP), os.fspath(path)) from None


def canonical_input_path(path: Path) -> Path:
    """Canonical parent plus the unresolved final name, so a final symlink stays rejectable."""
    return canonical_path(path.parent, strict=True) / path.name


def repository_root(value: str) -> Path:
    try:
        requested = canonical_path(Path(value), strict=True)
        if not requested.is_dir():
            raise OSError
    except OSError:
        raise EvidenceError("repository_unavailable", "resolve-repository") from None
    top = _git_text(requested, ("rev-parse", "--show-toplevel"), "resolve-repository")
    if top is None:
        raise EvidenceError("repository_unavailable", "resolve-repository")
    try:
        root = canonical_path(Path(top), strict=True)
        safe_text(os.fspath(root), MAX_PATH_BYTES)
    except (OSError, EvidenceError):
        raise EvidenceError("repository_unavailable", "resolve-repository") from None
    return root


def _revision(root: Path, reference: str, stage: str, allow_failure: bool = False) -> str | None:
    value = _git_text(root, ("rev-parse", "--verify", f"{reference}^{{commit}}"), stage, allow_failure)
    if value is None:
        return None
    value = value.lower()
    if REVISION.fullmatch(value) is None:
        raise EvidenceError("git_revision_invalid", stage)
    return value


def _git_path(raw: bytes) -> str:
    if not raw or len(raw) > MAX_PATH_BYTES or raw.startswith(b"/") or re.match(rb"^[A-Za-z]:", raw):
        raise EvidenceError("source_path_invalid", "read-source-manifest")
    path = os.fsdecode(raw)
    if any(part in ("", ".", "..") for part in path.split("/")) or SECRET.search(path):
        raise EvidenceError("source_path_invalid", "read-source-manifest")
    return path


def _add_entry(entries: list[dict], entry: dict) -> None:
    entries.append(entry)
    if len(entries) > MAX_FILES:
        raise EvidenceError("too_many_source_files", "read-source-manifest")


def _diff_entries(root: Path, baseline: str) -> list[dict]:
    _, changed = run_git(
        root, "diff", "--name-status", "-z", "--find-renames", baseline, "--",
        stage="read-source-manifest",
    )
    parts = changed.split(b"\0")
    entries: list[dict] = []
    index = 0
    while index < len(parts) and parts[index]:
        try:
            git_status = parts[index].decode("ascii")
        except UnicodeDecodeError:
            raise EvidenceError("git_status_invalid", "read-source-manifest") from None
        index += 1
        kind = git_status[:1]
        if kind not in "MADRCUT":
            raise EvidenceError("git_status_invalid", "read-source-manifest")
        if kind in "RC":
            if index + 1 >= len(parts) or not parts[index] or not parts[index + 1]:
                raise EvidenceError("git_status_invalid", "read-source-manifest")
            old_path, new_path = _git_path(parts[index]), _git_path(parts[index + 1])
            index += 2
            status = "renamed" if kind == "R" else "copied"
            relation = "renamed_from" if kind == "R" else "copied_from"
            _add_entry(entries, {"path": new_path, "git_status": git_status, "status": status, relation: old_path})
            if kind == "R" and old_path != new_path:
                _add_entry(entries, {"path": old_path, "git_status": git_status, "status": "deleted"})
        else:
            if index >= len(parts) or not parts[index]:
                raise EvidenceError("git_status_invalid", "read-source-manifest")
            path = _git_path(parts[index])
            index += 1
            status = {"D": "deleted", "A": "added", "T": "type_changed", "U": "conflicted"}.get(kind, "modified")
            _add_entry(entries, {"path": path, "git_status": git_status, "status": status})

    _, untracked = run_git(
        root, "ls-files", "--others", "--exclude-standard", "-z",
        stage="read-source-manifest",
    )
    for raw in untracked.split(b"\0"):
        if raw:
            _add_entry(entries, {"path": _git_path(raw), "git_status": "??", "status": "untracked"})
    if len({entry["path"] for entry in entries}) != len(entries):
        raise EvidenceError("duplicate_source_path", "read-source-manifest")
    return entries


def _empty_tree(root: Path) -> str:
    value = _git_text(root, ("hash-object", "-t", "tree", "--stdin"), "read-source-manifest")
    if value is None or REVISION.fullmatch(value) is None:
        raise EvidenceError("git_tree_invalid", "read-source-manifest")
    return value


_READ_FAILURE_CODES = {
    ReadFailure.INVALID: "input_not_regular",
    ReadFailure.TOO_LARGE: "input_too_large",
    ReadFailure.CHANGED: "input_unavailable",
    ReadFailure.UNREADABLE: "input_unavailable",
}


def _fingerprint(path: Path, stage: str, limit: int) -> dict:
    """Fingerprint the file observed at open under its canonical parent; a final symlink is refused."""
    try:
        canonical = canonical_input_path(path)
    except OSError:
        raise EvidenceError("input_unavailable", stage) from None
    try:
        result = fingerprint_bounded_regular_file(canonical, min(MAX_FILE_BYTES, limit))
    except BoundedReadError as error:
        raise EvidenceError(_READ_FAILURE_CODES[error.reason], stage) from None
    return {"path": os.fspath(canonical), "size_bytes": result.size, "sha256": result.sha256}


def _source_manifest(root: Path, entries: list[dict]) -> list[dict]:
    output: list[dict] = []
    total = 0
    for entry in entries:
        record = dict(entry)
        if record["status"] == "deleted":
            record.update(size_bytes=None, sha256=None)
        else:
            path = root.joinpath(*record["path"].split("/"))
            try:
                resolved = canonical_path(path, strict=True)
                resolved.relative_to(root)
            except (OSError, ValueError):
                raise EvidenceError("source_path_unavailable", "fingerprint-source") from None
            fingerprint = _fingerprint(path, "fingerprint-source", MAX_TOTAL_BYTES - total)
            total += fingerprint["size_bytes"]
            record.update(size_bytes=fingerprint["size_bytes"], sha256=fingerprint["sha256"])
        output.append(record)
    return sorted(output, key=lambda item: (os.fsencode(item["path"]), item["status"]))


def _canonical_source_paths(root: Path, source: list[dict]) -> set[str]:
    """Canonical present source paths; one that became unresolvable after fingerprinting is refused."""
    try:
        return {
            os.fspath(canonical_path(root.joinpath(*entry["path"].split("/")), strict=False))
            for entry in source if entry["status"] != "deleted"
        }
    except OSError:
        raise EvidenceError("source_path_unavailable", "fingerprint-source") from None


def _relative_requirement(value: str) -> str:
    safe_text(value, MAX_PATH_BYTES)
    if "\\" in value or Path(value).is_absolute() or re.match(r"^[A-Za-z]:", value) or any(part in ("", ".", "..") for part in value.split("/")):
        raise EvidenceError("source_requirement_invalid", "validate-requirements")
    return value


@dataclass(frozen=True)
class EvidenceRequest:
    """Caller-declared inputs; generation and verification build the same record."""

    base: Path
    output: Path | None
    baseline: str | None
    binaries: tuple[tuple[str, str, str], ...]
    receipts: tuple[tuple[str, str], ...]
    transition_rows: tuple[str, ...]
    unresolved: tuple[str, ...]
    require_source: tuple[str, ...]
    require_binary: tuple[str, ...]
    require_receipt: tuple[str, ...]


def _is_output(path: str, output: Path | None) -> bool:
    return output is not None and path == os.fspath(output)


def _artifacts(
    binaries: tuple[tuple[str, str, str], ...], receipts: tuple[tuple[str, str], ...],
    base: Path, output: Path | None,
) -> tuple[list[dict], list[dict]]:
    if len(binaries) + len(receipts) > MAX_ITEMS:
        raise EvidenceError("too_many_inputs", "validate-artifacts")
    roles: set[str] = set()
    paths: set[str] = set()
    total = 0
    binary_records, receipt_records = [], []
    for role, raw_path, identity in binaries:
        safe_text(role, 64)
        if ROLE.fullmatch(role) is None:
            raise EvidenceError("binary_role_invalid", "validate-binary")
        safe_text(identity, 128)
        if role in roles:
            raise EvidenceError("duplicate_binary_role", "validate-binary")
        roles.add(role)
        safe_text(raw_path, MAX_PATH_BYTES)
        path = Path(raw_path)
        if not path.is_absolute():
            path = base / path
        fingerprint = _fingerprint(path, "fingerprint-binary", MAX_TOTAL_BYTES - total)
        if _is_output(fingerprint["path"], output):
            raise EvidenceError("output_overlaps_input", "validate-output", safe_to_write=False)
        if fingerprint["path"] in paths:
            raise EvidenceError("duplicate_input_path", "validate-artifacts")
        paths.add(fingerprint["path"])
        total += fingerprint["size_bytes"]
        binary_records.append({"role": role, "identity": identity, **fingerprint})
    for claim, raw_path in receipts:
        if claim not in CLAIM_CLASSES:
            raise EvidenceError("claim_class_invalid", "validate-receipt")
        safe_text(raw_path, MAX_PATH_BYTES)
        path = Path(raw_path)
        if not path.is_absolute():
            path = base / path
        fingerprint = _fingerprint(path, "fingerprint-receipt", MAX_TOTAL_BYTES - total)
        if _is_output(fingerprint["path"], output):
            raise EvidenceError("output_overlaps_input", "validate-output", safe_to_write=False)
        if fingerprint["path"] in paths:
            raise EvidenceError("duplicate_input_path", "validate-artifacts")
        paths.add(fingerprint["path"])
        total += fingerprint["size_bytes"]
        receipt_records.append({"claim_class": claim, **fingerprint})
    binary_records.sort(key=lambda item: item["role"])
    receipt_records.sort(key=lambda item: (item["claim_class"], os.fsencode(item["path"])))
    return binary_records, receipt_records


def build_document(root: Path, request: EvidenceRequest) -> tuple[dict, int]:
    """Return the index document and its generator exit code for the declared inputs."""
    head = _revision(root, "HEAD", "resolve-head", allow_failure=True)
    baseline = head
    if request.baseline:
        safe_text(request.baseline, 64)
        if REVISION.fullmatch(request.baseline) is None:
            raise EvidenceError("baseline_revision_invalid", "resolve-baseline")
        baseline = _revision(root, request.baseline, "resolve-baseline")
    branch = _git_text(root, ("symbolic-ref", "--quiet", "--short", "HEAD"), "resolve-branch", allow_failure=True)
    if branch is not None:
        safe_text(branch)
    if any(len(values) > MAX_ITEMS for values in (request.transition_rows, request.unresolved, request.require_source, request.require_binary, request.require_receipt)):
        raise EvidenceError("too_many_inputs", "validate-input")
    rows = [safe_text(value, 80) for value in request.transition_rows]
    if any(ROW_ID.fullmatch(row) is None for row in rows) or len(set(rows)) != len(rows):
        raise EvidenceError("transition_row_invalid", "validate-transition-rows")
    unresolved = [safe_text(value) for value in request.unresolved]
    if len(set(unresolved)) != len(unresolved):
        raise EvidenceError("duplicate_unresolved_item", "validate-unresolved")
    required_sources = [_relative_requirement(value) for value in request.require_source]
    required_binaries = [safe_text(value, 64) for value in request.require_binary]
    if any(ROLE.fullmatch(value) is None for value in required_binaries):
        raise EvidenceError("binary_role_invalid", "validate-requirements")
    required_receipts = list(request.require_receipt)
    if any(value not in CLAIM_CLASSES for value in required_receipts):
        raise EvidenceError("claim_class_invalid", "validate-requirements")
    if len(set(required_sources)) != len(required_sources) or len(set(required_binaries)) != len(required_binaries) or len(set(required_receipts)) != len(required_receipts):
        raise EvidenceError("duplicate_requirement", "validate-requirements")

    raw_entries = _diff_entries(root, baseline or _empty_tree(root))
    source = _source_manifest(root, raw_entries)
    binaries, receipts = _artifacts(request.binaries, request.receipts, request.base, request.output)
    input_paths = {record["path"] for record in binaries + receipts}
    source_paths = _canonical_source_paths(root, source)
    if request.output is not None and os.fspath(request.output) in input_paths | source_paths:
        raise EvidenceError("output_overlaps_input", "validate-output", safe_to_write=False)
    if input_paths & source_paths:
        raise EvidenceError("duplicate_input_path", "validate-artifacts")

    missing = {
        "source_paths": sorted(set(required_sources) - {item["path"] for item in source}),
        "binary_roles": sorted(set(required_binaries) - {item["role"] for item in binaries}),
        "receipt_classes": sorted(set(required_receipts) - {item["claim_class"] for item in receipts}),
    }
    document = {
        "schema_version": SCHEMA_VERSION,
        "tool_version": TOOL_VERSION,
        "verdict": "complete",
        "repository": {
            "root": os.fspath(root), "head": head, "baseline": baseline,
            "branch": branch, "detached": branch is None,
        },
        "source_manifest": source,
        "binaries": binaries,
        "receipts": receipts,
        "transition_rows": sorted(rows),
        "unresolved": sorted(unresolved),
        "requirements": {
            "source_paths": sorted(required_sources),
            "binary_roles": sorted(required_binaries),
            "receipt_classes": sorted(required_receipts),
        },
    }
    if any(missing.values()):
        document["missing_required"] = missing
        document["failure"] = {"code": "required_input_missing", "stage": "validate-requirements"}
        document["verdict"] = "failed"
        return document, 1
    if unresolved:
        document["verdict"] = "incomplete"
        return document, 2
    return document, 0
