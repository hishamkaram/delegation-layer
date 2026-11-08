"""Selected installed runners retain path authority without executable-content reads."""

from __future__ import annotations

import unittest
import sys
from pathlib import Path


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.tasks.operations import NativeTaskOps
from scripts.acceptance.testing.invalid_calls import invoke_invalid
from scripts.acceptance.supervisor.runner_layout import BUNDLE_RUNNER
from scripts.acceptance.supervisor.runner_layout import RunnerPlacement

from scripts.acceptance.inspections.tests.fixtures import ROOT
from scripts.acceptance.inspections.tests.fixtures import TASK
from scripts.acceptance.inspections.tests.fixtures import InspectionFixture
from scripts.acceptance.inspections.tests.fixtures import FixtureProcesses
from scripts.acceptance.inspections.tests.fixtures import forbid_installed_content


def managed_ledger(fixture: InspectionFixture, placement: RunnerPlacement = BUNDLE_RUNNER,
                   state=None) -> NativeTaskOps:
    """Return a managed task ledger over the fixture's selected installation."""
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


def managed_runtime_inspection(check: unittest.TestCase) -> InspectionFixture:
    """Seed managed admission records without running a provider or supervisor."""
    fixture = InspectionFixture(check)
    fixture.ops = managed_ledger(fixture)
    fixture.inspection_records(managed_exit_proof=True)
    ordinary = fixture.durable_ordinary_admission(runner_ownership="managed")
    fixture.managed_ordinary_lifetime(ordinary)
    return fixture


def validate_all(ops: NativeTaskOps, fixture: InspectionFixture) -> None:
    row = fixture.ordinary_row()
    ops.validate_ordinary_queue_row(row, f"delegate:{ROOT}:{TASK}", TASK, 7, "default")
    journal = ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)
    ops._validate_inspection_queue_row(fixture.inspection_row(), journal, allow_failure=False)


class RunnerPlacementTests(unittest.TestCase):
    def test_selected_bundle_is_used_in_all_validators_without_content_reads(self):
        with forbid_installed_content():
            fixture = managed_runtime_inspection(self)
            self.assertEqual(fixture.ops._queued_runner(TASK), fixture.runner)
            validate_all(fixture.ops, fixture)
        self.assertEqual(list(fixture.state.rglob("delegate-run*")), [])

    def test_another_installed_path_cannot_replace_the_selected_runner(self):
        with forbid_installed_content():
            fixture = managed_runtime_inspection(self)
            other = fixture.base / "unselected-runner"
            other.write_bytes(b"runner")  # Independently authored fixture source, never copied.
            other.chmod(0o700)
            ops = managed_ledger(fixture)
            ops.runner = other
            ops.inspection_bindings = dict(fixture.ops.inspection_bindings)
            ops.inspection_attempts = set(fixture.ops.inspection_attempts)
            row = fixture.ordinary_row()
            with self.assertRaisesRegex(AcceptanceFailure, "managed runner executable changed"):
                ops.validate_ordinary_queue_row(row, f"delegate:{ROOT}:{TASK}", TASK, 7, "default")
            with self.assertRaisesRegex(AcceptanceFailure, "worker mismatch"):
                ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)
            journal = fixture.ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)
            with self.assertRaisesRegex(AcceptanceFailure, "argv mismatch"):
                ops._validate_inspection_queue_row(fixture.inspection_row(), journal, allow_failure=False)

    def test_in_place_source_replacement_does_not_rewrite_inspection_history(self):
        with forbid_installed_content():
            fixture = managed_runtime_inspection(self)
            request = fixture.state / "inspections" / TASK / "request.json"
            original = request.read_bytes()
            fixture.runner.write_bytes(b"new independently authored runner source")
            validate_all(fixture.ops, fixture)
            self.assertEqual(request.read_bytes(), original)

    def test_missing_or_unexecutable_worker_is_unavailable(self):
        for absent in (False, True):
            with self.subTest(absent=absent), forbid_installed_content():
                fixture = managed_runtime_inspection(self)
                if absent:
                    fixture.runner.unlink()
                else:
                    fixture.runner.chmod(0o600)
                with self.assertRaisesRegex(AcceptanceFailure, "worker executable unavailable"):
                    fixture.ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)

    def test_supervisor_metadata_and_config_data_still_require_validation(self):
        for change in ("missing-client", "config-data"):
            with self.subTest(change=change), forbid_installed_content():
                fixture = managed_runtime_inspection(self)
                if change == "missing-client":
                    fixture.pueue.unlink()
                    message = "supervisor executable unavailable"
                else:
                    fixture.config.write_bytes(b"changed supervisor configuration")
                    message = "supervisor binding mismatch"
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    fixture.ops.validate_inspection_journal(TASK, fixture.state / "inspections" / TASK)

    def test_selected_installation_is_independent_of_the_state_root(self):
        with forbid_installed_content():
            fixture = managed_runtime_inspection(self)
            models_state = fixture.base / "models-state"
            models_state.mkdir()
            model_ops = managed_ledger(fixture, state=models_state)
            self.assertEqual(model_ops._queued_runner(TASK), fixture.runner)
            self.assertEqual(list(models_state.iterdir()), [])

    def test_retired_state_install_placement_is_refused(self):
        fixture = InspectionFixture(self)
        arguments = (FixtureProcesses(), fixture.base / "delegate", fixture.runner, fixture.pueue,
                     fixture.state_parent, fixture.state, fixture.output)
        self.assertEqual(NativeTaskOps(*arguments).runner_placement, BUNDLE_RUNNER)
        for managed in (False, True):
            with self.subTest(managed=managed), self.assertRaisesRegex(AcceptanceFailure, "runner placement is invalid"):
                invoke_invalid(NativeTaskOps, *arguments, managed_runner=managed, runner_placement="state-installed")
        with self.assertRaisesRegex(AcceptanceFailure, "runner placement is invalid"):
            invoke_invalid(NativeTaskOps, *arguments, managed_runner=True, runner_placement="existing")


if __name__ == "__main__":
    unittest.main()
