"""Read-only native task-store observations bound to original directory ownership."""

from __future__ import annotations

from collections.abc import Callable, Mapping
import json
from pathlib import Path
import time

from scripts.acceptance.directories.operations import OwnedDirectory
from scripts.acceptance.evidence.json_decoder import unique_object
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require


MAX_TASK_OBSERVATION_BYTES = 1024 * 1024


TaskEntryRunner = Callable[[list[str], dict[str, str], float], str]


def observe_task_entry(state: Path, root: str, task_id: str, token: OwnedDirectory, *,
                       observer: Path, environment: Mapping[str, str], deadline: float,
                       run: TaskEntryRunner) -> dict[str, object]:
    """The caller fences creators; its process owner supplies a joined read-only command."""
    require(time.monotonic() < deadline, "bootstrap task observation deadline expired")
    require(_task_identity(root) and _task_identity(task_id),
            "bootstrap task observation lacks its exact root/task identity")
    require(token.directory == state, "bootstrap task observation names another owned root")
    token.assert_live()
    child = dict(environment)
    child.update(DELEGATE_NESTED_FIXTURE_MODE="task-entry",
                 DELEGATE_NESTED_FIXTURE_ROOT=str(state),
                 DELEGATE_NESTED_FIXTURE_TASK=task_id,
                 DELEGATE_NESTED_FIXTURE_ROOT_ID=root,
                 DELEGATE_NESTED_FIXTURE_ROOT_DEVICE=str(token.identity[0]),
                 DELEGATE_NESTED_FIXTURE_ROOT_INODE=str(token.identity[1]))
    output = run([str(observer), "-test.run=^TestNestedTaskAbsenceFixture$"], child,
                 min(deadline, time.monotonic() + 10))
    token.assert_live()
    require(time.monotonic() < deadline, "bootstrap task observation deadline expired")
    require(len(output.encode()) <= MAX_TASK_OBSERVATION_BYTES, "bootstrap task observer exceeded its output bound")
    prefix = "NESTED_TASK_OBSERVATION\t"
    lines = [line[len(prefix):] for line in output.splitlines() if line.startswith(prefix)]
    require(len(lines) == 1, "bootstrap task observer omitted its exact response")
    proof = json.loads(lines[0], object_pairs_hook=unique_object)
    require(isinstance(proof, dict)
            and set(proof) == {"root_id", "task_id", "entry_exists", "root_device", "root_inode"}
            and proof.get("root_id") == root and proof.get("task_id") == task_id
            and type(proof.get("entry_exists")) is bool
            and type(proof.get("root_device")) is int and type(proof.get("root_inode")) is int
            and (proof["root_device"], proof["root_inode"]) == token.identity,
            "bootstrap task observer changed identity or response shape")
    return proof
