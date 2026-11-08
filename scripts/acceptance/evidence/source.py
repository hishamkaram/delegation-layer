"""Bounded source observations shared by acceptance and release verification."""
from __future__ import annotations
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
import hashlib
import json
import os
import re
import stat
from scripts.maintainability.python_ast import module_name, parse_source_facts
from scripts.acceptance.evidence.bounded_files import BoundedReadError
from scripts.acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.operations import run_bounded_command

MAX_OUTPUT = 64 * 1024


MAX_SOURCE_FILE = 8 * 1024 * 1024


HEX_SHA256 = re.compile(r"^[0-9a-f]{64}$")


REVISION_RE = re.compile(r"^[0-9a-f]{40,64}$")


class VerificationError(Exception):
    def __init__(self, code: str, stage: str):
        super().__init__(code)
        self.code, self.stage = code, stage


def text(value: object, pattern: re.Pattern[str], stage: str) -> str:
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def revision(value: object, stage: str) -> str:
    return text(value, REVISION_RE, stage)


INHERITED_VARIABLES = ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ",
                       "GOROOT", "GOPATH", "GOMODCACHE", "GOCACHE")


GIT_TIMEOUT_SECONDS = 300


def child_environment(parent: Mapping[str, str]) -> dict[str, str]:
    """Build the credential-free child environment; Go VCS stamping stays enabled.

    Git ignores ``refs/replace`` so every object ID names its own content.
    """
    environment = {key: parent[key] for key in INHERITED_VARIABLES if key in parent}
    if not environment.get("PATH"):
        raise VerificationError("build_path_unavailable", "build-candidate")
    environment.update({
        "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_TERMINAL_PROMPT": "0", "GOTOOLCHAIN": "local",
    })
    return environment


def git(receipt: dict, environment: Mapping[str, str], name: str, arguments: list[str], *,
        cwd: Path, capture: bool = True) -> bytes:
    return command(receipt, name, ["git", *arguments], dict(environment), capture=capture,
                   timeout=GIT_TIMEOUT_SECONDS, cwd=cwd)


def command(receipt: dict, name: str, argv: list[str], env: dict[str, str], *,
            capture=False, timeout=30, cwd: Path | None = None, max_output: int = MAX_OUTPUT) -> bytes:
    """Run one recorded release control child; ``max_output`` bounds its captured output."""
    record = {"name": name, "argv": argv, "exit_code": None}
    if cwd is not None:
        record["cwd"] = str(cwd)
    commands = receipt.get("commands")
    if not isinstance(commands, list):
        raise VerificationError("receipt_commands_invalid", name)
    commands.append(record)
    try:
        result = run_bounded_command(
            argv, env, capture=capture, timeout=timeout, max_output=max_output, cwd=cwd,
        )
    except BoundedCommandError as error:
        if capture:
            record["exit_code"] = error.exit_code
        raise VerificationError(error.reason.value, name) from None
    record["exit_code"] = result.exit_code
    if result.exit_code:
        raise VerificationError("command_failed", name)
    return result.output


def _bounded_file_bytes(path: Path, limit: int, code: str) -> bytes:
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        return read_bounded_regular_file(path, limit)
    except BoundedReadError:
        raise VerificationError(code, "bounded-read") from None


def _reject_parent_components(path: Path, code: str) -> None:
    if ".." in path.parts:
        raise VerificationError(code, "validate-paths")


def _reject_symlink_components(path: Path, code: str) -> None:
    absolute = path.absolute()
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current /= part
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            break
        except OSError as error:
            raise VerificationError(code, "validate-paths") from error
        if stat.S_ISLNK(metadata.st_mode):
            raise VerificationError(code, "validate-paths")


STAGE = "observe-candidate-source"


MAX_TREE_LISTING_OUTPUT = 4 * 1024 * 1024


REGULAR_BLOB_MODES = frozenset({"100644", "100755"})


_TREE_ENTRY_RE = re.compile(rb"([0-7]{6}) (blob|tree|commit) ([0-9a-f]{40,64})\t(.+)", re.DOTALL)


@dataclass(frozen=True)
class TreeEntry:
    mode: str
    kind: str
    object_id: str

    @property
    def regular_blob(self) -> bool:
        return self.kind == "blob" and self.mode in REGULAR_BLOB_MODES


def tree_entries(output: bytes) -> dict[str, TreeEntry]:
    """Decode NUL-terminated ``git ls-tree -z`` output into entries keyed by exact path."""
    if output and not output.endswith(b"\0"):
        raise VerificationError("git_output_invalid", STAGE)
    entries: dict[str, TreeEntry] = {}
    lines = output.split(b"\0")[:-1] if output else []
    for line in lines:
        match = _TREE_ENTRY_RE.fullmatch(line)
        if match is None:
            raise VerificationError("git_output_invalid", STAGE)
        mode, kind, object_id, name = match.groups()
        try:
            path = name.decode("utf-8")
        except UnicodeError:
            raise VerificationError("source_path_not_utf8", STAGE) from None
        if path in entries:
            raise VerificationError("git_output_invalid", STAGE)
        entries[path] = TreeEntry(mode=mode.decode("ascii"), kind=kind.decode("ascii"),
                                  object_id=object_id.decode("ascii"))
    return entries


def _read(receipt: dict, environment: Mapping[str, str], name: str, arguments: list[str], *,
          repository: Path, max_output: int) -> bytes:
    """Run one recorded read-only Git command whose complete output must fit ``max_output``."""
    return command(receipt, name, ["git", *arguments], dict(environment), capture=True,
                   timeout=GIT_TIMEOUT_SECONDS, cwd=repository, max_output=max_output)


def read_regular_blobs(receipt: dict, environment: Mapping[str, str], *, repository: Path, tree: str,
                       paths: tuple[str, ...]) -> dict[str, bytes]:
    """Read the exact bytes of each named regular file of ``tree``; no checkout fallback.

    ``tree`` is the full ID of a commit or tree object, never a revision
    expression such as ``HEAD`` or a branch name, so no ref can choose the bytes.
    """
    revision(tree, STAGE)
    listing = _read(receipt, environment, "git-registry-entries",
                    ["--literal-pathspecs", "ls-tree", "-z", "--full-tree", tree, "--", *paths],
                    repository=repository, max_output=MAX_TREE_LISTING_OUTPUT)
    entries = tree_entries(listing)
    if not set(entries) <= set(paths):
        raise VerificationError("git_output_invalid", "git-registry-entries")
    blobs = {}
    for path in paths:
        entry = entries.get(path)
        if entry is None:
            raise VerificationError("registry_file_missing", STAGE)
        if not entry.regular_blob:
            raise VerificationError("registry_file_not_regular", STAGE)
        blobs[path] = _read(receipt, environment, "git-registry-blob", ["cat-file", "blob", entry.object_id],
                            repository=repository, max_output=MAX_SOURCE_FILE)
    return blobs


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
