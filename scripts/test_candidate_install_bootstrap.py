"""The install bootstrap probe owns its root, records its original faults and retains every unproven supervisor.

Every case calls the public probe with a no-spawn guard on the one child launch the private CLI capture
owns, installed before any setup. Ownership cases run the real core up to that launch; the shared core is
replaced only to isolate this application's mapping and success disposal. Released pins are proven by the
process's descriptor count. The version-only fake run in test_candidate_install and the native prototype
exercise real children.
"""

from __future__ import annotations

from collections.abc import Sequence
from contextlib import redirect_stdout
import io
import errno
import fcntl
from pathlib import Path
import os
import sys
import tempfile
import time
import unittest
from typing import TypeVar
from types import SimpleNamespace
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.invalid_calls import invoke_invalid
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.directories import operations
from scripts.delegate_acceptance.private_cli import capacity_entry, capture, commands, finalization, leases, packaged_defaults, root_cleanup, supervisor
from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.private_cli.fixture_lifetime import (
    FixtureLifetime, SupervisorCleanup, PredecessorCleanup,
)
from scripts.delegate_acceptance.private_cli.packaged_defaults import (
    INSTALLED_BINARIES, AdmittedTask, InstalledBootstrap, isolated_default_state_path, register_installed_default,
    shutdown_installed_default,
)
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.release_verification import candidate_install_bootstrap as bootstrap
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    BOOTSTRAP_SHUTDOWN, BootstrapTarget,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError, installed_executables


ROOT_ID = "1" * 32
TASK_ID = "2" * 32
# The canonical validator, kept to refuse a state root that is gone; cases otherwise replace its closure read.
REAL_AUTHORIZE = root_cleanup.authorize_managed_lifetime_closure


ExpectedError = TypeVar("ExpectedError", bound=BaseException)


class ProbeCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="candidate-bootstrap-test", label="candidate bootstrap test fixture")
        self.addCleanup(self.tree.dispose)
        # No child ever starts: the guard replaces the capture owner's launch before any setup.
        self.spawns: list[dict[str, str]] = []
        self.commands: list[list[str]] = []
        self.on_spawn = None
        self.guard = mock.patch.object(capture.subprocess, "Popen", self.no_spawn)
        self.guard.start()
        self.addCleanup(self.guard.stop)
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
            supervisor_directory=self.installed, binaries=installed_executables({name: (self.installed / name).resolve() for name in INSTALLED_BINARIES}))
        self.receipt: dict = {"candidate_install": {}}
        self.locations: dict[str, Path] = {}
        self.probe = bootstrap.InstalledBootstrapProbe(receipt=self.receipt, locations=self.locations,
                                                       parent=self.parent)
        self.installs: list = []
        self.disposals: list = []
        # A closed turn's closure record is Go-written; its read is the one edge replaced for the removal.
        closure = mock.patch.object(root_cleanup, "authorize_managed_lifetime_closure", return_value=CLOSURE)
        closure.start()
        self.addCleanup(closure.stop)

    def make_state(self, installed) -> None:
        """The private state root a closed turn leaves: its supervisor directory with the three lock files."""
        for name in ("transition.lock", "bootstrap.lock", ELECTION):
            path = installed.state / ".supervisor" / name
            path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            path.touch(mode=0o600)

    def facts(self) -> dict:
        return self.receipt["candidate_install"]["bootstrap"]

    def no_spawn(self, command, *, env, **_streams):
        """Record the refused launch, run the case's hook, then fail it as the OS would."""
        self.spawns.append(dict(env))
        self.commands.append(list(command))
        if self.on_spawn is not None:
            self.on_spawn(env)
        raise OSError(errno.EPERM, "no child may start in this test")

    def call(self, expected: type[ExpectedError]) -> ExpectedError:
        """Call the public probe once; every pin it held must be released whatever escapes."""
        before = open_descriptors()
        with self.assertRaises(expected) as caught:
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
        def run(installed, *, supervisor_cleanups, capture_root, disposal=None):
            self.installs.append((installed, capture_root))
            self.disposals.append(disposal)
            return outcome(installed, supervisor_cleanups)
        return mock.patch.object(bootstrap, "prove_installed_default_bootstrap", run)

    def pending(self, installed, ledger, *, config: bool) -> dict:
        entry = {"delegate": installed.delegate, "client": installed.supervisor_directory / "pueue", "config": installed.config,
                 "env": installed.env, "state": installed.state, "task_ids": [TASK_ID]}
        if config:
            installed.config.parent.mkdir(parents=True)
            installed.config.write_text("fixture\n")
        ledger.append(entry)
        return entry

    def entry(self, installed, ledger, name: str, *, task_ids: list[str], config: bool) -> dict[str, object]:
        """Register one more supervisor below the probe HOME; return its expected minimal record."""
        state = Path(installed.env["HOME"]) / name
        entry = {"delegate": installed.delegate, "client": installed.supervisor_directory / "pueue", "config": state / ".supervisor" / "pueue.yml",
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
            root_id=ROOT_ID, task_id=TASK_ID, delegate=self.target.binaries.delegate, runner=self.target.binaries.delegate_run,
            client=self.target.binaries.pueue, daemon=self.target.binaries.pueued, outcome="committed", worker_result="success",
            termination_cause="completed", scope_empty=True)
        def closed(installed, _ledger):
            self.make_state(installed)
            return result

        with self.core(closed):
            observation = self.probe(self.target)
        self.assertEqual(observation, fx.bootstrap(self.target.binaries))
        self.assertEqual(observation.shutdown, BOOTSTRAP_SHUTDOWN)
        (installed, capture), = self.installs
        root = self.locations["bootstrap_root"]
        self.assertFalse(root.exists())
        self.assertEqual(root.parent, self.parent)
        self.assertEqual(installed.delegate, self.installed / "delegate")
        self.assertEqual(installed.env["PATH"].split(":")[0], str(self.installed))
        self.assertEqual(installed.env["HOME"], str(root / "h"))
        self.assertEqual(dict(installed.executables), self.target.binaries.as_mapping())
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
            root_id=ROOT_ID, task_id=TASK_ID, delegate=self.target.binaries.delegate, runner=self.target.binaries.delegate_run,
            client=self.target.binaries.pueue, daemon=self.target.binaries.pueued, outcome="committed", worker_result="success",
            termination_cause="completed", scope_empty=True)

        def run(installed, ledger):
            self.make_state(installed)
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


HOLDER = """
import fcntl, os, sys, time
descriptor = os.open(sys.argv[1], os.O_RDWR | os.O_NOFOLLOW)
fcntl.flock(descriptor, fcntl.LOCK_EX)
open(sys.argv[2], "x").close()
deadline = time.monotonic() + 30
while not os.path.exists(sys.argv[3]):
    if time.monotonic() >= deadline:
        sys.exit(3)
    time.sleep(0.01)
"""
ELECTION = leases.DISPOSAL_ELECTION_LOCK
CLOSURE = {"root_id": ROOT_ID, "inspections": {}, "tasks": {
    TASK_ID: {"worker_wait": {"result": "success"}, "termination": {"cause": "completed", "scope_empty": True}}}}
BARRIER_SECONDS = 10


def election_free(state: Path) -> bool:
    """Probe the election lock from a fresh open; this process's own held lease reads as busy too."""
    descriptor = os.open(state / ".supervisor" / ELECTION, os.O_RDWR | os.O_NOFOLLOW)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return True
    except BlockingIOError:
        return False
    finally:
        os.close(descriptor)


class DisposalLeaseTests(ProbeCase):
    """One DisposalLeases owner spans the verification: held from authorized shutdown through the owned delete.

    The shutdown, the lease flocks, the owned root and its canonical remover are real; only the closure
    read and the daemon stop are replaced. The capture launch guard is lifted for the one owned child.
    """

    def setUp(self) -> None:
        super().setUp()
        self.guard.stop()
        self.processes = Processes(self.tree.root / "processes", {"PATH": "/usr/bin:/bin"})
        self.holders = 0
        self.opened: list[Path] = []
        real_open = leases._open_held_disposal

        def counted_open(state):
            self.opened.append(state)
            return real_open(state)

        for patcher in (
                mock.patch.object(leases, "_open_held_disposal", counted_open),
                mock.patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=CLOSURE),
                mock.patch.object(supervisor, "shutdown_observed_supervisor"),
                mock.patch.object(supervisor, "private_shutdown_closed", return_value=False),
                mock.patch.object(supervisor, "private_shutdown_evidence", return_value=(1, Path("endpoint"), (1, 2))),
                mock.patch.object(supervisor, "shutdown_custody_roots", return_value=()),
                mock.patch.object(commands, "run_json", return_value={"root_id": ROOT_ID, "supervisor_owner": {
                    "action": "stop-owner", "state": "not_initialized"}})):
            patcher.start()
            self.addCleanup(patcher.stop)

    def closed_turn(self, installed, ledger, *, disposal, after=None, config=False) -> InstalledBootstrap:
        """The shared core's shutdown step on a real state root: registered, authorized, shut down, entry dropped.

        ``config`` also writes the private config a dispatched supervisor has, which its pending closer requires.
        """
        cleanup = register_installed_default(installed, ledger)
        cleanup["task_ids"].append(TASK_ID)
        self.make_state(installed)
        if config:
            installed.config.write_text("fixture\n")
        admitted = AdmittedTask(root_id=ROOT_ID, task_id=TASK_ID, commands={}, pair=self.installed)
        task = shutdown_installed_default(
            installed, admitted, cleanup, ledger, capture_root=None, disposal=disposal)
        if after is not None:
            after(installed, disposal)
        return InstalledBootstrap(
            root_id=ROOT_ID, task_id=TASK_ID, delegate=self.target.binaries.delegate, runner=self.target.binaries.delegate_run,
            client=self.target.binaries.pueue, daemon=self.target.binaries.pueued, outcome="committed",
            worker_result=task["worker_wait"]["result"], termination_cause="completed", scope_empty=True)

    def shut_down_core(self, *, after=None):
        def run(installed, *, supervisor_cleanups, capture_root, disposal=None):
            self.disposals.append(disposal)
            return self.closed_turn(installed, supervisor_cleanups, disposal=disposal, after=after)
        return mock.patch.object(bootstrap, "prove_installed_default_bootstrap", run)

    def hold_election(self, state: Path):
        """Start an owned child that holds the state's election lock until released; return its release."""
        self.holders += 1
        ready = self.tree.root / f"ready-{self.holders}"
        release = self.tree.root / f"release-{self.holders}"
        lock = state / ".supervisor" / ELECTION
        lock.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        lock.touch(mode=0o600)
        process = self.processes.start(
            "election-holder", [sys.executable, "-I", "-c", HOLDER, lock, ready, release], self.tree.root)

        def stop() -> None:
            release.touch()
            process.wait(timeout=BARRIER_SECONDS)

        self.addCleanup(stop)
        wait_until(lambda: ready.exists() or process.poll() is not None, BARRIER_SECONDS, "election holder entry")
        self.assertTrue(ready.exists(), "the election holder exited before holding the lock")
        return stop

    def test_the_lease_taken_at_shutdown_is_held_through_the_owned_delete_without_reopening(self):
        at_delete: list[tuple[bool, bool]] = []
        real_remove = root_cleanup.remove_private_acceptance_root

        def observed(token, entries, managed_state_roots: Sequence[tuple[Path, Sequence[str]]] = (),
                     *, disposal=None, deadline=None):
            self.assertEqual(len(managed_state_roots), 1)
            state, _task_ids = managed_state_roots[0]
            self.assertIsNotNone(disposal)
            if disposal is None:
                self.fail("shutdown did not retain its disposal lease")
            at_delete.append((disposal.holds(state), election_free(state)))
            return real_remove(token, entries, managed_state_roots, disposal=disposal, deadline=deadline)

        with self.shut_down_core(), mock.patch.object(
                bootstrap, "remove_private_acceptance_root", observed, create=True):
            observation = self.probe(self.target)
        self.assertEqual(observation.task_id, TASK_ID)
        self.assertEqual(at_delete, [(True, False)])
        self.assertEqual(len(self.disposals), 1)
        (state,) = self.opened
        self.assertEqual(state, isolated_default_state_path(self.locations["bootstrap_root"] / "h"))
        self.assertFalse(self.locations["bootstrap_root"].exists())
        self.assertEqual((self.facts()["status"], self.facts()["root_disposition"]), ("verified", "removed"))
        self.assertFalse(self.owner().holds(state))

    def test_an_owned_child_holding_the_election_refuses_the_delete_and_retains_the_root(self):
        stops = []

        def hold_late(installed, disposal):
            if disposal is not None:
                disposal.release(installed.state)
            stops.append(self.hold_election(installed.state))

        before = open_descriptors()
        with self.shut_down_core(after=hold_late), self.assertRaises(VerificationError) as caught:
            self.probe(self.target)
        error = caught.exception
        for stop in stops:
            stop()
        self.assertEqual(open_descriptors(), before, "the probe left owned directory pins open")
        self.assertEqual(error.code, "candidate_bootstrap_cleanup_failed")
        facts = self.facts()
        self.assertEqual((facts["status"], facts["root_disposition"]), ("cleanup-failed", "retained"))
        self.assertIn("disposal lease " + ELECTION, facts["disposal_failure"]["message"])
        root = self.locations["bootstrap_root"]
        self.assertTrue(all((root / name).is_dir() for name in ("c", "h", "p", "w", "r")))
        self.assertFalse(self.owner().holds(isolated_default_state_path(root / "h")))

    def test_a_state_root_renamed_after_the_core_returns_is_refused_and_the_root_is_retained(self):
        sibling = []

        def rename_state(installed, _disposal):
            sibling.append(installed.state.with_name("delegation-layer-moved"))
            os.rename(installed.state, sibling[0])

        with self.shut_down_core(after=rename_state), mock.patch.object(
                root_cleanup, "authorize_managed_lifetime_closure", REAL_AUTHORIZE):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_cleanup_failed")
        facts = self.facts()
        self.assertEqual((facts["status"], facts["root_disposition"]), ("cleanup-failed", "retained"))
        self.assertEqual(facts["disposal_failure"]["type"], "AcceptanceFailure")
        self.assertNotIn("task_id", facts)
        self.assertTrue((sibling[0] / ".supervisor").is_dir())
        root = self.locations["bootstrap_root"]
        self.assertTrue(all((root / name).is_dir() for name in ("c", "h", "p", "w", "r")))

    def test_a_daemon_stop_fault_after_acquisition_recovers_under_the_exact_held_lease(self):
        conflicts: list[str] = []
        real_wait = leases.wait_until

        def bounded_wait(predicate, **kwargs):
            """A fresh transition-lock open conflicts with this process's own held lease; bound it, do not wait."""
            if not predicate():
                conflicts.append(kwargs["description"])
                raise TimeoutError("fresh lock open conflicted with the probe's own held lease")
            return real_wait(lambda: True, **kwargs)

        events: list[str] = []
        stops = iter((RuntimeError("daemon stop failed"), None))

        def stop_owner(_delegate, _env, arguments, **_options):
            state = Path(arguments[1])
            self.assertFalse(self.owner().holds(state), "owner drain must run outside disposal locks")
            events.append("owner")
            return {"root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}

        def stop_daemon(*_args, **_kwargs):
            self.assertTrue(self.owner().holds(self.opened[-1]))
            events.append("daemon")
            outcome = next(stops)
            if outcome is not None:
                raise outcome

        def failing_core(installed, *, supervisor_cleanups, capture_root, disposal=None):
            self.disposals.append(disposal)
            return self.closed_turn(installed, supervisor_cleanups, disposal=disposal, config=True)

        with mock.patch.object(bootstrap, "prove_installed_default_bootstrap", failing_core), \
                mock.patch.object(supervisor, "shutdown_observed_supervisor", stop_daemon), \
                mock.patch.object(commands, "run_json", stop_owner), \
                mock.patch.object(leases, "wait_until", bounded_wait):
            error = self.call(VerificationError)
        self.assertEqual(conflicts, [])
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        facts = self.facts()
        self.assertEqual(facts["failure"], {"type": "RuntimeError", "message": "daemon stop failed"})
        self.assertNotIn("cleanup_failures", facts)
        self.assertNotIn("unresolved_ownership", facts)
        state = isolated_default_state_path(self.locations["bootstrap_root"] / "h")
        self.assertEqual(self.opened, [state])
        self.assertEqual(events, ["owner", "daemon", "daemon"])
        self.assertEqual(facts["root_disposition"], "retained")
        self.assertTrue(self.locations["bootstrap_root"].is_dir())
        self.assertFalse(self.owner().holds(self.opened[0]))
        self.assertTrue(election_free(self.opened[0]))

    def test_a_failed_attempt_keeps_the_root_and_its_original_fault_and_frees_the_lease(self):
        def failed(_installed, _disposal):
            raise RuntimeError("collect did not publish success")

        with self.shut_down_core(after=failed):
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        facts = self.facts()
        self.assertEqual(facts["failure"], {"type": "RuntimeError", "message": "collect did not publish success"})
        self.assertEqual(facts["root_disposition"], "retained")
        root = self.locations["bootstrap_root"]
        self.assertTrue(root.is_dir())
        state = isolated_default_state_path(root / "h")
        self.assertTrue(election_free(state))
        self.assertFalse(self.owner().holds(state))

    def test_an_interrupted_attempt_keeps_the_root_and_frees_the_lease(self):
        def interrupted(_installed, _disposal):
            raise KeyboardInterrupt

        with self.shut_down_core(after=interrupted):
            self.call(KeyboardInterrupt)
        self.assertEqual((self.facts()["status"], self.facts()["root_disposition"]), ("interrupted", "retained"))
        state = isolated_default_state_path(self.locations["bootstrap_root"] / "h")
        self.assertTrue(election_free(state))
        self.assertFalse(self.owner().holds(state))

    def owner(self) -> leases.DisposalLeases:
        """The one disposal owner the probe passed to the shared core."""
        self.assertEqual(len(self.disposals), 1)
        self.assertIsInstance(self.disposals[0], leases.DisposalLeases)
        return self.disposals[0]

    def failing_release(self):
        real = leases.DisposalLeases.release_all

        def release_all(owner):
            real(owner)
            raise OSError(errno.EIO, "close failed")
        return mock.patch.object(leases.DisposalLeases, "release_all", release_all)

    def test_a_lease_close_fault_after_the_delete_reports_cleanup_failed_with_the_actual_removal(self):
        with self.shut_down_core(), self.failing_release():
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_cleanup_failed")
        facts = self.facts()
        self.assertEqual((facts["status"], facts["root_disposition"]), ("cleanup-failed", "removed"))
        self.assertEqual(facts["lease_release_failure"]["type"], "OSError")
        self.assertFalse(self.locations["bootstrap_root"].exists())

    def test_a_lease_close_fault_never_replaces_the_original_failure(self):
        def failed(_installed, _disposal):
            raise RuntimeError("collect did not publish success")

        with self.shut_down_core(after=failed), self.failing_release():
            error = self.call(VerificationError)
        self.assertEqual(error.code, "candidate_bootstrap_failed")
        facts = self.facts()
        self.assertEqual(facts["failure"]["message"], "collect did not publish success")
        self.assertEqual(facts["root_disposition"], "retained")
        self.assertEqual(facts["lease_release_failure"]["type"], "OSError")
        self.assertTrue(self.locations["bootstrap_root"].is_dir())


class FailureTests(ProbeCase):
    """Each replaced core launches no child, so the tree may remove the retained root afterwards."""

    def test_a_delegate_resolving_elsewhere_runs_nothing_and_keeps_the_root(self):
        shadow = self.tree.root / "shadow"
        shadow.mkdir()
        (shadow / "delegate").write_bytes(b"#!/bin/sh\nexit 0\n")
        (shadow / "delegate").chmod(0o755)
        self.target = BootstrapTarget(command_directory=shadow, delegate=self.target.delegate,
                                      supervisor_directory=self.installed, binaries=installed_executables({name: (self.installed / name).resolve() for name in INSTALLED_BINARIES}))
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
        self.assertEqual(facts["cleanup_failures"][0]["type"], "AcceptanceFailure")
        self.assertEqual(facts["cleanup_failures"][0]["message"], "managed lifetime root record is not a regular record")
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
        self.assertIsInstance(closer.call_args.kwargs["disposal"], leases.DisposalLeases)
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

        def closed(pending, ledger, *, capture_root, disposal):
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
        self.pending: SupervisorCleanup = {"delegate": Path("delegate"), "client": Path("pueue"), "config": self.config, "env": {}, "state": Path(temporary.name),
                        "task_ids": [TASK_ID]}
        self.ledger: list[SupervisorCleanup | PredecessorCleanup] = [self.pending]

    def test_an_absent_config_is_no_closure_and_keeps_the_entry(self):
        with mock.patch.object(supervisor, "close_private_root", side_effect=RuntimeError("unverified")) as down:
            with self.assertRaisesRegex(RuntimeError, "unverified"):
                supervisor.shutdown_pending_private_supervisor(
                self.pending, self.ledger, capture_root=None)
        down.assert_called_once()
        self.assertEqual(self.ledger, [self.pending])

    def test_a_failed_shutdown_keeps_the_entry_and_a_verified_one_removes_it(self):
        self.config.write_text("fixture\n")
        with mock.patch.object(supervisor, "close_private_root", side_effect=RuntimeError("unverified")):
            with self.assertRaisesRegex(RuntimeError, "unverified"):
                supervisor.shutdown_pending_private_supervisor(self.pending, self.ledger, capture_root=None)
        self.assertEqual(self.ledger, [self.pending])
        closure = {"root_id": ROOT_ID, "tasks": {}}
        with mock.patch.object(supervisor, "close_private_root", return_value=closure) as down:
            self.assertIs(supervisor.shutdown_pending_private_supervisor(
                self.pending, self.ledger, capture_root=None), closure)
        self.assertIs(down.call_args.args[0], self.pending)
        self.assertEqual(tuple(down.call_args.args[0]["task_ids"]), (TASK_ID,))
        self.assertEqual(self.ledger, [])

    def test_the_finalizer_and_the_bootstrap_probe_share_the_one_closer(self):
        self.assertIs(finalization.shutdown_pending_private_supervisor,
                      supervisor.shutdown_pending_private_supervisor)
        self.assertIs(bootstrap.shutdown_pending_private_supervisor,
                      supervisor.shutdown_pending_private_supervisor)
        lifetime = FixtureLifetime(
            supervisor_cleanups=self.ledger, capture_token=mock.Mock(spec=operations.OwnedDirectory))
        errors = []
        disposal = object()
        with mock.patch.object(finalization, "shutdown_pending_private_supervisor",
                               side_effect=RuntimeError("unverified")) as closer:
            self.assertEqual(
                finalization.shutdown_supplemental_supervisors(errors, lifetime, disposal=disposal), frozenset())
        closer.assert_called_once_with(
            self.pending, self.ledger, capture_root=lifetime.capture_token, disposal=disposal,
            reader=None, deadline=None, closed_roots=lifetime.closed_roots)
        self.assertEqual(len(errors), 1)
        self.assertIn("packaged default supervisor shutdown remained unverified", str(errors[0]))



class TerminalOwnerShutdownTests(ProbeCase):
    """The real installed-default consumer retains its ledger until both owned actors close."""

    def setUp(self) -> None:
        super().setUp()
        self.disposal = leases.DisposalLeases()
        self.addCleanup(self.disposal.release_all)
        self.state = self.tree.root / "terminal-state"
        self.installation = packaged_defaults.InstalledDefaults(
            delegate=self.target.delegate, supervisor_directory=self.installed,
            executables=tuple((name, self.installed / name) for name in INSTALLED_BINARIES),
            env={"HOME": str(self.parent)}, state=self.state, workspace=self.tree.root,
            brief=self.tree.root / "brief.md", response_directory=self.tree.root)
        self.ledger: list[SupervisorCleanup] = []
        self.cleanup = register_installed_default(self.installation, self.ledger)
        self.cleanup["task_ids"].append(TASK_ID)
        self.make_state(self.installation)
        self.installation.config.write_text("fixture\n")
        self.admitted = AdmittedTask(root_id=ROOT_ID, task_id=TASK_ID, commands={}, pair=self.installed)
        self.events: list[str] = []
        self.reply: dict[str, object] = {"root_id": ROOT_ID, "supervisor_owner": {
            "action": "stop-owner", "state": "serving_released", "process_closure": "pending",
            "startup": {"sequence": 1, "epoch": "c" * 32, "intent_sha256": "d" * 64},
            "process": {"pid": 31415, "start_token": "selected-start"}}}
        self.stop = self.patch(commands, "run_json", side_effect=self.stop_owner)
        self.canonical = self.patch(supervisor, "authorize_managed_lifetime_closure", return_value=CLOSURE)
        self.patch(supervisor, "shutdown_custody_roots", return_value=())
        self.patch(supervisor, "private_shutdown_evidence", return_value=(27182, Path("endpoint"), (1, 2)))
        self.daemon_present = True
        self.daemon = self.patch(supervisor, "shutdown_observed_supervisor", side_effect=self.close_daemon)
        self.absence = mock.Mock(side_effect=self.owner_gone)
        self.patch(supervisor, "private_daemon_process_gone",
                   side_effect=lambda pid, deadline, **options: not self.daemon_present if pid == 27182
                   else self.absence(pid, deadline, **options))
        self.patch(supervisor, "private_shutdown_endpoint_closed", return_value=True)
        self.patch(supervisor.time, "sleep")

    def patch(self, target, name, **options):
        patcher = mock.patch.object(target, name, **options)
        self.addCleanup(patcher.stop)
        return patcher.start()

    def stop_owner(self, delegate, _env, arguments, **options):
        self.assertEqual(delegate, self.target.delegate)
        self.assertEqual(arguments, ["--root", str(self.state), "supervisor", "stop-owner", "--json"])
        self.assertIsNone(options["timeout"])
        self.assertEqual(options["expected"], (0, 1, 3))
        self.assertFalse(self.disposal.holds(self.state))
        self.assertEqual(self.ledger, [self.cleanup])
        self.events.append("stop")
        return self.reply

    def close_daemon(self, *_args, **_options):
        self.assertTrue(self.disposal.holds(self.state))
        self.assertEqual(self.ledger, [self.cleanup])
        self.events.append("daemon")
        self.daemon_present = False

    def owner_gone(self, pid, deadline, *, capture_root):
        self.assertEqual(pid, 31415)
        self.assertIsNone(capture_root)
        self.assertGreater(deadline, time.monotonic())
        self.assertEqual(self.ledger, [self.cleanup])
        self.events.append("owner-absent")
        return True

    def shutdown(self):
        return shutdown_installed_default(self.installation, self.admitted, self.cleanup, self.ledger,
                                          capture_root=None, disposal=self.disposal)

    def test_installed_default_stops_owner_before_daemon_and_drops_ledger_only_after_pid_absence(self):
        self.shutdown()
        self.assertEqual(self.events, ["stop", "daemon", "owner-absent"])
        self.stop.assert_called_once()
        self.assertEqual(self.ledger, [])
        self.assertTrue(self.disposal.holds(self.state))

    def test_pending_failure_cleanup_uses_the_same_protocol_and_original_deadline(self):
        deadline = time.monotonic() + 1
        supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                       disposal=self.disposal, deadline=deadline)
        self.assertEqual(self.events, ["stop", "daemon", "owner-absent"])
        self.assertEqual(self.stop.call_args.kwargs["deadline"], deadline)
        self.assertEqual(self.absence.call_args.args[1], deadline)
        self.assertEqual(self.ledger, [])

    def test_daemon_failure_and_successor_never_replace_the_original_selected_owner(self):
        self.daemon.side_effect = RuntimeError("first daemon stop failed")
        with self.assertRaisesRegex(RuntimeError, "first daemon stop failed"):
            self.shutdown()
        selected = self.cleanup["owner"]
        self.assertIsNotNone(selected)
        self.assertEqual(self.cleanup["root_id"], ROOT_ID)
        self.assertTrue(self.disposal.holds(self.state))
        # The public current-owner endpoint would now select O2. Resume must never ask it.
        self.reply = {"root_id": ROOT_ID, "supervisor_owner": {
            "action": "stop-owner", "state": "serving_released", "process_closure": "pending",
            "startup": {"sequence": 2, "epoch": "e" * 32, "intent_sha256": "f" * 64},
            "process": {"pid": 31416, "start_token": "successor-start"}}}
        self.daemon.side_effect = self.close_daemon
        observations = iter((False, True))
        def absent(pid, deadline, *, capture_root):
            self.owner_gone(pid, deadline, capture_root=capture_root)
            self.assertIs(self.cleanup["owner"], selected)
            return next(observations)
        self.absence.side_effect = absent
        supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                       disposal=self.disposal)
        self.stop.assert_called_once()
        self.assertEqual(self.events, ["stop", "daemon", "owner-absent", "owner-absent"])
        self.assertEqual(self.ledger, [])

    def test_pending_resume_rejects_changed_root_before_daemon_or_process_observation(self):
        self.daemon.side_effect = RuntimeError("first daemon stop failed")
        with self.assertRaisesRegex(RuntimeError, "first daemon stop failed"):
            self.shutdown()
        selected = self.cleanup["owner"]
        self.canonical.return_value = {**CLOSURE, "root_id": "f" * 32}
        with self.assertRaisesRegex(RuntimeError, "originally selected root"):
            supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                           disposal=self.disposal)
        self.stop.assert_called_once()
        self.daemon.assert_called_once()
        self.absence.assert_not_called()
        self.assertIs(self.cleanup["owner"], selected)
        self.assertEqual(self.ledger, [self.cleanup])

    def test_owner_acknowledgement_keeps_the_ledger_while_actual_pid_absence_is_pending(self):
        observations = iter((False, False, True))
        def absent(pid, deadline, *, capture_root):
            self.owner_gone(pid, deadline, capture_root=capture_root)
            self.assertTrue(self.disposal.holds(self.state))
            return next(observations)
        self.absence.side_effect = absent
        self.shutdown()
        self.assertEqual(self.events, ["stop", "daemon", "owner-absent", "owner-absent", "owner-absent"])
        self.stop.assert_called_once()
        self.assertEqual(self.ledger, [])

    def test_busy_foreign_and_malformed_owner_results_keep_ledger_and_never_shutdown_daemon(self):
        responses: tuple[dict[str, object], ...] = (
            {"root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "busy"}},
            {**self.reply, "root_id": "f" * 32},
            {"root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "serving_released"}},
        )
        original = self.cleanup.copy()
        for response in responses:
            with self.subTest(response=response):
                self.cleanup = original.copy()
                self.ledger[:] = [self.cleanup]
                self.reply = response
                with self.assertRaises(RuntimeError):
                    self.shutdown()
                self.assertEqual(self.ledger, [self.cleanup])
                self.daemon.assert_not_called()
                self.absence.assert_not_called()
                self.assertFalse(self.disposal.holds(self.state))

    def test_absent_config_still_stops_and_observes_the_selected_owner(self):
        self.installation.config.unlink()
        self.daemon_present = False
        self.shutdown()
        self.assertEqual(self.events, ["stop", "owner-absent"])
        self.daemon.assert_not_called()
        self.assertEqual(self.ledger, [])
        self.assertTrue(self.disposal.holds(self.state))

    def test_warm_legacy_without_resident_still_closes_daemon_with_config_present(self):
        self.reply = {"root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}
        self.shutdown()
        self.assertEqual(self.events, ["stop", "daemon"])
        self.absence.assert_not_called()
        self.assertEqual(self.ledger, [])

    def test_unknown_owner_absence_retains_pending_failure_ledger_and_original_error(self):
        self.absence.side_effect = RuntimeError("selected owner presence is uncertain")
        with self.assertRaisesRegex(RuntimeError, "selected owner presence is uncertain"):
            supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                           disposal=self.disposal)
        self.stop.assert_called_once()
        self.assertEqual(self.events, ["stop", "daemon"])
        self.assertEqual(self.ledger, [self.cleanup])
        self.assertTrue(self.disposal.holds(self.state))

    def test_public_readiness_observer_runs_before_stop_and_never_after_it(self):
        deadline = time.monotonic() + 1
        observer = mock.Mock(side_effect=lambda *_args: self.events.append("readiness") or ROOT_ID)
        with mock.patch.object(supervisor, "disposal_ready_now", return_value=None):
            supervisor.shutdown_terminal_private_supervisor(
                self.target.delegate, self.installed / "pueue", self.installation.config, self.installation.env,
                state=self.state, task_ids=(TASK_ID,), capture_root=None, disposal=self.disposal,
                deadline=deadline, observe=observer, supervisor_cleanups=self.ledger)
        self.assertEqual(self.events, ["readiness", "stop", "daemon", "owner-absent"])
        observer.assert_called_once_with(TASK_ID, deadline)
        self.stop.assert_called_once()

    def test_completed_terminal_root_reuses_existing_closed_root_proof_after_later_recording_failure(self):
        lifetime = FixtureLifetime(delegate=self.target.delegate, private_config=self.installation.config,
                                   attempted_task_ids=[TASK_ID], disposal=self.disposal)
        deadline = time.monotonic() + 1
        closure = supervisor.shutdown_terminal_private_supervisor(
            self.target.delegate, self.installed / "pueue", self.installation.config, self.installation.env,
            state=self.state, task_ids=(TASK_ID,), capture_root=None, disposal=self.disposal,
            deadline=deadline, closed_roots=lifetime.closed_roots, supervisor_cleanups=self.ledger)
        recorded = lifetime.closed_roots[self.state]
        self.assertIsNotNone(recorded)
        if recorded is None:
            self.fail("terminal closure lost its held disposal proof")
        self.assertIs(recorded["closure"], closure)
        self.assertIs(recorded["disposal"], self.disposal.require_held(self.state))
        errors = []
        finalization.shutdown_primary_supervisor(
            SimpleNamespace(pueue=self.installed / "pueue"), errors, None, lifetime, True, False,
            predecessor_roots=frozenset(), deadline=deadline, disposal=self.disposal)
        self.assertEqual(errors, [])
        self.stop.assert_called_once()
        self.daemon.assert_called_once()
        self.absence.assert_called_once()

    def test_lost_response_after_stop_effect_never_selects_a_successor_on_failure_cleanup(self):
        def lost_response(*args, **options):
            self.stop_owner(*args, **options)
            raise ValueError("released owner response was not valid JSON")
        self.stop.side_effect = lost_response
        with self.assertRaisesRegex(ValueError, "not valid JSON"):
            self.shutdown()
        self.assertEqual(self.cleanup["owner"], "unresolved")
        self.assertEqual(self.cleanup["root_id"], ROOT_ID)
        self.reply = {"root_id": ROOT_ID, "supervisor_owner": {
            "action": "stop-owner", "state": "serving_released",
            "startup": {"sequence": 2, "epoch": "e" * 32, "intent_sha256": "f" * 64},
            "process": {"pid": 31416, "start_token": "successor-start"}}}
        self.stop.side_effect = self.stop_owner
        with self.assertRaisesRegex(RuntimeError, "stop outcome unknown for terminal root"):
            supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                           disposal=self.disposal)
        self.stop.assert_called_once()
        self.daemon.assert_not_called()
        self.absence.assert_not_called()
        self.assertEqual(self.ledger, [self.cleanup])
        facts: dict[str, object] = {}
        unresolved = bootstrap._publish_unresolved(self.ledger, facts)
        self.assertEqual(unresolved[0]["owner"], "unresolved")
        self.assertEqual(unresolved[0]["root_id"], ROOT_ID)

    def test_verified_daemon_close_then_owner_wait_fault_resumes_original_pid_without_daemon_command(self):
        self.absence.side_effect = RuntimeError("owner observation failed after daemon close")
        with self.assertRaisesRegex(RuntimeError, "owner observation failed"):
            self.shutdown()
        evidence = self.cleanup["daemon"]
        self.assertEqual(evidence, (27182, Path("endpoint"), (1, 2)))
        self.assertEqual(self.ledger, [self.cleanup])
        self.assertTrue(self.disposal.holds(self.state))
        self.assertFalse(self.daemon_present)
        self.absence.side_effect = self.owner_gone
        with mock.patch.object(supervisor, "private_shutdown_evidence",
                               side_effect=AssertionError("closed daemon metadata was reopened")):
            supervisor.shutdown_pending_private_supervisor(self.cleanup, self.ledger, capture_root=None,
                                                           disposal=self.disposal)
        self.stop.assert_called_once()
        self.daemon.assert_called_once()
        self.assertIs(self.cleanup["daemon"], evidence)
        self.assertEqual(self.events, ["stop", "daemon", "owner-absent"])
        self.assertEqual(self.ledger, [])

    def test_finalizer_suppresses_public_observer_for_every_attempted_stop_value(self):
        subject: supervisor.SelectedOwnerSubject = {"startup": {"sequence": 1, "epoch": "c" * 32, "intent_sha256": "d" * 64},
                   "process": {"pid": 31415, "start_token": "selected-start"}}
        for progress in (None, "unresolved", subject):
            with self.subTest(progress=progress):
                pending = self.cleanup.copy()
                pending["root_id"] = ROOT_ID
                pending["owner"] = progress
                observer = mock.Mock(side_effect=AssertionError("public observer ran after stop attempt"))
                lifetime = FixtureLifetime(private_config=self.installation.config, env=self.installation.env,
                                           managed_state_roots=[(self.state, [TASK_ID])], primary_observer=observer,
                                           supervisor_cleanups=[pending], disposal=self.disposal)
                self.canonical.side_effect = [AcceptanceFailure("not ready yet"), CLOSURE]
                errors = []
                _binding, ready, _deadline, failed = finalization.release_fixture_workers(
                    SimpleNamespace(scenario="capacity"), errors, lifetime, False, disposal=self.disposal)
                self.assertTrue(ready)
                self.assertFalse(failed)
                self.assertEqual(errors, [])
                observer.assert_not_called()
        self.stop.assert_not_called()

    def test_capacity_partial_terminal_failure_hands_same_selection_to_one_finalizer_route(self):
        self.ledger.clear()
        lifetime = FixtureLifetime(
            base=self.tree.root, delegate=self.target.delegate, private_config=self.installation.config,
            env=self.installation.env, old_libexec=self.installed, upgraded_libexec=self.installed,
            gate=self.tree.root / "capacity-gate", managed_state_roots=[(self.state, [TASK_ID])],
            attempted_task_ids=[TASK_ID], disposal=self.disposal)
        self.addCleanup(lambda: lifetime.release_gate.release() if lifetime.release_gate is not None else None)
        fixture = FixtureInstallation(
            delegate_binary=self.target.delegate, runner_binary=self.target.binaries.delegate_run,
            upgraded_delegate_binary=self.target.delegate, upgraded_runner_binary=self.target.binaries.delegate_run,
            pueue_binary=self.target.binaries.pueue, pueued_binary=self.target.binaries.pueued,
            workspace=self.tree.root, provider_bin=self.tree.root, brief=self.tree.root / "brief.md",
            upgraded_install=self.installed, old_daemon_marker=self.tree.root / "old-starts",
            upgraded_daemon_marker=self.tree.root / "new-starts", state=self.state,
            waiter_directory=self.tree.root / "waiters", lease_nonce="e" * 32,
            packaged_sources=(), source_root=self.tree.root)
        receipt = AcceptanceReceipt(self.tree.root / "capacity-receipt.json", 1, 1,
                                    {"pueue": self.target.binaries.pueue, "pueued": self.target.binaries.pueued})
        failure = RuntimeError("capacity terminal daemon fault")
        def select(delegate, _env, arguments, **options):
            pending = lifetime.supervisor_cleanups[0]
            if "predecessor" in pending:
                self.fail("capacity terminal cleanup selected a predecessor entry")
            self.cleanup = pending
            self.assertEqual(delegate, self.target.delegate)
            self.assertEqual(arguments, ["--root", str(self.state), "supervisor", "stop-owner", "--json"])
            self.assertIsNone(options["timeout"])
            self.assertFalse(self.disposal.holds(self.state))
            self.events.append("stop")
            return self.reply
        def daemon_fault(*_args, **_options):
            self.assertTrue(self.disposal.holds(self.state))
            self.assertEqual(lifetime.supervisor_cleanups, [self.cleanup])
            self.events.append("daemon")
            raise failure
        self.stop.side_effect = select
        self.daemon.side_effect = daemon_fault
        with (mock.patch.object(capacity_entry, "pueue_snapshot", return_value={"groups": {"default": {"parallel_tasks": 0}}}),
              mock.patch.object(capacity_entry, "verify_current_installation_upgrade", return_value=([], 1024))):
            with self.assertRaisesRegex(RuntimeError, "capacity terminal daemon fault"):
                capacity_entry.run_capacity_scenario(args=SimpleNamespace(parallel_tasks=1), dispatch_workers=1,
                    lease_nonce="e" * 32, lifetime=lifetime, response_directory=self.tree.root,
                    run_receipt=receipt, fixture=fixture, initial=None)
        original = self.cleanup["owner"]
        output = io.StringIO()
        with mock.patch.object(receipt, "write", wraps=receipt.write) as written, redirect_stdout(output):
            finalization.finalize_fixture(SimpleNamespace(scenario="capacity", output=receipt.output,
                                                         pueue=self.installed / "pueue"),
                                          failure, lifetime, receipt, False)
        self.stop.assert_called_once()
        self.assertEqual(self.daemon.call_count, 2)
        self.assertEqual(output.getvalue().count("packaged default supervisor shutdown remained unverified"), 1)
        self.assertIs(self.cleanup["owner"], original)
        self.assertEqual(lifetime.supervisor_cleanups, [self.cleanup])
        self.assertIs(written.call_args.args[2], failure)
        self.assertEqual(written.call_args.args[1]["pending_supervisor_count"], 1)
        self.assertFalse(self.disposal.holds(self.state))

    def test_root_changed_during_lease_acquisition_refuses_daemon_io(self):
        self.canonical.side_effect = [CLOSURE, CLOSURE, {**CLOSURE, "root_id": "f" * 32}]
        with self.assertRaisesRegex(RuntimeError, "originally selected root"):
            self.shutdown()
        self.stop.assert_called_once()
        self.daemon.assert_not_called()
        self.absence.assert_not_called()
        self.assertNotIn("daemon", self.cleanup)
        self.assertEqual(self.ledger, [self.cleanup])

    def test_missing_selected_delegate_retains_ledger_without_launching_any_control(self):
        pending = dict(self.cleanup)
        del pending["delegate"]
        ledger = [pending]
        with self.assertRaisesRegex(RuntimeError, "selected installed Delegate"):
            invoke_invalid(supervisor.shutdown_pending_private_supervisor, pending, ledger,
                           capture_root=None, disposal=self.disposal)
        self.stop.assert_not_called()
        self.daemon.assert_not_called()
        self.assertEqual(ledger, [pending])

if __name__ == "__main__":
    unittest.main()
