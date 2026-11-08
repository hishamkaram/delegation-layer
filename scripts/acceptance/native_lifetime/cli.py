#!/usr/bin/env python3
"""Run and validate the native lifetime backend proof for this host."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import argparse
import hashlib
import json
import os
import platform
import re
import stat
import shutil
import uuid


from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.processes.operations import run_bounded_command

from scripts.acceptance.native_lifetime.contract import FINAL_CLEANUP_STATUSES as FINAL_CLEANUP_STATUSES
from scripts.acceptance.native_lifetime.contract import MAX_EVIDENCE_ITEMS as MAX_EVIDENCE_ITEMS
from scripts.acceptance.native_lifetime.contract import MAX_EVIDENCE_LENGTH as MAX_EVIDENCE_LENGTH
from scripts.acceptance.native_lifetime.contract import MAX_STAGE_LENGTH as MAX_STAGE_LENGTH
from scripts.acceptance.native_lifetime.contract import _bounded_evidence as _bounded_evidence
from scripts.acceptance.native_lifetime.contract import _bounded_text as _bounded_text
from scripts.acceptance.native_lifetime.contract import _digest as _digest
from scripts.acceptance.native_lifetime.contract import _has_exact_keys as _has_exact_keys
from scripts.acceptance.native_lifetime.contract import _mapping as _mapping
from scripts.acceptance.native_lifetime.contract import _primitive_digest as _primitive_digest
from scripts.acceptance.native_lifetime.contract import _text as _text
from scripts.acceptance.native_lifetime.contract import _unique_object as _unique_object
from scripts.acceptance.native_lifetime.contract import native_go_architecture as native_go_architecture
from scripts.acceptance.native_lifetime.contract import validate_receipt as validate_receipt


MAX_RECEIPT_BYTES = 16 * 1024
MAX_SOURCE_BYTES = 32 * 1024 * 1024
MAX_SOURCE_FILES = 256
RECEIPT_ENV = "DELEGATE_NATIVE_LIFETIME_RECEIPT"


def native_os() -> str:
    current = platform.system()
    if current == "Linux":
        return "linux"
    if current == "Darwin":
        return "darwin"
    raise RuntimeError("native lifetime proof supports Linux and macOS only")


def source_fingerprint(root: Path) -> tuple[str, str, str]:
    revision_result = run_bounded_command(
        ["git", "rev-parse", "--verify", "HEAD"], dict(os.environ),
        cwd=root, capture=True, max_output=64 * 1024,
    )
    if revision_result.exit_code != 0:
        raise RuntimeError("git could not resolve the native proof source revision")
    revision = revision_result.output.decode("utf-8").strip()
    if re.fullmatch(r"[0-9a-fA-F]{40}|[0-9a-fA-F]{64}", revision) is None:
        raise RuntimeError("git returned an invalid source revision")

    package_files = sorted((root / "internal/lifetime").glob("*.go"))
    if not package_files or len(package_files) > MAX_SOURCE_FILES:
        raise RuntimeError("lifetime package source file count is outside the proof bound")
    package_hash = hashlib.sha256()
    total_bytes = 0
    for path in package_files:
        content = path.read_bytes()
        total_bytes += len(content)
        if total_bytes > MAX_SOURCE_BYTES:
            raise RuntimeError("lifetime package source exceeds the proof bound")
        package_hash.update(path.relative_to(root).as_posix().encode("utf-8"))
        package_hash.update(b"\0")
        package_hash.update(content)

    gate_hash = hashlib.sha256()
    gate_paths = [
        root / "scripts/acceptance/native_lifetime/cli.py",
        root / "scripts/acceptance/native_lifetime/tests/test_proof.py",
        *sorted((root / "scripts/acceptance/native_lifetime").glob("*.py")),
    ]
    for path in gate_paths:
        content = path.read_bytes()
        gate_hash.update(path.relative_to(root).as_posix().encode("utf-8"))
        gate_hash.update(b"\0")
        gate_hash.update(content)
    return revision.lower(), package_hash.hexdigest(), gate_hash.hexdigest()


def read_receipt(path: Path) -> tuple[object | None, list[str]]:
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as receipt_file:
            info = os.fstat(receipt_file.fileno())
            if not stat.S_ISREG(info.st_mode):
                return None, ["native lifetime receipt is not a regular file"]
            if info.st_size == 0 or info.st_size > MAX_RECEIPT_BYTES:
                return None, ["native lifetime receipt is empty or exceeds its byte limit"]
            content = receipt_file.read(MAX_RECEIPT_BYTES + 1)
    except OSError:
        return None, ["native lifetime receipt is missing or unreadable"]
    if len(content) == 0 or len(content) > MAX_RECEIPT_BYTES:
        return None, ["native lifetime receipt is empty or exceeds its byte limit"]
    try:
        return json.loads(content, object_pairs_hook=_unique_object), []
    except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
        return None, ["native lifetime receipt is not valid UTF-8 JSON"]


def run_gate(output: Path, root: Path) -> int:
    output = Path(os.path.abspath(output))
    output.parent.mkdir(parents=True, exist_ok=True)
    for stale in (output, Path(str(output) + ".tmp")):
        if stale.exists() or stale.is_symlink():
            stale.unlink()

    host_os = native_os()
    env = os.environ.copy()
    go_host_result = run_bounded_command(
        ["go", "env", "GOHOSTOS", "GOHOSTARCH", "GOOS", "GOARCH"], env,
        cwd=root, capture=True, max_output=64 * 1024,
    )
    if go_host_result.exit_code != 0:
        raise RuntimeError("Go could not report the native proof host configuration")
    go_host = go_host_result.output.decode("utf-8").splitlines()
    if len(go_host) != 4 or go_host[0] != host_os or go_host[2] != go_host[0] or go_host[3] != go_host[1]:
        raise RuntimeError("Go is configured for a non-native host; refusing cross-compiled proof")
    if native_go_architecture(platform.machine()) != go_host[1]:
        raise RuntimeError("Go host architecture does not match the native runner")

    revision, package_hash, gate_hash = source_fingerprint(root)
    env["GOOS"] = go_host[0]
    env["GOARCH"] = go_host[1]
    env[RECEIPT_ENV] = str(output)
    env["DELEGATE_NATIVE_PROOF_REVISION"] = revision
    env["DELEGATE_NATIVE_PROOF_PACKAGE_SHA256"] = package_hash
    env["DELEGATE_NATIVE_PROOF_GATE_SHA256"] = gate_hash
    go_executable = shutil.which("go", path=env.get("PATH"))
    if go_executable is None:
        raise RuntimeError("Go executable is unavailable for the native proof")
    capture_directory = output.parent / ("proof-processes-" + uuid.uuid4().hex)
    ownership = DirectoryOwner()
    processes = Processes(capture_directory, environment=env, owner=ownership)
    try:
        process = processes.start(
            "native-lifetime", [str(Path(go_executable).absolute()), "test", "-count=1", "-run",
                                "^TestNativeBackendProof$", "./internal/lifetime"], cwd=root,
        )
        # Go owns its ten-minute test timeout. Observation expiry never signals a
        # native scope; retain its exact capture and handle for diagnosis instead.
        code = process.wait(timeout=660, expected=None)
        sys.stdout.write(process.output().decode("utf-8", errors="replace"))
        sys.stderr.write(process.stderr_path.read_text(encoding="utf-8", errors="replace"))
    finally:
        ownership.release()
        print(f"native lifetime proof process evidence: {capture_directory}", file=sys.stderr)
    receipt, receipt_errors = read_receipt(output)
    if receipt_errors:
        for error in receipt_errors:
            print(f"native lifetime proof: {error}", file=sys.stderr)
        return code or 1
    validation_errors = validate_receipt(receipt, host_os, go_host[1], (revision, package_hash, gate_hash))
    for error in validation_errors:
        print(f"native lifetime proof: {error}", file=sys.stderr)
    if code != 0 or validation_errors:
        return code or 1
    print(f"native lifetime proof receipt: {output}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(prog='acceptance-native-lifetime', description=__doc__)
    parser.add_argument(
        "--output",
        default=os.environ.get(RECEIPT_ENV, "bin/native-lifetime-proof/receipt.json"),
        help="absolute or repository-relative receipt path",
    )
    arguments = parser.parse_args()
    root = Path(__file__).resolve().parents[3]
    output = Path(arguments.output)
    if not output.is_absolute():
        output = root / output
    try:
        return run_gate(output, root)
    except (OSError, RuntimeError, UnicodeError, BoundedCommandError) as error:
        print(f"native lifetime proof: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
