"""The prepared-fixture port used by the separately owned CLI facade wiring."""

from pathlib import Path
import sys
import unittest
from unittest.mock import MagicMock, patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.acceptance_tests.private_cli.modern_application_fixture import publish_pair
from scripts.delegate_acceptance.private_cli import contention_controls as controls
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class ModernFacadePortTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)
        root = self.fixture.root.directory
        brief = root / "brief.md"
        brief.write_text("finite brief\n")
        self.lifetime = FixtureLifetime(
            ownership=self.fixture.owner,
            base=root,
            base_token=self.fixture.root,
            env=self.fixture.processes.environment,
            private_config=self.fixture.supervisor / "pueue.yml",
            gate=root / "gate",
            old_install=self.fixture.baseline.delegate.parent,
            old_libexec=self.fixture.baseline.libexec,
            upgraded_libexec=self.fixture.candidate.libexec,
        )
        self.installed = FixtureInstallation(
            delegate_binary=self.fixture.baseline.delegate,
            runner_binary=self.fixture.baseline.runner,
            upgraded_delegate_binary=self.fixture.candidate.delegate,
            upgraded_runner_binary=self.fixture.candidate.runner,
            pueue_binary=Path("/usr/bin/true"),
            pueued_binary=Path("/usr/bin/true"),
            workspace=root,
            provider_bin=root,
            brief=brief,
            upgraded_install=self.fixture.candidate.delegate.parent,
            old_daemon_marker=root / "old-daemon",
            upgraded_daemon_marker=root / "new-daemon",
            state=self.fixture.state,
            waiter_directory=root,
            lease_nonce="nonce",
        )
        self.receipt = MagicMock(spec=AcceptanceReceipt)

    def test_prepared_old_candidate_inputs_use_one_owner_and_derive_finalizer_task_ids(self):
        evidence = {"full": {"nested": {"lease_records": [1, 2, 3]}}}
        observed = []

        def operation(tasks, baseline, candidate, fixture):
            self.assertIsInstance(tasks, NativeTaskOps)
            self.assertEqual(tasks.processes.environment, self.lifetime.env)
            self.assertEqual(baseline.delegate, self.fixture.baseline.delegate)
            self.assertEqual(candidate.delegate, self.fixture.candidate.delegate)
            self.assertEqual(fixture.controls.directory, self.lifetime.base / "modern-controls")
            observed.append(tasks)
            for index, task_id in enumerate(("b" * 32, "c" * 32)):
                tasks.dispatch_attempts.add(task_id)
                tasks.record_admission(
                    "task-" + str(index),
                    task_id,
                    {
                        "root_id": self.fixture.root_id,
                        "task_id": task_id,
                        "admission": "admitted",
                        "supervisor": {"matched": True, "numeric_task_id": index},
                    },
                )
            publish_pair(tasks.state, candidate.libexec)
            return evidence

        with (
            patch.object(controls, "run_modern_binary_contention", side_effect=operation),
            patch.object(controls, "shutdown_private_supervisor") as shutdown,
        ):
            result = controls.run_prepared_modern_contention(
                self.lifetime, self.installed, self.fixture.controls.directory, self.receipt
            )
        self.assertIs(result, evidence)
        self.assertEqual(self.lifetime.attempted_task_ids, ["b" * 32, "c" * 32])
        self.assertEqual(len(observed), 1)
        self.assertIs(shutdown.call_args.kwargs["capture_root"], self.fixture.root)
        self.assertEqual(shutdown.call_args.kwargs["state"], self.fixture.state)
        self.assertEqual(shutdown.call_args.kwargs["task_ids"], tuple(self.lifetime.attempted_task_ids))
        self.receipt.add_details.assert_called_once_with(modern_binary_contention=evidence)
        self.receipt.update_counts.assert_called_once_with(
            dispatch_completed_count=2,
            admitted_task_count=2,
            running_task_count=0,
            live_lease_count=0,
            collected_task_count=2,
        )

    def test_failed_prepared_operation_keeps_unanswered_attempt_and_reaps_registered_children(self):
        observed = []

        def fail(tasks, _baseline, _candidate, _fixture):
            tasks.dispatch_attempts.add("d" * 32)
            tasks.processes.start(
                "finite-control", [sys.executable, "-c", "import time; time.sleep(0.05)"], tasks.state_parent
            )
            observed.append(tasks)
            raise OSError("injected operation failure")

        with (
            patch.object(controls, "run_modern_binary_contention", side_effect=fail),
            patch.object(controls, "shutdown_private_supervisor") as shutdown,
        ):
            with self.assertRaisesRegex(OSError, "injected operation failure"):
                controls.run_prepared_modern_contention(
                    self.lifetime, self.installed, self.fixture.controls.directory, self.receipt
                )
        self.assertEqual(self.lifetime.attempted_task_ids, ["d" * 32])
        self.assertTrue(
            all(
                process.result is not None and process.result["natural_wait"] is True
                for process in observed[0].processes.entries
            )
        )
        shutdown.assert_not_called()
        self.receipt.stage.assert_not_called()
