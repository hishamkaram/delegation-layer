"""The install bootstrap probe owns its root, records its original faults and retains every unproven supervisor.

Every case calls the public probe with a no-spawn guard on the one child launch the private CLI capture
owns, installed before any setup. Ownership cases run the real core up to that launch; the shared core is
replaced only to isolate this application's mapping and success disposal. Released pins are proven by the
process's descriptor count. The version-only fake run in test_candidate_install and the native prototype
exercise real children.
"""

from __future__ import annotations

import errno
from pathlib import Path
import os
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.directories import operations
from scripts.delegate_acceptance.private_cli import capture, finalization, supervisor
from scripts.delegate_acceptance.private_cli.packaged_defaults import (
    INSTALLED_BINARIES, InstalledBootstrap, isolated_default_state_path,
)
from scripts.delegate_acceptance.release_verification import candidate_install_bootstrap as bootstrap
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    BOOTSTRAP_SHUTDOWN, BootstrapTarget,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


ROOT_ID = "1" * 32
TASK_ID = "2" * 32


class ProbeCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="candidate-bootstrap-test", label="candidate bootstrap test fixture")
        self.addCleanup(self.tree.dispose)
        # No child ever starts: the guard replaces the capture owner's launch before any setup.
        self.spawns: list[dict[str, str]] = []
        self.commands: list[list[str]] = []
        self.on_spawn = None
        guard = mock.patch.object(capture.subprocess, "Popen", self.no_spawn)
        guard.start()
        self.addCleanup(guard.stop)
        self.parent = self.tree.root / "home"
        self.installed = self.tree.root / "install" / "bin"
        self.parent.mkdir()
        self.installed.mkdir(parents=True)
        for name in INSTALLED_BINARIES:
            path = self.installed / name
            path.write_bytes(b"#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
        self.target = BootstrapTarget(
            command_directory=self.installed, delegate=(self.installed / "delegate").resolve(),
            supervisor_directory=self.installed, binaries=fx.NATIVE_BINARIES)
        self.receipt: dict = {"candidate_install": {}}
        self.locations: dict[str, Path] = {}
        self.probe = bootstrap.InstalledBootstrapProbe(receipt=self.receipt, locations=self.locations,
                                                       parent=self.parent)
        self.installs: list = []

    def facts(self) -> dict:
        return self.receipt["candidate_install"]["bootstrap"]

    def no_spawn(self, command, *, env, **_streams):
        """Record the refused launch, run the case's hook, then fail it as the OS would."""
        self.spawns.append(dict(env))
        self.commands.append(list(command))
        if self.on_spawn is not None:
            self.on_spawn(env)
        raise OSError(errno.EPERM, "no child may start in this test")

    def call(self, expected: type[BaseException]) -> BaseException:
        """Call the public probe once; every pin it held must be released whatever escapes."""
        before = open_descriptors()
        with self.assertRaises(BaseException) as caught:
            self.probe(self.target)
        self.assertEqual(open_descriptors(), before, "the probe left owned directory pins open")
        self.assertIsInstance(caught.exception, expected)
        return caught.exception

    def config_on_spawn(self, env) -> Path:
        """The launch control a dispatch that wrote its private config before failing would leave."""
        config = isolated_default_state_path(Path(env["HOME"])) / ".supervisor" / "pueue.yml"
        config.parent.mkdir(parents=True)
        config.write_text("fixture\n")
        return config

    def dispatched_record(self) -> dict[str, object]:
        """The minimal ledger record of the one real-core dispatch the guard refused."""
        (command,) = self.commands
        state = isolated_default_state_path(Path(self.spawns[0]["HOME"]))
        return {"config": str(state / ".supervisor" / "pueue.yml"), "state": str(state),
                "task_ids": [command[command.index("--id") + 1]]}

    def replace_root(self) -> int:
        """Move the created root aside and put a new directory at its path; return the original inode."""
        root = self.locations["bootstrap_root"]
        moved = self.tree.root / "moved-root"
        os.rename(root, moved)
        root.mkdir()
        return moved.stat().st_ino

    def core(self, outcome):
        """Replace the shared core: record its inputs, then return or raise ``outcome(installed, ledger)``."""
        def run(installed, *, supervisor_cleanups, capture_root):
            self.installs.append((installed, capture_root))
            return outcome(installed, supervisor_cleanups)
        return mock.patch.object(bootstrap, "prove_installed_default_bootstrap", run)

    def pending(self, installed, ledger, *, config: bool) -> dict:
        entry = {"client": installed.supervisor_directory / "pueue", "config": installed.config,
                 "env": installed.env, "state": installed.state, "task_ids": [TASK_ID]}
        if config:
            installed.config.parent.mkdir(parents=True)
            installed.config.write_text("fixture\n")
        ledger.append(entry)
        return entry

    def entry(self, installed, ledger, name: str, *, task_ids: list[str], config: bool) -> dict[str, object]:
        """Register one more supervisor below the probe HOME; return its expected minimal record."""
        state = Path(installed.env["HOME"]) / name
        entry = {"client": installed.supervisor_directory / "pueue", "config": state / ".supervisor" / "pueue.yml",
                 "env": installed.env, "state": state, "task_ids": task_ids}
        if config:
            entry["config"].parent.mkdir(parents=True)
            entry["config"].write_text("fixture\n")
        ledger.append(entry)
        return {"config": str(entry["config"]), "state": str(state), "task_ids": list(task_ids)}

    def published(self) -> list[dict[str, object]]:
        """The minimal fields of every recorded supervisor, without its optional evidence."""
        return [{name: record[name] for name in ("config", "state", "task_ids")}
                for record in self.facts().get("unresolved_ownership", [])]

    def assertRetained(self, code: str) -> Path:
        with self.assertRaises(VerificationError) as caught:
            self.probe(self.target)
        self.assertEqual(caught.exception.code, code)
        root = self.locations["bootstrap_root"]
        self.assertTrue(root.is_dir())
        self.assertEqual(self.facts()["root"], str(root))
        return root


class SuccessTests(ProbeCase):
    def test_a_closed_turn_maps_to_the_observation_and_removes_the_root(self):
        result = InstalledBootstrap(
            root_id=ROOT_ID, task_id=TASK_ID, delegate_sha256="4" * 64, runner_sha256="5" * 64,
            client_sha256="6" * 64, daemon_sha256="7" * 64, outcome="committed", worker_result="success",
            termination_cause="completed", scope_empty=True)
        with self.core(lambda _installed, _ledger: result):
            observation = self.probe(self.target)
        self.assertEqual(observation, fx.bootstrap(fx.NATIVE_BINARIES))
        self.assertEqual(observation.shutdown, BOOTSTRAP_SHUTDOWN)
        (installed, capture), = self.installs
        root = self.locations["bootstrap_root"]
        self.assertFalse(root.exists())
        self.assertEqual(root.parent, self.parent)
        self.assertEqual(installed.delegate, self.installed / "delegate")
        self.assertEqual(installed.env["PATH"].split(":")[0], str(self.installed))
        self.assertEqual(installed.env["HOME"], str(root / "h"))
        self.assertEqual(dict(installed.digests), fx.NATIVE_BINARIES.as_mapping())
        self.assertEqual(capture.directory, root / "c")
        self.assertEqual((self.facts()["status"], self.facts()["task_id"]), ("verified", TASK_ID))


class OwnershipTests(ProbeCase):
    """One local owner lifetime: failures retain and release every pin; only a verified turn removes."""

    def test_a_shutdown_interrupt_through_the_real_core_releases_the_owner_and_keeps_the_ledger(self):
        self.on_spawn = self.config_on_spawn
        with mock.patch.object(supervisor, "authorize_managed_lifetime_closure", side_effect=KeyboardInterrupt):
            self.call(KeyboardInterrupt)
        facts = self.facts()
        self.assertEqual(len(self.spawns), 1)
        self.assertEqual(facts["failure"]["type"], "PermissionError")
        self.assertEqual(facts["cleanup_failures"], [{"type": "KeyboardInterrupt", "interrupted": True}])
        (pending,) = facts["unresolved_ownership"]
        self.assertTrue(Path(pending["config"]).is_file())
        self.assertEqual(facts["root_disposition"], "retained")
        root = self.locations["bootstrap_root"]
        self.assertTrue(all((root / name).is_dir() for name in ("c", "h", "p", "w", "r")))

    def test_a_diagnostics_fault_after_shutdown_still_releases_the_owner(self):
        self.on_spawn = self.config_on_spawn
        with mock.patch.object(supervisor, "authorize_managed_lifetime_closure",
                               side_effect=RuntimeError("closure incomplete")), \
                mock.patch.object(bootstrap, "private_shutdown_evidence", side_effect=LookupError("diagnostics")):
            self.call(LookupError)
        self.assertEqual(self.facts()["failure"]["type"], "PermissionError")
        self.assertEqual(self.facts()["root_disposition"], "retained")
        self.assertTrue(self.locations["bootstrap_root"].is_dir())
        self.assertEqual(self.facts().get("unresolved_ownership"), [self.dispatched_record()])

    def test_a_first_entry_diagnostics_fault_keeps_every_later_entry_recorded(self):
        expected = []

        def two_dispatches(installed, ledger):
            expected.extend(self.entry(installed, ledger, name, task_ids=[task], config=False)
                            for name, task in (("first", TASK_ID), ("second", ROOT_ID)))
            raise RuntimeError("dispatch fault")

        with self.core(two_dispatches), \
                mock.patch.object(bootstrap, "private_shutdown_evidence", side_effect=LookupError("first entry")):
            error = self.call(LookupError)
        self.assertIsInstance(error.__context__, RuntimeError)
        facts = self.facts()
        self.assertEqual(facts["failure"], {"type": "RuntimeError", "message": "dispatch fault"})
        self.assertEqual(len(facts["cleanup_failures"]), 2)
        self.assertEqual(self.published(), expected)

    def test_a_shutdown_interrupt_with_a_diagnostics_fault_keeps_the_record_and_the_interrupt(self):
        self.on_spawn = self.config_on_spawn
        with mock.patch.object(supervisor, "authorize_managed_lifetime_closure", side_effect=KeyboardInterrupt), \
                mock.patch.object(bootstrap, "private_shutdown_evidence", side_effect=LookupError("diagnostics")):
            error = self.call(LookupError)
        self.assertIsInstance(error.__context__, KeyboardInterrupt)
        facts = self.facts()
        self.assertEqual(facts["cleanup_failures"], [{"type": "KeyboardInterrupt", "interrupted": True}])
        self.assertEqual(facts.get("unresolved_ownership"), [self.dispatched_record()])

    def test_an_immediate_interrupt_with_a_diagnostics_fault_keeps_the_record_and_the_interrupt(self):
        expected = []

        def interrupted(installed, ledger):
            expected.append(self.entry(installed, ledger, "dispatched", task_ids=[TASK_ID], config=True))
            raise KeyboardInterrupt

        with self.core(interrupted), \
                mock.patch.object(bootstrap, "private_shutdown_evidence", side_effect=LookupError("diagnostics")):
            error = self.call(LookupError)
        self.assertIsInstance(error.__context__, KeyboardInterrupt)
        self.assertEqual(self.published(), expected)
        self.assertEqual(self.facts()["status"], "interrupted")

    def test_a_completed_observation_still_drops_an_untouched_registration(self):
        expected = []

        def mixed(installed, ledger):
            self.entry(installed, ledger, "untouched", task_ids=[], config=False)
            expected.append(self.entry(installed, ledger, "dispatched", task_ids=[TASK_ID], config=False))
            raise RuntimeError("dispatch fault")

        with self.core(mixed):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_unresolved")
        self.assertEqual(self.published(), expected)
        self.assertIn("evidence", self.facts()["unresolved_ownership"][0])

    def test_an_untouched_registration_alone_is_no_unresolved_ownership(self):
        def untouched(installed, ledger):
            self.entry(installed, ledger, "untouched", task_ids=[], config=False)
            raise RuntimeError("dispatch fault")

        with self.core(untouched):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        self.assertNotIn("unresolved_ownership", self.facts())

        def interrupted(installed, ledger):
            self.entry(installed, ledger, "untouched", task_ids=[], config=False)
            raise KeyboardInterrupt

        with self.core(interrupted):
            self.call(KeyboardInterrupt)
        self.assertEqual(self.facts()["unresolved_ownership"], [])

    def test_a_root_creation_fault_records_only_the_intent(self):
        absent = self.tree.root / "absent-parent"
        self.probe = bootstrap.InstalledBootstrapProbe(receipt=self.receipt, locations=self.locations, parent=absent)
        error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        facts = self.facts()
        self.assertEqual((facts["parent"], facts["root"]), (str(absent), str(self.locations["bootstrap_root"])))
        self.assertNotIn("root_identity", facts)
        self.assertEqual(facts["failure"]["type"], "RuntimeError")
        self.assertFalse(self.locations["bootstrap_root"].exists())
        self.assertEqual(self.spawns, [])

    def test_the_creation_identity_survives_a_replaced_root_and_the_release_fault_keeps_the_primary(self):
        original = []

        def setup_fault(*_args):
            original.append(self.replace_root())
            raise RuntimeError("setup fault")

        with mock.patch.object(bootstrap, "install_pi_fixture", setup_fault):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        facts = self.facts()
        self.assertEqual(facts.get("root_identity", {}).get("inode"), original[0])
        self.assertNotEqual(self.locations["bootstrap_root"].stat().st_ino, original[0])
        self.assertEqual(facts["failure"], {"type": "RuntimeError", "message": "setup fault"})
        self.assertIn("release_failure", facts)
        self.assertEqual(self.spawns, [])

    def test_a_release_fault_never_replaces_a_cleanup_interrupt(self):
        def replaced_then_config(env):
            self.replace_root()
            self.config_on_spawn(env)

        self.on_spawn = replaced_then_config
        with mock.patch.object(supervisor, "authorize_managed_lifetime_closure", side_effect=KeyboardInterrupt):
            self.call(KeyboardInterrupt)
        self.assertIn("release_failure", self.facts())
        self.assertEqual(self.facts()["cleanup_failures"], [{"type": "KeyboardInterrupt", "interrupted": True}])

    def verified(self, outcome=None):
        result = InstalledBootstrap(
            root_id=ROOT_ID, task_id=TASK_ID, delegate_sha256="4" * 64, runner_sha256="5" * 64,
            client_sha256="6" * 64, daemon_sha256="7" * 64, outcome="committed", worker_result="success",
            termination_cause="completed", scope_empty=True)

        def run(installed, ledger):
            if outcome is not None:
                outcome(installed)
            return result
        return self.core(run)

    def test_a_partial_success_removal_fault_retains_and_consumes_each_pin_once(self):
        def replace_responses(installed):
            os.rename(installed.response_directory, self.tree.root / "moved-responses")
            installed.response_directory.mkdir()

        with self.verified(replace_responses):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_cleanup_failed")
        facts = self.facts()
        self.assertEqual((facts["status"], facts["root_disposition"]), ("cleanup-failed", "retained"))
        self.assertIn("disposal_failure", facts)
        self.assertNotIn("release_failure", facts)
        self.assertTrue(self.locations["bootstrap_root"].is_dir())

    def test_an_interrupted_success_removal_releases_every_unconsumed_pin(self):
        real = operations.remove_owned_directory
        calls = []

        def interrupt_second(token, deadline=None):
            calls.append(token.label)
            if len(calls) == 2:
                raise KeyboardInterrupt
            return real(token, deadline) if deadline is not None else real(token)

        with self.verified(), mock.patch.object(operations, "remove_owned_directory", interrupt_second):
            self.call(KeyboardInterrupt)
        facts = self.facts()
        self.assertEqual(facts.get("root_disposition"), "incomplete")
        self.assertNotIn("release_failure", facts)
        self.assertTrue(self.locations["bootstrap_root"].is_dir())
        self.assertEqual(self.spawns, [])


class FailureTests(ProbeCase):
    """Each replaced core launches no child, so the tree may remove the retained root afterwards."""

    def test_a_delegate_resolving_elsewhere_runs_nothing_and_keeps_the_root(self):
        shadow = self.tree.root / "shadow"
        shadow.mkdir()
        (shadow / "delegate").write_bytes(b"#!/bin/sh\nexit 0\n")
        (shadow / "delegate").chmod(0o755)
        self.target = BootstrapTarget(command_directory=shadow, delegate=self.target.delegate,
                                      supervisor_directory=self.installed, binaries=fx.NATIVE_BINARIES)
        with self.core(lambda _installed, _ledger: self.fail("the core must not run")):
            self.assertRetained("candidate_bootstrap_delegate_mismatch")
        self.assertEqual(self.facts()["delegate"], str(shadow / "delegate"))
        self.assertNotIn("unresolved_ownership", self.facts())

    def test_a_missing_config_after_an_attempted_dispatch_is_unresolved(self):
        def dispatched(installed, ledger):
            self.pending(installed, ledger, config=False)
            raise RuntimeError("delegate command failed: exit=8")

        with self.core(dispatched):
            self.assertRetained("candidate_bootstrap_unresolved")
        facts = self.facts()
        self.assertEqual(facts["failure"], {"type": "RuntimeError", "message": "delegate command failed: exit=8"})
        self.assertEqual(facts["cleanup_failures"][0]["type"], "SupervisorUnverified")
        (pending,) = facts["unresolved_ownership"]
        self.assertEqual(pending["task_ids"], [TASK_ID])

    def test_a_failed_cleanup_keeps_the_original_fault_and_the_supervisor(self):
        def dispatched(installed, ledger):
            self.pending(installed, ledger, config=True)
            raise RuntimeError("collect did not publish success")

        with self.core(dispatched), mock.patch.object(
                bootstrap, "shutdown_pending_private_supervisor", side_effect=OSError("shutdown refused")) as closer:
            root = self.assertRetained("candidate_bootstrap_unresolved")
        self.assertEqual(closer.call_args.kwargs["capture_root"].directory, root / "c")
        facts = self.facts()
        self.assertEqual(facts["failure"]["message"], "collect did not publish success")
        self.assertEqual(facts["cleanup_failures"], [{"type": "OSError", "message": "shutdown refused"}])
        (pending,) = facts["unresolved_ownership"]
        self.assertTrue(Path(pending["config"]).is_file())
        self.assertIn("evidence", pending)

    def test_a_verified_cleanup_still_fails_and_retains_the_root(self):
        def dispatched(installed, ledger):
            self.pending(installed, ledger, config=True)
            raise RuntimeError("status observed a different task")

        def closed(pending, ledger, *, capture_root):
            ledger.remove(pending)
            return {"root_id": ROOT_ID, "tasks": {}}

        with self.core(dispatched), mock.patch.object(bootstrap, "shutdown_pending_private_supervisor", closed):
            self.assertRetained("candidate_bootstrap_failed")
        self.assertNotIn("unresolved_ownership", self.facts())
        self.assertNotIn("cleanup_failures", self.facts())

    def test_an_interruption_escapes_unchanged_without_shutdown_and_keeps_the_record(self):
        interrupt = KeyboardInterrupt()

        def dispatched(installed, ledger):
            self.pending(installed, ledger, config=False)
            raise interrupt

        with self.core(dispatched), mock.patch.object(bootstrap, "shutdown_pending_private_supervisor") as closer, \
                self.assertRaises(KeyboardInterrupt) as caught:
            self.probe(self.target)
        self.assertIs(caught.exception, interrupt)
        closer.assert_not_called()
        self.assertEqual(self.facts()["status"], "interrupted")
        self.assertEqual(len(self.facts()["unresolved_ownership"]), 1)
        self.assertTrue(self.locations["bootstrap_root"].is_dir())


class PendingSupervisorCloserTests(unittest.TestCase):
    """One ordinary pending-supervisor closer serves the finalizer and the install bootstrap probe."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-pending-closer-")
        self.addCleanup(temporary.cleanup)
        self.config = Path(temporary.name) / "pueue.yml"
        self.pending = {"client": Path("pueue"), "config": self.config, "env": {}, "state": Path(temporary.name),
                        "task_ids": [TASK_ID]}
        self.ledger = [self.pending]

    def test_an_absent_config_is_no_closure_and_keeps_the_entry(self):
        with mock.patch.object(supervisor, "shutdown_private_supervisor") as down:
            self.assertIsNone(supervisor.shutdown_pending_private_supervisor(
                self.pending, self.ledger, capture_root=None))
        down.assert_not_called()
        self.assertEqual(self.ledger, [self.pending])

    def test_a_failed_shutdown_keeps_the_entry_and_a_verified_one_removes_it(self):
        self.config.write_text("fixture\n")
        with mock.patch.object(supervisor, "shutdown_private_supervisor", side_effect=RuntimeError("unverified")):
            with self.assertRaisesRegex(RuntimeError, "unverified"):
                supervisor.shutdown_pending_private_supervisor(self.pending, self.ledger, capture_root=None)
        self.assertEqual(self.ledger, [self.pending])
        closure = {"root_id": ROOT_ID, "tasks": {}}
        with mock.patch.object(supervisor, "shutdown_private_supervisor", return_value=closure) as down:
            self.assertIs(supervisor.shutdown_pending_private_supervisor(
                self.pending, self.ledger, capture_root=None), closure)
        self.assertEqual(down.call_args.kwargs["task_ids"], (TASK_ID,))
        self.assertEqual(self.ledger, [])

    def test_the_finalizer_and_the_bootstrap_probe_share_the_one_closer(self):
        self.assertIs(finalization.shutdown_pending_private_supervisor,
                      supervisor.shutdown_pending_private_supervisor)
        self.assertIs(bootstrap.shutdown_pending_private_supervisor,
                      supervisor.shutdown_pending_private_supervisor)
        lifetime = SimpleNamespace(supervisor_cleanups=self.ledger, base_token="capture-token")
        errors = []
        with mock.patch.object(finalization, "shutdown_pending_private_supervisor",
                               side_effect=RuntimeError("unverified")) as closer:
            self.assertEqual(finalization.shutdown_supplemental_supervisors(errors, lifetime), frozenset())
        closer.assert_called_once_with(self.pending, self.ledger, capture_root="capture-token")
        self.assertEqual(len(errors), 1)
        self.assertIn("packaged default supervisor shutdown remained unverified", str(errors[0]))


if __name__ == "__main__":
    unittest.main()
