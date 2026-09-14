"""Small, provider-neutral assertions shared by native acceptance drivers.

The supervisor process lifecycle remains in ``acceptance_supervisor_common``.
This module only contains observations that are common to provider acceptance
gates, so a provider driver does not have to import another provider's
fixtures or silently copy its oracles.
"""

from __future__ import annotations

import os
from pathlib import Path
import time
from typing import Callable

from acceptance_supervisor_common import digest, read_json, sha


class AcceptanceFailure(RuntimeError):
    """A provider acceptance oracle could not be established."""


def require(condition: object, message: str) -> None:
    if not condition:
        raise AcceptanceFailure(message)


def verify_collected_outcome(collected: object, directory: Path, expected: str) -> tuple[dict[str, object], bytes]:
    """Bind a public collection response to the immutable outcome and bytes."""
    require(isinstance(collected, dict), "collection response is not an object")
    outcome = read_json(directory / "outcome.json")
    require(isinstance(outcome, dict), "sole outcome authority is not an object")
    require(collected.get("outcome") == outcome, "CLI and sole outcome authority disagree")
    require(outcome.get("verdict") == expected, "collected outcome mismatch")
    payload = outcome.get("payload")
    require(isinstance(payload, dict), "outcome payload descriptor is absent")
    basename = payload.get("basename")
    require(isinstance(basename, str) and basename, "outcome payload basename is absent")
    data = (directory / basename).read_bytes()
    require(len(data) == payload.get("length") and sha(data) == payload.get("sha256"),
            "payload digest mismatch")
    return outcome, data


def clean_absolute(value: object, label: str) -> Path:
    require(isinstance(value, str) and bool(value), f"{label} must be a nonempty path")
    path = Path(value)
    require(path.is_absolute() and path == Path(os.path.normpath(value)),
            f"{label} must be absolute and clean")
    return path


def reject_tmp(path: Path, label: str) -> None:
    resolved = path.resolve(strict=False)
    excluded = [Path(item).resolve(strict=False)
                for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    require(not any(resolved == root or root in resolved.parents for root in excluded),
            f"{label} must be outside provider temporary roots: {resolved}")


def path_is_within(path: Path, parent: Path) -> bool:
    child = path.resolve(strict=False)
    ancestor = parent.resolve(strict=False)
    return child == ancestor or ancestor in child.parents


def ensure_private_directory(path: Path, label: str, create: bool = False) -> Path:
    reject_tmp(path, label)
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=False)
    require(path.is_dir() and not path.is_symlink(), f"{label} is not a private directory: {path}")
    os.chmod(path, 0o700)
    require(path.stat().st_mode & 0o077 == 0, f"{label} is group/world accessible: {path}")
    return path.resolve()


def write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(path, 0o600)


def wait_runner_done(
    client: Callable[[str, list[str]], object],
    root_id: str,
    task_id: str,
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it finishes successfully."""
    label = "delegate:" + root_id + ":" + task_id
    deadline = monotonic_fn() + timeout
    while monotonic_fn() < deadline:
        result = client("runner-status", ["status", "--json"])
        value = result.json()
        require(isinstance(value, dict), "supervisor status response is not an object")
        rows = value.get("tasks", {})
        require(isinstance(rows, dict), "supervisor status has no tasks object")
        matching = [row for row in rows.values()
                    if isinstance(row, dict) and row.get("label") == label]
        require(len(matching) == 1, "submitted task has no unique supervisor row")
        row = matching[0]
        status = row.get("status")
        if isinstance(status, dict) and "Done" in status:
            done = status["Done"]
            require(isinstance(done, dict) and done.get("result") == "Success",
                    "supervisor reports a failed runner")
            return row
        sleep_fn(0.1)
    raise RuntimeError("runner completion was not observed within acceptance deadline")


def snapshot(directory: Path, ignored_prefixes: tuple[str, ...] = (".ack",)) -> dict[str, dict[str, object]]:
    """Return an immutable regular-file snapshot below one evidence directory."""
    directory = Path(directory)
    require(directory.is_dir() and not directory.is_symlink(),
            "evidence directory is absent: " + str(directory))
    result: dict[str, dict[str, object]] = {}
    for path in sorted(directory.rglob("*")):
        if path.is_symlink():
            raise RuntimeError("evidence contains symlink: " + str(path))
        if not path.is_file() or path.name.endswith(".staging") or any(
                path.name.startswith(prefix) for prefix in ignored_prefixes):
            continue
        data = path.read_bytes()
        result[str(path.relative_to(directory))] = {"bytes": len(data), "sha256": sha(data)}
    return result


def task_snapshot(root: Path, task_id: str) -> dict[str, str]:
    """Snapshot the immutable records used by the contributor acceptance gate."""
    directory = Path(root) / "tasks" / task_id
    outcome = read_json(directory / "outcome.json")
    seal = read_json(directory / "provider.exit")
    names = ["task.json", "meta.json", "provider.start", "provider.exit", "outcome.json",
             "provider.ref.json", outcome["payload"]["basename"],
             *[entry["path"] for entry in seal["raw_manifest"]]]
    return {name: digest(directory / name) for name in names
            if (directory / name).exists()}
