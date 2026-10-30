"""Private control captures stay out of the provider ancestor base across every composition.

The real fixture setup, directory owner, capture writer and finalizer run here.
Only external process responses are faked: a finite fake Delegate CLI, or a
finite child that prints the response a patched control step would read. The
base is an ancestor of the provider execution views, so a capture must never
add an entry directly beneath it.
"""

import ast
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.resources import DirectoryPinOwner
from scripts.delegate_acceptance.private_cli import capacity_entry
from scripts.delegate_acceptance.private_cli import contention_controls
from scripts.delegate_acceptance.private_cli import finalization
from scripts.delegate_acceptance.private_cli import fixture_setup
from scripts.delegate_acceptance.private_cli import handoff
from scripts.delegate_acceptance.private_cli import initial
from scripts.delegate_acceptance.private_cli import legacy
from scripts.delegate_acceptance.private_cli import scenario_entry
from scripts.delegate_acceptance.private_cli import supervisor
from scripts.delegate_acceptance.private_cli import upgrade
from scripts.delegate_acceptance.private_cli.capture import run_captured_command
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records


EXTERNAL_OBSERVATION_PHASE = "external-supervisor-observation"
COMPOSITION_MODULES = (initial, capacity_entry, legacy, scenario_entry, finalization, contention_controls)
SLOW_TASK = "f" * 32
FAKE_DELEGATE = """#!{python}
import json, sys, time
args = sys.argv[1:]
command = next(name for name in ("dispatch", "status", "collect", "models") if name in args)
if command == "dispatch":
    response = {{"task_id": args[args.index("--id") + 1], "task_record": "created", "admission": "admitted",
                "root_id": "r" * 32}}
elif command == "status":
    response = {{"task_id": args[args.index("status") + 1], "root_id": "r" * 32}}
    if response["task_id"] == "{slow}":
        time.sleep(30)
elif command == "collect":
    response = {{"outcome": {{"verdict": "committed"}}}}
else:
    response = {{"status": "available", "models": [{{"id": "openai/gpt-4.1"}}]}}
sys.stdout.write(json.dumps(response, sort_keys=True))
"""


def capture_files(base):
    return sorted(path for path in base.rglob("capture-*") if path.suffix in (".stdout", ".stderr"))


def fake_control(capture_root, response):
    """Answer one control step through the real capture owner with a finite external process."""
    payload = json.dumps(response, sort_keys=True)
    completed = run_captured_command(
        [sys.executable, "-I", "-c", "import sys; sys.stdout.write(sys.argv[1])", payload],
        {"PATH": "/usr/bin:/bin"},
        10,
        capture_root=capture_root,
    )
    if completed.returncode != 0 or completed.stdout != payload.encode():
        raise RuntimeError("fake control response was not captured exactly")
    return json.loads(completed.stdout)


def exited_pid():
    process = subprocess.Popen([sys.executable, "-I", "-c", "pass"])
    process.wait(timeout=10)
    return process.pid


def pid_fallback(capture_root):
    """Run the real shutdown PID-presence fallback for an exited daemon through the given capture owner."""
    if not supervisor.private_daemon_process_gone(exited_pid(), time.monotonic() + 10, capture_root=capture_root):
        raise RuntimeError("exited fixture PID was not observed gone")


class CaptureLayoutCase(unittest.TestCase):
    """A fake home and fake binaries around the real private fixture setup."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-capture-layout-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.home = self.root / "home"
        self.home.mkdir()
        self.binaries = {}
        for name in ("delegate", "runner", "pueue", "pueued"):
            binary = self.root / ("fake-" + name)
            if name == "delegate":
                binary.write_text(FAKE_DELEGATE.format(python=sys.executable, slow=SLOW_TASK))
            else:
                binary.write_text("#!/bin/sh\nexit 0\n")
            binary.chmod(0o755)
            self.binaries[name] = binary
        self.responses = self.root / "responses"
        self.receipt = AcceptanceReceipt(self.root / "receipt.json", 1, 1, {})
        self.lifetime = FixtureLifetime(DirectoryPinOwner(self), DirectoryPinOwner(self))

    def arguments(self, scenario="capacity"):
        return SimpleNamespace(
            scenario=scenario, noexec_state_parent=None, upgraded_delegate=None, upgraded_runner=None,
            parallel_tasks=1, runtime_only=True, output=self.root / "receipt.json", **self.binaries,
        )

    def prepare(self, scenario="capacity"):
        with patch.object(Path, "home", return_value=self.home):
            return fixture_setup.prepare_fixture(
                self.arguments(scenario), "n" * 32, False, self.lifetime, self.responses, self.receipt,
            )

    def base_entries(self):
        return frozenset(path.name for path in self.lifetime.base.iterdir())

    def assert_captures_outside_base(self, before, expected_count, designed=frozenset()):
        """Every new capture lives in one owned child of base; base gains only its designed entries."""
        base = self.lifetime.base
        added = capture_files(base)
        self.assertEqual(self.base_entries() - before, designed)
        self.assertEqual([path for path in added if path.parent == base], [])
        self.assertEqual(len(added), expected_count)
        parents = {path.parent for path in added}
        self.assertEqual(len(parents), 1)
        (directory,) = parents
        self.assertEqual(directory.parent, base)
        self.assertNotIn(directory.name, designed)
        for path in added:
            self.assertEqual(stat.S_IMODE(path.lstat().st_mode), 0o600)
            self.assertTrue(stat.S_ISREG(path.lstat().st_mode))
        return directory, added


class CompositionCaptureRootTests(CaptureLayoutCase):
    def test_no_composition_forwards_the_root_token_as_capture_authority(self):
        for module in COMPOSITION_MODULES:
            tree = ast.parse(Path(module.__file__).read_text(encoding="utf-8"))
            forwarded = [
                keyword.lineno
                for node in ast.walk(tree)
                if isinstance(node, ast.Call)
                for keyword in node.keywords
                if keyword.arg == "capture_root"
                and isinstance(keyword.value, ast.Attribute)
                and keyword.value.attr == "base_token"
            ]
            with self.subTest(module=module.__name__):
                self.assertEqual(forwarded, [])

    def test_repeated_initial_controls_capture_two_exact_private_files_per_invocation_outside_base(self):
        fixture = self.prepare()
        before = self.base_entries()
        for repetition in (1, 2):
            initial.dispatch_initial_task(self.arguments(), fixture, self.lifetime, self.responses, self.receipt)
            directory, added = self.assert_captures_outside_base(before, 6 * repetition)
        stdout = [json.loads(path.read_bytes()) for path in added if path.suffix == ".stdout"]
        self.assertEqual(sum("admission" in response for response in stdout), 2)
        self.assertEqual(sum(response == {"outcome": {"verdict": "committed"}} for response in stdout), 2)
        self.assertEqual(sorted(response["task_id"] for response in stdout if set(response) == {"task_id", "root_id"}),
                         sorted(self.lifetime.attempted_task_ids))
        self.assertEqual([path.read_bytes() for path in added if path.suffix == ".stderr"], [b""] * 6)
        self.assertEqual(sorted(path.name for path in self.responses.iterdir()),
                         ["collect-initial.json", "dispatch-initial.json", "status-initial.json"])

    def test_legacy_models_and_capacity_observations_capture_outside_base(self):
        fixture = self.prepare()
        before = self.base_entries() - {"old"}
        legacy.complete_legacy_package_removal(
            fixture, SimpleNamespace(models_args=["--root", str(fixture.state), "models", "--json"]),
            self.lifetime, self.responses,
        )
        self.assert_captures_outside_base(before, 2)

        def upgrade(*, capture_root, **_options):
            self.assertEqual(self.lifetime.primary_observer("1" * 32, time.monotonic() + 10), "r" * 32)
            return [], 0

        with patch.object(capacity_entry, "LockedReleaseGate"), \
                patch.object(capacity_entry, "pueue_snapshot",
                             side_effect=lambda *_a, capture_root, **_k: fake_control(
                                 capture_root, {"groups": {"default": {"parallel_tasks": 0}}})), \
                patch.object(capacity_entry, "verify_current_installation_upgrade", side_effect=upgrade), \
                patch.object(capacity_entry, "shutdown_observed_private_supervisor",
                             side_effect=lambda *_a, capture_root, **_k: pid_fallback(capture_root)), \
                patch.object(capacity_entry, "record_capacity_scenario"):
            capacity_entry.run_capacity_scenario(
                args=self.arguments(), dispatch_workers=1, lease_nonce="n" * 32, lifetime=self.lifetime,
                response_directory=self.responses, run_receipt=self.receipt, fixture=fixture,
                initial=SimpleNamespace(initial_pair_directory=self.root),
            )
        directory, _ = self.assert_captures_outside_base(before, 8)
        with self.assertRaises(subprocess.TimeoutExpired) as raised:
            self.lifetime.primary_observer(SLOW_TASK, time.monotonic() + 1)
        directory, added = self.assert_captures_outside_base(before, 10)
        self.assertEqual({Path(path) for path in raised.exception.capture_paths} - set(added), set())
        self.assertEqual({Path(path).parent for path in raised.exception.capture_paths}, {directory})
        receipts = sorted(directory.glob("disposal-status-%s-*.json" % SLOW_TASK))
        self.assertEqual(len(receipts), 1)
        self.assertEqual(json.loads(receipts[0].read_text())["capture_paths"],
                         list(raised.exception.capture_paths))
        self.assertEqual(sorted(self.lifetime.base.glob("disposal-status-*")), [])

    def test_stop_and_modern_shutdown_pid_fallback_capture_outside_base(self):
        fixture = self.prepare()
        before = self.base_entries()
        with patch.object(scenario_entry, "run_default_stop_acceptance",
                          side_effect=lambda *, capture_root, **_k: fake_control(capture_root, {"stop": "ok"})):
            fixture.old_daemon_marker.write_text("started\n")
            scenario_entry.run_stop_scenario("n" * 32, self.lifetime, self.responses, self.receipt, fixture)
        designed = frozenset({fixture.old_daemon_marker.name})
        self.assert_captures_outside_base(before, 2, designed)
        with patch.object(contention_controls, "run_modern_binary_contention", return_value={"modern": True}), \
                patch.object(contention_controls, "select_installation"), \
                patch.object(contention_controls, "shutdown_private_supervisor",
                             side_effect=lambda *_a, capture_root, **_k: pid_fallback(capture_root)):
            contention_controls.run_prepared_modern_contention(
                self.lifetime, fixture, self.responses, self.receipt
            )
        self.assert_captures_outside_base(
            before, 4, designed | {"modern-controls", "modern-processes"}
        )

    def test_finalizer_shutdowns_use_a_live_capture_child_and_pid_fallback(self):
        fixture = self.prepare()
        before = self.base_entries()
        config = fixture.state / ".supervisor" / "pueue.yml"
        config.parent.mkdir()
        config.write_text("fixture: true\n")
        self.lifetime.private_config = config
        roots = []

        def shutdown(*_args, capture_root, **_options):
            capture_root.assert_live()
            roots.append(capture_root)
            pid_fallback(capture_root)

        target = SimpleNamespace(state=self.root / "predecessor", backup_handle=SimpleNamespace(close=lambda: None))
        pending = {"config": config, "client": fixture.provider_bin, "env": {}, "state": fixture.state}
        self.lifetime.supervisor_cleanups[:] = [pending, {"client": fixture.provider_bin, "env": {}, "task_ids": [],
                                                          "predecessor": target}]
        with patch.object(finalization, "shutdown_pending_private_supervisor",
                          side_effect=lambda pending, _ledger, *, capture_root, disposal: shutdown(
                              capture_root=capture_root)), \
                patch.object(finalization, "shutdown_predecessor_supervisor",
                             side_effect=lambda *_a, capture_root, **_k: (
                                 shutdown(capture_root=capture_root), finalization.PREDECESSOR_SHUT_DOWN)[1]), \
                patch.object(finalization, "shutdown_private_supervisor", side_effect=shutdown):
            errors = []
            finalization.shutdown_supplemental_supervisors(errors, self.lifetime)
            finalization.shutdown_primary_supervisor(
                self.arguments(), errors, None, self.lifetime, True, False,
                predecessor_roots=frozenset(), deadline=time.monotonic() + 30,
            )
        self.assertEqual(errors, [])
        self.assertEqual(len(roots), 3)
        self.assertEqual(len({id(root) for root in roots}), 1)
        directory, _ = self.assert_captures_outside_base(before, 6)
        self.assertEqual(roots[0].directory, directory)

    def test_success_root_cleanup_removes_and_consumes_the_capture_child_with_the_base(self):
        # This scenario registers no managed state root, so removal needs no fixture closure records.
        fixture = self.prepare(EXTERNAL_INSPECTION_HANDOFF_SCENARIO)
        initial.dispatch_initial_task(self.arguments(), fixture, self.lifetime, self.responses, self.receipt)
        base = self.lifetime.base
        directory, _ = self.assert_captures_outside_base(self.base_entries(), 6)
        errors = []
        success, cleanup = finalization.finish_root_cleanup(errors, None, self.lifetime, True)
        self.assertEqual(errors, [])
        self.assertTrue(success)
        self.assertEqual(cleanup, {"status": "removed", "verified": True})
        self.assertFalse(base.exists())
        self.assertFalse(directory.exists())
        self.assertTrue(self.lifetime.capture_token.consumed)


class RecoveryAndHandoffPathTests(CaptureLayoutCase):
    def test_recovery_package_and_retarget_markers_stay_at_the_fixture_base(self):
        fixture = self.prepare(SUPERVISOR_UPGRADE_RECOVERY_SCENARIO)
        base = self.lifetime.base
        before = self.base_entries()
        pair = self.root / "pair"
        binding = {"client_executable": str(pair / "pueue"), "daemon_executable": str(pair / "pueued"),
                   "client_sha256": "c", "daemon_sha256": "d", "endpoint": "unix:/fixture.sock"}
        staged = {}
        markers = []
        cleanup = {"predecessor": object()}

        def stage(*, base, **_options):
            staged["base"] = base
            return self.root / "pending", binding, base / "upgrade-legacy-control.yml", "", cleanup

        def collect(*_args):
            return None

        with patch.object(upgrade, "_upgrade_task_operations", return_value=(None, collect)), \
                patch.object(upgrade, "prepare_upgrade_history",
                             return_value=(pair, binding, {}, self.root, "history")), \
                patch.object(upgrade, "_prove_recovered_capacity"), \
                patch.object(upgrade, "snapshot", return_value="history"), \
                patch.object(upgrade, "read_json", return_value={"binding": binding}), \
                patch.object(upgrade, "assert_state_supervisor_pair", return_value=pair), \
                patch.object(upgrade, "stage_pending_package", side_effect=stage), \
                patch.object(upgrade, "interrupt_pending_migration",
                             side_effect=lambda *, capture_root, **_k: (
                                 fake_control(capture_root, {"blocked": True}), "/fixture.sock")[1]), \
                patch.object(upgrade, "install_wrapper",
                             side_effect=lambda _destination, _source, marker, *_a: markers.append(marker)), \
                patch.object(upgrade, "run_json",
                             side_effect=lambda *_a, capture_root, **_k: fake_control(
                                 capture_root, {"supervisor_control": {"state": "migrated"}})), \
                patch.object(upgrade, "await_pending_predecessor",
                             side_effect=lambda capture_root, _target: pid_fallback(capture_root)), \
                patch.object(upgrade, "release_pending_predecessor"), \
                patch.object(upgrade, "shutdown_private_supervisor",
                             side_effect=lambda *_a, capture_root, **_k: pid_fallback(capture_root)):
            scenario_entry.run_recovery_scenario("n" * 32, self.lifetime, self.responses, self.receipt, fixture)
        self.assertEqual(staged["base"], base)
        self.assertEqual(markers, [base / "retarget-client", base / "retarget-daemon"])
        directory, _ = self.assert_captures_outside_base(before, 8)
        self.assertNotEqual(directory, staged["base"])
        self.assertNotIn(directory, [marker.parent for marker in markers])

    def test_handoff_reads_the_base_client_call_log_and_captures_outside_base(self):
        fixture = self.prepare(EXTERNAL_INSPECTION_HANDOFF_SCENARIO)
        base = self.lifetime.base
        call_log = base / "old-client-starts-calls"
        read = []

        def run_case(case):
            task_commands, _control, evidence, _record = handoff._initialize_handoff_case(
                case, handoff.HandoffCleanup()
            )
            # The real installed old client wrapper logs a genuine external status observation.
            argv = ["--config", str(task_commands.external_config), "status", "--json"]
            observed = run_captured_command(
                [str(case.client), *argv],
                {"PATH": "/usr/bin:/bin", "DELEGATE_ACCEPTANCE_PHASE": EXTERNAL_OBSERVATION_PHASE},
                10,
                capture_root=case.capture_root,
            )
            self.assertEqual(observed.returncode, 0)
            logged = wrapper_call_records(call_log, "client", "old")
            self.assertEqual(logged[-1]["phase"], EXTERNAL_OBSERVATION_PHASE)
            self.assertEqual(logged[-1]["command"], "status")
            self.assertEqual(logged[-1]["arguments_sha256"],
                             hashlib.sha256(" ".join(argv).encode()).hexdigest())
            read.append((task_commands.external_client_calls(), logged[-1:], len(logged)))
            return evidence

        before = self.base_entries()
        with patch.object(handoff, "_run_handoff_case", side_effect=run_case):
            scenario_entry.run_handoff_scenarios("n" * 32, self.lifetime, self.responses, self.receipt, fixture)
        self.assertEqual(len(read), len(scenario_entry.HANDOFF_CASES))
        for index, (calls, expected, logged_count) in enumerate(read):
            self.assertEqual(calls, expected)
            self.assertEqual(logged_count, index + 1)
        designed = frozenset({
            call_log.name, "old-client-starts", "old-client-starts-arguments",
            "external-supervisor", "external-supervisor-processes",
            "external-handoff-waiting-brief.md",
            *("external-handoff-" + label for label, _action in scenario_entry.HANDOFF_CASES[1:]),
        })
        self.assert_captures_outside_base(before, 2 * len(scenario_entry.HANDOFF_CASES), designed)


class EarlyCaptureChildFailureTests(CaptureLayoutCase):
    def test_capture_child_collision_refuses_before_supervisor_setup_and_retains_the_base(self):
        outside = self.root / "outside"
        outside.mkdir()
        created = []
        real_mkdtemp = tempfile.mkdtemp

        def colliding_mkdtemp(*args, **options):
            path = real_mkdtemp(*args, **options)
            created.append(Path(path))
            (Path(path) / "private-control-captures").symlink_to(outside)
            return path

        with patch.object(fixture_setup.tempfile, "mkdtemp", side_effect=colliding_mkdtemp):
            with self.assertRaises(FileExistsError) as raised:
                self.prepare()
        base = created[0].resolve()
        self.assertIn("private-control-captures", str(raised.exception))
        self.assertEqual(self.lifetime.base, base)
        self.assertIsNone(self.lifetime.capture_token)
        self.assertIsNone(self.lifetime.private_config)
        self.assertEqual(self.lifetime.supervisor_cleanups, [])
        self.assertEqual(self.lifetime.managed_state_roots, [])
        self.assertEqual(sorted(path.name for path in base.iterdir()), ["private-control-captures"])
        with patch.object(finalization, "shutdown_private_supervisor") as shutdown:
            finalization.finalize_fixture(
                self.arguments(), RuntimeError("setup"), self.lifetime, self.receipt, False
            )
        shutdown.assert_not_called()
        self.assertTrue(base.is_dir())
        self.assertEqual(os.readlink(base / "private-control-captures"), str(outside))
        self.assertEqual(list(outside.iterdir()), [])
        written = json.loads((self.root / "receipt.json").read_text())
        self.assertEqual(written["cleanup"]["private_root"], {"status": "retained", "verified": True})


if __name__ == "__main__":
    unittest.main()
