"""Upgrade procedure fixtures and fail-closed actor launch boundaries."""

import contextlib
import errno
import io
import json
from pathlib import Path
import tempfile
import sys
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance.skill_routing import cli
from scripts.acceptance.skill_routing import upgrade


class UpgradeFixtureTests(unittest.TestCase):
    def record(self, value):
        if not isinstance(value, dict):
            self.fail("fixture response record is not an object")
        return value

    def state(self, scenario="planned"):
        return upgrade.initial(scenario, "/fixture/owned-root")

    def test_planned_upgrade_requires_quiescence_preparation_install_and_exact_completion(self):
        state = self.state()
        for command in (["fixture-action", "install", "upgrade", "delegation-layer"],
                        ["supervisor", "ensure-owner", "--prepare-upgrade"]):
            with self.subTest(command=command):
                self.assertEqual(upgrade.reply(state, command)[1], 1)
        upgrade.reply(state, ["scheduler", "stop"])
        prepared, code = upgrade.reply(state, ["supervisor", "ensure-owner", "--prepare-upgrade"])
        self.assertEqual(code, 0)
        control = self.record(prepared["supervisor_control"])
        report = self.record(control["upgrade"])
        self.assertEqual(report["operation_id"], upgrade.OPERATION)
        self.assertEqual(report["phase"], "awaiting_installation")
        self.assertEqual(upgrade.reply(state, ["fixture-action", "install", "uninstall", "delegation-layer"])[1], 2)
        self.assertEqual(upgrade.reply(state, ["fixture-action", "install", "upgrade", "delegation-layer"])[1], 0)
        self.assertEqual(upgrade.reply(state, ["supervisor", "ensure-owner", "--complete-upgrade", "wrong"])[1], 1)
        completion = ["delegate", "supervisor", "ensure-owner", "--complete-upgrade", upgrade.OPERATION, "--json"]
        self.assertEqual(report["completion_command"], completion)
        completed, code = upgrade.reply(state, completion[1:])
        self.assertEqual(code, 0)
        self.assertTrue(self.record(self.record(completed["supervisor_control"])["upgrade"])["completed"])
        self.assertEqual(state["scheduler"], "stopped")
        self.assertEqual(upgrade.reply(state, ["fixture-action", "bootstrap", "bootstrap"])[1], 0)
        self.assertEqual(state["scheduler"], "ready")

    def test_homebrew_discovery_names_installed_formula_without_advancing_preparation(self):
        state = self.state("planned")
        original = dict(state)
        for suffix in ([], ["delegation-layer"], ["hishamkaram/tap/delegation-layer"]):
            response, code = upgrade.reply(state, ["fixture-action", "install", "list", "--versions", *suffix])
            self.assertEqual((code, response), (0, {"terminal_output": "delegation-layer 0.7.0\n"}))
            self.assertEqual(state, original)
        response, code = upgrade.reply(state, ["fixture-action", "install", "services", "list"])
        self.assertEqual((code, response), (0, {"terminal_output": "Name Status User File\n"}))
        self.assertEqual(upgrade.reply(state, ["fixture-action", "install", "upgrade", "delegate"])[1], 2)
        self.assertEqual(state, original)

    def test_public_schedule_discovery_and_serve_preserve_original_paused_definition(self):
        state = self.state("planned")
        before, _ = upgrade.reply(state, ["schedule", "list", "--json"])
        self.assertEqual(before["schedules"], [{
            "id": "e" * 32, "state": "paused", "created_at": "2026-11-01T00:00:00Z",
            "cursor": "2026-11-01T00:00:00Z", "cron": "* * * * *", "timezone": "UTC",
        }])
        upgrade.reply(state, ["scheduler", "stop"])
        status, _ = upgrade.reply(state, ["scheduler", "status", "--json"])
        command = self.record(status["scheduler"])["serve_command"]
        self.assertEqual(command, ["delegate", "--root", "/fixture/owned-root", "scheduler", "serve"])
        self.assertEqual(upgrade.reply(state, ["scheduler", "serve"])[1], 1)
        upgrade.reply(state, ["supervisor", "ensure-owner", "--prepare-upgrade"])
        upgrade.reply(state, ["fixture-action", "install", "upgrade", "delegation-layer"])
        upgrade.reply(state, ["supervisor", "ensure-owner", "--complete-upgrade", upgrade.OPERATION])
        self.assertEqual(upgrade.reply(state, ["scheduler", "serve"])[1], 0)
        after, _ = upgrade.reply(state, ["schedule", "list", "--json"])
        self.assertEqual(before["schedules"], after["schedules"])
        self.assertEqual(upgrade.reply(state, ["schedule", "resume", "e" * 32])[1], 2)

    def test_brew_discovery_uses_native_text_output_at_owned_runtime_boundary(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.json"
            runtime.write_text("{}")
            runtime.with_name("upgrade-state.json").write_text(json.dumps(self.state("planned")))
            with contextlib.redirect_stdout(io.StringIO()) as output:
                code = upgrade.invoke(runtime, ["--root", "/fixture/owned-root", "fixture-action", "install", "list", "--versions"])
            self.assertEqual((code, output.getvalue()), (0, "delegation-layer 0.7.0\n"))

    def test_reactive_handoff_retains_operation_and_distinguishes_progress_from_fault(self):
        state = self.state("interrupted")
        first, code = upgrade.reply(state, ["supervisor", "ensure-owner"])
        self.assertEqual(code, 3)
        self.assertEqual(self.record(first["supervisor_control"])["reason_code"], "handoff_in_progress")
        done, code = upgrade.reply(state, ["supervisor", "ensure-owner"])
        self.assertEqual(code, 0)
        self.assertEqual(self.record(self.record(done["supervisor_control"])["upgrade"])["operation_id"], upgrade.OPERATION)
        failed, code = upgrade.reply(self.state("fault"), ["supervisor", "ensure-owner"])
        self.assertEqual(code, 1)
        self.assertEqual(self.record(failed["supervisor_control"])["next_action"], "stop_and_report")

    def test_fixture_never_authorizes_its_own_settlement_confirmation_or_unknown_mutations(self):
        state = self.state("inspection-blocker")
        prepared, code = upgrade.reply(state, ["settle", upgrade.INSPECTION, "--kind", "inspection", "--prepare"])
        self.assertEqual(code, 0)
        challenge = self.record(prepared["settlement"])["challenge"]
        self.assertEqual(upgrade.reply(state, ["settle", upgrade.INSPECTION, "--kind", "inspection", "--confirm", challenge])[1], 1)
        for command in (["cancel", upgrade.INSPECTION], ["dispatch", "--auto"], ["supervisor", "reset"]):
            with self.subTest(command=command):
                response, code = upgrade.reply(state, command)
                self.assertEqual(code, 2)
                error = response["error"]
                self.assertIsInstance(error, str)
                self.assertIn("no real command", str(error))

    def test_current_label_blockers_do_not_change_kind_or_grant_closure(self):
        for scenario, kind, action in (("task-blocker", "task", "cancel_task"),
                                       ("inspection-blocker", "inspection", "settle_inspection")):
            response, code = upgrade.reply(self.state(scenario), ["supervisor", "status"])
            self.assertEqual(code, 3)
            blocker = self.record(self.record(response["supervisor_control"])["first_blocker"])
            self.assertEqual((blocker["kind"], blocker["next_action"]), (kind, action))
            self.assertNotIn("outcome", response)

    def test_eligible_inspection_and_prepare_match_public_maintenance_contract(self):
        state = self.state("inspection-blocker")
        response, code = upgrade.reply(state, ["supervisor", "status"])
        self.assertEqual(code, 3)
        self.assertEqual(self.record(response["supervisor_control"])["first_blocker"], {
            "kind": "inspection", "operation_id": "b" * 32,
            "reason_code": "inspection_closure_unproven", "next_action": "settle_inspection",
        })
        prepared, code = upgrade.reply(state, ["settle", "b" * 32, "--kind", "inspection", "--prepare"])
        self.assertEqual(code, 0)
        self.assertEqual(prepared["settlement"], {
            "target_kind": "inspection", "operation_id": "b" * 32,
            "acknowledged_missing_facts": ["scope_closure_unproven"],
            "administrative_state": "prepared", "native_execution": "unknown",
            "permission": "allow_supervisor_maintenance", "challenge": "c" * 64,
            "held_claims": ["scheduler", "resource", "session", "workspace", "custody", "deletion"],
        })

    def test_evidence_fault_names_only_the_missing_public_fact(self):
        response, code = upgrade.reply(self.state("fault"), ["supervisor", "ensure-owner"])
        self.assertEqual(code, 1)
        self.assertEqual(response["control_fact"], {
            "resource_kind": "inspection", "resource_id": "b" * 32,
            "fact_kind": "evidence_record", "name": "completion.json",
            "reason_code": "evidence_missing", "next_action": "inspect_evidence",
        })

    def test_boundary_rejects_corrupt_fixture_state(self):
        for field, value in (("root", "relative"), ("phase", "invented"), ("operation_id", "different"),
                             ("scenario", "unknown"), ("scheduler", None)):
            state = dict(self.state(), **{field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                upgrade.validate_state(state)

    def test_context_keeps_both_candidate_skills_but_refuses_unmediated_integrations(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            for name in upgrade.SKILLS:
                skill = workspace / ".agents/skills" / name / "SKILL.md"
                skill.parent.mkdir(parents=True)
                skill.write_text("candidate")
            metadata = "\n".join(str(workspace / ".agents/skills" / name / "SKILL.md") for name in upgrade.SKILLS)
            context = [{"type": "additional_tools", "tools": [{"name": "functions"}]},
                       {"role": "system", "content": [{"text": metadata + "\ncompeting-skill"}]}]
            self.assertEqual(upgrade.context_controls(context, workspace)["skills"], list(upgrade.SKILLS))
            context[0]["tools"].append({"name": "unmediated-service"})
            with self.assertRaisesRegex(ValueError, "unmediated"):
                upgrade.context_controls(context, workspace)

    def test_candidate_actor_never_starts_when_native_containment_probe_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            selected = root / "selected"
            selected.write_text("owned synthetic metadata only")
            selected.chmod(0o700)
            owner = Mock(client="/mock/pueue", daemon_path="/mock/pueued")
            owner.processes.environment = {}
            case = upgrade.cases()[0]
            with patch.object(cli, "AttemptSupervisor", return_value=owner), \
                    patch.object(cli, "capture_candidate_metadata", return_value={}), \
                    patch.object(cli, "verify_upgrade_controls", side_effect=RuntimeError("native boundary unavailable")), \
                    patch.object(cli.shutil, "which", return_value="/mock/tool"), \
                    contextlib.redirect_stdout(io.StringIO()):
                receipt = cli.run_case(output=root, harness="codex", model=None, case=case,
                                       repetition=1, mode="routing", delegate_bin=selected)
            self.assertEqual(receipt["status"], "blocked")
            owner.execute.assert_not_called()
            self.assertNotIn("upgrade_controls", receipt)
            owner.close.assert_called_once()

    def test_write_canary_accepts_only_native_access_denials(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "must-not-exist"
            for code in (errno.EACCES, errno.EPERM, errno.EROFS, errno.EIO):
                with self.subTest(errno=code), \
                        patch.object(Path, "write_text", side_effect=OSError(code, "native canary result")), \
                        patch("subprocess.run", return_value=Mock(returncode=1)) as shell, \
                        patch.object(sys, "argv", ["probe", str(target)]), \
                        contextlib.redirect_stdout(io.StringIO()) as output:
                    if code == errno.EIO:
                        with self.assertRaises(OSError) as failure:
                            exec(cli.UPGRADE_WRITE_PROBE, {})
                        self.assertEqual(failure.exception.errno, errno.EIO)
                        shell.assert_not_called()
                    else:
                        exec(cli.UPGRADE_WRITE_PROBE, {})
                        self.assertIn('"outside_and_absolute_shell_writes": "denied"', output.getvalue())
                        shell.assert_called_once()
            self.assertFalse(target.exists())

    def test_upgrade_candidate_copies_both_skills_without_changing_installed_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            cli.prepare_workspace(workspace, "codex", include_upgrade=True)
            for name in upgrade.SKILLS:
                copied = workspace / ".agents/skills" / name / "SKILL.md"
                self.assertEqual(copied.read_bytes(), (cli.REPO / "skills" / name / "SKILL.md").read_bytes())

    def test_upgrade_invocation_disables_native_agent_tools_without_hiding_skills(self):
        arguments = upgrade.arguments(Path("/fixture/workspace"), Path("/fixture/runtime"))
        index = arguments.index("agents.enabled=false")
        self.assertEqual(arguments[index - 1], "-c")
        self.assertNotIn("skills.enabled=false", arguments)


if __name__ == "__main__":
    unittest.main()
