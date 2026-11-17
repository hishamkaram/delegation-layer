"""Seed sanitized historical journals in an owned, exclusively held fixture root.

These journals are compatibility fixtures, not observations produced by the
released baseline. Production readers validate them during native acceptance.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

from scripts.acceptance.evidence.encoding import canonical_go_json
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.leases import private_fixture_transition
from scripts.acceptance.directories.operations import DirectoryOwner, OwnedDirectory


FIXTURE_PATH = Path("internal/inspection/internal/journal/testdata/upgrade-maintenance")


def record_digest(record: object) -> str:
    return hashlib.sha256(canonical_go_json(record)).hexdigest()


def write_private_record(path: Path, data: bytes) -> None:
    """Publish no group/other access, including when the caller's umask is 0."""
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as stream:
        stream.write(data)


def rebased_records(records: dict[str, dict], root_id: str) -> dict[str, dict]:
    """Change only root identity and its integrity links, preserving old shapes."""
    # JSON is the fixture boundary: copy rather than mutate the caller's source.
    result = json.loads(json.dumps(records))
    request = result["request.json"]
    request["root_id"] = root_id
    request["task"]["root_id"] = root_id
    request["task_sha256"] = record_digest(request["task"])
    request["group"] = "delegation-inspection-" + root_id
    request["label"] = request["group"] + "-" + request["task_id"]
    digest = record_digest(request)
    for record in result.values():
        if "request_sha256" in record:
            record["request_sha256"] = digest
    if "completion.json" in result:
        result["completion.json"]["result_sha256"] = record_digest(result["result.json"])
        result["worker-observation.json"]["completion_sha256"] = record_digest(result["completion.json"])
    return result


def seed(source_root: Path, state: Path, root_id: str, owner: DirectoryOwner | None = None,
         tokens: list[OwnedDirectory] | None = None,
         namespace_owner: DirectoryOwner | None = None) -> dict[str, str]:
    """The acceptance driver retains the bootstrap EX throughout fixture writes."""
    fixtures = source_root / FIXTURE_PATH
    manifest = json.loads((fixtures / "manifest.json").read_text())
    cases = manifest.get("cases")
    require(isinstance(cases, list) and len(cases) == 7, "historical fixture inventory changed")
    expected: dict[str, str] = {}
    with private_fixture_transition(state):
        inspections = state / "inspections"
        if not inspections.exists():
            if namespace_owner is None:
                raise RuntimeError("fresh inspection fixture requires its namespace directory owner")
            namespace_owner.create("fixture inspection namespace", inspections, state)
        require(inspections.is_dir() and not inspections.is_symlink()
                and inspections.resolve(strict=True) == inspections, "fixture inspection namespace is unsafe")
        for case in cases:
            directory = fixtures / case["name"]
            records = {path.name: json.loads(path.read_text()) for path in directory.iterdir()
                       if path.suffix == ".json"}
            records = rebased_records(records, root_id)
            task_id = records["request.json"]["task_id"]
            target = inspections / task_id
            target.mkdir(mode=0o700)
            if owner is not None:
                token = owner.adopt_created("synthetic historical inspection", target, target.parent)
                if tokens is not None:
                    tokens.append(token)
            for name, record in records.items():
                data = canonical_go_json(record)
                write_private_record(target / name, data)
                expected[str(Path(task_id) / name)] = hashlib.sha256(data).hexdigest()
            write_private_record(target / ".operation.lock", b"")
    return expected


def require_preserved(state: Path, expected: dict[str, str]) -> None:
    for relative, digest in expected.items():
        path = state / "inspections" / relative
        require(path.is_file() and not path.is_symlink()
                and hashlib.sha256(path.read_bytes()).hexdigest() == digest,
                "upgrade rewrote or removed seeded inspection evidence: " + relative)
