"""The task ledger forwards a selected child environment to its process owner unchanged.

Run natively with ``python3 -m unittest scripts.acceptance.tasks.tests.test_task_environment -v``.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping, Sequence
from pathlib import Path
import sys
import tempfile
import unittest
from typing import Never, NotRequired, TypedDict, Unpack

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.tasks.operations import NativeTaskOps
from scripts.acceptance.tasks.contracts import TaskProcesses
from scripts.acceptance.testing.invalid_calls import invoke_invalid

PYTHON = str(Path(sys.executable).resolve())
TASK = "b" * 32
# Prints one JSON object that no admission accepts, so the ledger refuses it after the launch.
MARKER_CHILD = "import json, os; print(json.dumps({'marker': os.environ.get('PORT_SELECTED_MARKER')}))"


class RecordingProcesses:
    """Fake process owner that records exactly what the ledger passes at the launch boundary."""

    def __init__(self) -> None:
        self.launches: list[dict[str, object]] = []

    def run(self, name: str, argv: list[object], cwd: Path,
            expected: int | set[int] | None = 0, timeout: float = 15, stdin: bytes | None = None,
            *, environment: Mapping[str, str] | None = None) -> Never:
        self.launches.append({"expected": expected, "timeout": timeout, "stdin": stdin,
                              "environment": environment})
        raise RuntimeError("launch recorded")

    def drain(self, timeout: float = 70, exclude: Iterable[object] = ()) -> Sequence[Mapping[str, object]]:
        raise AssertionError("recording fixture must not drain processes")


class EnvironmentOptions(TypedDict):
    environment: NotRequired[Mapping[str, str] | None]


class TaskEnvironmentTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="task-environment-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        for name in ("state", "state-parent", "output"):
            (self.base / name).mkdir()

    def ops(self, processes: TaskProcesses) -> NativeTaskOps:
        ops = NativeTaskOps(processes, self.base / "delegate", self.base / "runner", self.base / "pueue",
                            self.base / "state-parent", self.base / "state", self.base / "output")
        ops.root_id = "a" * 32
        return ops

    def launch(self, ops: NativeTaskOps, command: str, **options: Unpack[EnvironmentOptions]) -> None:
        with self.assertRaisesRegex(RuntimeError, "launch recorded"):
            if command == "dispatch":
                ops.dispatch("fresh", TASK, ["delegate"], **options)
            else:
                ops.continue_task("resume", ["delegate"], **options)

    def test_dispatch_and_continue_forward_the_selected_environment_unchanged(self) -> None:
        selection = {"PORT_SELECTED_MARKER": "selected"}
        cases: tuple[tuple[str, EnvironmentOptions, Mapping[str, str] | None], ...] = (
            ("unset", {}, None), ("none", {"environment": None}, None),
            ("empty", {"environment": {}}, {}), ("mapping", {"environment": selection}, selection))
        for command in ("dispatch", "continue"):
            for label, options, expected in cases:
                with self.subTest(command=command, selection=label):
                    processes = RecordingProcesses()
                    ops = self.ops(processes)
                    self.launch(ops, command, **options)
                    self.assertEqual(len(processes.launches), 1)
                    self.assertEqual(processes.launches[0]["environment"], expected)
                    if expected is not None:
                        self.assertIs(processes.launches[0]["environment"], options["environment"])
                    self.assertEqual(processes.launches[0]["expected"], None)
                    self.assertEqual(processes.launches[0]["timeout"], 45)

    def test_dispatch_launches_the_real_process_owner_with_only_the_selected_environment(self) -> None:
        owner = DirectoryOwner()
        self.addCleanup(owner.rollback)
        processes = Processes(self.base / "captures", {"PORT_SELECTED_MARKER": "constructor"}, owner=owner)

        def settle() -> None:
            for process in processes.entries:
                process.process.wait(timeout=10)
                process.stdout.close()
                process.stderr.close()
        self.addCleanup(settle)
        ops = self.ops(processes)
        cases = (("selected", {"PORT_OTHER_MARKER": "x", "PORT_SELECTED_MARKER": "selected"}, "selected"),
                 ("emptied", {}, None), ("default", None, "constructor"))
        for index, (label, selection, expected) in enumerate(cases):
            task = chr(ord("c") + index) * 32
            with self.assertRaisesRegex(AcceptanceFailure, "was not admitted"):
                ops.dispatch(label, task, [PYTHON, "-c", MARKER_CHILD], environment=selection)
            self.assertEqual(processes.entries[index].json(), {"marker": expected})

    def test_malformed_selection_is_refused_by_the_process_owner_before_launch(self) -> None:
        owner = DirectoryOwner()
        self.addCleanup(owner.rollback)
        processes = Processes(self.base / "captures", {}, owner=owner)
        ops = self.ops(processes)
        with self.assertRaisesRegex(RuntimeError, "keys and values must be strings"):
            invoke_invalid(ops.dispatch, "fresh", TASK, [PYTHON, "-c", "pass"],
                           environment={"PORT_SELECTED_MARKER": 1})
        self.assertEqual((processes.entries, processes.pending(), ops.tasks), ([], [], {}))
        self.assertEqual(list(processes.directory.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
