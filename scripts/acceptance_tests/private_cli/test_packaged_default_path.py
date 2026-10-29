"""Implementation methods for PackagedDefaultPathTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import json
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import packaged_defaults


class PackagedDefaultPathTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-packaged-default-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_command_construction_omits_every_global_path_override(self):
        brief = self.root / "brief.md"
        workspace = self.root / "workspace"
        task_id = "a" * 32

        commands = private_cli.packaged_default_commands(brief, workspace, task_id)

        self.assertEqual(commands["dispatch"], [
            "dispatch", "--provider", "pi:json", "--brief", str(brief),
            "--cwd", str(workspace), "--id", task_id, "--permission", "read-only",
            "--budget", private_cli.TASK_BUDGET, "--json",
        ])
        self.assertEqual(commands["status"], ["status", task_id, "--json"])
        self.assertEqual(commands["collect"], [
            "collect", task_id, "--watch", private_cli.COLLECT_WATCH, "--json",
        ])
        for command in commands.values():
            self.assertNotIn("--root", command)
            self.assertNotIn("--runner", command)
            self.assertNotIn("--pueue-config", command)

    def test_isolated_home_maps_to_each_supported_default_state_path(self):
        with patch.object(private_cli.platform, "system", return_value="Linux"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / ".config" / "delegation-layer",
            )
        with patch.object(private_cli.platform, "system", return_value="Darwin"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / "Library" / "Application Support" / "delegation-layer",
            )

    def test_adjacent_package_copies_exact_regular_binary_identities(self):
        sources = {}
        for index, name in enumerate(("delegate", "delegate-run", "pueue", "pueued")):
            source = self.root / ("source-" + name)
            source.write_bytes((name + "-%d" % index).encode())
            source.chmod(0o700)
            sources[name] = source
        owner = private_cli.DirectoryOwner()
        self.addCleanup(owner.release)

        install = private_cli.install_adjacent_package(
            owner, sources, self.root / "package",
        )

        for name, source in sources.items():
            copied = install / name
            self.assertTrue(copied.is_file())
            self.assertFalse(copied.is_symlink())
            self.assertNotEqual(copied.stat().st_ino, source.stat().st_ino)
            self.assertEqual(copied.read_bytes(), source.read_bytes())
            self.assertEqual(
                private_cli.file_fingerprint(copied)["sha256"],
                private_cli.file_fingerprint(source)["sha256"],
            )


ROOT_ID = "1" * 32
TASK_ID = "2" * 32


class InstalledDefaultCoreTests(unittest.TestCase):
    """The shared installed-default core binds every step to the one admitted task."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-installed-default-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        (self.root / "bin").mkdir()
        self.installed = packaged_defaults.InstalledDefaults(
            delegate=self.root / "bin" / "delegate", supervisor_directory=self.root / "bin",
            digests=tuple((name, "a" * 64) for name in packaged_defaults.INSTALLED_BINARIES),
            env={"HOME": str(self.root / "h"), "PATH": str(self.root / "bin")}, state=self.state,
            workspace=self.root / "w", brief=self.root / "brief.md", response_directory=self.root)
        self.admitted = packaged_defaults.AdmittedTask(
            root_id=ROOT_ID, task_id=TASK_ID,
            commands=packaged_defaults.packaged_default_commands(self.root / "brief.md", self.root / "w", TASK_ID),
            pair=self.root / "pair")
        self.ledger = []

    def closure(self, tasks):
        return {"root_id": ROOT_ID, "tasks": tasks, "inspections": {}}

    def test_only_the_dispatched_tasks_own_closure_counts(self):
        task = {"worker_wait": {"result": "success"}, "termination": {"cause": "completed", "scope_empty": True}}
        self.assertIs(packaged_defaults.require_task_closure(self.closure({TASK_ID: task}), self.admitted), task)
        for closure in (self.closure({}), self.closure({"3" * 32: task}), {**self.closure({TASK_ID: task}),
                                                                         "root_id": "3" * 32},
                        {"root_id": ROOT_ID, "tasks": []}):
            with self.subTest(closure=closure), self.assertRaisesRegex(RuntimeError, "does not include"):
                packaged_defaults.require_task_closure(closure, self.admitted)

    def test_registration_precedes_dispatch_and_a_refused_admission_keeps_the_entry(self):
        responses = ({"task_record": "created", "admission": "admitted"},
                     {"task_record": "created", "admission": "queued", "root_id": ROOT_ID})
        for response in responses:
            with self.subTest(response=response):
                ledger = []
                cleanup = packaged_defaults.register_installed_default(self.installed, ledger)
                self.assertEqual(ledger, [cleanup])

                def dispatched(_delegate, _env, args, **_options):
                    return {**response, "task_id": args[args.index("--id") + 1]}

                with patch.object(packaged_defaults, "run_json", dispatched), self.assertRaises(RuntimeError):
                    packaged_defaults.dispatch_installed_default(self.installed, cleanup, capture_root=None)
                self.assertEqual(ledger, [cleanup])
                self.assertEqual(len(cleanup["task_ids"]), 1)
        self.state.mkdir()
        with self.assertRaisesRegex(RuntimeError, "not fresh"):
            packaged_defaults.register_installed_default(self.installed, [])

    def collect(self, status, outcome, closure):
        responses = iter((status, {"outcome": outcome}))
        meta = self.state / "tasks" / TASK_ID / "meta.json"
        meta.parent.mkdir(parents=True, exist_ok=True)
        runner = packaged_defaults.managed_state_runner(self.state, "a" * 64)
        meta.write_text(json.dumps({"runner_executable": str(runner)}))
        with patch.object(packaged_defaults, "run_json", lambda *_args, **_options: next(responses)), \
                patch.object(packaged_defaults, "file_fingerprint", return_value={"sha256": "a" * 64}), \
                patch.object(packaged_defaults, "await_managed_lifetime_closure", return_value=closure) as awaited:
            digest = packaged_defaults.collect_installed_default(self.installed, self.admitted, capture_root=None)
        awaited.assert_called_once_with(self.state, (TASK_ID,))
        return digest

    def test_status_collect_and_closure_must_name_the_same_root_and_task(self):
        same = {"root_id": ROOT_ID, "task_id": TASK_ID}
        committed = {**same, "verdict": "committed"}
        task = {"termination": {"cause": "completed"}}
        self.assertEqual(self.collect(same, committed, self.closure({TASK_ID: task})), "a" * 64)
        for status, outcome, closure in (
                ({**same, "root_id": "3" * 32}, committed, self.closure({TASK_ID: task})),
                (same, {**committed, "task_id": "3" * 32}, self.closure({TASK_ID: task})),
                (same, {**committed, "verdict": "rejected"}, self.closure({TASK_ID: task})),
                (same, committed, self.closure({}))):
            with self.subTest(status=status, outcome=outcome, closure=closure), self.assertRaises(RuntimeError):
                self.collect(status, outcome, closure)

    def test_verified_shutdown_releases_the_entry_before_the_revalidated_closure_is_checked(self):
        cleanup = packaged_defaults.register_installed_default(self.installed, self.ledger)
        with patch.object(packaged_defaults, "shutdown_private_supervisor", return_value=self.closure({})) as down:
            with self.assertRaisesRegex(RuntimeError, "does not include"):
                packaged_defaults.shutdown_installed_default(
                    self.installed, self.admitted, cleanup, self.ledger, capture_root=None)
        self.assertEqual(down.call_args.args[0], self.root / "pair" / "pueue")
        self.assertEqual(down.call_args.kwargs["task_ids"], (TASK_ID,))
        self.assertEqual(self.ledger, [])

    def test_a_disposal_owner_is_forwarded_to_the_shutdown_and_defaults_to_none(self):
        task = {"worker_wait": {"result": "success"}, "termination": {"cause": "completed", "scope_empty": True}}
        owner = object()
        for kwargs, expected in (({}, None), ({"disposal": owner}, owner)):
            with self.subTest(kwargs=kwargs):
                cleanup = packaged_defaults.register_installed_default(self.installed, self.ledger)
                with patch.object(packaged_defaults, "shutdown_private_supervisor",
                                  return_value=self.closure({TASK_ID: task})) as down:
                    packaged_defaults.shutdown_installed_default(
                        self.installed, self.admitted, cleanup, self.ledger, capture_root=None, **kwargs)
                self.assertIs(down.call_args.kwargs["disposal"], expected)

    def test_the_bootstrap_core_hands_its_disposal_owner_to_the_shutdown_step(self):
        owner = object()
        with patch.object(packaged_defaults, "register_installed_default", return_value="entry"), \
                patch.object(packaged_defaults, "dispatch_installed_default", return_value=self.admitted), \
                patch.object(packaged_defaults, "collect_installed_default", return_value="a" * 64), \
                patch.object(packaged_defaults, "file_fingerprint", return_value={"sha256": "a" * 64}), \
                patch.object(packaged_defaults, "shutdown_installed_default", return_value={}) as down:
            packaged_defaults.prove_installed_default_bootstrap(
                self.installed, supervisor_cleanups=self.ledger, capture_root=None, disposal=owner)
        self.assertIs(down.call_args.kwargs["disposal"], owner)
