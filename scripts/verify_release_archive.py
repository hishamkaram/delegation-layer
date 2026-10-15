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

try:
    from acceptance_supervisor_common import (
        DirectoryOwner,
        acquire_exclusive_file_lock,
        cleanup_journal_digest,
        prepare_directory_cleanup,
        read_directory_cleanup,
        release_exclusive_file_lock,
        resume_directory_cleanup,
    )
    from native_acceptance_evidence import EvidenceError, validate_aggregate_receipt
except ModuleNotFoundError:
    from scripts.acceptance_supervisor_common import (
        DirectoryOwner,
        acquire_exclusive_file_lock,
        cleanup_journal_digest,
        prepare_directory_cleanup,
        read_directory_cleanup,
        release_exclusive_file_lock,
        resume_directory_cleanup,
    )
    from scripts.native_acceptance_evidence import EvidenceError, validate_aggregate_receipt

MAX_RECEIPT = 32 * 1024
MAX_OUTPUT = 64 * 1024
MAX_ARCHIVE = 256 * 1024 * 1024
MAX_BINARY = 128 * 1024 * 1024
MAX_SOURCE_FILE = 8 * 1024 * 1024
MAX_TREE_ENTRIES = 4096
BINARIES = ("delegate", "delegate-run", "pueue", "pueued")
TAG_RE = re.compile(r"^v([0-9]+\.[0-9]+\.[0-9]+)$")
CHECKSUM_RE = re.compile(r"^([0-9a-fA-F]{64})  ([^/\\\r\n]+)$")
REVISION_RE = re.compile(r"^[0-9a-f]{40,64}$")
SOURCE_FILES = {
    "archive-smoke": (".github/workflows/release.yml",),
    "native-acceptance": (
        ".github/workflows/release.yml",
        ".github/workflows/release-native-acceptance.yml",
        "scripts/acceptance_native_all.sh",
        "scripts/acceptance_native.py",
        "scripts/acceptance_provider_common.py",
        "scripts/native_acceptance_evidence.py",
    ),
}


class VerificationError(Exception):
    def __init__(self, code: str, stage: str):
        super().__init__(code)
        self.code, self.stage = code, stage


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as error:
        raise VerificationError("receipt_directory_sync_failed", "write-receipt") from error


def _bounded_file_bytes(path: Path, limit: int, code: str) -> bytes:
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
            raise VerificationError(code, "bounded-read")
        if metadata.st_size > limit:
            raise VerificationError(code, "bounded-read")
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            opened = os.fstat(descriptor)
            if ((opened.st_dev, opened.st_ino) != (metadata.st_dev, metadata.st_ino)
                    or not stat.S_ISREG(opened.st_mode) or opened.st_size > limit):
                raise VerificationError(code, "bounded-read")
            chunks: list[bytes] = []
            remaining = limit
            while remaining:
                chunk = os.read(descriptor, min(1024 * 1024, remaining))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            if os.read(descriptor, 1):
                raise VerificationError(code, "bounded-read")
            return b"".join(chunks)
        finally:
            os.close(descriptor)
    except VerificationError:
        raise
    except OSError:
        raise VerificationError(code, "bounded-read") from None


def bounded_digest(path: Path, limit: int, code: str = "file_too_large") -> str:
    return hashlib.sha256(_bounded_file_bytes(path, limit, code)).hexdigest()


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


def _bounded_children(path: Path, limit: int = MAX_TREE_ENTRIES) -> list[Path]:
    children: list[Path] = []
    try:
        with os.scandir(path) as entries:
            for entry in entries:
                if len(children) >= limit:
                    raise VerificationError("directory_too_large", "bounded-walk")
                children.append(Path(entry.path))
    except VerificationError:
        raise
    except OSError:
        raise VerificationError("directory_unreadable", "bounded-walk") from None
    return children


def write_receipt(path: Path, receipt: dict) -> None:
    data = (json.dumps(receipt, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    if len(data) > MAX_RECEIPT:
        raise VerificationError("receipt_too_large", "write-receipt")
    _reject_parent_components(path, "unsafe_receipt_path")
    _reject_symlink_components(path, "unsafe_receipt_path")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, path)
        _fsync_directory(path.parent)
    finally:
        try:
            os.unlink(name)
        except FileNotFoundError:
            pass


def _acquire_receipt_lock(path: Path) -> int:
    """Serialize one release receipt from replay through terminal publication."""
    _reject_parent_components(path, "unsafe_receipt_path")
    _reject_symlink_components(path, "unsafe_receipt_path")
    path.parent.mkdir(parents=True, exist_ok=True)
    _reject_symlink_components(path.parent, "unsafe_receipt_path")
    return acquire_exclusive_file_lock(path.with_name("." + path.name + ".lock"))


def _terminate_process(process: subprocess.Popen[bytes], name: str) -> None:
    try:
        process.kill()
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=1)
    except subprocess.TimeoutExpired:
        process.kill()
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            raise VerificationError("command_termination_timeout", name) from None


def _pending_pipe_bytes(process: subprocess.Popen[bytes]) -> int | None:
    """Inspect a pipe without adding bytes beyond the capture bound."""
    if process.stdout is None:
        return None
    try:
        import fcntl
        import termios

        result = fcntl.ioctl(process.stdout.fileno(), termios.FIONREAD, struct.pack("I", 0))
        return struct.unpack("I", result)[0]
    except (ImportError, OSError, struct.error, AttributeError):
        return None


def _bounded_command_output(
        process: subprocess.Popen[bytes], name: str, timeout: float) -> bytes:
    if process.stdout is None:
        raise VerificationError("command_capture_unavailable", name)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    captured = bytearray()
    deadline = time.monotonic() + timeout
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                _terminate_process(process, name)
                raise VerificationError("command_timeout", name)
            events = selector.select(remaining)
            if not events:
                if process.poll() is None:
                    _terminate_process(process, name)
                    raise VerificationError("command_timeout", name)
                continue
            if len(captured) == MAX_OUTPUT:
                pending = _pending_pipe_bytes(process)
                if pending is not None:
                    if pending > 0:
                        _terminate_process(process, name)
                        raise VerificationError("command_output_exceeded", name)
                    chunk = b""
                    if process.poll() is None:
                        chunk = os.read(process.stdout.fileno(), 1)
                    if chunk:
                        _terminate_process(process, name)
                        raise VerificationError("command_output_exceeded", name)
                    break
                read_size = 1
            else:
                read_size = min(8192, MAX_OUTPUT - len(captured))
            chunk = os.read(process.stdout.fileno(), read_size)
            if not chunk:
                break
            if len(captured) + len(chunk) > MAX_OUTPUT:
                _terminate_process(process, name)
                raise VerificationError("command_output_exceeded", name)
            captured.extend(chunk)
    finally:
        selector.close()
        process.stdout.close()
    try:
        process.wait(timeout=max(0.0, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        _terminate_process(process, name)
        raise VerificationError("command_timeout", name) from None
    return bytes(captured)


def command(receipt: dict, name: str, argv: list[str], env: dict[str, str], *, capture=False, timeout=30) -> bytes:
    record = {"name": name, "argv": argv, "exit_code": None}
    commands = receipt.get("commands")
    if not isinstance(commands, list):
        raise VerificationError("receipt_commands_invalid", name)
    commands.append(record)
    if not capture:
        try:
            process = subprocess.Popen(
                argv, env=env, stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
        except subprocess.TimeoutExpired:
            raise VerificationError("command_timeout", name) from None
        except OSError:
            raise VerificationError("command_start_failed", name) from None
        try:
            result = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            _terminate_process(process, name)
            raise VerificationError("command_timeout", name) from None
        record["exit_code"] = result
        if result:
            raise VerificationError("command_failed", name)
        return b""
    try:
        process = subprocess.Popen(
            argv, env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        )
    except OSError:
        raise VerificationError("command_start_failed", name) from None
    try:
        output = _bounded_command_output(process, name, timeout)
    except VerificationError:
        record["exit_code"] = process.returncode
        raise
    record["exit_code"] = process.returncode
    if process.returncode:
        raise VerificationError("command_failed", name)
    return output


def platform_identity() -> dict[str, str]:
    os_name = {"Linux": "Linux", "Darwin": "Darwin"}.get(platform.system())
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(
        platform.machine().lower()
    )
    if os_name is None or arch is None:
        raise VerificationError("unsupported_native_platform", "inspect-platform")
    return {"os": os_name, "kernel": platform.release(), "arch": arch}


def source_fingerprint(kind: str) -> dict:
    root = Path(__file__).resolve().parent.parent
    names = tuple(dict.fromkeys((
        "scripts/verify_release_archive.py", "scripts/acceptance_supervisor_common.py",
        *SOURCE_FILES[kind],
    )))
    files = [{"path": name, "sha256": bounded_digest(root / name, MAX_SOURCE_FILE,
                                                        "source_file_invalid")} for name in names]
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"sha256": hashlib.sha256(encoded).hexdigest(), "files": files}


def release_paths(args, receipt_path: Path) -> tuple[Path, Path, Path]:
    value = os.environ.get("RUNNER_TEMP")
    if not value:
        raise VerificationError("runner_temp_unavailable", "validate-paths")
    runner_value = Path(value)
    _reject_parent_components(runner_value, "unsafe_release_paths")
    _reject_symlink_components(runner_value, "unsafe_release_paths")
    _reject_parent_components(Path(args.assets_dir), "unsafe_release_paths")
    _reject_symlink_components(Path(args.assets_dir), "unsafe_release_paths")
    _reject_parent_components(Path(args.extract_dir), "unsafe_release_paths")
    _reject_symlink_components(Path(args.extract_dir), "unsafe_release_paths")
    _reject_parent_components(receipt_path, "unsafe_release_paths")
    _reject_symlink_components(receipt_path, "unsafe_release_paths")
    runner = runner_value.resolve()
    assets, extracted = runner / "delegation-layer-release", runner / "delegation-layer-release" / "extracted"
    if Path(args.assets_dir).resolve() != assets or Path(args.extract_dir).resolve() != extracted:
        raise VerificationError("unsafe_release_paths", "validate-paths")
    if assets.is_symlink() or extracted.is_symlink():
        raise VerificationError("unsafe_release_paths", "validate-paths")
    try:
        receipt_path.resolve().relative_to(assets)
    except ValueError:
        return runner, assets, extracted
    raise VerificationError("receipt_inside_cleanup_scope", "validate-paths")


def expected_archive(info: dict[str, str], tag: str) -> str:
    match = TAG_RE.fullmatch(tag)
    if match is None:
        raise VerificationError("invalid_release_tag", "validate-tag")
    return f"delegation-layer_{match.group(1)}_{info['os']}_{info['arch']}.tar.gz"


def resolve_release_identity(repo: str, tag: str, receipt: dict, env: dict[str, str]) -> dict[str, str]:
    ref = f"refs/tags/{tag}"
    raw = command(receipt, "github-release-ref", [
        "gh", "api", f"repos/{repo}/git/ref/tags/{tag}",
        "--jq", ".ref + \"\\t\" + .object.type + \"\\t\" + .object.sha",
    ], env, capture=True, timeout=30)
    try:
        observed_ref, object_type, object_sha = raw.decode("ascii").strip().split("\t")
    except (UnicodeError, ValueError):
        raise VerificationError("release_ref_invalid", "resolve-release-identity") from None
    if observed_ref != ref or not REVISION_RE.fullmatch(object_sha):
        raise VerificationError("release_ref_invalid", "resolve-release-identity")
    depth = 0
    while object_type == "tag":
        depth += 1
        if depth > 3:
            raise VerificationError("release_tag_chain_too_deep", "resolve-release-identity")
        raw = command(receipt, "github-release-tag-object", [
            "gh", "api", f"repos/{repo}/git/tags/{object_sha}",
            "--jq", ".object.type + \"\\t\" + .object.sha",
        ], env, capture=True, timeout=30)
        try:
            object_type, object_sha = raw.decode("ascii").strip().split("\t")
        except (UnicodeError, ValueError):
            raise VerificationError("release_tag_object_invalid", "resolve-release-identity") from None
        if not REVISION_RE.fullmatch(object_sha):
            raise VerificationError("release_tag_object_invalid", "resolve-release-identity")
    if object_type != "commit":
        raise VerificationError("release_commit_invalid", "resolve-release-identity")
    return {"repository": repo, "tag": tag, "ref": ref, "commit_sha": object_sha}


def gh_environment() -> dict[str, str]:
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        raise VerificationError("github_auth_unavailable", "github-auth")
    env = os.environ.copy()
    env.update({"GH_TOKEN": token, "GH_PROMPT_DISABLED": "1"})
    return env


def remember_directory(receipt: dict, token) -> None:
    receipt["cleanup"]["directory_identities"][str(token.directory.resolve())] = {
        "path": str(token.directory.resolve()),
        "parent": str(token.parent.resolve()),
        "device": token.identity[0], "inode": token.identity[1],
        "parent_device": token.parent_identity[0], "parent_inode": token.parent_identity[1],
    }


def verify_checksum(assets: Path, name: str, receipt: dict) -> Path:
    archive, checksums = assets / name, assets / "checksums.txt"
    if any(path.is_symlink() or not path.is_file() for path in (archive, checksums)):
        others = [item for item in _bounded_children(assets)
                  if item.name.startswith("delegation-layer_") and item.name.endswith(".tar.gz")]
        code = "archive_path_mismatch" if not archive.exists() and others else "release_asset_missing"
        raise VerificationError(code, "locate-assets")
    if archive.stat().st_size > MAX_ARCHIVE:
        raise VerificationError("archive_too_large", "verify-checksum")
    if checksums.stat().st_size > MAX_OUTPUT:
        raise VerificationError("checksum_file_too_large", "verify-checksum")
    try:
        raw = _bounded_file_bytes(checksums, MAX_OUTPUT, "checksum_file_too_large")
    except VerificationError:
        raise VerificationError("checksum_file_too_large", "verify-checksum") from None
    try:
        lines = raw.decode("ascii").splitlines()
    except UnicodeError:
        raise VerificationError("checksum_file_invalid", "verify-checksum") from None
    entries = {}
    for line in lines:
        match = CHECKSUM_RE.fullmatch(line)
        if match is None or match.group(2) in entries:
            raise VerificationError("checksum_file_invalid", "verify-checksum")
        entries[match.group(2)] = match.group(1).lower()
    archive_hash = bounded_digest(archive, MAX_ARCHIVE, "archive_too_large")
    if entries.get(name) != archive_hash:
        raise VerificationError("archive_checksum_mismatch", "verify-checksum")
    receipt["artifact"].update({
        "archive_name": name, "archive_path": str(archive.resolve()), "archive_sha256": archive_hash,
        "checksums_path": str(checksums.resolve()),
        "checksums_sha256": bounded_digest(checksums, MAX_OUTPUT, "checksum_file_too_large"),
    })
    return archive


def extract_archive(archive: Path, target: Path, owner: DirectoryOwner, receipt: dict) -> None:
    token = owner.create("release extraction", target, target.parent)
    remember_directory(receipt, token)
    receipt["artifact"]["extraction_path"] = str(target.resolve())
    found = set()
    try:
        with tarfile.open(archive, "r|gz") as bundle:
            for count, member in enumerate(bundle, 1):
                parts = PurePosixPath(member.name).parts
                if (count > 256 or len(member.name) > 4096 or member.size < 0
                        or member.size > MAX_BINARY or member.name.startswith("/")
                        or "\\" in member.name or ".." in parts):
                    raise VerificationError("archive_member_invalid", "extract-archive")
                if member.name in BINARIES:
                    if not member.isfile() or member.name in found:
                        raise VerificationError("archive_member_invalid", "extract-archive")
                    source = bundle.extractfile(member)
                    if source is None:
                        raise VerificationError("archive_member_invalid", "extract-archive")
                    with source, (target / member.name).open("xb") as output:
                        shutil.copyfileobj(source, output, 1024 * 1024)
                    (target / member.name).chmod(member.mode & 0o777)
                    found.add(member.name)
                elif not member.isdir() and not (member.isfile() and len(parts) == 1):
                    raise VerificationError("archive_member_invalid", "extract-archive")
    except (OSError, tarfile.TarError):
        raise VerificationError("archive_invalid", "extract-archive") from None
    if found != set(BINARIES):
        raise VerificationError("release_binary_missing", "extract-archive")


def inspect_binaries(directory: Path, receipt: dict) -> dict[str, Path]:
    paths = {}
    for name in BINARIES:
        path = directory / name
        try:
            info = path.lstat()
        except OSError:
            raise VerificationError("release_binary_missing", "inspect-binaries") from None
        resolved = path.resolve(strict=True)
        if (not stat.S_ISREG(info.st_mode) or not os.access(path, os.X_OK)
                or resolved.parent != directory.resolve() or info.st_size > MAX_BINARY):
            raise VerificationError("release_binary_invalid", "inspect-binaries")
        receipt["binaries"][name] = {
            "path": str(resolved), "sha256": bounded_digest(path, MAX_BINARY, "release_binary_invalid"),
        }
        paths[name] = resolved
    return paths


def binary_version(output: bytes, name: str) -> str:
    try:
        line = output.decode("utf-8").splitlines()[0].strip()
        value = line[len(name) + 1:].split(maxsplit=1)[0] if line.startswith(name + " ") else ""
    except (UnicodeError, IndexError):
        value = ""
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", value):
        raise VerificationError("version_output_invalid", f"version-{name}")
    return value


def exercise_default_path(paths: dict[str, Path], directory: Path, receipt: dict,
                          package_version: str, info: dict[str, str], home: Path) -> None:
    config = home / ".config"
    config.mkdir()
    if info["os"] == "Darwin":
        (home / "Library" / "Application Support").mkdir(parents=True)
    search_path = os.pathsep.join((str(directory), "/usr/bin", "/bin", "/usr/sbin", "/sbin"))
    env = {"PATH": search_path, "HOME": str(home), "XDG_CONFIG_HOME": str(config),
           "TMPDIR": str(home), "LANG": "C", "LC_ALL": "C"}
    for name in ("delegate", "pueue", "pueued"):
        found = shutil.which(name, path=search_path)
        if found is None or Path(found).resolve() != paths[name]:
            raise VerificationError("binary_path_mismatch", f"resolve-{name}")

    actual = binary_version(command(receipt, "delegate-version", ["delegate", "version"], env, capture=True), "delegate")
    receipt["delegate_build_identity"] = {"version": actual}
    if actual != package_version:
        raise VerificationError("delegate_version_mismatch", "verify-delegate-version")
    for name in ("pueue", "pueued"):
        receipt["binaries"][name]["version"] = binary_version(
            command(receipt, f"{name}-version", [name, "--version"], env, capture=True), name
        )
    for label, argv in (
        ("delegate-providers", ["delegate", "providers", "--json"]),
        ("delegate-capabilities", ["delegate", "capabilities", "--provider", "pi:json", "--json"]),
    ):
        command(receipt, label, argv, env)

    base = home / "Library" / "Application Support" if info["os"] == "Darwin" else config
    state = base / "delegation-layer"
    before = "present" if state.exists() else "absent"
    argv = ["delegate", "supervisor", "status", "--json"]
    try:
        status = json.loads(command(receipt, "delegate-default-supervisor-status", argv, env, capture=True))
        observed = status["supervisor_control"]["state"]
    except (UnicodeError, json.JSONDecodeError, KeyError, TypeError):
        raise VerificationError("default_path_observation_invalid", "observe-default-state") from None
    after = "present" if state.exists() else "absent"
    receipt["default_path_observation"] = {
        "path_source": "PATH", "command_name": "delegate-default-supervisor-status",
        "explicit_path_overrides": [],
        "isolated_home": str(home), "default_state_root": str(state),
        "state_before": before, "observed_state": observed, "state_after": after,
        "delegate_pueue_config_override": "unset",
    }
    if status.get("command") != "supervisor" or (before, observed, after) != ("absent", "not_initialized", "absent"):
        raise VerificationError("default_path_observation_mismatch", "observe-default-state")


def new_receipt(kind: str, tag: str, revision: str) -> dict:
    match = TAG_RE.fullmatch(tag)
    return {
        "schema_version": 1, "claim": "packaged-default-path", "gate": kind,
        "verdict": "in_progress", "stage": "initialize", "tag": tag,
        "package_version": match.group(1) if match else None,
        # source_revision is retained as a compatibility label for older
        # consumers; the distinct fields below are authoritative.
        "source_revision": revision, "harness_revision": revision, "release_revision": None,
        "release_identity": {},
        "platform": {}, "artifact": {}, "binaries": {}, "delegate_build_identity": {},
        "default_path_observation": {}, "commands": [], "source_harness": {},
        "cleanup": {"status": "pending", "removed_paths": [], "retained_paths": [],
                    "directory_identities": {}, "errors": []},
        "failure": None,
        "unresolved_limits": [
            "No authenticated provider acceptance or task execution." if kind == "archive-smoke"
            else "Native evidence covers only the providers and scenarios recorded by acceptance_native_all.sh."
        ],
    }


def set_failure(receipt: dict, code: str, stage: str) -> None:
    receipt.update({"verdict": "fail", "stage": stage, "failure": {"code": code, "stage": stage}})


def _release_cleanup_authorization(
        receipt: dict, final_verdict: str, target_identity: dict) -> str:
    immutable = {
        key: value for key, value in receipt.items()
        if key not in {"cleanup", "completion", "proof", "verdict", "stage", "failure"}
    }
    immutable["final_verdict"] = final_verdict
    immutable["cleanup_target_identity"] = target_identity
    data = json.dumps(
        immutable, sort_keys=True, separators=(",", ":"), allow_nan=False,
    ).encode()
    return hashlib.sha256(data).hexdigest()


def _release_cleanup_transaction(receipt: dict) -> tuple[dict, dict, str]:
    cleanup = receipt.get("cleanup")
    proof = receipt.get("proof")
    transaction = cleanup.get("transaction") if isinstance(cleanup, dict) else None
    if (not isinstance(cleanup, dict) or not isinstance(proof, dict)
            or proof.get("state") != "committed"
            or proof.get("final_verdict") not in {"pass", "blocked"}
            or not isinstance(proof.get("authorization_sha256"), str)
            or not isinstance(transaction, dict)
            or transaction.get("authorization_sha256") != proof["authorization_sha256"]
            or not isinstance(transaction.get("journal_path"), str)
            or not isinstance(transaction.get("target_identity"), dict)):
        raise VerificationError("release_cleanup_journal_invalid", "cleanup-replay")
    authorization = _release_cleanup_authorization(
        receipt, proof["final_verdict"], transaction["target_identity"],
    )
    if authorization != proof["authorization_sha256"]:
        raise VerificationError("release_cleanup_authorization_changed", "cleanup-replay")
    return cleanup, transaction, authorization


def _replay_prepared_cleanup(
        path: Path,
        *,
        gate: str,
        claim: str,
        tag: str,
        revision: str) -> int | None:
    if not path.exists() and not path.is_symlink():
        return None
    if path.is_symlink():
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
    try:
        raw = _bounded_file_bytes(path, MAX_RECEIPT, "release_cleanup_journal_invalid")
        receipt = json.loads(raw)
    except (VerificationError, UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay") from error
    if not isinstance(receipt, dict):
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
    matches_request = (receipt.get("gate") == gate and receipt.get("claim") == claim
                       and receipt.get("tag") == tag
                       and receipt.get("source_revision") == revision)
    completion = receipt.get("completion")
    if (isinstance(completion, dict) and completion.get("state") == "complete"):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        cleanup, transaction, authorization = _release_cleanup_transaction(receipt)
        journal = Path(transaction["journal_path"])
        try:
            record = read_directory_cleanup(journal, authorization)
            digest = cleanup_journal_digest(journal)
        except (OSError, RuntimeError, ValueError) as error:
            raise VerificationError("release_cleanup_journal_invalid", "receipt-replay") from error
        if (receipt.get("verdict") not in {"pass", "blocked"}
                or cleanup.get("status") != "complete"
                or record.get("state") != "complete"
                or record.get("target") != transaction["target_identity"]
                or transaction.get("journal_sha256") != digest):
            raise VerificationError("release_terminal_receipt_invalid", "receipt-replay")
        return 0
    cleanup = receipt.get("cleanup")
    if (receipt.get("verdict") == "fail" and isinstance(cleanup, dict)
            and cleanup.get("status") == "retained-for-investigation"):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        return 1
    if (receipt.get("stage") == "complete" and receipt.get("verdict") in {"pass", "blocked"}):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        if (isinstance(cleanup, dict)
                and cleanup.get("status") == "retained-for-native-acceptance"):
            return 0
        raise VerificationError("release_terminal_receipt_invalid", "receipt-replay")
    if (not isinstance(cleanup, dict) or cleanup.get("status") not in {"prepared", "incomplete"}
            or not isinstance(completion, dict) or completion.get("state") != "prepared"):
        if receipt.get("verdict") != "in_progress" or receipt.get("stage") != "initialize":
            raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
        if not matches_request:
            raise VerificationError("release_existing_receipt_mismatch", "receipt-replay")
        return None
    if not matches_request:
        raise VerificationError("release_cleanup_journal_mismatch", "cleanup-replay")
    cleanup, transaction, authorization = _release_cleanup_transaction(receipt)
    journal = Path(transaction["journal_path"])
    expected_journal = path.with_name(path.stem + ".cleanup.json")
    if journal.absolute() != expected_journal.absolute():
        raise VerificationError("release_cleanup_journal_mismatch", "cleanup-replay")
    identity = transaction["target_identity"]
    try:
        if not journal.exists() and not journal.is_symlink():
            prepare_directory_cleanup(
                journal,
                "release verification assets",
                identity,
                Path(identity["parent"]),
                authorization,
            )
        else:
            record = read_directory_cleanup(journal, authorization)
            if record.get("target") != identity:
                raise RuntimeError("release cleanup target identity changed")
        record = resume_directory_cleanup(journal, authorization, identity)
        digest = cleanup_journal_digest(journal)
    except (OSError, RuntimeError, ValueError) as error:
        raise VerificationError("release_cleanup_failed", "cleanup-replay") from error
    if record.get("state") != "complete" or record.get("target") != identity:
        raise VerificationError("release_cleanup_incomplete", "cleanup-replay")
    receipt["verdict"] = receipt["proof"]["final_verdict"]
    receipt["stage"] = "complete"
    identities = cleanup.get("directory_identities")
    removed = list(cleanup.get("removed_paths", []))
    removed.extend(sorted(identities) if isinstance(identities, dict) else [identity["path"]])
    receipt["cleanup"].update({
        "status": "complete",
        "removed_paths": list(dict.fromkeys(removed)),
        "retained_paths": [],
    })
    receipt["cleanup"]["transaction"] = {
        **transaction,
        "journal_sha256": digest,
    }
    receipt["failure"] = None
    receipt["completion"] = {"state": "complete"}
    write_receipt(path, receipt)
    return 0


def finish_gate(path: Path, receipt: dict, owner: DirectoryOwner, action,
                cleanup_mode: str, locations: dict[str, Path]) -> int:
    error = None
    try:
        action()
    except VerificationError as caught:
        error = caught
    except Exception:
        error = VerificationError("verification_internal_error", receipt["stage"])

    identities = receipt["cleanup"]["directory_identities"]
    temporary = locations.get("isolated_home")
    incidental_removed = (
        [str(temporary)] if temporary is not None and not os.path.lexists(temporary) else []
    )
    final_verdict = receipt.get("verdict") if receipt.get("verdict") == "blocked" else "pass"
    if error is None and cleanup_mode == "remove":
        assets = locations.get("assets")
        target = identities.get(str(assets.resolve())) if assets is not None else None
        if not isinstance(target, dict):
            error = VerificationError("release_cleanup_identity_missing", "cleanup")

    if error is None and cleanup_mode == "remove":
        journal = path.with_name(path.stem + ".cleanup.json")
        authorization = _release_cleanup_authorization(receipt, final_verdict, target)
        receipt["stage"] = "cleanup-prepared"
        receipt["verdict"] = "in_progress"
        receipt["failure"] = None
        receipt["proof"] = {
            "state": "committed",
            "authorization_sha256": authorization,
            "final_verdict": final_verdict,
        }
        receipt["cleanup"].update({
            "status": "prepared",
            "removed_paths": incidental_removed,
            "retained_paths": [],
            "transaction": {
                "journal_path": str(journal.absolute()),
                "authorization_sha256": authorization,
                "target_identity": target,
            },
        })
        receipt["completion"] = {"state": "prepared"}
        try:
            write_receipt(path, receipt)
        except BaseException:
            owner.release()
            raise

        try:
            owner.release()
            prepare_directory_cleanup(
                journal,
                "release verification assets",
                target,
                Path(target["parent"]),
                authorization,
            )
            record = resume_directory_cleanup(journal, authorization, target)
            digest = cleanup_journal_digest(journal)
            if record.get("state") != "complete" or record.get("target") != target:
                raise RuntimeError("release cleanup did not reach its committed state")
        except (OSError, RuntimeError, ValueError) as caught:
            receipt["cleanup"]["status"] = "incomplete"
            receipt["cleanup"]["errors"].append("release_cleanup_failed")
            set_failure(receipt, "release_cleanup_failed", "cleanup")
            receipt["completion"] = {"state": "prepared"}
            write_receipt(path, receipt)
            print("release verification failed: release_cleanup_failed", file=sys.stderr)
            return 1

        receipt["verdict"] = final_verdict
        receipt["stage"] = "complete"
        receipt["failure"] = None
        receipt["cleanup"].update({
            "status": "complete",
            "removed_paths": list(dict.fromkeys([*incidental_removed, *identities])),
            "retained_paths": [],
            "transaction": {
                **receipt["cleanup"]["transaction"],
                "journal_sha256": digest,
            },
        })
        receipt["completion"] = {"state": "complete"}
        write_receipt(path, receipt)
        return 0

    try:
        owner.release()
    except Exception:
        receipt["cleanup"]["errors"].append("owned_directory_identity_check_failed")
        if error is None:
            error = VerificationError("release_cleanup_failed", "cleanup")

    paths = list(identities)
    if error is None:
        receipt["verdict"] = final_verdict
        receipt["stage"] = "complete"
        receipt["cleanup"].update({
            "status": "retained-for-native-acceptance",
            "removed_paths": incidental_removed,
            "retained_paths": paths,
        })
    else:
        set_failure(receipt, error.code, error.stage)
        receipt["cleanup"]["status"] = "retained-for-investigation"
        retained = paths + [str(item) for item in locations.values() if os.path.lexists(item)]
        receipt["cleanup"]["retained_paths"] = list(dict.fromkeys(
            item for item in retained if os.path.lexists(item)
        ))
    write_receipt(path, receipt)
    if error is not None:
        print(f"release verification failed: {error.code}", file=sys.stderr)
        return 1
    return 0


def verify(args: argparse.Namespace) -> int:
    receipt_path = Path(args.receipt).absolute()
    lock_fd = _acquire_receipt_lock(receipt_path)
    try:
        return _verify_locked(args, receipt_path)
    finally:
        release_exclusive_file_lock(lock_fd)


def _verify_locked(args: argparse.Namespace, receipt_path: Path) -> int:
    replayed = _replay_prepared_cleanup(
        receipt_path, gate=args.mode, claim="packaged-default-path",
        tag=args.tag, revision=args.source_revision,
    )
    if replayed is not None:
        return replayed
    receipt = new_receipt(args.mode, args.tag, args.source_revision)
    write_receipt(receipt_path, receipt)
    owner = DirectoryOwner()
    locations = {}

    def action():
        runner, assets, extracted = release_paths(args, receipt_path)
        locations.update(assets=assets, extracted=extracted)
        if not TAG_RE.fullmatch(args.tag) or not REVISION_RE.fullmatch(args.source_revision):
            raise VerificationError("release_identity_invalid", "validate-release-identity")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo):
            raise VerificationError("repository_invalid", "validate-repository")
        info = platform_identity()
        receipt["platform"] = info
        receipt["source_harness"] = source_fingerprint(args.mode)
        release_identity = resolve_release_identity(args.repo, args.tag, receipt, gh_environment())
        receipt["release_identity"] = release_identity
        receipt["release_revision"] = release_identity["commit_sha"]
        receipt["harness_revision"] = args.source_revision
        name = expected_archive(info, args.tag)
        if args.mode == "archive-smoke":
            token = owner.create("release assets", assets, runner)
        else:
            if not assets.is_dir():
                raise VerificationError("release_assets_missing", "prepare-assets")
            token = owner.adopt_created("release assets", assets, runner)
        remember_directory(receipt, token)
        if args.mode == "archive-smoke":
            command(receipt, "github-release-download", [
                "gh", "release", "download", args.tag, "--repo", args.repo,
                "--pattern", name, "--pattern", "checksums.txt", "--dir", str(assets),
            ], gh_environment(), timeout=120)
        receipt["stage"] = "verify-checksum"
        archive = verify_checksum(assets, name, receipt)
        command(receipt, "github-attestation-verify", [
            "gh", "attestation", "verify", str(archive), "--repo", args.repo,
            "--signer-workflow", f"{args.repo}/.github/workflows/release.yml",
            "--source-ref", release_identity["ref"],
            "--source-digest", release_identity["commit_sha"],
        ], gh_environment(), timeout=60)
        receipt["artifact"]["attestation"] = "verified"
        receipt["stage"] = "extract-archive"
        extract_archive(archive, extracted, owner, receipt)
        paths = inspect_binaries(extracted, receipt)
        with tempfile.TemporaryDirectory(prefix="delegate-release-home-", dir=assets) as home:
            locations["isolated_home"] = Path(home)
            receipt["stage"] = "exercise-packaged-cli"
            exercise_default_path(paths, extracted, receipt, receipt["package_version"], info, Path(home))

    return finish_gate(receipt_path, receipt, owner, action,
                       "remove" if args.mode == "archive-smoke" else "retain", locations)


def read_receipt(path: Path) -> tuple[dict, str]:
    try:
        data = _bounded_file_bytes(path, MAX_RECEIPT, "archive_receipt_invalid")
        value = json.loads(data)
    except VerificationError as error:
        code = "archive_receipt_missing_or_invalid" if not path.exists() else "archive_receipt_invalid"
        raise VerificationError(code, "read-archive-receipt") from error
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("archive_receipt_missing_or_invalid", "read-archive-receipt") from None
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        raise VerificationError("archive_receipt_invalid", "read-archive-receipt")
    return value, hashlib.sha256(data).hexdigest()


def exit_code(path: str | None) -> int | None:
    if not path:
        return None
    try:
        raw = _bounded_file_bytes(Path(path), 16, "exit_code_file_invalid").decode("ascii")
    except (VerificationError, UnicodeError):
        return None
    return int(raw.strip()) if re.fullmatch(r"-?[0-9]{1,3}\n?", raw) else None


def finalize_native(args: argparse.Namespace) -> int:
    receipt_path = Path(args.receipt).absolute()
    lock_fd = _acquire_receipt_lock(receipt_path)
    try:
        return _finalize_native_locked(args, receipt_path)
    finally:
        release_exclusive_file_lock(lock_fd)


def _finalize_native_locked(args: argparse.Namespace, receipt_path: Path) -> int:
    replayed = _replay_prepared_cleanup(
        receipt_path, gate="native-acceptance", claim="native-acceptance-case-results",
        tag=args.tag, revision=args.source_revision,
    )
    if replayed is not None:
        return replayed
    receipt = new_receipt("native-acceptance", args.tag, args.source_revision)
    receipt["claim"] = "native-acceptance-case-results"
    receipt["claims_scope"] = "provider-permission-scenario-case-results-only"
    write_receipt(receipt_path, receipt)
    owner, locations = DirectoryOwner(), {}

    def action():
        runner, assets, extracted = release_paths(args, receipt_path)
        locations.update(assets=assets, extracted=extracted)
        if not REVISION_RE.fullmatch(args.source_revision):
            raise VerificationError("source_revision_invalid", "validate-source-revision")
        info = platform_identity()
        receipt["platform"] = info
        receipt["source_harness"] = source_fingerprint("native-acceptance")
        receipt["stage"] = "validate-archive-receipt"
        aggregate_argument = getattr(args, "aggregate_receipt", None)
        archived, archive_receipt_hash = read_receipt(Path(args.archive_receipt).absolute())
        match = TAG_RE.fullmatch(args.tag)
        if (match is None or archived.get("claim") != "packaged-default-path"
                or archived.get("gate") != "native-acceptance" or archived.get("verdict") != "pass"
                or archived.get("tag") != args.tag or archived.get("package_version") != match.group(1)
                or archived.get("platform") != info
                or archived.get("harness_revision") != args.source_revision
                or archived.get("source_revision") != args.source_revision):
            raise VerificationError("archive_receipt_not_passing", "validate-archive-receipt")
        release_identity = archived.get("release_identity")
        if (not isinstance(release_identity, dict)
                or release_identity.get("tag") != args.tag
                or release_identity.get("ref") != f"refs/tags/{args.tag}"
                or (getattr(args, "repo", None) is not None
                    and release_identity.get("repository") != args.repo)
                or not REVISION_RE.fullmatch(str(release_identity.get("commit_sha", "")))
                or archived.get("release_revision") != release_identity.get("commit_sha")):
            raise VerificationError("release_identity_unbound", "validate-archive-receipt")
        if not aggregate_argument:
            raise VerificationError("aggregate_receipt_missing", "validate-aggregate-receipt")
        _reject_parent_components(Path(aggregate_argument), "unsafe_aggregate_path")
        _reject_parent_components(Path(args.archive_receipt), "unsafe_archive_receipt_path")
        aggregate_path = Path(aggregate_argument).absolute()
        try:
            aggregate_path.resolve().relative_to(assets.resolve())
        except ValueError:
            pass
        else:
            raise VerificationError("aggregate_receipt_inside_cleanup_scope", "validate-aggregate-receipt")
        aggregate_root = aggregate_path.parent
        if not aggregate_root.is_dir() or aggregate_root.is_symlink():
            raise VerificationError("aggregate_receipt_missing_or_invalid", "validate-aggregate-receipt")
        locations["aggregate_root"] = aggregate_root
        artifact = archived.get("artifact")
        archive_name = expected_archive(info, args.tag)
        archive_file, checksum_file = assets / archive_name, assets / "checksums.txt"
        if (not isinstance(artifact, dict) or artifact.get("attestation") != "verified"
                or not re.fullmatch(r"[0-9a-f]{64}", str(artifact.get("archive_sha256", "")))
                or not re.fullmatch(r"[0-9a-f]{64}", str(artifact.get("checksums_sha256", "")))
                or artifact.get("archive_name") != archive_name
                or archive_file.is_symlink() or checksum_file.is_symlink()
                or Path(str(artifact.get("archive_path", ""))).resolve() != archive_file.resolve()
                or Path(str(artifact.get("checksums_path", ""))).resolve() != checksum_file.resolve()
                or Path(str(artifact.get("extraction_path", ""))).resolve() != extracted.resolve()
                or not archive_file.is_file() or not checksum_file.is_file()
                or bounded_digest(archive_file, MAX_ARCHIVE, "archive_too_large")
                != artifact.get("archive_sha256")
                or bounded_digest(checksum_file, MAX_OUTPUT, "checksum_file_too_large")
                != artifact.get("checksums_sha256")):
            raise VerificationError("archive_receipt_invalid", "validate-archive-receipt")
        observation = archived.get("default_path_observation")
        if (not isinstance(observation, dict) or observation.get("path_source") != "PATH"
                or observation.get("command_name") != "delegate-default-supervisor-status"
                or observation.get("explicit_path_overrides") != []
                or (observation.get("state_before"), observation.get("observed_state"), observation.get("state_after"))
                != ("absent", "not_initialized", "absent")
                or observation.get("delegate_pueue_config_override") != "unset"):
            raise VerificationError("default_path_observation_invalid", "validate-archive-receipt")
        commands = archived.get("commands")
        required_commands = {
            "github-attestation-verify", "delegate-version", "pueue-version", "pueued-version",
            "delegate-providers", "delegate-capabilities", "delegate-default-supervisor-status",
        }
        if not isinstance(commands, list):
            raise VerificationError("archive_receipt_inconclusive", "validate-archive-receipt")
        command_map = {row.get("name"): row for row in commands if isinstance(row, dict)}
        if (len(command_map) != len(commands) or not required_commands.issubset(command_map)
                or any(row.get("exit_code") != 0 for row in command_map.values())
                or command_map["delegate-default-supervisor-status"].get("argv")
                != ["delegate", "supervisor", "status", "--json"]):
            raise VerificationError("archive_receipt_inconclusive", "validate-archive-receipt")

        asset_token = owner.adopt_created("release assets", assets, runner)
        recorded = archived.get("cleanup", {}).get("directory_identities", {})
        asset_identity = recorded.get(str(assets.resolve())) if isinstance(recorded, dict) else None
        if (not isinstance(asset_identity, dict)
                or asset_identity.get("path") != str(assets.resolve())
                or asset_identity.get("parent") != str(runner.resolve())
                or (asset_identity.get("device"), asset_identity.get("inode")) != asset_token.identity
                or (asset_identity.get("parent_device"), asset_identity.get("parent_inode"))
                != asset_token.parent_identity):
            raise VerificationError("release_directory_identity_changed", "validate-release-directory")
        remember_directory(receipt, asset_token)
        extracted_token = owner.adopt_created("release extraction", extracted, assets)
        extracted_identity = recorded.get(str(extracted.resolve())) if isinstance(recorded, dict) else None
        if (not isinstance(extracted_identity, dict)
                or extracted_identity.get("path") != str(extracted.resolve())
                or extracted_identity.get("parent") != str(assets.resolve())
                or (extracted_identity.get("device"), extracted_identity.get("inode"))
                != extracted_token.identity
                or (extracted_identity.get("parent_device"), extracted_identity.get("parent_inode"))
                != extracted_token.parent_identity):
            raise VerificationError("release_directory_identity_changed", "validate-release-directory")
        remember_directory(receipt, extracted_token)

        current = inspect_binaries(extracted, receipt)
        old_binaries = archived.get("binaries")
        if not isinstance(old_binaries, dict) or set(old_binaries) != set(BINARIES):
            raise VerificationError("archive_receipt_invalid", "validate-binary-identity")
        for name, path in current.items():
            previous = old_binaries[name]
            if (not isinstance(previous, dict) or previous.get("path") != str(path)
                    or previous.get("sha256") != receipt["binaries"][name]["sha256"]):
                raise VerificationError("release_binary_identity_changed", "validate-binary-identity")
            if "version" in previous:
                receipt["binaries"][name]["version"] = previous["version"]
        build = archived.get("delegate_build_identity")
        if not isinstance(build, dict) or build.get("version") != match.group(1):
            raise VerificationError("archive_receipt_invalid", "validate-build-identity")

        try:
            aggregate, aggregate_hash = validate_aggregate_receipt(
                aggregate_path,
                expected_tag=args.tag,
                expected_source_revision=args.source_revision,
            )
        except EvidenceError as error:
            code = error.args[0] if error.args else "aggregate_receipt_invalid"
            raise VerificationError(code, "validate-aggregate-receipt") from None
        if aggregate.get("platform") not in (None, info):
            raise VerificationError("aggregate_platform_mismatch", "validate-aggregate-receipt")
        aggregate_status = aggregate.get("status")
        aggregate_exit = aggregate.get("exit_code")
        if aggregate_status not in {"PASS", "BLOCKED", "FAIL"} or type(aggregate_exit) is not int:
            raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
        recorded_code = exit_code(getattr(args, "exit_code_file", None))
        if recorded_code is None or recorded_code != aggregate_exit:
            raise VerificationError("native_acceptance_exit_mismatch", "validate-aggregate-receipt")
        if ((aggregate_exit == 0 and args.acceptance_outcome != "success")
                or (aggregate_exit != 0 and args.acceptance_outcome == "success")):
            raise VerificationError("native_acceptance_outcome_mismatch", "validate-aggregate-receipt")
        if args.acceptance_outcome in {"cancelled", "skipped"}:
            raise VerificationError("native_acceptance_outcome_incomplete", "validate-aggregate-receipt")
        aggregate_cases = aggregate.get("cases")
        if not isinstance(aggregate_cases, list):
            raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
        expected_case_binaries = {
            "delegate_sha256": receipt["binaries"]["delegate"]["sha256"],
            "runner_sha256": receipt["binaries"]["delegate-run"]["sha256"],
            "pueue_sha256": receipt["binaries"]["pueue"]["sha256"],
            "pueued_sha256": receipt["binaries"]["pueued"]["sha256"],
        }
        for case in aggregate_cases:
            if (not isinstance(case, dict) or case.get("status") != "PASS"
                    or case.get("evidence_status") != "RESOLVED"):
                continue
            identities = case.get("binary_identities")
            hashes = identities.get("sha256") if isinstance(identities, dict) else None
            if (not isinstance(hashes, dict) or identities.get("state") != "complete"
                    or any(hashes.get(key) != digest
                           for key, digest in expected_case_binaries.items())):
                raise VerificationError(
                    "native_case_binary_identity_mismatch", "validate-binary-identity",
                )
        case_hashes = [
            {
                "case_id": case["case_id"], "provider": case["provider"],
                "permission": case["permission"], "scenario": case["scenario"],
                "status": case["status"], "exit_code": case["exit_code"],
                "evidence_status": case["evidence_status"],
                "receipt_path": case["receipt_path"], "receipt_sha256": case["receipt_sha256"],
            }
            for case in aggregate_cases if isinstance(case, dict)
        ]
        if len(case_hashes) != len(aggregate_cases):
            raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
        receipt["aggregate_receipt_sha256"] = aggregate_hash
        receipt["release_identity"] = release_identity.copy()
        receipt["release_revision"] = release_identity["commit_sha"]
        receipt["harness_revision"] = args.source_revision
        receipt["native_acceptance"] = {
            "claims_scope": "provider-permission-scenario-case-results-only",
            "aggregate_status": aggregate_status,
            "aggregate_exit_code": aggregate_exit,
            "aggregate_receipt_path": str(aggregate_path.resolve()),
            "aggregate_receipt_sha256": aggregate_hash,
            "case_receipt_hashes": case_hashes,
            "blocked_scope": "authentication-or-prerequisite-only",
        }

        receipt["artifact"] = artifact.copy()
        receipt["delegate_build_identity"] = build.copy()
        receipt["default_path_observation"] = observation.copy()
        receipt["archive_receipt_sha256"] = archive_receipt_hash
        receipt["acceptance"] = {
            "command": ["./scripts/acceptance_native_all.sh"], "outcome": args.acceptance_outcome,
            "exit_code": recorded_code, "binary_directory": str(extracted.resolve()),
            "aggregate_status": aggregate_status, "case_results": case_hashes,
        }
        if aggregate_status == "FAIL":
            failure_code = (
                "native_acceptance_failed"
                if aggregate.get("case_results_verdict") == "FAIL"
                else "native_acceptance_evidence_unresolved"
            )
            raise VerificationError(failure_code, "native-acceptance")
        if aggregate_status == "BLOCKED":
            policy = aggregate.get("policy")
            require_pass = isinstance(policy, dict) and policy.get("require_at_least_one_pass") is True
            if require_pass:
                raise VerificationError("native_acceptance_pass_required", "native-acceptance-policy")
            receipt["verdict"] = "blocked"

    return finish_gate(receipt_path, receipt, owner, action, "remove", locations)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    verify_parser = commands.add_parser("verify")
    verify_parser.add_argument("--mode", choices=("archive-smoke", "native-acceptance"), required=True)
    verify_parser.add_argument("--assets-dir", required=True)
    verify_parser.add_argument("--extract-dir", required=True)
    verify_parser.add_argument("--receipt", required=True)
    verify_parser.add_argument("--tag", required=True)
    verify_parser.add_argument("--repo", required=True)
    verify_parser.add_argument("--source-revision", required=True)
    final_parser = commands.add_parser("finalize-native")
    final_parser.add_argument("--archive-receipt", required=True)
    final_parser.add_argument("--aggregate-receipt", required=True)
    final_parser.add_argument("--assets-dir", required=True)
    final_parser.add_argument("--extract-dir", required=True)
    final_parser.add_argument("--receipt", required=True)
    final_parser.add_argument("--exit-code-file")
    final_parser.add_argument("--acceptance-outcome", choices=("success", "failure", "cancelled", "skipped"), required=True)
    final_parser.add_argument("--tag", required=True)
    final_parser.add_argument("--repo")
    final_parser.add_argument("--source-revision", required=True)
    args = parser.parse_args(argv)
    return verify(args) if args.command == "verify" else finalize_native(args)


if __name__ == "__main__":
    raise SystemExit(main())
