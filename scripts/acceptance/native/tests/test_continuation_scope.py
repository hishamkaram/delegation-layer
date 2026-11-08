"""Native continuations launch under their caller's own scope and prove it from records."""

from __future__ import annotations

from argparse import Namespace
import hashlib
import json
import os
import operator
from typing import NotRequired, TypedDict
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import MagicMock, patch
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.directories import operations as directories_operations

from scripts.acceptance.native import cli as entry
from scripts.acceptance.native.tests.support import adjacent_runner_bundle
from scripts.acceptance.native.tests.support import fixture_record
from scripts.acceptance.native.tests.support import invoke_with_invalid_arguments
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.directories.operations import rollback_owned_directories
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.native import workflow as gate
from scripts.acceptance.native import dispatch as acceptance_dispatch
from scripts.acceptance.native import scenarios as scenarios_impl
from scripts.acceptance.native import continuation_scope as scope
from scripts.acceptance.native.errors import BlockedFailure
from scripts.acceptance.native_evidence import supplemental as supplemental
from scripts.acceptance.native_evidence.contract import EvidenceError


FRESH, RESUME, MISS, ROOT = "1" * 32, "2" * 32, "3" * 32, "f" * 32
SESSION = "0d7c6e2a-4a8f-4f3e-9b55-2f8b3a2f7c10"
AMBIENT = {"HOME": "/home/fixture", "PATH": "/usr/bin", "TMPDIR": "/ambient-tmp", "OPENAI_API_KEY": "secret"}


class RecordedTaskOverrides(TypedDict):
    tmpdir: Path
    roots: NotRequired[list[str]]
    model: NotRequired[str | None]
    executable: NotRequired[str]
    recorded: NotRequired[bool]


class SessionMissOptions(TypedDict, total=False):
    verdict: str
    stderr: str | None
    exit_code: int
    prior: str
    reference: str | None
    session_directory: Path | None


class EnvironmentOptions(TypedDict, total=False):
    observed: dict[str, object] | None
    aggregate: bool
    candidate: supplemental.Candidate | Exception | None


class LaunchScopeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-launch-scope-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve(strict=True)

    def scopes(self, *, pi_sessions: bool = False) -> scope.LaunchScopes:
        owner = DirectoryOwner()
        scopes = scope.create_launch_scopes(owner, self.base, AMBIENT, pi_sessions=pi_sessions)
        self.addCleanup(rollback_owned_directories, owner.transfer())
        return scopes

    def test_each_scope_is_a_full_copy_that_changes_only_its_owned_tmpdir(self):
        scopes = self.scopes()
        environments = {name: scopes.environment(name) for name in scope.TMPDIR_MARKERS}
        for name, environment in environments.items():
            self.assertEqual({key: value for key, value in environment.items() if key != "TMPDIR"},
                             {key: value for key, value in AMBIENT.items() if key != "TMPDIR"})
            self.assertEqual(environment["TMPDIR"], str(self.base / scope.TMPDIR_MARKERS[name]))
            self.assertTrue(Path(environment["TMPDIR"]).is_dir())
        self.assertEqual(set(environments), {"predecessor", "successor"})
        self.assertEqual(len({environment["TMPDIR"] for environment in environments.values()} |
                             {AMBIENT["TMPDIR"]}), 3)
        environments["successor"]["HOME"] = "/changed"
        self.assertEqual(scopes.environment("successor")["HOME"], AMBIENT["HOME"])
        with self.assertRaises(TypeError):
            invoke_with_invalid_arguments(operator.setitem, (scopes.ambient, "HOME", "/changed"), {})
        self.assertIsNone(scopes.pi_sessions)

    def test_pi_session_directory_is_an_owned_marker_only(self):
        scopes = self.scopes(pi_sessions=True)
        sessions = scopes.pi_sessions
        if sessions is None:
            self.fail("the Pi launch scope must own both session directories")
        control, empty = sessions
        self.assertTrue(control.is_dir() and empty.is_dir() and control != empty)
        self.assertEqual(scopes.environment("successor", pi_session=empty)[scope.PI_SESSION_DIRECTORY_KEY],
                         str(empty))
        with self.assertRaisesRegex(AcceptanceFailure, "not owned"):
            scopes.environment("successor", pi_session=self.base)
        for unscoped in ("replay", "observer"):
            with self.assertRaisesRegex(AcceptanceFailure, "unknown native launch scope"):
                scopes.environment(unscoped)


class RecordedScopeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-recorded-scope-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        markers = {name: self.root / marker for name, marker in scope.TMPDIR_MARKERS.items()}
        for marker in markers.values():
            marker.mkdir()
        self.owner = SimpleNamespace(
            state=self.root / "state",
            launch_scopes=scope.LaunchScopes(AMBIENT, markers, None))
        self.markers = markers

    def write_task(self, task_id: str, tmpdir: Path, *, roots: list[str] | None = None,
                   model: str | None = "model-a", executable: str = "/usr/bin/provider",
                   recorded: bool = True) -> None:
        directory = self.owner.state / "tasks" / task_id
        directory.mkdir(parents=True, exist_ok=True)
        meta = {"environment": ["HOME=/home/fixture", "PATH=/usr/bin", "TMPDIR=" + str(tmpdir)],
                "effective_config": {"policy": {"writable_roots": [str(tmpdir)] if roots is None else roots}},
                "provider_executable": executable}
        if recorded:
            meta["environment_recorded"] = True
        (directory / "meta.json").write_text(json.dumps(meta))
        (directory / "task.json").write_text(json.dumps({"requested_config": {"model": model, "effort": None}}))

    def test_successor_recorded_in_its_own_scope_is_proven(self):
        self.write_task(FRESH, self.markers["predecessor"])
        self.write_task(RESUME, self.markers["successor"])
        facts = scope.require_continuation_scope(self.owner, FRESH, RESUME)
        self.assertEqual(facts["writable_roots"], "own-tmpdir")
        self.assertEqual(facts["tmpdir_scopes"], {"fresh": "predecessor", "resume": "successor", "replay": "ambient"})
        self.assertTrue(facts["selection_inherited"])
        self.assertNotIn("/home/fixture", json.dumps(facts))
        self.assertEqual(fixture_record(facts["meta_sha256"])["resume"], hashlib.sha256(
            (self.owner.state / "tasks" / RESUME / "meta.json").read_bytes()).hexdigest())

    def test_shared_ambient_roots_beside_each_own_tmpdir_are_accepted(self):
        for task_id, own in ((FRESH, "predecessor"), (RESUME, "successor")):
            self.write_task(task_id, self.markers[own],
                            roots=["/tmp", "/var/tmp", AMBIENT["TMPDIR"], str(self.markers[own] / "work")])
        self.assertEqual(scope.require_continuation_scope(self.owner, FRESH, RESUME)["writable_roots"],
                         "own-tmpdir")

    def test_roots_missing_any_own_tmpdir_are_refused(self):
        for label, (fresh_roots, resume_roots) in {
                "both missing": (["/tmp"], ["/tmp"]),
                "predecessor missing": (["/tmp"], None),
                "successor missing": (None, ["/tmp"])}.items():
            with self.subTest(label):
                self.write_task(FRESH, self.markers["predecessor"], roots=fresh_roots)
                self.write_task(RESUME, self.markers["successor"], roots=resume_roots)
                with self.assertRaisesRegex(AcceptanceFailure, "writable roots from its own TMPDIR"):
                    scope.require_continuation_scope(self.owner, FRESH, RESUME)

    def test_successor_that_kept_its_predecessors_scope_is_refused(self):
        refusals: dict[str, tuple[RecordedTaskOverrides, str]] = {
            "copied predecessor TMPDIR": (
                {'tmpdir': self.markers["predecessor"]}, "caller's launch TMPDIR"),
            "ambient TMPDIR": ({'tmpdir': Path(AMBIENT["TMPDIR"])}, "caller's launch TMPDIR"),
            "predecessor roots": (
                {'tmpdir': self.markers["successor"], 'roots': [str(self.markers["successor"]), str(self.markers["predecessor"] / "x")]},
                "another scope's TMPDIR"),
            "own roots dropped": ({'tmpdir': self.markers["successor"], 'roots': ["/tmp"]},
                                  "writable roots from its own TMPDIR"),
            "model override": ({'tmpdir': self.markers["successor"], 'model': "model-b"}, "model or effort"),
            "executable change": ({'tmpdir': self.markers["successor"], 'executable': "/other/provider"},
                                  "provider executable"),
            "legacy record": ({'tmpdir': self.markers["successor"], 'recorded': False},
                              "no recorded launch environment"),
        }
        for label, (successor, message) in refusals.items():
            with self.subTest(label):
                self.write_task(FRESH, self.markers["predecessor"])
                self.write_task(RESUME, **successor)
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    scope.require_continuation_scope(self.owner, FRESH, RESUME)


class SuccessorAdmissionTests(unittest.TestCase):
    def owner(self, root: Path, command: str) -> tuple[
            SimpleNamespace, Path, list[tuple[list[object], dict[str, str] | None]]]:
        brief = root / "brief.txt"
        brief.write_text("brief\n")
        calls: list[tuple[list[object], dict[str, str] | None]] = []

        def continue_task(name, argv, *, request=None, environment=None):
            calls.append((argv, environment))
            return RESUME, {"admission": "admitted", "command": "continue", "task_id": RESUME,
                            "parent_task_id": FRESH, "root_id": ROOT,
                            "supervisor": {"matched": True, "numeric_task_id": 2}}

        def dispatch(name, task, argv, *, request=None, environment=None):
            calls.append((argv, environment))
            return {"admission": "admitted", "task_id": task, "root_id": ROOT,
                    "supervisor": {"matched": True, "numeric_task_id": 2}}

        owner = SimpleNamespace(
            delegate=Path("/bin/delegate"), state=root / "state", config=root / "pueue.yml",
            runner=Path("/bin/delegate-run"), workspace=root, provider="claude:print", mode="read-only",
            scenario="read-only", args=Namespace(model="model-a", effort="high"),
            root_id=ROOT, numeric_ids={}, tasks={}, continuation_command=command,
            ops=SimpleNamespace(managed_runner=True, root_id=None, continue_task=continue_task,
                                dispatch=dispatch, dispatch_observations=[]))
        owner.task_command_prefix = lambda: acceptance_dispatch.task_command_prefix(owner)
        owner.dispatch_argv = lambda *values: acceptance_dispatch.dispatch_argv(owner, *values)
        owner.dispatch = lambda *values, **options: acceptance_dispatch.dispatch(owner, *values, **options)
        owner.continue_task = lambda *values, **options: acceptance_dispatch.continue_task(
            owner, *values, **options)
        owner.continue_public = lambda *values, **options: acceptance_dispatch.continue_public(
            owner, *values, **options)
        return owner, brief, calls

    def test_public_continue_passes_the_caller_scope_and_no_selection_override(self):
        with tempfile.TemporaryDirectory(prefix="native-successor-admission-") as directory:
            owner, brief, calls = self.owner(Path(directory), "continue")
            environment = {"TMPDIR": "/owned-b"}
            self.assertEqual(scope.admit_successor(owner, "resume", brief, FRESH, environment), RESUME)
            argv, passed = calls[-1]
            self.assertEqual(passed, environment)
            self.assertEqual(argv[argv.index("continue"):argv.index("continue") + 3], ["continue", "--task", FRESH])
            self.assertFalse({"--provider", "--model", "--effort", "--permission", "--resume-task"} & set(argv))

    def test_default_command_keeps_dispatch_resume_under_the_caller_scope(self):
        with tempfile.TemporaryDirectory(prefix="native-successor-admission-") as directory:
            owner, brief, calls = self.owner(Path(directory), "dispatch")
            environment = {"TMPDIR": "/owned-b"}
            scope.admit_successor(owner, "resume", brief, FRESH, environment)
            argv, passed = calls[-1]
            self.assertEqual(passed, environment)
            self.assertEqual(argv[argv.index("--resume-task") + 1], FRESH)
            self.assertNotIn("continue", argv)


class ContinuationCommandSelectionTests(unittest.TestCase):
    def test_only_plain_resumable_cases_select_public_continue(self):
        self.assertEqual(gate._continuation_command("claude:print", "write", None), "dispatch")
        self.assertEqual(gate._continuation_command("claude:print", "read-only", "continue"), "continue")
        self.assertEqual(gate._continuation_command("pi:json", scope_scenario(), None), "continue")
        for provider, scenario, requested in (
                ("codex:exec", "timeout-resume", "continue"), ("claude:print", "interactive", "continue"),
                ("codex:exec", "non-git", "continue"), ("codex:review", "read-only", "continue"),
                ("pi:json", scope_scenario(), "dispatch")):
            with self.subTest(provider=provider, scenario=scenario):
                with self.assertRaises(AcceptanceFailure):
                    gate._continuation_command(provider, scenario, requested)

    def test_pi_session_miss_is_only_a_pi_read_only_case(self):
        self.assertEqual(gate._scenario("pi:json", "read-only", scope_scenario()), scope_scenario())
        for provider, mode in (("claude:print", "read-only"), ("pi:json", "workspace-write")):
            with self.subTest(provider=provider, mode=mode), \
                    self.assertRaisesRegex(AcceptanceFailure, "requires pi:json read-only"):
                gate._scenario(provider, mode, scope_scenario())

    def test_entry_exposes_the_contributor_selector_with_dispatch_by_default(self):
        parser = entry.build_parser()
        self.assertIsNone(parser.parse_args(["--provider", "pi:json"]).continuation_command)
        self.assertEqual(parser.parse_args(["--provider", "pi:json", "--continuation-command", "continue"])
                         .continuation_command, "continue")
        with self.assertRaises(SystemExit):
            parser.parse_args(["--provider", "pi:json", "--continuation-command", "resume"])


def scope_scenario() -> str:
    return scenarios_impl.PI_SESSION_MISS_SCENARIO


class ResumableScenarioScopeTests(unittest.TestCase):
    def setUp(self):
        for helper, value in (("workspace_snapshot", {}), ("task_digest_snapshot", {}),
                              ("require_same_session", None), ("require_replay_unchanged", None)):
            patcher = patch.object(scenarios_impl, helper, return_value=value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def owner(self, command: str) -> MagicMock:
        owner = MagicMock()
        owner.args = Namespace(model=None, effort=None)
        owner.provider, owner.mode, owner.scenario = "claude:print", "read-only", "read-only"
        owner.continuation_command = command
        owner.launch_environment.side_effect = lambda name, **_: {"TMPDIR": name}
        owner.dispatch.return_value = FRESH
        owner.continue_task.return_value = RESUME
        owner.continue_public.return_value = RESUME
        owner.require_continuation_scope.return_value = {"selection_inherited": True}
        owner.tasks = {"fresh": FRESH, "resume": RESUME}
        owner.lifecycle_receipt.return_value = {}
        owner.provider_runtime_receipt.return_value = {}
        owner.production_acp_receipt.return_value = {}
        owner.harness_observation = {"harness_revision": "a" * 40, "harness_sha256": "7" * 64,
                                     "committed": True}
        owner.canonical_aggregate_sha256 = "c" * 64
        return owner

    def test_each_turn_is_admitted_in_its_own_scope_and_proven_before_replay(self):
        for command, admission in (("continue", "continue_public"), ("dispatch", "continue_task")):
            with self.subTest(command=command):
                owner = self.owner(command)
                scenarios_impl._run_resumable(owner)
                self.assertEqual(owner.dispatch.call_args.kwargs["environment"], {"TMPDIR": "predecessor"})
                successor = getattr(owner, admission)
                self.assertEqual(successor.call_args.kwargs["environment"], {"TMPDIR": "successor"})
                self.assertEqual(successor.call_args.args[2], FRESH)
                names = [call[0] for call in owner.mock_calls]
                self.assertLess(names.index("require_continuation_scope"), names.index("replay"))
                owner.require_continuation_scope.assert_called_once_with(FRESH, RESUME)
                self.assertEqual(owner.success_receipt["continuation_command"], command)
                self.assertEqual(owner.success_receipt["continuation_scope"], {"selection_inherited": True})
                if command == "continue":
                    self.assertEqual(owner.success_receipt["harness"], owner.harness_observation)
                    self.assertEqual(owner.success_receipt["canonical_aggregate_sha256"], "c" * 64)
                else:
                    self.assertNotIn("harness", owner.success_receipt)
                    self.assertNotIn("canonical_aggregate_sha256", owner.success_receipt)


class PiSessionMissTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-pi-session-miss-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.empty = self.root / "s2"
        self.empty.mkdir()
        self.directory = self.root / "state" / "tasks" / MISS
        (self.directory / "raw").mkdir(parents=True)

    def owner(self, *, verdict: str = "rejected", stderr: str | None = None, exit_code: int = 1,
              prior: str = SESSION, reference: str | None = None,
              session_directory: Path | None = None) -> SimpleNamespace:
        stderr_bytes = (stderr if stderr is not None
                        else f"\x1b[31mNo session found matching '{SESSION}'\x1b[39m\n").encode()
        (self.directory / "raw" / "stderr").write_bytes(stderr_bytes)
        outcome = {"verdict": verdict, "evidence_sha256": "e" * 64,
                   "payload": {"basename": "publish.reject", "length": 0,
                               "sha256": hashlib.sha256(b"").hexdigest()}}
        (self.directory / "outcome.json").write_text(json.dumps(outcome))
        (self.directory / "publish.reject").write_bytes(b"")
        (self.directory / "provider.exit").write_text(json.dumps({
            "root_id": ROOT, "task_id": MISS, "invocation_state": "started", "exit_code": exit_code,
            "manifest_sha256": "e" * 64, "raw_manifest": [
                {"path": "raw/stderr", "size": len(stderr_bytes),
                 "sha256": hashlib.sha256(stderr_bytes).hexdigest()}]}))
        if reference is not None:
            (self.directory / "provider.ref.json").write_text(json.dumps({"conversation_id": reference}))
        write_pi_meta(self.directory, self.empty if session_directory is None else session_directory)
        record = {"prior_session": {"predecessor_task_id": RESUME, "conversation_id": prior}}
        terminal = []
        return SimpleNamespace(
            state=self.root / "state", root_id=ROOT, terminal=terminal,
            wait_done=lambda *_args, **_kwargs: {"status": {"Done": {"result": "Success"}}},
            _request_binding=lambda *_args, **_kwargs: (self.directory, record, {}, "", "", None),
            ops=SimpleNamespace(root_id=None, expect_terminal_result=lambda *values: terminal.append(values),
                                collect=lambda *_args, **_kwargs: {"outcome": outcome}))

    def miss(self, owner):
        return scope.require_session_miss(owner, MISS, RESUME, SESSION, self.empty)

    def test_native_session_not_found_refusal_is_the_expected_miss(self):
        owner = self.owner(reference=SESSION)
        facts = self.miss(owner)
        self.assertEqual((facts["publication"], facts["session_lookup"], facts["fresh_session_created"]),
                         ("rejected", "not-found", False))
        self.assertEqual(owner.terminal, [("session-miss", "Success")])

    def test_other_outcomes_never_count_as_a_session_miss(self):
        cases: dict[str, tuple[SessionMissOptions, str]] = {
            "unexpected resume": ({'verdict': "committed"}, "was not rejected"),
            "transport failure": ({'stderr': "connect ECONNREFUSED\n"}, "native session-not-found"),
            "other session": ({'stderr': "No session found matching 'another'\n"}, "native session-not-found"),
            "provider crash": ({'exit_code': 134}, "sealed native exit"),
            "wrong predecessor": ({'prior': "another-session"}, "exact predecessor session"),
            "substituted session": ({'reference': "fresh-session"}, "substituted another provider session"),
        }
        for label, (values, message) in cases.items():
            with self.subTest(label):
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    self.miss(self.owner(**values))

    def test_miss_launched_under_another_session_directory_is_refused(self):
        control = self.root / "s1"
        control.mkdir()
        with self.assertRaisesRegex(AcceptanceFailure, "did not record its owned session directory"):
            self.miss(self.owner(session_directory=control))

    def test_unsealed_stderr_is_refused(self):
        owner = self.owner()
        (self.directory / "raw" / "stderr").write_bytes(b"No session found matching 'tampered'\n")
        with self.assertRaisesRegex(AcceptanceFailure, "no sealed bounded stderr"):
            self.miss(owner)

    def test_miss_is_refused_unless_the_empty_directory_is_observed_empty(self):
        outside = self.root / "outside"
        outside.mkdir()
        (outside / "session.jsonl").write_text("{}")
        replacement = self.root / "replacement"
        replacement.mkdir()

        def file_entry():
            (self.empty / "session.jsonl").write_text("{}")

        def child_symlink():
            (self.empty / "link").symlink_to(outside, target_is_directory=True)

        def replaced_by_symlink():
            self.empty.rmdir()
            self.empty.symlink_to(replacement, target_is_directory=True)

        for label, change in {"file entry": file_entry, "child symlink": child_symlink,
                              "replaced by symlink": replaced_by_symlink,
                              "missing": lambda: self.empty.rmdir()}.items():
            with self.subTest(label):
                # Each case owns a new empty directory under the owned temporary root.
                self.empty = self.root / ("s2-" + label.replace(" ", "-"))
                self.empty.mkdir()
                owner = self.owner()
                change()
                with self.assertRaisesRegex(AcceptanceFailure, "empty session directory"):
                    self.miss(owner)
                self.assertEqual(sorted(path.name for path in outside.iterdir()), ["session.jsonl"])

    def test_unobservable_empty_directory_fails_closed(self):
        owner = self.owner()
        with patch.object(scope.os, "scandir", side_effect=PermissionError("denied")), \
                self.assertRaisesRegex(AcceptanceFailure, "empty session directory could not be observed"):
            self.miss(owner)

    def test_authentication_refusal_is_blocked_not_a_miss(self):
        owner = self.owner()
        with patch.object(scope, "authentication_unavailable", return_value=True), \
                self.assertRaises(BlockedFailure):
            self.miss(owner)

    def test_launch_must_record_and_be_granted_its_owned_session_directory(self):
        control = self.root / "s1"
        control.mkdir()
        directory = self.root / "state" / "tasks" / FRESH
        directory.mkdir(parents=True)
        owner = SimpleNamespace(state=self.root / "state")
        write_pi_meta(directory, control)
        scope.require_session_directory(owner, FRESH, control)
        for label, (recorded, roots, message) in {
                "other selector": (self.empty, [str(control)], "did not record"),
                "no selector": (None, [str(control)], "did not record"),
                "not granted": (control, ["/tmp"], "was not granted")}.items():
            with self.subTest(label):
                write_pi_meta(directory, recorded, roots=roots)
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    scope.require_session_directory(owner, FRESH, control)


def write_pi_meta(directory: Path, session_directory: Path | None, *,
                  roots: list[str] | None = None) -> None:
    """Record one Pi launch with its session-directory selector and granted roots."""
    environment = ["HOME=/home/fixture", "TMPDIR=/owned-b"]
    if session_directory is not None:
        environment.append(scope.PI_SESSION_DIRECTORY_KEY + "=" + os.path.realpath(session_directory))
    granted = [os.path.realpath(session_directory)] if roots is None and session_directory else roots or []
    (directory / "meta.json").write_text(json.dumps({
        "environment_recorded": True, "environment": environment,
        "effective_config": {"policy": {"writable_roots": granted}}}))


class PiSessionMissScenarioTests(unittest.TestCase):
    def owner(self, launches: int) -> MagicMock:
        owner = MagicMock()
        owner.args = Namespace(model=None, effort=None)
        owner.provider, owner.mode = "pi:json", "read-only"
        owner.scenario, owner.continuation_command = scope_scenario(), "continue"
        owner.launch_scopes.pi_sessions = (Path("/owned/s1"), Path("/owned/s2"))
        owner.launch_environment.side_effect = lambda name, *, pi_session: {"TMPDIR": name,
                                                                              "SESSION": str(pi_session)}
        owner.dispatch.return_value = FRESH
        owner.continue_public.side_effect = [RESUME, MISS]
        owner.session_ref.return_value = SESSION
        owner.success_receipt = None
        owner.provider_runtime_receipt.return_value = {}
        # Guardian launches count every guardian command, not provider turns.
        owner.lifetime_closure = {"ordinary": {"tasks": {MISS: {"launch_count": launches}}}}
        owner.lifecycle_receipt.return_value = {"natural_daemon_shutdown": True,
                                                "lifetime_closure": owner.lifetime_closure}
        owner.harness_observation = {"harness_revision": "a" * 40, "harness_sha256": "7" * 64,
                                     "committed": True}
        owner.canonical_aggregate_sha256 = "c" * 64
        return owner

    def run_scenario(self, owner: MagicMock) -> MagicMock:
        miss = MagicMock(return_value={"session_lookup": "not-found"})
        with patch.object(scenarios_impl, "workspace_snapshot", return_value={}), \
                patch.object(scenarios_impl, "task_digest_snapshot", return_value={}), \
                patch.object(scenarios_impl, "require_session_directory") as selected, \
                patch.object(scenarios_impl, "require_session_miss", miss):
            scenarios_impl.run_pi_session_miss(owner)
        self.assertEqual([call.args for call in selected.call_args_list],
                         [(owner, FRESH, Path("/owned/s1")), (owner, RESUME, Path("/owned/s1"))])
        return miss

    def test_control_turns_share_one_directory_and_the_miss_uses_the_empty_one(self):
        owner = self.owner(3)
        miss = self.run_scenario(owner)
        self.assertEqual(owner.dispatch.call_args.kwargs["environment"]["SESSION"], "/owned/s1")
        resume, missed = owner.continue_public.call_args_list
        self.assertEqual((resume.args[2], resume.kwargs["environment"]["SESSION"]), (FRESH, "/owned/s1"))
        self.assertEqual((missed.args[2], missed.kwargs["environment"]["SESSION"]), (RESUME, "/owned/s2"))
        miss.assert_called_once_with(owner, MISS, RESUME, SESSION, Path("/owned/s2"))
        receipt = owner.success_receipt
        self.assertEqual(receipt["gate"], "acceptance-native-pi-session-miss")
        self.assertEqual(receipt["tasks"], {"fresh": FRESH, "resume": RESUME, "session_miss": MISS})
        self.assertEqual(receipt["provider_launches"], 3)
        self.assertEqual(receipt["session_directories"],
                         {"fresh": "control", "resume": "control", "session_miss": "empty"})
        self.assertEqual(receipt["harness"], owner.harness_observation)
        self.assertEqual(receipt["canonical_aggregate_sha256"], "c" * 64)
        self.assertEqual(receipt["lifetime_closure"], owner.lifetime_closure)

    def test_guardian_launch_count_is_not_a_provider_turn_count(self):
        for launches in (1, 3, 4):
            with self.subTest(launches=launches):
                owner = self.owner(launches)
                self.run_scenario(owner)
                owner.shutdown.assert_called_once_with()
                self.assertEqual(owner.success_receipt["provider_launches"], 3)
                self.assertTrue(owner.success_receipt["natural_daemon_shutdown"])

    def test_unproven_lifetime_closure_writes_no_success_receipt(self):
        owner = self.owner(3)
        owner.shutdown.side_effect = AcceptanceFailure("managed lifetime closure is not proven: "
                                                       "lifetime_closure_incomplete")
        with self.assertRaisesRegex(AcceptanceFailure, "lifetime closure is not proven"):
            self.run_scenario(owner)
        self.assertIsNone(owner.success_receipt)


class AcceptanceEnvironmentOwnershipTests(unittest.TestCase):
    """The process owner keeps the ambient snapshot; continue cases bind their candidate first."""

    COMMITTED: dict[str, object] = {"harness_revision": "a" * 40, "harness_sha256": "7" * 64, "committed": True}
    AGGREGATE_SHA256 = "c" * 64

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-ambient-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.delegate, self.runner = adjacent_runner_bundle(self.root)
        self.home = self.root / "home"
        self.aggregate = self.root / "native-acceptance-aggregate.json"

    def candidate(self, **overrides) -> supplemental.Candidate:
        binaries = {"delegate": str(self.delegate.resolve()), "delegate-run": str(self.runner.resolve()),
                    "pueue": str(Path("/bin/sh").resolve()), "pueued": str(Path("/bin/sh").resolve()),
                    **overrides}
        return supplemental.Candidate(self.COMMITTED, self.AGGREGATE_SHA256, binaries, frozenset())

    def construct(self, command: str | None, *, observed: dict[str, object] | None = None,
                  aggregate: bool = True, candidate: supplemental.Candidate | Exception | None = None):
        output = self.root / f"output-{command}-{len(list(self.root.iterdir()))}"
        output.mkdir()
        argv = ["--provider", "claude:print", "--permission", "read-only", "--output", str(output),
                "--delegate", str(self.delegate), "--runner", str(self.runner), "--pueue", "/bin/sh",
                "--pueued", "/bin/sh", "--provider-executable", "/bin/sh"]
        if command is not None:
            argv.extend(["--continuation-command", command])
        if aggregate:
            argv.extend(["--canonical-aggregate", str(self.aggregate)])
        args = entry.build_parser().parse_args(argv)
        read_candidate = (
            patch.object(gate.supplemental, "candidate_from_aggregate", side_effect=candidate)
            if isinstance(candidate, Exception) else
            patch.object(gate.supplemental, "candidate_from_aggregate",
                         return_value=self.candidate() if candidate is None else candidate))
        with patch.object(gate.Path, "home", return_value=self.home), \
                patch.object(gate, "reject_tmp"), patch.object(gate, "require_discovery"), \
                patch.object(gate, "native_environment", return_value=dict(AMBIENT)), \
                patch.object(gate.supplemental, "observe_continuation_harness",
                             return_value=self.COMMITTED if observed is None else observed) as observe, \
                read_candidate as read:
            operation = gate.NativeAcceptance(args, managed_runner=False)
        self.addCleanup(directories_operations.rollback_owned_directories, operation.owned_directories)
        return operation, observe, read

    def test_processes_keep_the_ambient_snapshot_for_every_scenario(self):
        for command in (None, "dispatch", "continue"):
            with self.subTest(command=command):
                operation, observe, read = self.construct(command, aggregate=command == "continue")
                self.assertEqual(operation.processes.environment, AMBIENT)
                self.assertEqual(operation.environment, AMBIENT)
                if command == "continue":
                    self.assertEqual(operation.harness_observation, self.COMMITTED)
                    read.assert_called_once_with(self.aggregate, self.COMMITTED, self.delegate)
                    self.assertEqual(operation.canonical_aggregate_sha256, self.AGGREGATE_SHA256)
                else:
                    observe.assert_not_called()
                    read.assert_not_called()
                    self.assertIsNone(operation.harness_observation)
                    self.assertIsNone(operation.canonical_aggregate_sha256)

    def test_unbound_continue_case_is_refused_before_any_root_or_process(self):
        refusals: dict[str, tuple[EnvironmentOptions, str]] = {
            "no canonical aggregate": ({'aggregate': False}, "requires --canonical-aggregate"),
            "uncommitted harness": ({'observed': {**self.COMMITTED, "committed": False}}, "committed harness"),
            "unpassed aggregate": ({'candidate': EvidenceError("canonical_aggregate_not_passed")},
                                   "canonical_aggregate_not_passed"),
            "wrong revision": ({'candidate': EvidenceError("aggregate_source_revision_mismatch")},
                               "aggregate_source_revision_mismatch"),
            "missing aggregate": ({"candidate": FileNotFoundError(str(self.aggregate))}, "not bound"),
            "other runner": ({"candidate": self.candidate(**{"delegate-run": str(self.root / "foreign-runner")})}, "candidate_delegate-run_mismatch"),
            "other pueued": ({"candidate": self.candidate(pueued=str(self.root / "foreign-pueued"))}, "candidate_pueued_mismatch"),
        }
        for label, (options, message) in refusals.items():
            with self.subTest(label), patch.object(gate, "Processes") as processes:
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    self.construct("continue", **options)
                processes.assert_not_called()
                self.assertFalse(self.home.exists())

if __name__ == "__main__":
    unittest.main()
