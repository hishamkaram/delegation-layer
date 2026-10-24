"""Hash the actual static Python import closure without importing inspected code.

``observe_harness`` also compares that closure with one exact commit of the
checkout it was read from. It creates no receipt and decides no approval.
"""

from __future__ import annotations

from collections.abc import Mapping
import hashlib
import json
from pathlib import Path

from scripts.maintainability.python_ast import module_name, parse_source_facts
from .candidate_build_source import child_environment, git
from .candidate_codec import revision
from .candidate_source_observation import read_regular_blobs
from .contract import HEX_SHA256, MAX_SOURCE_FILE, SOURCE_FILES, VerificationError
from .filesystem import _bounded_file_bytes


PARSER_SOURCE = "scripts/maintainability/python_ast.py"


HARNESS_STAGE = "observe-harness"


# The committed-blob reader's refusals that mean a fingerprinted file is not committed.
NOT_COMMITTED_CODES = frozenset({"registry_file_missing", "registry_file_not_regular"})


def _python_modules(root: Path) -> dict[str, list[str]]:
    modules: dict[str, list[str]] = {}
    for path in sorted((root / "scripts").rglob("*.py")):
        name = path.relative_to(root).as_posix()
        module = module_name(name)
        modules.setdefault(module, []).append(name)
    return modules


def _import_source_paths(module: str, modules: dict[str, list[str]]) -> list[str]:
    components = module.split(".")
    # Hash all maintained candidates without guessing the runtime import search path.
    return [path for size in range(1, len(components) + 1)
            for path in modules.get(".".join(components[:size]), [])]


def fingerprint_sources(root: Path, seeds: tuple[str, ...]) -> dict[str, object]:
    modules = _python_modules(root)
    pending = [*seeds, PARSER_SOURCE]
    observed: dict[str, bytes] = {}
    while pending:
        name = pending.pop()
        if name in observed:
            continue
        data = _bounded_file_bytes(root / name, MAX_SOURCE_FILE, "source_file_invalid")
        observed[name] = data
        if not name.endswith(".py"):
            continue
        pending.extend(_import_source_paths(module_name(name), modules))
        try:
            facts = parse_source_facts(name, data.decode("utf-8"), set(modules))
        except (UnicodeError, SyntaxError, RecursionError) as error:
            raise VerificationError("source_file_invalid", "source-fingerprint") from error
        for item in facts["imports"]:
            target = item["target"].removeprefix("py:")
            pending.extend(_import_source_paths(target, modules))
    files = [{"path": name, "sha256": hashlib.sha256(data).hexdigest()}
             for name, data in sorted(observed.items())]
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"sha256": hashlib.sha256(encoded).hexdigest(), "files": files}


def source_seeds(kind: str) -> tuple[str, ...]:
    """The fingerprint seeds of one release gate: the shared entry points and the gate's own files."""
    return ("scripts/verify_release_archive.py", "scripts/acceptance_supervisor_common.py", *SOURCE_FILES[kind])


def source_fingerprint(kind: str) -> dict[str, object]:
    root = Path(__file__).resolve().parents[3]
    return fingerprint_sources(root, source_seeds(kind))


def observe_harness(receipt: dict, environment: Mapping[str, str], *, checkout: Path,
                    seeds: tuple[str, ...]) -> tuple[str, str, bool]:
    """Return the HEAD commit, fingerprint SHA-256 and committed match of ``checkout``'s harness.

    HEAD is resolved once, and every committed byte is read from that exact
    commit through the credential-free canonical environment, so neither a
    ``refs/replace`` ref nor user or system Git configuration chooses the
    compared objects. A missing, non-regular or different committed file is a
    mismatch, never an error. A malformed fingerprint, a failed or unproven Git
    command, and a working file that changes during the observation refuse.
    Every command is recorded in the caller's ``receipt``.
    """
    git_environment = child_environment(environment)
    head = _head_commit(receipt, git_environment, checkout=checkout)
    fingerprint, files = _fingerprint_facts(fingerprint_sources(checkout, seeds))
    committed = _committed(receipt, git_environment, checkout=checkout, commit=head, files=files)
    for path, digest in files.items():
        data = _bounded_file_bytes(checkout / path, MAX_SOURCE_FILE, "source_file_invalid")
        if hashlib.sha256(data).hexdigest() != digest:
            raise VerificationError("source_changed_during_observation", HARNESS_STAGE)
    return head, fingerprint, committed


def _head_commit(receipt: dict, environment: Mapping[str, str], *, checkout: Path) -> str:
    output = git(receipt, environment, "git-harness-head", ["rev-parse", "--verify", "HEAD^{commit}"],
                 cwd=checkout)
    try:
        return revision(output.decode("ascii").strip(), HARNESS_STAGE)
    except UnicodeError:
        raise VerificationError("git_output_invalid", "git-harness-head") from None


def _fingerprint_facts(fingerprint: dict[str, object]) -> tuple[str, dict[str, str]]:
    """The fingerprint SHA-256 and each relative path's SHA-256; a malformed fingerprint refuses."""
    total, files = fingerprint.get("sha256"), fingerprint.get("files")
    if (not isinstance(total, str) or HEX_SHA256.fullmatch(total) is None
            or not isinstance(files, list) or not files):
        raise VerificationError("source_fingerprint_invalid", HARNESS_STAGE)
    digests: dict[str, str] = {}
    for row in files:
        if not isinstance(row, dict):
            raise VerificationError("source_fingerprint_invalid", HARNESS_STAGE)
        path, digest = row.get("path"), row.get("sha256")
        if not isinstance(path, str) or not path or path.startswith("/") or ".." in Path(path).parts:
            raise VerificationError("source_path_invalid", HARNESS_STAGE)
        if not isinstance(digest, str) or HEX_SHA256.fullmatch(digest) is None or path in digests:
            raise VerificationError("source_fingerprint_invalid", HARNESS_STAGE)
        digests[path] = digest
    return total, digests


def _committed(receipt: dict, environment: Mapping[str, str], *, checkout: Path, commit: str,
               files: dict[str, str]) -> bool:
    try:
        blobs = read_regular_blobs(receipt, environment, repository=checkout, tree=commit, paths=tuple(files))
    except VerificationError as error:
        if error.code in NOT_COMMITTED_CODES:
            return False
        raise
    return all(hashlib.sha256(blobs[path]).hexdigest() == digest for path, digest in files.items())
