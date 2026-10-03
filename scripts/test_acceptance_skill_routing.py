"""Receipt integrity and isolation checks; these do not prove model selection."""
import json
import contextlib
import io
import runpy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, Mock

import acceptance_skill_routing as gate


class RoutingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        home = patch.object(gate.Path, "home", return_value=self.root)
        home.start()
        self.addCleanup(home.stop)
        self.case = {"name": "review", "prompt": "Ask Codex for a review.", "external": True}

    def receipt(self):
        run = self.root / "review-1"
        run.mkdir()
        (run / "stdout.txt").write_text('tool call evidence\n')
        gate.write_json(run / "receipt.json", {
            "status": "unreviewed", "case": self.case, "harness": "codex", "repetition": 1,
            "mode": "live", "model_requested": None, "budget_seconds": 180,
            "candidate": gate.candidate_digests(),
            "stdout.txt_sha256": gate.digest(run / "stdout.txt")})
        return run

    def judgment(self, run, status="passed"):
        gate.write_json(run / "judgment.json", {
            "status": status, "receipt_sha256": gate.digest(run / "receipt.json"),
            "evidence": "stdout.txt:1 has the observed tool call; full transcript reviewed."})

    def test_missing_and_unreviewed_attempts_never_pass(self):
        self.assertEqual(gate.summarize(self.root, [self.case], 1, "codex")["status"], "blocked")
        self.receipt()
        self.assertEqual(gate.summarize(self.root, [self.case], 1, "codex")["status"], "blocked")

    def test_required_repetitions_cannot_be_hidden(self):
        run = self.receipt()
        self.judgment(run)
        self.assertEqual(gate.summarize(self.root, [self.case], 1, "codex")["status"], "passed")
        self.assertEqual(gate.summarize(self.root, [self.case], 3, "codex")["status"], "blocked")

    def test_failure_is_not_neutralized_by_missing_attempts(self):
        self.judgment(self.receipt(), "failed")
        self.assertEqual(gate.summarize(self.root, [self.case], 3, "codex")["status"], "failed")

    def test_changed_transcript_and_stale_judgment_are_rejected(self):
        run = self.receipt()
        self.judgment(run)
        (run / "stdout.txt").write_text('changed')
        with self.assertRaisesRegex(ValueError, "changed transcript"):
            gate.summarize(self.root, [self.case], 1, "codex")
        receipt = gate.read_json(run / "receipt.json")
        receipt["new_field"] = True
        (run / "receipt.json").write_text(json.dumps(receipt))
        with self.assertRaisesRegex(ValueError, "stale judgment"):
            gate.summarize(self.root, [self.case], 1, "codex")

    def test_wrong_harness_or_case_cannot_reuse_a_receipt(self):
        self.judgment(self.receipt())
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(self.root, [self.case], 1, "claude")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(self.root, [dict(self.case, prompt="Different task")], 1, "codex")

    def test_fresh_workspace_contains_candidate_and_no_routing_instructions(self):
        workspace = self.root / "workspace"
        gate.prepare_workspace(workspace, "claude")
        self.assertEqual((workspace / ".claude/skills/agent-integration/SKILL.md").read_bytes(),
                         (gate.SKILL / "SKILL.md").read_bytes())
        self.assertFalse((workspace / "CLAUDE.md").exists())
        self.assertFalse((workspace / "AGENTS.md").exists())
        self.assertFalse((workspace / ".claude/settings.json").exists())

    def test_missing_executable_produces_blocked_receipt(self):
        with patch.object(gate.shutil, "which", return_value=None):
            gate.run_case(self.root, "codex", None, self.case, 1)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual(receipt["reason"], "required_executable_missing")

    def test_personal_claude_skill_cannot_masquerade_as_candidate(self):
        profile = self.root / "claude-profile"
        personal = profile / "skills/agent-integration"
        personal.mkdir(parents=True)
        (personal / "SKILL.md").write_text("old installed skill")
        with patch.dict(gate.os.environ, {"CLAUDE_CONFIG_DIR": str(profile)}), \
                patch.object(gate, "AttemptSupervisor") as owner:
            gate.run_case(self.root, "claude", None, self.case, 1)
        owner.assert_not_called()
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["reason"], "claude_candidate_shadowed_by_personal_skill")
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual((personal / "SKILL.md").read_text(), "old installed skill")

    def test_wrong_mode_model_or_candidate_is_rejected(self):
        self.judgment(self.receipt())
        for options in ({"mode": "routing"}, {"model": "different"}, {"budget": 99}):
            with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
                gate.summarize(self.root, [self.case], 1, "codex", **options)
        with patch.object(gate, "candidate_digests", return_value={"changed": "hash"}):
            with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
                gate.summarize(self.root, [self.case], 1, "codex")

    def test_failed_catalog_still_records_attempt(self):
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.capture.side_effect = ValueError("bad catalog")
            gate.run_case(self.root, "codex", None, self.case, 1, "routing")
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["status"], "blocked")
        owner.return_value.close.assert_called_once()

    def test_deadline_and_cleanup_failure_are_never_passes(self):
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.execute.side_effect = TimeoutError()
            owner.return_value.capture.return_value = self.root / "stdout"
            (self.root / "stdout").write_text("codex version")
            gate.run_case(self.root, "codex", None, self.case, 1)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["reason"], "supervised_deadline_exceeded")
        self.judgment(self.root / "review-1")
        with self.assertRaisesRegex(ValueError, "unsuccessful capture"):
            gate.summarize(self.root, [self.case], 1, "codex")

    def test_caller_queue_is_replaced_by_owned_queue(self):
        with patch.object(gate, "AttemptSupervisor") as owner, \
                patch.object(gate.shutil, "which", return_value="/mock/bin/tool"), \
                patch.dict(gate.os.environ, {"DELEGATE_PUEUE_CONFIG": "/user/queue.yml"}):
            instance = owner.return_value
            instance.config = self.root / "private-queue.json"
            instance.processes.environment = {}
            instance.capture.side_effect = ValueError("stop after setup")
            gate.run_case(self.root, "codex", None, self.case, 1)
        self.assertNotIn("DELEGATE_PUEUE_CONFIG", owner.call_args.args[1])
        self.assertEqual(instance.processes.environment["DELEGATE_PUEUE_CONFIG"], str(instance.config))
        instance.close.assert_called_once()

    def test_commands_do_not_preload_skill_or_disable_competitors(self):
        for harness in ("codex", "claude", "hermes"):
            argv, stdin = gate.command(harness, None, self.case["prompt"])
            self.assertNotIn("--skills", argv)
            self.assertNotIn("--bare", argv)
            self.assertNotIn("--safe-mode", argv)
            self.assertNotIn("--append-system-prompt", argv)
            self.assertTrue(stdin == self.case["prompt"] or self.case["prompt"] in argv)

    def test_copied_receipt_is_not_an_independent_repetition(self):
        run = self.receipt()
        self.judgment(run)
        gate.shutil.copytree(run, self.root / "review-2")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(self.root, [self.case], 2, "codex")

    def test_fixture_accepts_global_flags_and_version_alias(self):
        catalog = {"providers": [{"id": "codex:exec"}]}
        gate.write_json(self.root / "catalog.json", catalog)
        for argv, expected in ((["--root", "/state", "providers", "--json"], catalog),
                               (["providers", "--root=/state", "--json"], catalog),
                               (["version"], "delegate routing-fixture")):
            output = io.StringIO()
            with patch.dict(gate.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(self.root)}), \
                    patch.object(gate.sys, "argv", ["delegate", *argv]), contextlib.redirect_stdout(output):
                with self.assertRaises(SystemExit) as stopped:
                    runpy.run_path(str(gate.REPO / "scripts/skill_routing_delegate_fixture.py"), run_name="__main__")
                self.assertEqual(stopped.exception.code, 0)
            actual = output.getvalue().strip()
            self.assertEqual(json.loads(actual) if isinstance(expected, dict) else actual, expected)

    def test_supervised_deadline_stops_only_owned_queue(self):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root
        owner.daemon = Mock()
        owner.processes = Mock(entries=[])
        owner.task_id = "7"
        queued = Mock()
        queued.json.return_value = {"tasks": {"7": {"status": "Running"}}}
        ended = Mock()
        ended.json.return_value = {"tasks": {"7": {"status": {"Done": {"result": "Killed"}}}}}
        owner.control = Mock(side_effect=[Mock(), queued, Mock(), ended, Mock()])
        with patch.object(gate.time, "sleep"):
            owner.close()
        self.assertEqual(owner.control.call_args_list[2].args, ("kill", "--all"))
        self.assertEqual(owner.control.call_args_list[-1].args, ("shutdown",))
        owner.daemon.wait.assert_called_once_with(30, expected=0)
        self.assertTrue((self.root / "supervisor-closed.json").is_file())

    def test_cleanup_removes_queued_work_before_shutdown(self):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root
        owner.daemon = Mock()
        owner.processes = Mock(entries=[])
        queued = Mock()
        queued.json.return_value = {"tasks": {"8": {"status": {"Queued": {}}}}}
        empty = Mock()
        empty.json.return_value = {"tasks": {}}
        owner.control = Mock(side_effect=[Mock(), queued, Mock(), Mock(), empty, Mock()])
        with patch.object(gate.time, "sleep"):
            owner.close()
        self.assertEqual(owner.control.call_args_list[0].args, ("pause", "--all", "--wait"))
        self.assertEqual(owner.control.call_args_list[2].args, ("remove", "8"))
        self.assertEqual(owner.control.call_args_list[3].args, ("kill", "--all"))
        owner.daemon.wait.assert_called_once_with(30, expected=0)

    def test_unobserved_control_retains_ownership(self):
        owner = object.__new__(gate.AttemptSupervisor)
        pending = Mock()
        pending.poll.return_value = None
        pending.wait.side_effect = RuntimeError("observation expired")
        owner.processes = Mock(entries=[pending])
        owner.daemon = None
        with self.assertRaisesRegex(RuntimeError, "observation expired"):
            owner.close()
        pending.wait.assert_called_once_with(1)

    def test_suite_is_balanced_with_fixed_holdout(self):
        cases = gate.read_json(gate.CASES)["cases"]
        self.assertEqual(len(cases), 20)
        self.assertEqual(len({c["name"] for c in cases}), 20)
        for split, count in (("development", 12), ("validation", 8)):
            rows = [c for c in cases if c["split"] == split]
            self.assertEqual(len(rows), count)
            self.assertEqual(sum(c["external"] for c in rows), count // 2)


if __name__ == "__main__":
    unittest.main()
