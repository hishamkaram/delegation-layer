"""A managed native acceptance refuses a mismatched runner pair before any process."""

import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

import acceptance_supervisor_common as supervisor_common
from scripts import acceptance_native as entry
from scripts.acceptance_tests.native.support import adjacent_runner_bundle
from scripts.delegate_acceptance.native import acceptance as gate


class ManagedRunnerPairTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-runner-pair-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.delegate, self.runner = adjacent_runner_bundle(self.root)
        self.elsewhere = self.root / "elsewhere"
        self.elsewhere.mkdir()
        self.foreign = self.elsewhere / "delegate-run"
        shutil.copyfile(self.runner, self.foreign)
        self.foreign.chmod(0o700)

    def test_canonical_delegate_sibling_is_the_selected_runner(self):
        gate.require_adjacent_runner(self.delegate, self.runner)
        linked = self.elsewhere / "delegate"
        linked.symlink_to(self.delegate)
        gate.require_adjacent_runner(linked, self.runner)

    def test_mismatched_symlinked_missing_or_unexecutable_sibling_blocks(self):
        cases = {"mismatch": lambda: (self.delegate, self.foreign)}

        def symlinked():
            self.runner.unlink()
            self.runner.symlink_to(self.foreign)
            return self.delegate, self.foreign

        def missing():
            self.runner.unlink()
            return self.delegate, self.foreign

        def unexecutable():
            self.runner.chmod(0o600)
            return self.delegate, self.runner

        cases.update(symlinked=symlinked, missing=missing, unexecutable=unexecutable)
        for label, arrange in cases.items():
            with self.subTest(label=label):
                self.setUp()
                delegate, runner = arrange()
                with self.assertRaises(gate.BlockedFailure):
                    gate.require_adjacent_runner(delegate, runner)

    def test_mismatched_pair_blocks_before_any_process_or_owned_root(self):
        home = self.root / "home"
        output = self.root / "evidence"
        output.mkdir()
        with patch.object(gate.Path, "home", return_value=home), \
                patch.object(entry, "choose_output", return_value=output), \
                patch.object(gate, "reject_tmp"), \
                patch.object(gate, "require_discovery"), \
                patch.object(gate, "Processes") as processes:
            code = entry.main(["--provider", "codex:exec", "--permission", "read-only",
                               "--output", str(output), "--delegate", str(self.delegate),
                               "--runner", str(self.foreign), "--pueue", "/bin/sh",
                               "--pueued", "/bin/sh", "--provider-executable", "/bin/sh"])
        self.assertEqual(code, 2)
        processes.assert_not_called()
        self.assertEqual(list((home / "delegation-layer-live").iterdir()), [])
        self.assertEqual({path.name for path in output.iterdir()}, {"failure.json"})
        failure = json.loads((output / "failure.json").read_text())
        self.assertEqual(failure["status"], "BLOCKED")
        self.assertIn("delegate-run", json.dumps(failure))

    def test_internal_custom_fixture_does_not_require_a_managed_pair(self):
        home = self.root / "home"
        output = self.root / "evidence"
        output.mkdir()
        args = entry.build_parser().parse_args([
            "--provider", "codex:exec", "--permission", "read-only", "--output", str(output),
            "--delegate", str(self.delegate), "--runner", str(self.foreign), "--pueue", "/bin/sh",
            "--pueued", "/bin/sh", "--provider-executable", "/bin/sh"])
        with patch.object(gate.Path, "home", return_value=home), \
                patch.object(gate, "reject_tmp"), \
                patch.object(gate, "require_discovery"):
            operation = gate.NativeAcceptance(args, managed_runner=False)
        try:
            self.assertFalse(operation.ops.managed_runner)
            self.assertIn("--runner", operation.task_command_prefix())
        finally:
            self.assertTrue(supervisor_common.rollback_owned_directories(operation.owned_directories))
        self.assertTrue(os.path.isdir(home / "delegation-layer-live"))


if __name__ == "__main__":
    unittest.main()
