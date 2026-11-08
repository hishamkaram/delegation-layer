"""Synthetic diagnosis joins, evidence boundaries and paid-execution guards."""
import contextlib
import io
import json
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest.mock import MagicMock, Mock, patch

from scripts.acceptance.processes.contracts import BoundedCommandResult
import scripts.acceptance.skill_routing.cli as gate


class DiagnosisRoutingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        home = patch.object(gate.Path, "home", return_value=self.root)
        home.start()
        self.addCleanup(home.stop)

    def test_paired_routing_validates_join_and_preserves_neutral_prompts(self):
        suite = gate.read_json(gate.CASES)["cases"]
        leaves = gate.paired_scenarios(suite)
        self.assertEqual(len(leaves), 6)
        for mutation in ("missing", "duplicate", "prompt", "external"):
            changed = json.loads(json.dumps(suite))
            selected = [case for case in changed if "paired_case_id" in case]
            if mutation == "missing":
                changed.remove(selected[0])
            elif mutation == "duplicate":
                selected[0]["paired_case_id"] = selected[1]["paired_case_id"]
            elif mutation == "prompt":
                selected[0]["prompt"] = "Diagnose an authentication failure"
            else:
                selected[0]["external"] = True
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                gate.paired_scenarios(changed)

    def test_delegate_pair_uses_real_inspection_class_and_neutral_stderr(self):
        pair = next(case for case in gate.read_json(gate.SKILL / "evals/cases.json")["cases"]
                    if case["id"] == "delegate-supported")
        primary = json.loads(pair["visible"]["inputs"][0]["content"])
        control = json.loads(pair["control"]["visible"]["inputs"][0]["content"])
        self.assertEqual(primary["failure"]["code"], "provider_preflight_failed")
        self.assertEqual(primary["failure"]["inspection"]["class"], "inspection_worker_failed")
        self.assertEqual(primary["task_record"], "not_created")
        self.assertEqual(primary["failure"]["next_action"], "stop_and_report")
        primary["failure"].pop("inspection")
        self.assertEqual(primary, control)
        self.assertEqual(pair["visible"]["inputs"][1], pair["control"]["visible"]["inputs"][1])

    def test_existing_suite_selection_omits_ineligible_paired_cases(self):
        argv = ["routing", "--harness", "claude", "--output", str(self.root / "capture"), "--summarize"]
        with patch.object(gate.sys, "argv", argv), \
                patch.object(gate, "summarize", return_value={"status": "passed"}) as summarize, \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(gate.main(), 0)
        self.assertTrue(summarize.call_args.kwargs["cases"])
        self.assertFalse(any("paired_case_id" in case for case in summarize.call_args.kwargs["cases"]))
        with patch.object(gate.sys, "argv", argv + ["--case", "handoff-a"]), \
                patch.object(gate, "run_case") as run, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit) as stopped:
            gate.main()
        self.assertEqual(stopped.exception.code, 2)
        run.assert_not_called()
        self.assertFalse((self.root / "capture").exists())

    def prepare_diagnosis(self):
        fixture = self.root / "diagnostic-fixture"
        workspace = self.root / "diagnostic-workspace"
        state = self.root / "diagnostic-state"
        for path in (fixture, workspace, state):
            path.mkdir()
        case = next(case for case in gate.read_json(gate.CASES)["cases"]
                    if "paired_case_id" in case)
        gate.write_json(fixture / "runtime.json", {
            "scenario": case["fixture_scenario"], "case": case["name"],
            "provider": case["provider"], "delegate": "/not-called",
            "path": "/private/path", "state_root": str(state),
        })
        gate.materialize_diagnosis(fixture, workspace, state, case)
        return fixture, workspace, state, case

    def call_diagnosis(self, fixture, state, argv):
        stdout = io.StringIO()
        with patch.dict(gate.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(fixture),
                                         "DELEGATE_ROOT": str(self.root / "unrelated-ambient-root")}), \
                patch.object(gate.sys, "argv", ["delegate", *argv]), \
                patch("scripts.acceptance.processes.operations.run_bounded_command") as run, contextlib.redirect_stdout(stdout):
            with self.assertRaises(SystemExit) as stopped:
                runpy.run_path(str(gate.REPO / "scripts/acceptance/skill_routing/delegate_fixture.py"),
                               run_name="__main__")
        run.assert_not_called()
        return stopped.exception.code, json.loads(stdout.getvalue())

    def test_diagnosis_materializes_only_visible_inputs_and_sealed_evidence(self):
        fixture, workspace, state, case = self.prepare_diagnosis()
        response = gate.read_json(workspace / "handoff.json")
        self.assertEqual(response["task_id"], gate.routing_fixture.task_id(state, case["name"]))
        self.assertEqual(response["root_id"], gate.routing_fixture.root_id(state))
        self.assertEqual((fixture / "calls.jsonl").read_text(), "")
        for command in ("status", "collect", "logs"):
            code, result = self.call_diagnosis(fixture, state, [
                "--root", str(state), command, response["task_id"], "--json"])
            self.assertEqual(code, 4 if command == "collect" else 0)
            self.assertEqual(result["publication"], "rejected")
            self.assertEqual(result["failure"]["next_action"], "stop_and_report")
            if command == "logs":
                descriptor = result["raw"][0]
                self.assertTrue(descriptor["sealed"])
                raw = Path(descriptor["path"])
                self.assertEqual(raw, workspace / "evidence/stderr.txt")
                self.assertEqual(descriptor["sha256"], gate.digest(raw))
                self.assertEqual(descriptor["size"], raw.stat().st_size)
        visible = (workspace / "handoff.json").read_text() + (fixture / "runtime.json").read_text()
        self.assertNotIn(case["paired_case_id"], visible)
        self.assertNotIn("rationale", visible)
        self.assertEqual(gate.read_json(fixture / "routing-state.json")["refused_commands"], 0)

    def test_diagnosis_never_forwards_mutations_or_unknown_task_reads(self):
        fixture, workspace, state, case = self.prepare_diagnosis()
        task_id = gate.read_json(workspace / "handoff.json")["task_id"]
        commands = (["dispatch", "--auto"], ["continue", "--task", task_id],
                    ["cancel", task_id], ["recheck", task_id], ["supervisor", "migrate"],
                    ["logs", "f" * 32], ["status", "unknown"],
                    ["preflight", "--provider", case["provider"], "--live"],
                    ["preflight", "--provider", case["provider"], "--live=true"],
                    ["dispatch", "logs", task_id], ["logs", task_id, "--unknown"],
                    ["dispatch", "--model", "--help"], ["continue", "--brief", "--help"])
        original = gate.read_json(fixture / "routing-state.json")["response"]
        for command in commands:
            with self.subTest(command=command):
                code, response = self.call_diagnosis(
                    fixture, state, ["--root", str(state), *command, "--json"])
                self.assertEqual(code, 2)
                self.assertEqual(response["failure"]["code"], "synthetic_action_refused")
        recorded = gate.read_json(fixture / "routing-state.json")
        self.assertEqual(recorded["response"], original)
        self.assertEqual(recorded["refused_commands"], len(commands))

    def test_diagnosis_preflight_matches_public_static_shape(self):
        fixture, workspace, state, case = self.prepare_diagnosis()
        base = ["--root", str(state), "preflight", "--provider", case["provider"], "--json"]
        for suffix in ([], ["--cwd", "relative"]):
            code, result = self.call_diagnosis(fixture, state, base + suffix)
            self.assertEqual(code, 2)
            self.assertEqual(result["failure"]["code"], "synthetic_action_refused")
        code, result = self.call_diagnosis(fixture, state, base + ["--cwd", str(workspace)])
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "ready")
        self.assertEqual(result["capability"]["verification"], "preflight")
        self.assertEqual(result["capability"]["reason_code"], "static_admission_ready")
        self.assertEqual(result["capability"]["live_acceptance"]["authentication"], "unknown")
        self.assertNotIn("live_acceptance", result)

    def test_diagnosis_keeps_real_static_catalog_and_control_version(self):
        fixture, _, state, _ = self.prepare_diagnosis()
        for argv, code, answer, diagnostic in ((["--version"], 0, "delegate candidate\n", ""),
                                               (["providers", "--json"], 0, '{"providers": []}\n', ""),
                                               (["logs", "--help"], 2, "", "Usage error: delegate\n")):
            stdout, stderr = io.StringIO(), io.StringIO()
            with patch.dict(gate.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(fixture),
                                             "DELEGATE_ROOT": str(self.root / "unrelated-ambient-root")}), \
                    patch.object(gate.sys, "argv", ["delegate", *argv]), \
                    patch("scripts.acceptance.processes.operations.run_bounded_command",
                          return_value=BoundedCommandResult(code, answer.encode(), diagnostic.encode())) as run, \
                    contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr), \
                    self.assertRaises(SystemExit) as stopped:
                runpy.run_path(str(gate.REPO / "scripts/acceptance/skill_routing/delegate_fixture.py"), run_name="__main__")
            self.assertEqual(stopped.exception.code, code)
            self.assertEqual(stdout.getvalue(), answer)
            self.assertEqual(stderr.getvalue(), diagnostic)
            self.assertEqual(run.call_args.args[0], ["/not-called", *argv])

    def test_unavailable_real_validation_never_becomes_synthetic_success(self):
        fixture, _, _, _ = self.prepare_diagnosis()
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.dict(gate.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(fixture)}), \
                patch.object(gate.sys, "argv", ["delegate", "--version"]), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr), \
                self.assertRaises(SystemExit) as stopped:
            runpy.run_path(str(gate.REPO / "scripts/acceptance/skill_routing/delegate_fixture.py"),
                           run_name="__main__")
        self.assertEqual(stopped.exception.code, 1)
        self.assertEqual(stdout.getvalue(), "")
        self.assertEqual(stderr.getvalue(), "routing fixture validation unavailable: command_start_failed\n")

    def test_diagnosis_materialization_refuses_collisions_and_unsafe_paths(self):
        fixture, workspace, state, case = self.prepare_diagnosis()
        original = (workspace / "handoff.json").read_bytes()
        with self.assertRaisesRegex(ValueError, "collides"):
            gate.materialize_diagnosis(fixture, workspace, state, case)
        self.assertEqual((workspace / "handoff.json").read_bytes(), original)
        skill = self.root / "unsafe-skill" / "agent-integration"
        (skill / "evals").mkdir(parents=True)
        data = gate.read_json(gate.SKILL / "evals/cases.json")
        data["cases"][0]["visible"]["inputs"][0]["path"] = "../outside.json"
        gate.write_json(skill / "evals/cases.json", data)
        with patch.object(gate, "SKILL", skill), self.assertRaisesRegex(ValueError, "safe relative"):
            gate.materialize_diagnosis(fixture, workspace, state, case)
        self.assertFalse((self.root / "outside.json").exists())

    def test_diagnosis_stops_uncertain_creation_and_keeps_cancellation_terminal(self):
        fixture, workspace, state, _ = self.prepare_diagnosis()
        state_path = fixture / "routing-state.json"
        original = gate.read_json(state_path)
        for task_record in ("not_created", "unknown"):
            saved = json.loads(json.dumps(original))
            saved["response"]["task_record"] = task_record
            state_path.write_text(json.dumps(saved))
            for command in ("status", "collect", "logs"):
                with self.subTest(task_record=task_record, command=command):
                    code, response = self.call_diagnosis(fixture, state, [
                        "--root", str(state), command, saved["task_id"], "--json"])
                    self.assertEqual(code, 2)
                    self.assertEqual(response["failure"]["code"], "synthetic_action_refused")
        saved = json.loads(json.dumps(original))
        saved["response"]["failure"]["code"] = "task_cancelled"
        saved["response"]["continuation"] = {"resumable": True}
        state_path.write_text(json.dumps(saved))
        code, response = self.call_diagnosis(fixture, state, [
            "--root", str(state), "collect", saved["task_id"], "--json"])
        self.assertEqual(code, 4)
        self.assertEqual(response["failure"]["code"], "task_cancelled")
        code, _ = self.call_diagnosis(fixture, state, [
            "--root", str(state), "continue", "--task", saved["task_id"], "--json"])
        self.assertEqual(code, 2)

    def test_diagnosis_context_requires_only_candidate_and_no_external_tools(self):
        workspace = self.root / "workspace"
        workspace.mkdir()
        gate.prepare_workspace(workspace, "codex")
        candidate = workspace / ".agents/skills/agent-integration/SKILL.md"
        document = [
            {"type": "additional_tools", "tools": [{"name": "functions"}, {"name": "clock"}]},
            {"content": [{"text": '### Available skills\n- `r0` = `' + str(candidate.parent.parent) + '`\n'
                          '- agent-integration: diagnostic guidance (file: r0/agent-integration/SKILL.md)'}]},
            {"role": "developer", "content": [{"text": gate.read_json(gate.SKILL / "references/routing-rule.json")}]},
        ]
        self.assertEqual(gate.validate_diagnosis_context(document, workspace)["candidate_skill"], str(candidate))
        changed = json.loads(json.dumps(document))
        changed[0]["tools"].append({"name": "collaboration"})
        with self.assertRaisesRegex(ValueError, "external collaboration"):
            gate.validate_diagnosis_context(changed, workspace)
        changed = json.loads(json.dumps(document))
        changed[1]["content"][0]["text"] += '\n- agent-integration: old (file: /old/SKILL.md)'
        with self.assertRaisesRegex(ValueError, "candidate-only"):
            gate.validate_diagnosis_context(changed, workspace)
        changed = json.loads(json.dumps(document))
        changed.append({"role": "developer", "content": document[2]["content"]})
        with self.assertRaisesRegex(ValueError, "exact candidate routing instruction"):
            gate.validate_diagnosis_context(changed, workspace)
        changed = json.loads(json.dumps(document))
        changed.append({"role": "user", "content": [{"text": "Old Delegate routing: stop without diagnosis."}]})
        with self.assertRaisesRegex(ValueError, "contaminate"):
            gate.validate_diagnosis_context(changed, workspace)
        with self.assertRaisesRegex(ValueError, "exact candidate routing instruction"):
            gate.validate_diagnosis_context(document[:2], workspace)
        for malformed in ({}, [None], [{"type": "additional_tools", "tools": ["unknown"]}]):
            with self.subTest(malformed=malformed), self.assertRaises(ValueError):
                gate.validate_diagnosis_context(malformed, workspace)

    def test_diagnosis_controls_are_invocation_scoped_and_block_before_paid_work(self):
        workspace = self.root / "workspace"
        args = gate.diagnosis_arguments(workspace, self.root / "fixture")
        self.assertIn("--ignore-user-config", args)
        self.assertIn("--ignore-rules", args)
        self.assertIn("sandbox_workspace_write.network_access=false", args)
        self.assertIn("mcp_servers={}", args)
        self.assertIn("agents.enabled=false", args)
        self.assertIn("notify=[]", args)
        self.assertIn("multi_agent", args)
        self.assertIn("multi_agent_v2", args)
        self.assertIn("hooks", args)
        self.assertNotIn("--dangerously-bypass-hook-trust", args)
        instruction = next(argument for argument in args if argument.startswith("developer_instructions="))
        self.assertEqual(json.loads(instruction.split("=", 1)[1]), gate.read_json(gate.SKILL / "references/routing-rule.json"))
        command, _ = gate.command("codex", "requested-model", "prompt", diagnosis=True)
        self.assertIn("workspace-write", command)
        self.assertNotIn("danger-full-access", command)
        self.assertEqual(command[command.index("--model") + 1], "requested-model")
        help_file = self.root / "help.txt"
        help_file.write_text("--ignore-user-config --ignore-rules --disable")
        proof = self.root / "proof.json"
        proof.write_text('{"network_and_ipc_connections":"denied"}')
        context = self.root / "context.json"
        context.write_text('[{"type":"additional_tools","tools":[{"name":"collaboration"}]}]')
        owner = Mock()
        owner.capture.side_effect = [help_file, context]
        ownership = gate.DirectoryOwner()
        self.addCleanup(ownership.rollback)
        with patch.object(gate, "verify_network_boundary", return_value=proof), \
                self.assertRaisesRegex(ValueError, "external collaboration"):
            gate.verify_diagnosis_controls(owner, "codex", workspace, ownership=ownership, model="requested-model")
        self.assertIn('model="requested-model"', owner.capture.call_args.args[1])
        owner.execute.assert_not_called()

    def test_network_probe_requires_denied_connection_to_existing_listener(self):
        for error in (PermissionError(), FileNotFoundError(), ConnectionRefusedError(), None):
            with self.subTest(error=type(error).__name__):
                ownership = gate.DirectoryOwner()
                self.addCleanup(ownership.rollback)
                owner = Mock()
                proof = self.root / "proof.json"
                connection = MagicMock()
                connection.connect.side_effect = error
                def new_socket(family):
                    if family in (gate.socket.AF_INET, gate.socket.AF_INET6):
                        raise PermissionError()
                    return connection
                fake = Mock(AF_INET=gate.socket.AF_INET, AF_INET6=gate.socket.AF_INET6,
                            AF_UNIX=gate.socket.AF_UNIX)
                fake.socket.side_effect = new_socket
                def capture(name, argv):
                    self.assertEqual(name, "diagnosis-sandbox")
                    self.assertTrue(Path(argv[-1]).exists())
                    output = io.StringIO()
                    with patch.dict(gate.sys.modules, {"socket": fake}), \
                            patch.object(gate.sys, "argv", ["probe", argv[-1]]), \
                            contextlib.redirect_stdout(output):
                        # Only the maintained probe is executed, with native sockets replaced.
                        exec(argv[-2], {})
                    proof.write_text(output.getvalue())
                    return proof
                owner.capture.side_effect = capture
                listener = MagicMock()
                listener.__enter__.return_value.bind.side_effect = lambda address: Path(address).touch(exist_ok=False)
                with patch.object(gate.socket, "socket", return_value=listener):
                    if isinstance(error, PermissionError):
                        result = gate.verify_network_boundary(owner, "codex", ownership=ownership)
                        self.assertEqual(gate.read_json(result), {"network_and_ipc_connections": "denied"})
                    else:
                        with self.assertRaises((FileNotFoundError, ConnectionRefusedError, SystemExit)):
                            gate.verify_network_boundary(owner, "codex", ownership=ownership)
                self.assertFalse(any(path.exists() for path in self.root.glob(".delegate-eval-*/probe.sock")))
