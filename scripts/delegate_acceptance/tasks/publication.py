"""Observe immutable task publications and exact evidence snapshots."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.validation import require
from pathlib import Path


def verify_collected_outcome(collected: object, directory: Path, expected: str) -> tuple[dict[str, object], bytes]:
    """Bind a public collection response to the immutable outcome and bytes."""
    require(isinstance(collected, dict), "collection response is not an object")
    outcome = read_json(directory / "outcome.json")
    require(isinstance(outcome, dict), "sole outcome authority is not an object")
    require(collected.get("outcome") == outcome, "CLI and sole outcome authority disagree")
    require(outcome.get("verdict") == expected, "collected outcome mismatch")
    return outcome, read_outcome_payload(directory, outcome)


def read_outcome_payload(directory: Path, outcome: dict[str, object]) -> bytes:
    """Read only the protocol's regular task-local publication file."""
    names = {"committed": "result.txt", "rejected": "publish.reject"}
    expected = names.get(outcome.get("verdict"))
    require(expected is not None, "outcome verdict has no publication payload")
    payload = outcome.get("payload")
    require(isinstance(payload, dict), "outcome payload descriptor is absent")
    require(payload.get("basename") == expected, "outcome payload basename is invalid")
    length = payload.get("length")
    require(type(length) is int and length >= 0, "outcome payload length is invalid")
    path = directory / expected
    require(directory.is_dir() and not directory.is_symlink() and
            path.is_file() and not path.is_symlink(),
            "outcome payload is not a regular task-local file")
    data = path.read_bytes()
    require(len(data) == length and sha(data) == payload.get("sha256"),
            "payload digest mismatch")
    return data


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
