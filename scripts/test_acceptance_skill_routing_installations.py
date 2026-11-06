"""Actorless checks for selected installations and retained source/evidence hashes."""
import contextlib
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import acceptance_skill_routing as gate
import skill_routing_delegate_fixture as fixture


class InstalledRoutingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.selected = self.root / "selected-delegate"
        self.selected.write_text("synthetic installed fixture; never execute\n", encoding="utf-8")
        self.selected.chmod(0o700)
        self.case = {"name": "review", "prompt": "Ask Codex for a review.", "external": True}
        home = patch.object(gate.Path, "home", return_value=self.root)
        home.start()
        self.addCleanup(home.stop)

    @contextlib.contextmanager
    def forbid_selected_content(self):
        original_open = Path.open
        original_bytes = Path.read_bytes
        original_copy = gate.shutil.copyfile

        def reject_selected(path):
            if Path(path).resolve() == self.selected:
                raise AssertionError("selected executable content accessed")

        def open_data(path, *args, **kwargs):
            reject_selected(path)
            return original_open(path, *args, **kwargs)

        def read_data(path):
            reject_selected(path)
            return original_bytes(path)

        def copy_source(source, destination, **kwargs):
            reject_selected(source)
            return original_copy(source, destination, **kwargs)

        with patch.object(Path, "open", open_data), patch.object(Path, "read_bytes", read_data), \
                patch.object(gate.shutil, "copyfile", copy_source):
            yield

    def test_capture_runs_selected_backend_in_place_without_content_reads(self):
        metadata = self.root / "capabilities.txt"
        metadata.write_text("synthetic native capabilities\n", encoding="utf-8")
        for mode in ("live", "routing"):
            case = dict(self.case, name=mode)
            run = self.root / (mode + "-1")
            owner = Mock(client="/mock/bin/pueue", daemon_path="/mock/bin/pueued")
            owner.processes.environment = {}
            owner.capture.return_value = metadata

            def observe_selection(*_):
                runtime = gate.read_json(run / "candidate-bin/runtime.json")
                self.assertEqual(runtime["delegate"], str(self.selected))
                if mode == "routing":
                    runtime = gate.read_json(run / "fixture/runtime.json")
                    self.assertEqual(runtime["delegate"], str(self.selected))
                    self.assertNotIn("delegate_sha256", runtime)
                return {"result": "Success"}

            owner.execute.side_effect = observe_selection
            with self.subTest(mode=mode), self.forbid_selected_content(), \
                    patch.object(gate, "AttemptSupervisor", return_value=owner), \
                    patch.object(gate.shutil, "which", side_effect=lambda name, **_: str(self.selected) if name == "delegate" else "/mock/bin/tool"), \
                    contextlib.redirect_stdout(io.StringIO()):
                receipt = gate.run_case(output=self.root, harness="codex", model=None,
                                        case=case, repetition=1, mode=mode,
                                        delegate_bin=self.selected if mode == "live" else None)
                self.assertEqual(receipt["status"], "unreviewed")
                self.assertEqual(receipt["delegate_executable"], str(self.selected))
                self.assertNotIn("delegate_sha256", receipt)
                self.assertEqual(receipt["delegate_catalog_sha256"], gate.digest(metadata))
                self.assertTrue(receipt["candidate"])
                self.assertEqual([call.args[1] for call in owner.capture.call_args_list][1:], [
                    [str(self.selected), "--version"], [str(self.selected), "providers", "--json"]])
                owner.close.assert_called_once()
                self.assertFalse(Path(receipt["state_root"]).exists())
                self.assertFalse((run / "candidate-bin").exists())
                self.assertTrue(self.selected.is_file())

    def test_summary_ignores_historical_executable_digest_and_keeps_evidence(self):
        run = self.root / "review-1"
        run.mkdir()
        transcript = run / "stdout.txt"
        transcript.write_text("synthetic tool-call evidence\n", encoding="utf-8")
        receipt = {
            "status": "unreviewed", "case": self.case, "harness": "codex", "repetition": 1,
            "mode": "live", "model_requested": None, "budget_seconds": 180,
            "candidate": gate.candidate_digests(), "fixture_scenario": None,
            "delegate_executable": str(self.selected), "delegate_sha256": "a" * 64,
            "stdout.txt_sha256": gate.digest(transcript),
        }
        gate.write_json(run / "receipt.json", receipt)
        gate.write_json(run / "judgment.json", {
            "status": "passed", "receipt_sha256": gate.digest(run / "receipt.json"),
            "evidence": "stdout.txt:1 records the synthetic tool call.",
        })
        original = (run / "receipt.json").read_bytes()
        with self.forbid_selected_content():
            result = gate.summarize(output=self.root, cases=[self.case], repetitions=1,
                                    harness="codex", delegate_bin=self.selected)
        self.assertEqual(result["status"], "passed")
        self.assertEqual((run / "receipt.json").read_bytes(), original)
        transcript.write_text("changed evidence\n", encoding="utf-8")
        with self.forbid_selected_content(), self.assertRaisesRegex(ValueError, "changed transcript"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1,
                           harness="codex", delegate_bin=self.selected)

    def test_synthetic_fixture_uses_availability_without_reading_selected_content(self):
        runtime = {"delegate": str(self.selected), "delegate_sha256": "a" * 64}
        with self.forbid_selected_content():
            self.assertTrue(fixture.available_delegate(runtime))
        self.selected.chmod(0o600)
        with self.forbid_selected_content():
            self.assertFalse(fixture.available_delegate(runtime))
        self.selected.unlink()
        self.assertFalse(fixture.available_delegate(runtime))
