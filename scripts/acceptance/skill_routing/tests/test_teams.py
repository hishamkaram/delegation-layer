"""Routing fixture isolation and real-response preservation, not model judgment."""

import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.processes.contracts import BoundedCommandResult
from scripts.acceptance.skill_routing import cli, delegate_fixture, teams


class TeamRoutingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="Team routing ' é-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.home = self.root / "catalog-home"
        self.environment = teams.catalog_environment(self.home)

    def test_only_backend_environment_selects_owned_catalog_and_keeps_other_context(self):
        with patch.dict(os.environ, {"HOME": "/native/lead", "XDG_CONFIG_HOME": "/native/config",
                                     "CODEX_HOME": "/native/codex", "CLAUDE_CONFIG_DIR": "/native/claude"}):
            original = dict(os.environ)
            result = teams.backend_environment({"path": "/private/tools", "catalog_environment": self.environment},
                                               self.root, "teams")
            self.assertEqual(result["HOME"], str(self.home))
            self.assertEqual(result["XDG_CONFIG_HOME"], str(self.home / "config"))
            self.assertEqual(result["PATH"], "/private/tools")
            self.assertEqual(result["CODEX_HOME"], "/native/codex")
            self.assertEqual(result["CLAUDE_CONFIG_DIR"], "/native/claude")
            self.assertEqual(dict(os.environ), original)
        for selected in (None, {"HOME": "/native/lead"}, teams.catalog_environment(self.root / "unowned")):
            with self.subTest(selected=selected), self.assertRaises(ValueError):
                teams.backend_environment({"path": "/private/tools", "catalog_environment": selected}, self.root, "teams")
        with self.assertRaises(ValueError):
            teams.catalog_environment(Path("relative"))

    def test_composite_discovery_requires_both_owned_frontdoors_with_exact_workspace(self):
        fixture = self.root / "fixture"
        fixture.mkdir()
        path = fixture / "agent-calls.jsonl"
        case = {"team_scenario": "default", "external_optional": True}
        cwd = str(self.root / "workspace")
        default = {"argv": ["teams", "default", "show", "--cwd", cwd, "--json"], "error": None}
        listed = {"argv": ["teams", "list", "--cwd=" + cwd, "--json"], "error": None}
        def write(rows):
            path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        write([default, listed])
        cli.require_composite_discovery(self.root, case)
        alias = self.root / "owned-parent-alias"
        alias.symlink_to(self.root, target_is_directory=True)
        aliased = {"argv": ["teams", "list", "--cwd", str(alias / "workspace"), "--json"], "error": None}
        write([default, aliased])
        cli.require_composite_discovery(self.root, case)
        alias.unlink()
        with self.assertRaises(cli.TeamDiscoveryFrontdoorMissing):
            cli.require_composite_discovery(self.root, case)
        for rows in ([], [{"argv": ["--version"], "error": None}], [default],
                     [default, {**listed, "error": "refused"}],
                     [default, {"argv": listed["argv"]}],
                     [default, {"argv": ["teams", "list", "--cwd", "workspace", "--json"], "error": None}],
                     [default, {"argv": ["teams", "list", "--cwd", "/other", "--json"], "error": None}],
                     [default, {"argv": ["teams", "list", "--cwd", cwd, "--cwd", "/other"], "error": None}]):
            with self.subTest(rows=rows):
                write(rows)
                with self.assertRaises(cli.TeamDiscoveryFrontdoorMissing):
                    cli.require_composite_discovery(self.root, case)
        path.write_text("not-json\n")
        with self.assertRaises(cli.TeamDiscoveryFrontdoorMissing):
            cli.require_composite_discovery(self.root, case)
        path.unlink()
        with self.assertRaises(cli.TeamDiscoveryFrontdoorMissing):
            cli.require_composite_discovery(self.root, case)
        cli.require_composite_discovery(self.root, {"team_scenario": "default", "external": False})

    def test_composite_capture_blocks_empty_shim_log_and_summary_rechecks_retained_calls(self):
        selected = self.root / "delegate"
        selected.write_text("#!/bin/sh\nexit 0\n")
        selected.chmod(0o700)
        metadata = self.root / "metadata.txt"
        metadata.write_text("controlled metadata")
        case = {"name": "composite", "prompt": "Research and plan the correction.", "external": False,
                "team_scenario": "default", "external_optional": True}
        with patch.object(cli, "_candidate_shadow_reason", return_value=None), \
                patch.object(cli.shutil, "which", return_value="/mock/tool"), \
                patch.object(cli, "_prepare_routing_fixture"), patch.object(cli, "AttemptSupervisor") as supervisor:
            owner = supervisor.return_value
            owner.processes.environment = {}
            owner.capture.return_value = metadata
            owner.execute.return_value = {"result": "Success"}
            receipt = cli.run_case(output=self.root, harness="codex", model=None, case=case, repetition=1,
                                   mode="routing", delegate_bin=selected)
            self.assertEqual((receipt["status"], receipt["reason"]), ("blocked", "team_discovery_frontdoor_missing"))
            self.assertEqual(receipt["guidance"], "Both Teams default show and list must use the fixture-controlled Delegate command.")
            owner.close.assert_called_once()

        retained_case = {**case, "name": "retained"}
        run = self.root / "retained-1"
        (run / "fixture").mkdir(parents=True)
        log = run / "fixture/agent-calls.jsonl"
        log.write_text("")
        cli.write_json(run / "receipt.json", {
            "status": "unreviewed", "case": retained_case, "harness": "codex", "repetition": 1,
            "mode": "routing", "model_requested": None, "budget_seconds": 180,
            "candidate": cli.candidate_digests(), "team_scenario": "default",
        })
        cli.write_json(run / "judgment.json", {"status": "passed", "receipt_sha256": cli.digest(run / "receipt.json"),
                                                "evidence": "Controlled retained-log policy fixture."})
        with self.assertRaises(cli.TeamDiscoveryFrontdoorMissing):
            cli.summarize(output=self.root, cases=[retained_case], repetitions=1, harness="codex", mode="routing")
        log.write_text("".join(json.dumps({"argv": ["teams", *action, "--cwd", str(run / "workspace")], "error": None})
                               + "\n" for action in (["default", "show"], ["list"])))
        self.assertEqual(cli.summarize(output=self.root, cases=[retained_case], repetitions=1,
                                       harness="codex", mode="routing")["status"], "passed")

    def run_fixture(self, arguments, response, *, exit_code=0, isolated=True):
        runtime = {"delegate": "/candidate/delegate", "path": "/private/tools"}
        if isolated:
            runtime["catalog_environment"] = self.environment
        (self.root / "runtime.json").write_text(json.dumps(runtime))
        stdout, stderr = io.StringIO(), io.StringIO()
        result = BoundedCommandResult(exit_code, json.dumps(response).encode())
        with patch.dict(os.environ, {"DELEGATE_ROUTING_FIXTURE": str(self.root)}), \
                patch("sys.argv", ["delegate", *arguments]), \
                patch.object(delegate_fixture, "run_bounded_command", return_value=result) as run, \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr), \
                self.assertRaises(SystemExit) as stopped:
            delegate_fixture.main()
        return stopped.exception.code, stdout.getvalue(), stderr.getvalue(), run

    def test_real_catalog_pages_context_changes_and_failures_are_not_rewritten(self):
        # The backend owns these contracts. A routing fixture must preserve faults,
        # cursor/context identity and deleted candidates rather than invent a Team.
        reference = {"scope": "project", "team_id": "general",
                     "project": {"kind": "git", "path": str(self.root / "workspace/.git")}}
        common = {"schema_version": 1, "command": "teams", "action": "list"}
        observed = [
            (0, dict(common, status="ok", catalog_id="first",
                     teams=[{"reference": reference, "revision": 1, "name": "general",
                             "purpose": "Engineering work allocation", "role_count": 5, "member_count": 5}],
                     next_cursor="project:general")),
            (0, dict(common, status="empty", catalog_id="second", teams=[])),
            (0, dict(common, status="partial", catalog_id="first", teams=[],
                     diagnostics=[{"code": "project_context", "message": "Only user Teams are listed; project context failed"}])),
            (1, dict(common, status="failed", failure={"code": "catalog_io", "next_action": "inspect_before_retry"},
                     error="team catalog unavailable: file is not a database")),
            (2, {"schema_version": 1, "command": "teams", "action": "show", "status": "failed",
                 "failure": {"code": "not_found", "next_action": "correct_request"}, "error": "team not found"}),
        ]
        for expected_code, response in observed:
            with self.subTest(response=response):
                arguments = ["--root", str(self.root / "state"), "teams", "list", "--cwd", str(self.root),
                             "--after", "cursor", "--limit", "1", "--json"]
                code, output, error, run = self.run_fixture(arguments, response, exit_code=expected_code)
                self.assertEqual((code, json.loads(output), error), (expected_code, response, ""))
                self.assertEqual(run.call_args.args[0], ["/candidate/delegate", *arguments])
                self.assertEqual(run.call_args.args[1]["HOME"], str(self.home))
        records = [json.loads(line) for line in (self.root / "calls.jsonl").read_text().splitlines()]
        self.assertEqual(len(records), len(observed))
        self.assertTrue(all(record == arguments for record in records))

    def test_unisolated_team_read_fails_before_real_cli(self):
        code, output, error, run = self.run_fixture(["teams", "list", "--json"], {}, isolated=False)
        self.assertEqual((code, output), (2, ""))
        self.assertIn("isolated backend catalog", error)
        run.assert_not_called()

    def test_seed_uses_production_mutations_and_permission_free_definitions(self):
        self.home.mkdir()
        commands = teams.seed_commands("better-alternative", self.home, self.root)
        self.assertEqual([arguments[:2] for arguments in commands],
                         [["teams", "register"], ["teams", "register"], ["teams", "default"]])
        general = json.loads((self.home / "general.json").read_text())
        specialist = json.loads((self.home / "python-specialists.json").read_text())
        self.assertEqual({role["id"] for role in general["roles"]},
                         {"research", "planning", "implementation", "testing", "review"})
        self.assertEqual([role["id"] for role in specialist["roles"]], ["implementation"])
        self.assertNotEqual(general["purpose"], specialist["purpose"])
        for definition in (general, specialist):
            for member in definition["members"]:
                self.assertEqual(set(member["execution_defaults"]), {"provider", "model", "effort"})
        self.assertEqual(teams.seed_commands("empty", self.home, self.root), [])
        with self.assertRaises(ValueError):
            teams.seed_commands("unknown", self.home, self.root)

    def test_team_cases_are_routing_only_and_assignment_prompts_are_destination_unprimed(self):
        cases = [case for case in cli.read_json(cli.CASES)["cases"] if "team_scenario" in case]
        intents = [case for case in cases if case["name"] in {"teams-" + role for role in
                   ("research", "planning", "implementation", "testing", "review")}]
        self.assertEqual(len(intents), 5)
        for case in intents:
            self.assertNotIn("team", case["prompt"].lower())
            for destination in ("codex", "claude", "delegate"):
                self.assertNotIn(destination, case["prompt"].lower())
        self.assertEqual({case["team_scenario"] for case in cases}, teams.SCENARIOS)
        with patch.object(cli, "prepare_workspace") as prepare, \
                self.assertRaisesRegex(ValueError, "isolated routing backend"):
            cli.run_case(output=self.root, harness="codex", model=None, case=intents[0], repetition=1, mode="live")
        prepare.assert_not_called()
        self.assertFalse((self.root / "teams-research-1/workspace").exists())

    def test_composite_cases_require_discovery_without_prescribing_worker_allocation(self):
        cases = [case for case in cli.read_json(cli.CASES)["cases"]
                 if case["name"].startswith("teams-composite-")]
        self.assertEqual({case["name"] for case in cases},
                         {"teams-composite-code-test-review", "teams-composite-research-plan"})
        for case in cases:
            self.assertIs(case["external"], False)
            self.assertIs(case["external_optional"], True)
            self.assertIs(case["require_candidate_read"], True)
            self.assertEqual(case["team_scenario"], "default")
            for word in ("worker", "agent", "team", "provider", "codex", "claude", "delegate"):
                self.assertNotIn(word, case["prompt"].lower())

    def test_live_suite_omits_team_cases_and_explicit_selection_fails_before_output(self):
        output = self.root / "not-created"
        arguments = ["routing", "--harness", "codex", "--mode", "live", "--output", str(output), "--summarize"]
        with patch("sys.argv", arguments), \
                patch.object(cli, "summarize", return_value={"status": "passed"}) as summarize, \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(), 0)
        selected = summarize.call_args.kwargs["cases"]
        self.assertTrue(selected)
        self.assertFalse(any("team_scenario" in case for case in selected))
        with patch("sys.argv", arguments + ["--case", "teams-review"]), \
                patch.object(cli, "summarize") as summarize, \
                contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as stopped:
            cli.main()
        self.assertEqual(stopped.exception.code, 2)
        summarize.assert_not_called()
        self.assertFalse(output.exists())

    def test_codex_shadow_checks_both_native_skill_locations_without_mutation(self):
        with patch.object(cli.Path, "home", return_value=self.root), \
                patch.dict(os.environ, {"CODEX_HOME": str(self.root / "custom-codex")}):
            self.assertIsNone(cli._candidate_shadow_reason("codex"))
            for location in (self.root / "custom-codex/skills", self.root / ".agents/skills"):
                skill = location / "agent-integration/SKILL.md"
                skill.parent.mkdir(parents=True)
                skill.write_text("older installed guidance")
                self.assertEqual(cli._candidate_shadow_reason("codex"), "codex_candidate_shadowed_by_personal_skill")
                self.assertEqual(skill.read_text(), "older installed guidance")
                skill.unlink()
