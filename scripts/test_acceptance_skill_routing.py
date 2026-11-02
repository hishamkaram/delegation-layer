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
import acceptance_provider_common as provider_common
from scripts.acceptance_tests.resources import DirectoryPinOwner


class RoutingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        home = patch.object(gate.Path, "home", return_value=self.root)
        home.start()
        self.addCleanup(home.stop)
        self.case = {"name": "review", "prompt": "Ask Codex for a review.", "external": True}
        self.candidate = self.root / "delegate-candidate"
        self.candidate.write_text("test candidate executable")
        self.candidate.chmod(0o700)

    def test_failed_workspace_preparation_rolls_back_owned_directories(self):
        with patch.object(gate, "prepare_workspace", side_effect=RuntimeError("fixture setup failed")):
            with self.assertRaisesRegex(RuntimeError, "fixture setup failed"):
                gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                              delegate_bin=self.candidate)
        self.assertFalse((self.root / "review-1" / "workspace").exists())

    def receipt(self, case=None, mode="live", fixture_scenario=None):
        case = self.case if case is None else case
        run = self.root / (case["name"] + "-1")
        run.mkdir()
        (run / "stdout.txt").write_text('tool call evidence\n')
        gate.write_json(run / "receipt.json", {
            "status": "unreviewed", "case": case, "harness": "codex", "repetition": 1,
            "mode": mode, "model_requested": None, "budget_seconds": 180,
            "candidate": gate.candidate_digests(),
            "fixture_scenario": fixture_scenario,
            "stdout.txt_sha256": gate.digest(run / "stdout.txt")})
        return run

    def judgment(self, run, status="passed"):
        gate.write_json(run / "judgment.json", {
            "status": status, "receipt_sha256": gate.digest(run / "receipt.json"),
            "evidence": "stdout.txt:1 has the observed tool call; full transcript reviewed."})

    def test_missing_and_unreviewed_attempts_never_pass(self):
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")["status"], "blocked")
        self.receipt()
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")["status"], "blocked")

    def test_required_repetitions_cannot_be_hidden(self):
        run = self.receipt()
        self.judgment(run)
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")["status"], "passed")
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=3, harness="codex")["status"], "blocked")

    def test_failure_is_not_neutralized_by_missing_attempts(self):
        self.judgment(self.receipt(), "failed")
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=3, harness="codex")["status"], "failed")

    def test_changed_transcript_and_stale_judgment_are_rejected(self):
        run = self.receipt()
        self.judgment(run)
        (run / "stdout.txt").write_text('changed')
        with self.assertRaisesRegex(ValueError, "changed transcript"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")
        receipt = gate.read_json(run / "receipt.json")
        receipt["new_field"] = True
        (run / "receipt.json").write_text(json.dumps(receipt))
        with self.assertRaisesRegex(ValueError, "stale judgment"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")

    def test_wrong_harness_or_case_cannot_reuse_a_receipt(self):
        self.judgment(self.receipt())
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="claude")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(output=self.root, cases=[dict(self.case, prompt="Different task")], repetitions=1,
                harness="codex")

    def test_fresh_workspace_contains_candidate_and_no_routing_instructions(self):
        workspace = self.root / "workspace"
        owner = gate.DirectoryOwner()
        owner.create("test workspace", workspace, self.root)
        gate.prepare_workspace(workspace, "claude")
        self.assertEqual((workspace / ".claude/skills/agent-integration/SKILL.md").read_bytes(),
                         (gate.SKILL / "SKILL.md").read_bytes())
        self.assertFalse((workspace / "CLAUDE.md").exists())
        self.assertFalse((workspace / "AGENTS.md").exists())
        self.assertFalse((workspace / ".claude/settings.json").exists())
        self.assertFalse((workspace / ".claude/skills/agent-integration/evals").exists())
        self.assertIn("evals/routing.json", gate.candidate_digests())
        owner.rollback()

    def test_missing_executable_produces_blocked_receipt(self):
        with patch.object(gate.shutil, "which", return_value=None):
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                          delegate_bin=self.candidate)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual(receipt["reason"], "required_executable_missing")
        self.assertFalse(Path(receipt["state_root"]).exists())
        self.assertFalse((self.root / "review-1/workspace").exists())
        cleanup = gate.read_json(self.root / "review-1/cleanup.json")
        self.assertEqual(cleanup["status"], "removed")

    def test_missing_supervisor_does_not_create_a_private_queue_directory(self):
        with patch.object(gate.shutil, "which", return_value=None), \
                patch.object(gate.tempfile, "mkdtemp") as make_directory:
            with self.assertRaisesRegex(FileNotFoundError, "private test supervisor unavailable"):
                gate.AttemptSupervisor(self.root / "run", {}, gate.DirectoryOwner())
        make_directory.assert_not_called()

    def test_closed_attempt_removes_private_supervisor_and_disposable_workspace(self):
        queue_parent = self.root / "queues"
        queue = queue_parent / "skill-queue-run"
        queue.mkdir(parents=True)
        with patch.object(gate, "AttemptSupervisor") as supervisor, \
                patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner = supervisor.return_value
            owner.base = queue
            owner.config = self.root / "queue.yml"
            owner.processes.environment = {}
            owner.capture.side_effect = ValueError("stop after setup")
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                          delegate_bin=self.candidate)

        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["status"], "blocked")
        self.assertFalse(Path(receipt["state_root"]).exists())
        self.assertFalse((self.root / "review-1/workspace").exists())
        self.assertTrue(queue.exists())
        cleanup = gate.read_json(self.root / "review-1/cleanup.json")
        self.assertEqual(cleanup["status"], "removed")

    def test_personal_claude_skill_cannot_masquerade_as_candidate(self):
        profile = self.root / "claude-profile"
        personal = profile / "skills/agent-integration"
        personal.mkdir(parents=True)
        (personal / "SKILL.md").write_text("old installed skill")
        with patch.dict(gate.os.environ, {"CLAUDE_CONFIG_DIR": str(profile)}), \
                patch.object(gate, "AttemptSupervisor") as owner:
            gate.run_case(output=self.root, harness="claude", model=None, case=self.case, repetition=1,
                          delegate_bin=self.candidate)
        owner.assert_not_called()
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["reason"], "claude_candidate_shadowed_by_personal_skill")
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual((personal / "SKILL.md").read_text(), "old installed skill")

    def test_wrong_mode_model_or_candidate_is_rejected(self):
        self.judgment(self.receipt())
        for options in ({"mode": "routing"}, {"model": "different"}, {"budget": 99}):
            with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
                gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex", **options)
        with patch.object(gate, "candidate_digests", return_value={"changed": "hash"}):
            with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
                gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")

    def test_failed_catalog_still_records_attempt(self):
        candidate = self.root / "candidate"
        candidate.write_text("test candidate")
        candidate.chmod(0o700)
        capture = self.root / "capture.txt"
        capture.write_text("fixture capture")
        def capture_prerequisite(name, argv):
            if name == "delegate-catalog":
                raise RuntimeError("supervised delegate-catalog prerequisite failed")
            return capture
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.processes.environment = {}
            owner.return_value.client = "/mock/bin/pueue"
            owner.return_value.daemon_path = "/mock/bin/pueued"
            owner.return_value.capture.side_effect = capture_prerequisite
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case,
                          repetition=1, mode="routing", delegate_bin=candidate)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["status"], "blocked")
        self.assertEqual(owner.return_value.capture.call_args.args[0], "delegate-catalog")
        owner.return_value.execute.assert_not_called()
        owner.return_value.close.assert_called_once()

    def test_live_and_routing_reach_scenario_without_provider_version(self):
        capture = self.root / "capture.txt"
        capture.write_text("fixture capture")
        candidate = self.root / "candidate"
        candidate.write_text("test candidate")
        candidate.chmod(0o700)
        for repetition, mode in enumerate(("live", "routing"), 1):
            with self.subTest(mode=mode), patch.object(gate, "AttemptSupervisor") as supervisor, \
                    patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
                owner = supervisor.return_value
                owner.client = "/mock/bin/pueue"
                owner.daemon_path = "/mock/bin/pueued"
                owner.processes.environment = {}
                owner.capture.return_value = capture
                owner.execute.return_value = {"result": "Success"}
                gate.run_case(output=self.root, harness="codex", model=None, case=self.case,
                              repetition=repetition, mode=mode, delegate_bin=candidate)
                owner.execute.assert_called_once()
                self.assertEqual([call.args[0] for call in owner.capture.call_args_list],
                                 ["git-init", "delegate-version", "delegate-catalog"])
                receipt = gate.read_json(self.root / f"review-{repetition}/receipt.json")
                self.assertNotIn("harness_version", receipt)
                self.assertEqual(receipt["status"], "unreviewed")

    def test_legacy_optional_harness_version_does_not_change_receipt_reading(self):
        run = self.receipt()
        receipt = gate.read_json(run / "receipt.json")
        receipt["harness_version"] = "historical optional release"
        (run / "receipt.json").write_text(json.dumps(receipt))
        self.judgment(run)
        original = (run / "receipt.json").read_bytes()
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=1,
                                        harness="codex")["status"], "passed")
        self.assertEqual((run / "receipt.json").read_bytes(), original)

    def test_deadline_and_cleanup_failure_are_never_passes(self):
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.execute.side_effect = TimeoutError()
            owner.return_value.capture.return_value = self.root / "stdout"
            (self.root / "stdout").write_text("codex version")
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                          delegate_bin=self.candidate)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["reason"], "supervised_deadline_exceeded")
        self.judgment(self.root / "review-1")
        with self.assertRaisesRegex(ValueError, "unsuccessful capture"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex")

    def test_caller_queue_is_replaced_by_owned_queue(self):
        with patch.object(gate, "AttemptSupervisor") as owner, \
                patch.object(gate.shutil, "which", return_value="/mock/bin/tool"), \
                patch.dict(gate.os.environ, {"DELEGATE_PUEUE_CONFIG": "/user/queue.yml"}):
            instance = owner.return_value
            instance.config = self.root / "private-queue.json"
            instance.processes.environment = {}
            instance.capture.side_effect = ValueError("stop after setup")
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                          delegate_bin=self.candidate)
        self.assertNotIn("DELEGATE_PUEUE_CONFIG", owner.call_args.args[1])
        self.assertEqual(instance.processes.environment["DELEGATE_PUEUE_CONFIG"], str(instance.config))
        instance.close.assert_called_once()

    def test_missing_candidate_preserves_selection_and_summarizes_blocked(self):
        missing = self.root / "missing-candidate"
        gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1, delegate_bin=missing)
        receipt = gate.read_json(self.root / "review-1/receipt.json")
        self.assertEqual(receipt["delegate_executable"], str(missing))
        self.assertIsNone(receipt["delegate_sha256"])
        self.assertEqual(gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex",
            delegate_bin=missing)["status"], "blocked")

    def test_renamed_candidate_gets_a_private_delegate_entry(self):
        binary = self.root / "renamed-candidate"
        binary.write_text("candidate executable")
        binary.chmod(0o700)
        with patch.object(gate, "AttemptSupervisor") as owner, patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner.return_value.capture.side_effect = ValueError("stop after setup")
            gate.run_case(output=self.root, harness="codex", model=None, case=self.case, repetition=1,
                delegate_bin=binary)
        private = self.root / "review-1/candidate-bin/delegate"
        env = owner.call_args.args[1]
        self.assertEqual(env["PATH"].split(gate.os.pathsep)[0], str(private.parent))
        self.assertIn(str(private.parent), Path(env["BASH_ENV"]).read_text())
        self.assertFalse(private.exists())

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
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex", routing_context=True)
        binary = self.root / "candidate-delegate"
        binary.write_text("candidate bytes")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=1, harness="codex", delegate_bin=binary)

    def test_commands_do_not_preload_skill_or_disable_competitors(self):
        for harness in ("codex", "claude", "hermes"):
            argv, stdin = gate.command(harness, None, self.case["prompt"])
            self.assertNotIn("--skills", argv)
            self.assertNotIn("--bare", argv)
            self.assertNotIn("--safe-mode", argv)
            self.assertNotIn("--append-system-prompt", argv)
            self.assertTrue(stdin == self.case["prompt"] or self.case["prompt"] in argv)

    def test_explicit_routing_prompt_teaches_leading_root_without_changing_default_prompt(self):
        state = self.root / "owned ' quoted state"
        case = dict(self.case, root_mode="explicit", prompt="Use @ROOT@ for the task.")
        prompt = gate.routing_prompt(case, state)
        self.assertIn(str(state), prompt)
        self.assertNotIn("@ROOT@", prompt)
        prefix = prompt.split("`", 2)[1]
        self.assertEqual(gate.shlex.split(prefix), ["delegate", "--root", str(state)])
        self.assertIn("before the command", prompt)
        self.assertEqual(gate.routing_prompt(self.case, state), self.case["prompt"])


    def test_copied_receipt_is_not_an_independent_repetition(self):
        run = self.receipt()
        self.judgment(run)
        gate.shutil.copytree(run, self.root / "review-2")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(output=self.root, cases=[self.case], repetitions=2, harness="codex")

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

    def synthetic_fixture(self, scenario, argv, result, candidate=True):
        fixture = self.root / "synthetic-fixture"
        fixture.mkdir(exist_ok=True)
        state = self.root / "synthetic-state"
        state.mkdir(exist_ok=True)
        workspace = self.root / "synthetic-workspace"
        workspace.mkdir(exist_ok=True)
        brief = self.root / "synthetic-brief.txt"
        brief.write_text("finite routing brief\n")
        delegate = self.root / "synthetic-delegate"
        if candidate:
            delegate.write_text("candidate delegate\n")
            delegate.chmod(0o700)
            delegate_sha256 = gate.digest(delegate)
        else:
            if delegate.exists():
                delegate.unlink()
            delegate_sha256 = None
        (fixture / "runtime.json").write_text(json.dumps({
            "delegate": str(delegate), "path": "/private/path",
            "delegate_sha256": delegate_sha256, "scenario": scenario,
            "provider": "codex:exec", "state_root": str(state),
        }))
        stdout, stderr = io.StringIO(), io.StringIO()
        environment = {"DELEGATE_ROUTING_FIXTURE": str(fixture),
                       "DELEGATE_ROOT": str(self.root / "unrelated-ambient-root")}
        with patch.dict(gate.os.environ, environment), \
                patch.object(gate.sys, "argv", ["delegate", *argv]), \
                patch("subprocess.run", return_value=result) as run, \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            with self.assertRaises(SystemExit) as stopped:
                runpy.run_path(str(gate.REPO / "scripts/skill_routing_delegate_fixture.py"), run_name="__main__")
        return stopped.exception.code, json.loads(stdout.getvalue()), stderr.getvalue(), run

    def synthetic_dispatch_argv(self, state=None):
        state = self.root / "synthetic-state" if state is None else state
        return ["--root", str(state), "dispatch", "--provider", "codex:exec",
                "--brief", str(self.root / "synthetic-brief.txt"),
                "--cwd", str(self.root / "synthetic-workspace"),
                "--permission", "read-only", "--budget", "30s", "--json"]

    def synthetic_task_id(self):
        return json.loads((self.root / "synthetic-fixture/routing-state.json").read_text())["task_id"]

    def synthetic_task_argv(self, command, task_id=None):
        task_id = self.synthetic_task_id() if task_id is None else task_id
        return ["--root", str(self.root / "synthetic-state"), command, task_id, "--json"]

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

    def assert_migration_guidance_matches_contract(self):
        roots = (
            gate.SKILL,
            gate.REPO / "plugins/delegation-layer/skills/agent-integration",
        )
        for root in roots:
            skill = (root / "SKILL.md").read_text(encoding="utf-8")
            result_handling = (root / "references/result-handling.md").read_text(encoding="utf-8")
            result_handling_flat = " ".join(result_handling.split())
            continuation = (root / "references/continuation.md").read_text(encoding="utf-8")
            routing_rule = json.loads((root / "references/routing-rule.json").read_text(encoding="utf-8"))
            # The entry loads the shared decisions before observation; the
            # detailed exceptions belong to the packaged reference.
            self.assertLess(skill.index("references/result-handling.md#task-creation"),
                            skill.index("### 5. Observe and collect"))
            generic_stop = result_handling_flat.index(
                "`task_record: not_created` with any other next action")
            self.assertLess(result_handling_flat.index("supervisor_upgrade_required"), generic_stop)
            self.assertLess(result_handling_flat.index("workspace_provision_pending"), generic_stop)
            self.assertLess(result_handling_flat.index("workspace_busy"), generic_stop)
            self.assertLess(result_handling_flat.index("resource_pressure"), generic_stop)
            self.assertIn("same `continue`", result_handling_flat)
            self.assertIn("exact original admission argv once", result_handling_flat)
            self.assertIn("supervisor_control.state: migrated", result_handling_flat)
            self.assertIn("supervisor_control.next_action: retry_dispatch", result_handling_flat)
            self.assertIn("task_record: unknown", result_handling_flat)
            self.assertIn("do not retry an uncertain task creation", result_handling_flat)
            self.assertIn("argv-equivalent `continue`", continuation)
            self.assertIn("continue remains continue", routing_rule)

    def test_composed_skill_migration_retry_preserves_continue_intent(self):
        original_continue = [
            "delegate", "--root", "/state", "continue", "--task", "a" * 32,
            "--brief", "/brief", "--budget", "30m", "--json",
        ]
        failed_continue = {
            "task_record": "not_created",
            "failure": {
                "code": "supervisor_upgrade_required",
                "next_action": "run_supervisor_migrate",
            },
        }
        migrated = {
            "supervisor_control": {
                "state": "migrated",
                "next_action": "retry_dispatch",
            },
        }
        successor = {
            "command": "continue",
            "task_record": "created",
            "task_id": "b" * 32,
            "parent_task_id": "a" * 32,
        }
        mismatched_continue = dict(failed_continue, failure={
            "code": "supervisor_upgrade_required", "next_action": "stop_and_report"})
        mismatched_migration = {"state": "migrated", "next_action": "retry_dispatch"}

        def may_retry(admission, migration):
            return (admission.get("task_record") == "not_created"
                    and admission.get("failure", {}).get("code") == "supervisor_upgrade_required"
                    and admission.get("failure", {}).get("next_action") == "run_supervisor_migrate"
                    and migration.get("supervisor_control", {}).get("state") == "migrated"
                    and migration.get("supervisor_control", {}).get("next_action") == "retry_dispatch")

        self.assertTrue(may_retry(failed_continue, migrated))
        self.assertFalse(may_retry(mismatched_continue, migrated))
        self.assertFalse(may_retry(failed_continue, mismatched_migration))

        calls = [original_continue.copy()]
        if may_retry(failed_continue, migrated):
            calls.append(["delegate", "--root", "/state", "supervisor", "migrate", "--json"])
            calls.append(original_continue.copy())
        responses = [failed_continue, migrated, successor]
        continue_calls = [argv for argv in calls if "continue" in argv]
        dispatch_calls = [argv for argv in calls if "dispatch" in argv]
        direct_provider_calls = [argv for argv in calls if argv[0] != "delegate"]
        linked = [response for response in responses
                  if response.get("task_record") == "created"
                  and response.get("parent_task_id") == "a" * 32]
        self.assertEqual(continue_calls, [original_continue, original_continue])
        self.assertEqual(calls.count(["delegate", "--root", "/state", "supervisor", "migrate", "--json"]), 1)
        self.assertEqual(len(linked), 1)
        self.assertEqual(dispatch_calls, [])
        self.assertEqual(direct_provider_calls, [])

        self.assert_migration_guidance_matches_contract()

        evaluations = json.loads((gate.SKILL / "evals/evals.json").read_text(encoding="utf-8"))
        migration_case = next(case for case in evaluations["cases"]
                              if case["name"] == "continue-supervisor-migration")
        self.assertEqual(migration_case["expected"], [
            "run same-root supervisor migrate exactly once",
            "retry the argv-equivalent continue exactly once",
            "one linked successor",
            "zero fresh dispatch commands",
            "zero direct provider calls",
        ])

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

    def test_recoverable_synthetic_fixture_binds_one_dispatch_and_one_recheck(self):
        scenario = "recoverable-rejection"
        def normal(command):
            return Mock(
                returncode=2,
                stdout=json.dumps({"command": command, "failure": {
                    "code": "provider_preparation_failed",
                    "stage": "prepare-provider",
                    "next_action": "stop_and_report"}}),
                stderr="")
        code, dispatch, stderr, _ = self.synthetic_fixture(
            scenario, self.synthetic_dispatch_argv(), normal("dispatch"))
        self.assertEqual(code, 0)
        self.assertEqual(stderr, "")
        self.assertEqual(dispatch["task_record"], "created")
        self.assertEqual(dispatch["fixture"]["source"], "synthetic-routing-evidence")
        self.assertEqual(dispatch["fixture"]["provider_execution"], "not_run")
        task_id = self.synthetic_task_id()

        code, terminal, stderr, _ = self.synthetic_fixture(
            scenario, self.synthetic_task_argv("status", task_id), normal("status"))
        self.assertEqual(code, 0)
        self.assertEqual(stderr, "")
        self.assertEqual(terminal["publication"], "rejected")
        self.assertEqual(terminal["outcome"]["verdict"], "rejected")
        self.assertEqual(terminal["failure"]["next_action"], "recheck")

        code, recovered, stderr, _ = self.synthetic_fixture(
            scenario, self.synthetic_task_argv("recheck", task_id), normal("recheck"))
        self.assertEqual(code, 0)
        self.assertEqual(stderr, "")
        self.assertEqual(recovered["publication"], "rejected")
        self.assertEqual(recovered["outcome"]["verdict"], "rejected")
        self.assertEqual(recovered["recheck"]["status"], "recovered")
        self.assertEqual(recovered["recheck"]["assessment_id"],
                         json.loads((self.root / "synthetic-fixture/routing-state.json").read_text())["assessment_id"])
        self.assertEqual(recovered["recheck"]["answer"]["basename"], "result.txt")
        self.assertTrue(recovered["recheck"]["answer"]["sha256"])
        answer_path = Path(recovered["recheck"]["answer_path"])
        self.assertTrue(answer_path.is_absolute())
        self.assertEqual(gate.digest(answer_path), recovered["recheck"]["answer"]["sha256"])
        self.assertEqual(answer_path.stat().st_size, recovered["recheck"]["answer"]["length"])
        self.assertTrue(Path(recovered["recheck"]["report_path"]).is_file())
        self.assertNotIn("raw", json.dumps(recovered))

        code, duplicate, stderr, _ = self.synthetic_fixture(
            scenario, self.synthetic_task_argv("recheck", task_id), normal("recheck"))
        self.assertEqual(code, 2)
        self.assertEqual(stderr, "")
        self.assertEqual(duplicate["failure"]["next_action"], "stop_and_report")
        state = json.loads((self.root / "synthetic-fixture/routing-state.json").read_text())
        self.assertEqual((state["dispatch_count"], state["recheck_count"]), (1, 1))
        self.assertTrue(state["sealed_evidence"]["sealed"])
        self.assertFalse(state["sealed_evidence"]["raw_content_returned"])
        events = [json.loads(line) for line in
                  (self.root / "synthetic-fixture/routing-events.jsonl").read_text().splitlines()]
        self.assertEqual([event["action"] for event in events],
                         ["dispatch", "status", "recheck", "duplicate-recheck"])
        self.assertEqual({event["root"] for event in events}, {str(self.root / "synthetic-state")})
        self.assertEqual({event["task_id"] for event in events}, {task_id})

    def test_still_rejected_synthetic_fixture_uses_rejection_exit_and_no_answer(self):
        scenario = "still-rejected-recheck"
        def normal(command):
            return Mock(returncode=2, stdout=json.dumps({"command": command}), stderr="")
        self.synthetic_fixture(scenario, self.synthetic_dispatch_argv(), normal("dispatch"))
        task_id = self.synthetic_task_id()
        self.synthetic_fixture(scenario, self.synthetic_task_argv("collect", task_id), normal("collect"))
        code, rejected, stderr, _ = self.synthetic_fixture(
            scenario, self.synthetic_task_argv("recheck", task_id), normal("recheck"))
        self.assertEqual(code, 4)
        self.assertEqual(stderr, "")
        self.assertEqual(rejected["publication"], "rejected")
        self.assertEqual(rejected["outcome"]["verdict"], "rejected")
        self.assertEqual(rejected["recheck"]["status"], "rejected")
        self.assertNotIn("answer", rejected["recheck"])
        self.assertNotIn("answer_path", rejected["recheck"])

    def test_synthetic_fixture_preserves_invalid_requests_and_missing_delegate(self):
        scenario = "recoverable-rejection"
        invalid = Mock(returncode=2, stdout=json.dumps({
            "command": "dispatch",
            "failure": {"code": "invalid_request", "next_action": "correct_request"},
        }), stderr="")
        argv = ["--root", str(self.root / "synthetic-state"), "dispatch",
                "--provider", "codex:exec", "--json"]
        code, response, _, _ = self.synthetic_fixture(scenario, argv, invalid)
        self.assertEqual((code, response), (2, json.loads(invalid.stdout)))
        self.assertFalse((self.root / "synthetic-fixture/routing-state.json").exists())

        missing = Mock(returncode=2, stdout=json.dumps({"command": "dispatch"}), stderr="")
        code, response, _, _ = self.synthetic_fixture(
            scenario, self.synthetic_dispatch_argv(), missing, candidate=False)
        self.assertEqual((code, response), (2, json.loads(missing.stdout)))
        self.assertFalse((self.root / "synthetic-fixture/routing-state.json").exists())

    def test_synthetic_fixture_rejects_a_different_root_or_task(self):
        scenario = "recoverable-rejection"
        def normal(command):
            return Mock(returncode=2, stdout=json.dumps({"command": command}), stderr="")
        self.synthetic_fixture(scenario, self.synthetic_dispatch_argv(), normal("dispatch"))
        task_id = self.synthetic_task_id()
        different_root = self.root / "different-state"
        different_root.mkdir()
        wrong_root = ["--root", str(different_root), "recheck", task_id, "--json"]
        code, response, _, _ = self.synthetic_fixture(scenario, wrong_root, normal("recheck"))
        self.assertEqual((code, response), (2, {"command": "recheck"}))
        state = json.loads((self.root / "synthetic-fixture/routing-state.json").read_text())
        self.assertEqual(state["recheck_count"], 0)

    def test_changed_fixture_scenario_cannot_reuse_a_routing_receipt(self):
        case = dict(self.case, name="recovery", fixture_scenario="recoverable-rejection")
        run = self.receipt(case, mode="routing", fixture_scenario="recoverable-rejection")
        self.judgment(run)
        changed = dict(case, fixture_scenario="still-rejected-recheck")
        with self.assertRaisesRegex(ValueError, "receipt selection mismatch"):
            gate.summarize(output=self.root, cases=[changed], repetitions=1, harness="codex", mode="routing")

    def test_supervised_deadline_retains_running_work_without_signaling(self):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root
        owner.daemon = Mock()
        owner.processes = Mock(entries=[])
        owner.task_id = "7"
        queued = Mock()
        queued.json.return_value = {"tasks": {"7": {"status": "Running"}}}
        owner.control = Mock(side_effect=[Mock(), queued])
        with patch.object(gate.time, "sleep"), \
                patch.object(gate.time, "monotonic", side_effect=[0, 0, 31]):
            with self.assertRaisesRegex(RuntimeError, "stop not observed"):
                owner.close()
        self.assertEqual([call.args for call in owner.control.call_args_list], [
            ("pause", "--all", "--wait"), ("status", "--json"),
        ])
        owner.daemon.wait.assert_not_called()
        self.assertFalse((self.root / "supervisor-closed.json").exists())

    def test_supervisor_config_uses_canonical_temporary_directory(self):
        actual = self.root / "actual"
        actual.mkdir()
        alias = self.root / "alias"
        alias.symlink_to(actual, target_is_directory=True)
        run = self.root / "run"
        run.mkdir()
        created = actual / "created"
        created.mkdir()
        ownership = gate.DirectoryOwner()
        with patch.object(gate.tempfile, "mkdtemp", return_value=str(created)), \
                patch.object(gate.shutil, "which", return_value="/mock/bin/tool"):
            owner = gate.AttemptSupervisor(run, {}, ownership)
        self.assertEqual(owner.config, created / "pueue.json")
        config = gate.read_json(owner.config)
        self.assertEqual(config["shared"]["unix_socket_path"],
                         str(created / "run/p.sock"))
        self.assertEqual(gate.read_json(run / "supervisor.json")["config"], str(owner.config))
        ownership.rollback()

    def test_cleanup_removes_queued_work_before_shutdown(self):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root
        owner.daemon = Mock()
        owner.processes = Mock(entries=[])
        queued = Mock()
        queued.json.return_value = {"tasks": {"8": {"status": {"Queued": {}}}}}
        empty = Mock()
        empty.json.return_value = {"tasks": {}}
        owner.control = Mock(side_effect=[Mock(), queued, Mock(), empty, Mock()])
        with patch.object(gate.time, "sleep"):
            owner.close()
        self.assertEqual(owner.control.call_args_list[0].args, ("pause", "--all", "--wait"))
        self.assertEqual(owner.control.call_args_list[2].args, ("remove", "8"))
        self.assertNotIn(("kill", "--all"),
                         [call.args for call in owner.control.call_args_list])
        self.assertEqual(owner.control.call_args_list[-1].args, ("shutdown",))
        owner.daemon.wait.assert_called_once_with(30, expected=0)

    def test_nested_incomplete_lifetime_blocks_outer_shutdown(self):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root / "run"
        owner.run.mkdir()
        owner.daemon = Mock()
        owner.processes = Mock(entries=[])
        state = self.root / "nested-state"
        state.mkdir()
        gate.write_json(state / "root.json", {
            "schema_version": 1, "root_id": "a" * 32,
            "created_at": "2026-10-13T00:00:00Z",
        })
        inspection = state / "inspections" / ("b" * 32)
        inspection.mkdir(parents=True)

        task_record = {
            "schema_version": 1, "root_id": "a" * 32, "task_id": "b" * 32,
            "provider": "fixture:test", "mode": "read-only", "canonical_cwd": "/fixture",
            "requested_config": {}, "budget_nanos": 60_000_000_000,
            "brief_sha256": "0" * 64, "brief_length": 1,
        }
        group = provider_common._inspection_group("a" * 32)
        gate.write_json(inspection / "request.json", {
            "schema_version": 1, "root_id": "a" * 32, "task_id": "b" * 32,
            "task": task_record,
            "task_sha256": provider_common.sha(provider_common._canonical_task_bytes(task_record)),
            "group": group, "label": group + "-" + "b" * 32,
            "created_at": "2026-10-13T00:00:01Z",
            "deadline": "2026-10-13T00:01:01Z",
            "binding": {
                "definition_revision": "fixture-v1", "definition_sha256": "c" * 64,
                "helper_executable": "/fixture/helper", "helper_sha256": "d" * 64,
                "worker_executable": "/fixture/worker", "worker_sha256": "e" * 64,
                "runner_ownership": "managed", "worker_exit_proof": "guardian-wait-v1",
                "supervisor": {
                    "client_executable": "/fixture/pueue", "client_sha256": "f" * 64,
                    "resolved_config_sha256": "1" * 64, "endpoint": "unix:/fixture/p.sock",
                    "config_path": "/fixture/pueue.yml", "config_digest": "2" * 64,
                    "observed_version": "pueue fixture",
                },
            },
        })
        request_sha = gate.digest(inspection / "request.json")
        gate.write_json(inspection / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "numeric_task_id": 37,
        })
        gate.write_json(inspection / "submission.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-10-13T00:00:02Z",
        })
        owner.delegate_state_roots = {state}
        empty = Mock()
        empty.json.return_value = {"tasks": {}}
        owner.control = Mock(side_effect=[Mock(), empty])

        with self.assertRaisesRegex(RuntimeError, "managed inspection lifetime evidence is absent"):
            owner.close()

        self.assertNotIn(("shutdown",), [call.args for call in owner.control.call_args_list])
        self.assertTrue(state.exists())
        self.assertTrue((owner.run / "nested-lifetime-closure.json").is_file())

    def closure_owner(self, name="guard", *, initialized=True):
        owner = object.__new__(gate.AttemptSupervisor)
        owner.run = self.root / name
        owner.run.mkdir()
        state = self.root / (name + "-state")
        state.mkdir()
        if initialized:
            gate.write_json(state / "root.json", {
                "schema_version": 1, "root_id": "a" * 32, "created_at": "2026-10-13T00:00:00Z"})
        owner.delegate_state_roots = {state}
        owner.nested_lifetime_closure = None
        owner.processes = Mock(entries=[])
        owner.daemon = Mock()
        owner.control = Mock()
        return owner, state

    def test_queue_labels_accept_only_canonical_validated_owned_root_ids(self):
        owner, state = self.closure_owner()
        labels = [None, "harness", "delegate:" + "a" * 32 + ":" + "b" * 32,
                  "delegation-inspection-" + "a" * 32 + "-" + "c" * 32]
        snapshot = Mock()
        snapshot.json.return_value = {
            "tasks": {str(index): {"label": label, "status": {"Done": {}}} for index, label in enumerate(labels)}}
        owner.control.side_effect = [Mock(), snapshot, Mock()]
        owner.close()
        result = owner.nested_lifetime_closure
        self.assertEqual(result["roots"], [{"path": str(state), "root_id": "a" * 32,
                                          "task_count": 0, "inspection_count": 0}])
        self.assertEqual([call.args for call in owner.control.call_args_list],
                         [("pause", "--all", "--wait"), ("status", "--json"), ("shutdown",)])
        owner.daemon.wait.assert_called_once_with(30, expected=0)

    def test_queue_guard_uses_validator_result_instead_of_unvalidated_record(self):
        owner, _ = self.closure_owner()
        validated = {"root_id": "b" * 32, "tasks": {}, "inspections": {}}
        with patch.object(gate, "validate_managed_lifetime_closure", return_value=validated) as validate:
            result = owner.authorize_nested_lifetime_closure(snapshot={
                "tasks": {"7": {"label": "delegate:" + "b" * 32 + ":" + "c" * 32}}})
        validate.assert_called_once()
        self.assertEqual(result["roots"][0]["root_id"], validated["root_id"])

    def test_queue_guard_rejects_foreign_roots_including_empty_owned_root(self):
        for index, (initialized, inspection) in enumerate(((False, False), (False, True), (True, False), (True, True))):
            with self.subTest(initialized=initialized, inspection=inspection):
                owner, state = self.closure_owner(str(index), initialized=initialized)
                label = ("delegation-inspection-" + "b" * 32 + "-" + "c" * 32 if inspection else
                         "delegate:" + "b" * 32 + ":" + "c" * 32)
                with self.assertRaisesRegex(ValueError, "unowned Delegate label"):
                    owner.authorize_nested_lifetime_closure(snapshot={"tasks": {"7": {"label": label}}})
                self.assertEqual(gate.read_json(owner.run / "nested-lifetime-closure.json")["status"], "failed")
                self.assertIsNone(owner.nested_lifetime_closure)
                self.assertTrue(state.exists())

    def test_queue_guard_records_malformed_snapshot_failures(self):
        snapshots = [None, [], {}, {"tasks": []}, {"tasks": {"7": None}}, {"tasks": {"7": {}}},
                     {"tasks": {"7": {"label": False}}}, {"tasks": {"7": {"label": "delegate:bad"}}},
                     {"tasks": {"7": {"label": "delegation-inspection-" + "a" * 32 + ":" + "c" * 32}}}]
        for index, snapshot in enumerate(snapshots):
            with self.subTest(snapshot=snapshot):
                owner, _ = self.closure_owner(str(index))
                with self.assertRaisesRegex(ValueError, "private queue"):
                    owner.authorize_nested_lifetime_closure(snapshot=snapshot)
                self.assertEqual(gate.read_json(owner.run / "nested-lifetime-closure.json")["status"], "failed")
                self.assertIsNone(owner.nested_lifetime_closure)

    def test_queue_guard_does_not_replace_canonical_root_validation(self):
        owner, state = self.closure_owner()
        record = state / "root.json"
        record.write_text('{"root_id":"' + "a" * 32 + '"}')
        with self.assertRaises(RuntimeError):
            owner.authorize_nested_lifetime_closure(snapshot={"tasks": {}})
        self.assertEqual(gate.read_json(owner.run / "nested-lifetime-closure.json")["status"], "failed")
        self.assertIsNone(owner.nested_lifetime_closure)

    def test_queue_guard_skips_snapshot_when_daemon_never_started(self):
        owner, _ = self.closure_owner(initialized=False)
        owner.daemon = None
        owner.close()
        self.assertEqual(owner.nested_lifetime_closure, {"status": "closed", "roots": []})
        owner.control.assert_not_called()

    def test_absolute_candidate_queue_bypass_retains_fixture_and_blocks_shutdown(self):
        owner, state = self.closure_owner(initialized=False)
        ownership = DirectoryPinOwner(self)
        ownership.adopt_created("Delegate state", state, self.root)
        fixture = ownership.create("routing fixture", owner.run / "fixture", owner.run).directory
        snapshot = Mock()
        snapshot.json.return_value = {"tasks": {"7": {
            "label": "delegate:" + "b" * 32 + ":" + "c" * 32, "status": {"Done": {}},
            "original_command": str(self.candidate) + " dispatch --auto"}}}
        owner.control.side_effect = [Mock(), snapshot]
        receipt = {"status": "unreviewed"}
        gate._finalize_routing_attempt(owner.run, owner, ownership, receipt)
        self.assertEqual(receipt["reason"], "supervisor_cleanup_unproven")
        self.assertEqual(gate.read_json(owner.run / "nested-lifetime-closure.json")["status"], "failed")
        self.assertEqual(gate.read_json(owner.run / "cleanup.json")["status"], "retained")
        self.assertTrue(state.exists() and fixture.exists())
        self.assertEqual([call.args for call in owner.control.call_args_list],
                         [("pause", "--all", "--wait"), ("status", "--json")])
        owner.daemon.wait.assert_not_called()
        self.assertFalse((owner.run / "supervisor-closed.json").exists())

    def test_uncertain_daemon_start_keeps_attempt_unclosed_and_state_retained(self):
        tool = self.root / "bin" / "tool"
        tool.parent.mkdir()
        tool.write_bytes(b"recorded executable; the fake Popen never runs it\n")
        queue = self.root / "queue"
        queue.mkdir()
        run = self.root / "run"
        run.mkdir()
        ownership = DirectoryPinOwner(self)
        with patch.object(gate.tempfile, "mkdtemp", return_value=str(queue)), \
                patch.object(gate.shutil, "which", return_value=str(tool)):
            owner = gate.AttemptSupervisor(run, {}, ownership)
        # The fake raises from inside Popen and creates no child.
        with patch("subprocess.Popen", side_effect=OSError("errpipe read failed")), \
                self.assertRaisesRegex(OSError, "errpipe read failed"):
            owner.start()
        capture = run / "processes" / "001-daemon"
        self.assertTrue((capture / "invocation.json").is_file())
        receipt = {"status": "unreviewed"}
        gate._finalize_routing_attempt(run, owner, ownership, receipt)
        self.assertEqual(receipt["status"], "failed")
        self.assertEqual(receipt["reason"], "supervisor_cleanup_unproven")
        self.assertEqual(gate.read_json(run / "receipt.json"), receipt)
        self.assertFalse(gate.read_json(run / "cleanup.json")["supervisor_shutdown_observed"])
        self.assertEqual(owner.processes.pending(), [{
            "pid": None, "argv": [str(tool), "-c", str(owner.config)], "directory": str(capture)}])
        self.assertEqual(sorted(path.name for path in capture.iterdir()),
                         ["invocation.json", "stderr", "stdout"])
        self.assertTrue((queue / "state").is_dir() and owner.config.is_file())
        self.assertFalse((run / "nested-lifetime-closure.json").exists())

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
        diagnosis = [case for case in cases if "paired_case_id" in case]
        leaves = gate.paired_scenarios(cases)
        self.assertEqual(len(diagnosis), 6)
        self.assertEqual(len(gate.read_json(gate.SKILL / "evals/cases.json")["cases"]), 3)
        self.assertEqual({case["paired_case_id"] for case in diagnosis}, set(leaves))
        self.assertEqual(len({case["fixture_scenario"] for case in diagnosis}), 6)
        self.assertTrue(all(case["external"] is False for case in diagnosis))
        cases = [case for case in cases if "paired_case_id" not in case]
        holdouts = [c for c in cases if c.get("holdout")]
        self.assertEqual({c["name"] for c in holdouts}, {
            "recheck-recoverable", "recheck-still-rejected",
            "external-collaboration-holdout"})
        self.assertEqual({c.get("fixture_scenario") for c in holdouts},
                         {None, "recoverable-rejection", "still-rejected-recheck"})
        self.assertTrue(all(c["external"] and c["split"] == "validation" for c in holdouts))
        regressions = [c for c in cases if c.get("regression")]
        self.assertEqual({c["name"] for c in regressions}, {
            "requested-model-effort", "ambiguous-model", "parallel-receipts",
            "subagent-external", "explicit-bypass"})
        self.assertEqual(sum(c["external"] for c in regressions), 4)
        cases = [c for c in cases if not c.get("regression") and not c.get("holdout")]
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
