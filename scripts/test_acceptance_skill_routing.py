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
        self.root = Path(self.temp.name).resolve()
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

    def test_missing_candidate_preserves_selection_and_summarizes_blocked(self):
        missing = self.root / "missing-candidate"
        gate.run_case(self.root, "codex", None, self.case, 1, delegate_bin=missing)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["delegate_executable"], str(missing))
        self.assertIsNone(receipt["delegate_sha256"])
        self.assertEqual(gate.summarize(self.root, [self.case], 1, "codex", delegate_bin=missing)["status"], "blocked")

    def test_renamed_candidate_gets_a_private_delegate_entry(self):
        binary = self.root / "renamed-candidate"
        binary.write_text("candidate executable")
        binary.chmod(0o700)
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.capture.side_effect = ValueError("stop after setup")
            gate.run_case(self.root, "codex", None, self.case, 1, delegate_bin=binary)
        private = self.root / "review-1/candidate-bin/delegate"
        self.assertEqual(private.resolve(), binary)
        env = owner.call_args.args[1]
        self.assertEqual(env["PATH"].split(gate.os.pathsep)[0], str(private.parent))
        self.assertIn(str(private.parent), Path(env["BASH_ENV"]).read_text())

    def test_hook_mode_is_explicit_and_uses_the_candidate_helper(self):
        workspace = self.root / "workspace with ' quote"
        claude = gate.routing_hook_arguments("claude", workspace)
        config = json.loads(claude[1])
        self.assertEqual(set(config["hooks"]), {"SessionStart", "SubagentStart"})
        for event, groups in config["hooks"].items():
            self.assertEqual(gate.shlex.split(groups[0]["hooks"][0]["command"]),
                             ["/bin/sh", str(workspace / ".claude/skills/agent-integration/scripts/delegate-routing-context.sh"), event])
        codex = gate.routing_hook_arguments("codex", workspace)
        self.assertIn("--dangerously-bypass-hook-trust", codex)
        self.assertTrue(any(value.startswith("hooks.SubagentStart=") for value in codex))
        with self.assertRaisesRegex(ValueError, "supported only"):
            gate.routing_hook_arguments("hermes", workspace)

    def test_hook_and_binary_receipts_cannot_masquerade_as_other_modes(self):
        self.judgment(self.receipt())
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(self.root, [self.case], 1, "codex", routing_context=True)
        binary = self.root / "candidate-delegate"
        binary.write_text("candidate bytes")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(self.root, [self.case], 1, "codex", delegate_bin=binary)

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

    def run_fixture(self, argv, result):
        (self.root / "runtime.json").write_text(json.dumps({"delegate": "/candidate/delegate", "path": "/private/path"}))
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.dict(gate.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(self.root)}), \
                patch.object(gate.sys, "argv", ["delegate", *argv]), \
                patch("subprocess.run", return_value=result) as run, \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as stopped:
                runpy.run_path(str(gate.REPO / "scripts/skill_routing_delegate_fixture.py"), run_name="__main__")
        self.assertEqual(run.call_args.args[0], ["/candidate/delegate", *argv])
        self.assertEqual(run.call_args.kwargs["env"]["PATH"], "/private/path")
        self.assertNotIn("timeout", run.call_args.kwargs)
        self.assertEqual(json.loads((self.root / "calls.jsonl").read_text().splitlines()[-1]), argv)
        return stopped.exception.code, stdout.getvalue(), stderr.getvalue()

    def test_fixture_preserves_actual_cli_responses(self):
        cases = [
            (["--root", "/state", "providers", "--json"], 0, '{"providers": []}\n', ""),
            (["version"], 0, "delegate candidate\n", ""),
            (["--help"], 0, "Usage: delegate\n", ""),
            (["task"], 2, "", "error: unknown command\n"),
            (["dispatch", "--provider", "ccr:print", "--json"], 2,
             '{"failure":{"next_action":"correct_request"}}\n', ""),
            (["dispatch", "--provider", "ccr:print", "--model", "local-review", "--json"], 2,
             '{"failure":{"next_action":"stop_and_report"}}\n', ""),
        ]
        for argv, code, stdout, stderr in cases:
            with self.subTest(argv=argv):
                result = Mock(returncode=code, stdout=stdout, stderr=stderr)
                self.assertEqual(self.run_fixture(argv, result), (code, stdout, stderr))

    def test_fixture_models_override_only_valid_unavailable_ccr_discovery(self):
        response = {"command": "models", "provider": "ccr:print", "reason_code": "discovery_unavailable"}
        code, stdout, stderr = self.run_fixture(
            ["models", "--provider", "ccr:print", "--json"],
            Mock(returncode=2, stdout=json.dumps(response), stderr=""))
        self.assertEqual(code, 0)
        self.assertEqual(stderr, "")
        self.assertEqual(json.loads(stdout)["models"][0]["id"], "local-review")
        for change, code in [({"provider": "codex:exec"}, 2),
                             ({"reason_code": "invalid_request"}, 2),
                             ({"command": "dispatch"}, 2), ({}, 1)]:
            payload = json.dumps(dict(response, **change))
            self.assertEqual(self.run_fixture(["models", "--json"],
                             Mock(returncode=code, stdout=payload, stderr="")), (code, payload, ""))

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

    def test_supervisor_config_uses_canonical_temporary_directory(self):
        actual = self.root / "actual"
        actual.mkdir()
        alias = self.root / "alias"
        alias.symlink_to(actual, target_is_directory=True)
        run = self.root / "run"
        run.mkdir()
        with patch.object(gate.tempfile, "mkdtemp", return_value=str(alias)), \
                patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner = gate.AttemptSupervisor(run, {})
        self.assertEqual(owner.config, actual.resolve() / "pueue.json")
        config = gate.read_json(owner.config)
        self.assertEqual(config["shared"]["unix_socket_path"],
                         str(actual.resolve() / "run/p.sock"))
        self.assertEqual(gate.read_json(run / "supervisor.json")["config"], str(owner.config))

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
        self.assertEqual(len({c["name"] for c in cases}), len(cases))
        regressions = [c for c in cases if c.get("regression")]
        self.assertEqual({c["name"] for c in regressions}, {
            "requested-model-effort", "ambiguous-model", "parallel-receipts",
            "subagent-external", "explicit-bypass"})
        self.assertEqual(sum(c["external"] for c in regressions), 4)
        cases = [c for c in cases if not c.get("regression")]
        ccr_cases = [c for c in cases if c["name"].startswith("ccr-")]
        self.assertEqual(len(ccr_cases), 6)
        self.assertTrue(any(not c["external"] for c in ccr_cases))
        self.assertTrue(any(c.get("model") == "local-review" for c in ccr_cases))
        cases = [c for c in cases if not c["name"].startswith("ccr-")]
        self.assertEqual(len(cases), 20)
        for split, count in (("development", 12), ("validation", 8)):
            rows = [c for c in cases if c["split"] == split]
            self.assertEqual(len(rows), count)
            self.assertEqual(sum(c["external"] for c in rows), count // 2)


if __name__ == "__main__":
    unittest.main()
