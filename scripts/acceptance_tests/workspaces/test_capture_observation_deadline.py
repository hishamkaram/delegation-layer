"""Composed deadline tests for the capture observation: the real _wait_capture over the real _wait_workspace.

Only the private-control transport (_control) and the clock are faked. _wait_capture owns the one capture
observation deadline (WAIT_SECONDS); the nested status observer must not impose an earlier, independent cutoff
while the workspace is still pending, and terminal results must still fail at once.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import scripts.delegate_acceptance.workspaces.scenarios.operations as scenario_operations
from scripts.delegate_acceptance.workspaces.scenarios.context import WorkspaceContext
from scripts.delegate_acceptance.workspaces.scenarios.model import (
    ADMISSION_COMMAND_TIMEOUT_SECONDS, WAIT_SECONDS,
)

WORKSPACE_ID = "a" * 32
TASK_ID = "b" * 32
OTHER_TASK_ID = "c" * 32
MAX_WATCH_SECONDS = 5.0
Response = tuple[int, dict[str, object]]


class FakeClock:
    """The only time source of the observation: sleeping and slow commands advance it, nothing else does."""

    def __init__(self) -> None:
        self.now = 1000.0

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.now += seconds


@dataclass(frozen=True)
class Step:
    """One scripted private-control call: how long it takes and what it returns or raises."""

    elapsed: float
    response: Response | None = None
    error: BaseException | None = None


@dataclass(frozen=True)
class Call:
    started: float
    watch_seconds: float
    timeout: float
    expected: tuple[int, ...]


@dataclass
class ControlScript:
    clock: FakeClock
    steps: list[Step]
    calls: list[Call] = field(default_factory=list)

    def __call__(
        self, _context: WorkspaceContext, args: list[str], *, label: str, task_id: str | None = None,
        expected: tuple[int, ...] = (0,), timeout: float = 45, phase: str | None = None,
    ) -> Response:
        if not self.steps:
            raise AssertionError("unscripted control call at t=%.1f (%s)" % (self.clock.now, label))
        step = self.steps.pop(0)
        watch = args[args.index("--watch") + 1]
        self.calls.append(Call(self.clock.now, float(watch.removesuffix("s")), timeout, expected))
        self.clock.now += step.elapsed
        if step.error is not None:
            raise step.error
        if step.response is None:
            raise AssertionError("scripted step has neither a response nor an error")
        return step.response


def workspace_response(state: str, code: int = 0, **fields: object) -> Response:
    return code, {"workspace": {"id": WORKSPACE_ID, "state": state, **fields}}


def busy(path: str, capture: dict[str, object] | None = None) -> Response:
    if capture is None:
        return workspace_response("busy", path=path)
    return workspace_response("busy", path=path, capture=capture)


def undetermined() -> Response:
    """The exact public exit-3 shape of a slow observation (internal/app/workspace_response.go): watch again, nothing failed."""
    return workspace_response("undetermined", code=3, next_action="watch_workspace")


def capture_of(subject_id: str, status: str = "captured") -> dict[str, object]:
    return {"status": status, "subject": {"kind": "task", "id": subject_id}}


class CaptureObservationDeadlineTests(unittest.TestCase):
    def setUp(self) -> None:
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.state = Path(directory.name) / "state"
        self.context = mock.Mock(spec=WorkspaceContext)
        self.context.state = self.state
        self.task: dict[str, object] = {"workspace_id": WORKSPACE_ID, "task_id": TASK_ID}
        self.clock = FakeClock()

    def observe(self, steps: list[Step]) -> tuple[ControlScript, object]:
        """Run the real _wait_capture; return the script and the result, or the exception it raised."""
        script = ControlScript(self.clock, steps)
        with mock.patch.object(scenario_operations, "_control", script), \
                mock.patch.object(scenario_operations, "time", self.clock):
            try:
                return script, scenario_operations._wait_capture(self.context, self.task)
            except (RuntimeError, TimeoutError, subprocess.TimeoutExpired) as error:
                return script, error

    def path(self) -> str:
        return str(self.state.parent / "checkout")

    def test_a_pending_status_call_slower_than_fifteen_seconds_does_not_end_the_capture_observation(self) -> None:
        own = capture_of(TASK_ID)
        script, outcome = self.observe([
            Step(0.5, busy(self.path())),                      # busy workspace, no capture of this subject yet
            Step(16.0, undetermined()),                        # exit 3 watch_workspace: slower than the old 15 s slice
            Step(0.5, busy(self.path(), own)),                 # the subject's own capture arrives
        ])
        self.assertNotIsInstance(outcome, BaseException, "observation ended early: %r" % (outcome,))
        assert isinstance(outcome, tuple)
        response, capture = outcome
        self.assertIs(capture, own)
        self.assertEqual(len(script.calls), 3)
        self.assertGreater(script.calls[2].started - script.calls[0].started, 15.0)
        self.assertEqual(script.steps, [])

    def test_the_watch_window_and_transport_budget_are_unchanged(self) -> None:
        script, _outcome = self.observe([
            Step(0.5, busy(self.path())), Step(16.0, undetermined()), Step(0.5, busy(self.path(), capture_of(TASK_ID))),
        ])
        self.assertTrue(all(call.watch_seconds <= MAX_WATCH_SECONDS for call in script.calls))
        self.assertEqual({call.timeout for call in script.calls}, {ADMISSION_COMMAND_TIMEOUT_SECONDS})
        self.assertEqual({call.expected for call in script.calls}, {(0, 3)})

    def test_the_watch_shrinks_to_the_remaining_outer_budget_and_the_outer_deadline_still_ends_the_wait(self) -> None:
        start = self.clock.now
        script, outcome = self.observe([Step(WAIT_SECONDS - 2.0, undetermined()), Step(WAIT_SECONDS, undetermined())])
        self.assertIsInstance(outcome, TimeoutError)
        self.assertEqual([call.watch_seconds for call in script.calls], [MAX_WATCH_SECONDS, 2.0])
        self.assertTrue(all(call.started < start + WAIT_SECONDS for call in script.calls))  # no control after the deadline
        self.assertEqual(script.steps, [])

    def test_pending_forever_ends_at_the_one_outer_deadline_with_no_control_call_after_it(self) -> None:
        start = self.clock.now
        step = WAIT_SECONDS / 3
        script, outcome = self.observe([Step(step, busy(self.path())) for _ in range(3)])
        self.assertIsInstance(outcome, TimeoutError)
        self.assertIn("workspace capture observation expired", str(outcome))
        self.assertIn(TASK_ID, str(outcome))
        self.assertEqual(len(script.calls), 3)
        self.assertTrue(all(call.started < start + WAIT_SECONDS for call in script.calls))
        self.assertGreaterEqual(self.clock.now, start + WAIT_SECONDS)

    def test_another_subjects_capture_is_ignored_until_the_deadline(self) -> None:
        script, outcome = self.observe([Step(WAIT_SECONDS / 2, busy(self.path(), capture_of(OTHER_TASK_ID)))
                                        for _ in range(2)])
        self.assertIsInstance(outcome, TimeoutError)
        self.assertEqual(len(script.calls), 2)

    def test_terminal_workspace_states_fail_at_once_after_a_pending_observation(self) -> None:
        for state in ("failed", "unreadable"):
            with self.subTest(state=state):
                self.clock = FakeClock()
                script, outcome = self.observe([Step(16.0, undetermined()), Step(0.1, workspace_response(state))])
                self.assertIsInstance(outcome, RuntimeError)
                self.assertIn("workspace entered terminal failure", str(outcome))
                self.assertEqual(len(script.calls), 2)
                self.assertEqual(script.steps, [])

    def test_an_unavailable_capture_of_the_subject_fails_at_once(self) -> None:
        unavailable = capture_of(TASK_ID, status="unavailable")
        script, outcome = self.observe([Step(16.0, undetermined()), Step(0.1, busy(self.path(), unavailable))])
        self.assertIsInstance(outcome, RuntimeError)
        self.assertIn("workspace capture became unavailable", str(outcome))
        self.assertEqual(len(script.calls), 2)

    def test_a_transport_timeout_propagates_unchanged(self) -> None:
        expired = subprocess.TimeoutExpired(["delegate", "workspace", "status"], ADMISSION_COMMAND_TIMEOUT_SECONDS)
        script, outcome = self.observe([Step(16.0, undetermined()), Step(1.0, error=expired)])
        self.assertIs(outcome, expired)
        self.assertEqual(len(script.calls), 2)
        self.assertEqual(script.steps, [])


if __name__ == "__main__":
    unittest.main()
