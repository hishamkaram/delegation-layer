"""Bounded, secret-free receipt encoding and finalization."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import platform
import sys
import time

from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.evidence.files import write_json
from .model import (
    MAX_COMPACT_DEPTH, MAX_COMPACT_ITEMS, MAX_COMPACT_STRING, MAX_RECEIPT_BYTES,
)



def _compact(value: object, depth: int = 0) -> object:
    """Preserve complete evidence and reject values outside explicit bounds."""
    return _compact_value(value, depth, set())


def _compact_value(value: object, depth: int, active: set[int]) -> object:
    require(depth <= MAX_COMPACT_DEPTH, "receipt nesting exceeds bound")
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, bytes):
        return {"bytes": len(value), "sha256": hashlib.sha256(value).hexdigest()}
    if isinstance(value, str):
        require(len(value) <= MAX_COMPACT_STRING, "receipt string exceeds bound")
        return value
    if isinstance(value, dict):
        require(len(value) <= MAX_COMPACT_ITEMS, "receipt object exceeds item bound")
        identity = id(value)
        require(identity not in active, "receipt contains a cyclic object")
        active.add(identity)
        try:
            return {
                str(key): _compact_value(item, depth + 1, active)
                for key, item in sorted(value.items(), key=lambda item: str(item[0]))
            }
        finally:
            active.remove(identity)
    if isinstance(value, (list, tuple)):
        require(len(value) <= MAX_COMPACT_ITEMS, "receipt array exceeds item bound")
        identity = id(value)
        require(identity not in active, "receipt contains a cyclic object")
        active.add(identity)
        try:
            return [_compact_value(item, depth + 1, active) for item in value]
        finally:
            active.remove(identity)
    if isinstance(value, (int, float, bool)) or value is None:
        return value
    result = str(value)
    require(len(result) <= MAX_COMPACT_STRING, "receipt value exceeds string bound")
    return result


def file_fingerprint(path: Path) -> dict[str, object]:
    candidate = Path(path)
    try:
        resolved = candidate.resolve(strict=True)
        info = resolved.stat()
        if not resolved.is_file():
            raise OSError("not a regular file")
        return {
            "name": resolved.name,
            "bytes": info.st_size,
            "sha256": hashlib.sha256(resolved.read_bytes()).hexdigest(),
        }
    except OSError:
        return {"name": candidate.name, "status": "unavailable"}


class ReceiptFinalizer:
    """Write one bounded receipt before any owned-directory cleanup."""

    def __init__(self, output: Path, argv: list[str], binaries: dict[str, Path]):
        self.output = Path(output)
        self.argv = list(argv)
        self.binaries = dict(binaries)
        self.started = time.time_ns()
        self.source_manifest: dict[str, object] = {"status": "not-collected"}
        self.details: dict[str, object] = {}
        self.write_count = 0
        self.tool_fingerprints = {
            name: file_fingerprint(path) for name, path in sorted(self.binaries.items())
        }

    def refresh_tool_fingerprints(self) -> None:
        self.tool_fingerprints = {
            name: file_fingerprint(path) for name, path in sorted(self.binaries.items())
        }

    def set_source_manifest(self, manifest: dict[str, object]) -> None:
        self.source_manifest = manifest

    def set_details(self, **details: object) -> None:
        self.details.update(details)

    def _payload(
        self,
        verdict: str,
        reason: str,
        cleanup: dict[str, object],
    ) -> dict[str, object]:
        platform_evidence = {
            "system": platform.system(),
            "kernel": platform.release(),
            "architecture": platform.machine(),
            "python": file_fingerprint(Path(sys.executable)),
        }
        payload = {
            "schema_version": 1,
            "claim_class": "native-fixture-only",
            "verdict": verdict,
            "reason": reason,
            "started_unix_ns": self.started,
            "finished_unix_ns": time.time_ns(),
            "invocation": self.argv,
            "platform": platform_evidence,
            "tools": self.tool_fingerprints,
            "source_manifest": self.source_manifest,
            "details": self.details,
            "cleanup": cleanup,
            "assurance": "native fixture lifecycle only; no live provider claim",
        }
        encoded = json.dumps(_compact(payload), sort_keys=True, separators=(",", ":")).encode()
        require(len(encoded) <= MAX_RECEIPT_BYTES, "compact receipt exceeds bound")
        return _compact(payload)

    def finalize(self, verdict: str, reason: str, cleanup: dict[str, object]) -> None:
        write_json(self.output, self._payload(verdict, reason, cleanup), replace=True)
        self.write_count += 1
