"""The one queued-runner resolver follows Go's supervisor runner placement."""

from __future__ import annotations

import unittest

from scripts.acceptance_provider_common import AcceptanceFailure, NativeTaskOps
from scripts.acceptance_supervisor_common import digest
from scripts.acceptance_tests.invalid_calls import invoke_invalid
from scripts.delegate_acceptance.supervisor.runner_layout import (
    BUNDLE_RUNNER,
    STATE_INSTALLED_RUNNER,
    RunnerPlacement,
    managed_state_runner,
)

from .fixtures import ROOT, TASK, InspectionFixture, FixtureProcesses


PLACEMENTS: tuple[RunnerPlacement, ...] = (BUNDLE_RUNNER, STATE_INSTALLED_RUNNER)


def managed_ledger(fixture: InspectionFixture, placement: RunnerPlacement,
                   state=None) -> NativeTaskOps:
    """Return a managed task ledger over the fixture's records and runner."""
    ops = NativeTaskOps(FixtureProcesses(), fixture.base / "delegate", fixture.runner,
                        fixture.pueue, fixture.state_parent, state or fixture.state, fixture.output,
                        managed_runner=True, runner_placement=placement)
    ops.bind_supervisor(fixture.config)
    ops.root_id = ROOT
    ops.tasks = {"fresh": TASK}
    ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
    ops.numbers = {"fresh": 7}
    ops.dispatch_attempts = {TASK}
    return ops


def managed_runtime_inspection(check: unittest.TestCase,
                               placement: RunnerPlacement) -> InspectionFixture:
    """Seed a CCR-shaped managed admission: runtime inspection, then ordinary task."""
    fixture = InspectionFixture(check)
    fixture.ops = managed_ledger(fixture, placement)
    fixture.inspection_records(managed_exit_proof=True)
    ordinary = fixture.durable_ordinary_admission(runner_ownership="managed")
    fixture.managed_ordinary_lifetime(ordinary)
    return fixture


def validate_all(ops: NativeTaskOps, fixture: InspectionFixture) -> None:
    """Run the ordinary binding, inspection journal, and inspection queue validators."""
    row = fixture.ordinary_row()
    ops.validate_ordinary_queue_row(row, f"delegate:{ROOT}:{TASK}", TASK, 7, "default")
    journal = ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)
    ops._validate_inspection_queue_row(fixture.inspection_row(), journal, allow_failure=False)


class RunnerPlacementTests(unittest.TestCase):
    def test_each_placement_binds_the_go_selected_runner_in_all_three_validators(self):
        expected = {
            BUNDLE_RUNNER: lambda fixture: fixture.runner,
            STATE_INSTALLED_RUNNER: lambda fixture: managed_state_runner(
                fixture.state, digest(fixture.runner)),
        }
        for placement in PLACEMENTS:
            with self.subTest(placement=placement):
                fixture = managed_runtime_inspection(self, placement)
                self.assertEqual(fixture.ops._queued_runner(TASK), expected[placement](fixture))
                validate_all(fixture.ops, fixture)

    def test_wrong_placement_is_refused_even_when_both_files_have_identical_bytes(self):
        for recorded, selected in ((BUNDLE_RUNNER, STATE_INSTALLED_RUNNER),
                                   (STATE_INSTALLED_RUNNER, BUNDLE_RUNNER)):
            with self.subTest(recorded=recorded, selected=selected):
                fixture = managed_runtime_inspection(self, recorded)
                fixture.managed_runner_copy()
                self.assertEqual(digest(fixture.managed_runner_copy()), digest(fixture.runner))
                ops = managed_ledger(fixture, selected)
                # Same expected source binding and attempts; only placement differs.
                ops.inspection_bindings = dict(fixture.ops.inspection_bindings)
                ops.inspection_attempts = set(fixture.ops.inspection_attempts)
                row = fixture.ordinary_row()
                with self.assertRaisesRegex(AcceptanceFailure,
                                            "ordinary task managed runner executable changed"):
                    ops.validate_ordinary_queue_row(row, f"delegate:{ROOT}:{TASK}", TASK, 7, "default")
                with self.assertRaisesRegex(AcceptanceFailure, "worker mismatch"):
                    ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)
                journal = fixture.ops.validate_inspection_journal(
                    TASK, fixture.state / "inspections" / TASK)
                with self.assertRaisesRegex(AcceptanceFailure, "argv mismatch"):
                    ops._validate_inspection_queue_row(
                        fixture.inspection_row(), journal, allow_failure=False)

    def test_changed_runner_bytes_are_refused_for_each_placement(self):
        for placement in PLACEMENTS:
            with self.subTest(placement=placement):
                fixture = managed_runtime_inspection(self, placement)
                fixture.placed_runner().write_bytes(b"replaced runner")
                with self.assertRaises(AcceptanceFailure):
                    fixture.ops.validate_inspection_journal(
                        TASK, fixture.state / "inspections" / TASK)
        fixture = managed_runtime_inspection(self, STATE_INSTALLED_RUNNER)
        fixture.managed_runner_copy().write_bytes(b"replaced runner")
        with self.assertRaisesRegex(AcceptanceFailure, "managed runner executable changed"):
            fixture.ops._queued_runner(TASK)

    def test_state_installed_runner_belongs_to_its_own_state_root(self):
        fixture = managed_runtime_inspection(self, STATE_INSTALLED_RUNNER)
        models_state = fixture.base / "models-state"
        models_state.mkdir()
        model_ops = managed_ledger(fixture, STATE_INSTALLED_RUNNER, state=models_state)
        with self.assertRaisesRegex(AcceptanceFailure, "managed runner executable changed"):
            model_ops._queued_runner(TASK)
        bundle_model_ops = managed_ledger(fixture, BUNDLE_RUNNER, state=models_state)
        self.assertEqual(bundle_model_ops._queued_runner(TASK), fixture.runner)

    def test_placement_is_a_typed_managed_only_choice(self):
        fixture = InspectionFixture(self)
        arguments = (FixtureProcesses(), fixture.base / "delegate", fixture.runner, fixture.pueue,
                     fixture.state_parent, fixture.state, fixture.output)
        self.assertEqual(NativeTaskOps(*arguments).runner_placement, BUNDLE_RUNNER)
        with self.assertRaisesRegex(AcceptanceFailure, "only a managed admission installs"):
            NativeTaskOps(*arguments, runner_placement=STATE_INSTALLED_RUNNER)
        with self.assertRaisesRegex(AcceptanceFailure, "runner placement is invalid"):
            invoke_invalid(NativeTaskOps, *arguments, managed_runner=True, runner_placement="existing")


if __name__ == "__main__":
    unittest.main()
