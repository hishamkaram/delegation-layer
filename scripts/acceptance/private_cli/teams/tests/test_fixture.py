"""Exercise real fixture processes; this does not prove real CLI Team admission."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.private_cli.arguments import parse_arguments
from scripts.acceptance.private_cli.fixtures import install_pi_fixture
from scripts.acceptance.private_cli.teams.scenario import require_public_team


NONCE = "a" * 32
ARGUMENTS = ["--mode", "json", "--tools", "read,bash,edit,write",
             "--model", "openai/gpt-4.1", "--thinking", "low"]


class TeamWriteFixtureTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-team-write-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.provider = self.root / "provider"
        self.provider.mkdir()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        install_pi_fixture(self.provider, self.root / "gate", self.root / "waiters", NONCE)

    def run_fixture(self, arguments=ARGUMENTS, prompt="DELEGATE_TEAMS_WRITE:" + NONCE + "\n"):
        return subprocess.run([str(self.provider / "pi"), *arguments], cwd=self.workspace,
                              input=prompt, text=True, capture_output=True, timeout=10,
                              env={"PATH": "/usr/bin:/bin", "HOME": str(self.root)})

    def test_write_requires_actual_create_edit_command_and_accepted_stream(self):
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.workspace / ("team-write-" + NONCE + ".txt")).read_bytes(),
                         b"created:" + b"a" * 32 + b"\nedited:" + b"a" * 32 + b"\n")
        self.assertEqual((self.workspace / ("team-command-" + NONCE + ".txt")).read_bytes(),
                         b"command:" + b"a" * 32 + b"\n")
        events = [json.loads(line) for line in result.stdout.splitlines()]
        self.assertEqual([event["type"] for event in events], [
            "session", "agent_start", "turn_start", "message_start", "message_end", "turn_end", "agent_end",
        ])
        self.assertEqual(events[-1]["messages"][0]["content"],
                         [{"type": "text", "text": "team workspace-write accepted " + NONCE}])
        self.assertEqual(events[-1]["messages"][0]["stopReason"], "stop")

    def test_read_only_missing_duplicate_or_wrong_write_tools_never_create_files(self):
        cases = [
            ["--mode", "json", "--tools", "read,grep,find,ls", *ARGUMENTS[4:]],
            [*ARGUMENTS[:2], *ARGUMENTS[4:]],
            [*ARGUMENTS, "--tools", "read,bash,edit,write"],
            [*ARGUMENTS[:3]],
        ]
        for arguments in cases:
            with self.subTest(arguments=arguments):
                result = self.run_fixture(arguments=arguments)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list(self.workspace.iterdir()), [])
                self.assertEqual(result.stdout, "")

    def test_wrong_nonce_duplicate_marker_and_wrong_model_or_effort_never_create_files(self):
        for prompt in ("DELEGATE_TEAMS_WRITE:" + "b" * 32 + "\n", "DELEGATE_TEAMS_WRITE:" + NONCE + "\n"):
            arguments = ARGUMENTS if "b" * 32 in prompt else [*ARGUMENTS[:-1], "high"]
            result = self.run_fixture(arguments=arguments, prompt=prompt)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(self.workspace.iterdir()), [])
        for arguments, prompt in ((ARGUMENTS, ("DELEGATE_TEAMS_WRITE:" + NONCE + "\n") * 2),
                                  ([*ARGUMENTS[:5], "other/model", *ARGUMENTS[6:]], "DELEGATE_TEAMS_WRITE:" + NONCE)):
            result = self.run_fixture(arguments=arguments, prompt=prompt)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(self.workspace.iterdir()), [])

    def test_existing_artifact_is_not_overwritten(self):
        artifact = self.workspace / ("team-write-" + NONCE + ".txt")
        artifact.write_text("preserve existing artifact\n")
        result = self.run_fixture()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(artifact.read_text(), "preserve existing artifact\n")
        self.assertFalse((self.workspace / ("team-command-" + NONCE + ".txt")).exists())

    def test_existing_read_only_fixture_still_has_no_write_side_effect(self):
        result = self.run_fixture(arguments=["--mode", "json", "--tools", "read,grep,find,ls"],
                                  prompt="ordinary private acceptance\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(list(self.workspace.iterdir()), [])
        self.assertEqual(json.loads(result.stdout.splitlines()[-1])["type"], "agent_end")


class TeamScenarioArgumentsTests(unittest.TestCase):
    def test_public_task_views_cannot_drop_or_change_admitted_team_context(self):
        context = {"target": {"member_id": "writer"}, "team_revision": 1}
        for phase in ("dispatch", "status", "collect"):
            require_public_team({"task_id": "a" * 32, "team": context}, "a" * 32, context, phase)
            for response in ({"task_id": "a" * 32}, {"task_id": "b" * 32, "team": context},
                             {"task_id": "a" * 32, "team": {**context, "team_revision": 2}}):
                with self.subTest(phase=phase, response=response), self.assertRaisesRegex(RuntimeError, phase):
                    require_public_team(response, "a" * 32, context, phase)

    def test_only_one_runtime_task_in_the_current_package_is_accepted(self):
        base = ["acceptance-private-cli", "--pueue", "/pueue", "--pueued", "/pueued", "--output", "/receipt",
                "--scenario", "teams", "--parallel-tasks", "1", "--dispatch-workers", "1", "--runtime-only"]
        with patch.object(sys, "argv", base):
            args, workers, _parser = parse_arguments()
        self.assertEqual((args.scenario, args.parallel_tasks, workers), ("teams", 1, 1))
        for suffix in (["--parallel-tasks", "2"], ["--dispatch-workers", "2"],
                       ["--upgraded-source-root", "/other"]):
            with self.subTest(suffix=suffix), patch.object(sys, "argv", [*base, *suffix]), \
                    patch("sys.stderr"), self.assertRaises(SystemExit):
                parse_arguments()
        with patch.object(sys, "argv", base[:-1]), patch("sys.stderr"), self.assertRaises(SystemExit):
            parse_arguments()
