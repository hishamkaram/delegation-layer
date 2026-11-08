"""Success evidence is durably committed before cleanup and published only after it."""

import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.native import cli as entry
from scripts.acceptance.native.tests.support import adjacent_runner_bundle
from scripts.acceptance.native.tests.support import lifetime_closure_source
from scripts.acceptance.evidence.bounded_files import BoundedReadError
from scripts.acceptance.evidence.bounded_files import ReadFailure
from scripts.acceptance.native import workflow as gate
from scripts.acceptance.native import lifecycle as lifecycle_impl


FRESH, RESUME = "1" * 32, "2" * 32
CANDIDATE = "success-candidate.json"


class RecordingAcceptance(gate.NativeAcceptance):
    """The real native owner, recorded so a test can inspect and re-enter it."""

    instances: list["RecordingAcceptance"] = []

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        RecordingAcceptance.instances.append(self)


class SuccessCommitTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-success-commit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.delegate, self.runner = adjacent_runner_bundle(self.root)
        self.launches = 0
        self.after_scenario = None
        self.model_files: dict[Path, bytes] = {}
        RecordingAcceptance.instances = []

    def scenario(self, owner) -> int:
        """Finite provider-scenario stub: one completed run with its full facts."""
        self.launches += 1
        tasks = {"fresh": FRESH, "resume": RESUME}
        owner.success_receipt = {
            "gate": "acceptance-native", "status": "passed", "provider": owner.provider,
            "mode": owner.mode, "scenario": owner.scenario, "requested_model": None,
            "requested_effort": None, "tasks": tasks,
            "provider_sessions": {"fresh": "session-1", "resume": "session-1"},
            "provider_runtime_identities": {FRESH: {"version": "1.0"}, RESUME: {"version": "1.0"}},
            "replay_launches": 0, "credentials_in_receipt": False,
            "natural_daemon_shutdown": True, "lifetime_closure": lifetime_closure_source(tasks),
        }
        # Real model-discovery inspection, request and capture bytes under its owned root.
        for relative, data in (("tasks/" + FRESH + "/inspection.json", b'{"class": "eligible"}\n'),
                               ("tasks/" + FRESH + "/request.json", b'{"brief": "model discovery"}\n'),
                               ("tasks/" + FRESH + "/raw/stdout", b"captured model output\n")):
            path = owner.models_state / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            self.model_files[path] = data
        owner.closed = True
        if self.after_scenario is not None:
            self.after_scenario()
        return 0

    def main(self, *patches) -> tuple[int, RecordingAcceptance]:
        home = self.root / "home"
        with patch.object(gate.Path, "home", return_value=home), \
                patch.object(entry, "choose_output", return_value=self.output), \
                patch.object(entry, "NativeAcceptance", RecordingAcceptance), \
                patch.object(gate, "reject_tmp"), \
                patch.object(gate, "require_discovery"), \
                patch.object(gate.acceptance_scenarios, "run", side_effect=self.scenario):
            for active in patches:
                active.start()
            try:
                code = entry.main([
                    "--provider", "codex:exec", "--permission", "read-only",
                    "--output", str(self.output), "--delegate", str(self.delegate),
                    "--runner", str(self.runner), "--pueue", "/bin/sh", "--pueued", "/bin/sh",
                    "--provider-executable", "/bin/sh"])
            finally:
                for active in reversed(patches):
                    active.stop()
        self.assertEqual(self.launches, 1)
        return code, RecordingAcceptance.instances[-1]

    def assert_everything_retained(self, run: RecordingAcceptance) -> None:
        for path in (run.base, run.state, run.workspace, run.models_state, self.output / "processes"):
            self.assertTrue(path.is_dir(), path)
        for owned in run.owned_directories:
            self.assertTrue(owned.directory.is_dir(), owned.label)
        self.assertEqual(len(self.model_files), 3)
        for path, data in self.model_files.items():
            self.assertEqual(path.read_bytes(), data, path)
        self.assertFalse((self.output / "success.json").exists())

    def test_uncommitted_candidate_fails_and_retains_all_sources_across_reentry(self):
        real_write_json = lifecycle_impl.write_json

        def fsync_failed_after_rename(path, value, replace=False):
            real_write_json(path, value, replace=replace)
            raise OSError("parent directory fsync failed")

        def conflict():
            (self.output / CANDIDATE).write_text("{}\n")

        def symlinked():
            (self.output / CANDIDATE).symlink_to(self.root / "elsewhere.json")

        faults = {
            "write": (patch.object(lifecycle_impl, "write_json", side_effect=OSError("disk full")), None),
            "fsync-after-rename": (patch.object(lifecycle_impl, "write_json",
                                                side_effect=fsync_failed_after_rename), None),
            "readback": (patch.object(lifecycle_impl, "read_bounded_regular_file",
                                      side_effect=BoundedReadError(ReadFailure.CHANGED)), None),
            "conflict": (None, conflict),
            "symlink": (None, symlinked),
        }
        for label, (fault, arrange) in faults.items():
            with self.subTest(fault=label):
                self.setUp()
                self.after_scenario = arrange
                code, run = self.main(*([fault] if fault is not None else []))
                self.assertEqual(code, 1)
                self.assert_everything_retained(run)
                cleanup = json.loads((self.output / "cleanup.json").read_text())
                self.assertIn("acceptance base", cleanup["retained"])
                self.assertEqual(cleanup["raw_process_captures"], "retained")
                self.assertFalse(run.success_candidate is not None and run.success_candidate.confirmed)
                # Even with diagnostics written, re-entry keeps every source and raw capture.
                with patch.object(gate.NativeAcceptance, "_write_failure_diagnostics", return_value=True):
                    self.assertFalse(run.finalize(False))
                    self.assertFalse(run.finalize(True))
                self.assert_everything_retained(run)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "candidate was not committed"):
                    run.publish_success()

    def test_publication_failure_after_cleanup_keeps_the_full_committed_candidate(self):
        code, run = self.main(patch.object(lifecycle_impl, "write_bytes", side_effect=OSError("disk full")))
        self.assertEqual(code, 1)
        self.assertFalse((self.output / "success.json").exists())
        data = (self.output / CANDIDATE).read_bytes()
        if run.success_candidate is None:
            self.fail("success cleanup must retain its committed candidate")
        self.assertEqual(hashlib.sha256(data).hexdigest(), run.success_candidate.sha256)
        self.assertEqual(json.loads(data), run.success_receipt)
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual((cleanup["status"], cleanup["completion"]), ("removed", "passed"))
        self.assertEqual(json.loads((self.output / "failure.json").read_text())["status"], "failed")
        self.assertFalse(run.base.exists())

    def test_cleanup_failure_preserves_the_candidate_without_a_pass(self):
        code, run = self.main(patch.object(lifecycle_impl, "finalize_acceptance_artifacts", return_value=False))
        self.assertEqual(code, 1)
        self.assertFalse((self.output / "success.json").exists())
        self.assertEqual(json.loads((self.output / CANDIDATE).read_text()), run.success_receipt)

    def test_published_bytes_are_the_confirmed_candidate_not_mutable_memory(self):
        real_finalize = lifecycle_impl.finalize

        def finalize_then_mutate(owner, success, *, blocked=False, shutdown_attempted=False):
            result = real_finalize(owner, success, blocked=blocked, shutdown_attempted=shutdown_attempted)
            owner.success_receipt["tasks"]["fresh"] = "mutated"
            owner.success_receipt["lifetime_closure"]["ordinary"]["tasks"].clear()
            return result

        code, run = self.main(patch.object(lifecycle_impl, "finalize", side_effect=finalize_then_mutate))
        self.assertEqual(code, 0)
        candidate = self.output / CANDIDATE
        data = candidate.read_bytes()
        self.assertEqual((self.output / "success.json").read_bytes(), data)
        published = json.loads(data)
        self.assertEqual(published["tasks"]["fresh"], FRESH)
        self.assertEqual(set(published["lifetime_closure"]["ordinary"]["tasks"]), {FRESH, RESUME})
        identity = os.stat(candidate)
        self.assertTrue(lifecycle_impl.commit_success_candidate(run))
        self.assertEqual((os.stat(candidate).st_ino, candidate.read_bytes()), (identity.st_ino, data))
        with self.assertRaises(FileExistsError):
            run.publish_success()
        self.assertEqual((self.output / "success.json").read_bytes(), data)

    def test_publisher_compares_the_callers_gate_not_the_candidates(self):
        bridge = "one-time-opencode-native-acp-proof"
        for label, gate_name, expected, published in (
                ("canonical-default", "acceptance-native", None, True),
                ("bridge-explicit", bridge, bridge, True),
                ("bridge-under-default", bridge, None, False),
                ("bridge-wrong-gate", bridge, "other-native-proof", False),
                ("canonical-under-bridge", "acceptance-native", bridge, False),
                ("empty-expectation", "acceptance-native", "", False)):
            with self.subTest(case=label):
                output = self.root / label
                output.mkdir()
                owner = object.__new__(gate.NativeAcceptance)
                owner.output, owner.success_candidate = output, None
                owner.success_receipt = {"gate": gate_name, "status": "passed"}
                self.assertTrue(lifecycle_impl.commit_success_candidate(owner))
                options = {} if expected is None else {"expected_gate": expected}
                if published:
                    owner.publish_success(**options)
                    self.assertEqual((output / "success.json").read_bytes(), (output / CANDIDATE).read_bytes())
                else:
                    with self.assertRaises(gate.AcceptanceFailure):
                        owner.publish_success(**options)
                    self.assertFalse((output / "success.json").exists())


if __name__ == "__main__":
    unittest.main()
