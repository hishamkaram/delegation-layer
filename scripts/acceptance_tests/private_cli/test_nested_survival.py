"""Negative oracles for the native nested-survival scenario."""

import tempfile
import threading
import time
import fcntl
import os
import unittest
from pathlib import Path
from unittest.mock import patch, Mock
from types import SimpleNamespace

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.private_cli.nested_policy import (
    require_backend, require_challenge, require_descendant_absent, require_setup_margin, require_worker_deadline,
    queue_ended_evidence,
)
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.finalization import release_fixture_workers
from scripts.delegate_acceptance.private_cli.capture import borrow_capture_root, run_captured_text_command
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, rollback_owned_directories
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.private_cli.nested_observation import await_record
from scripts.delegate_acceptance.private_cli.nested_cleanup import fence_creator
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.private_cli.leases import ordinary_lifetime_owner_live
from scripts.delegate_acceptance.private_cli.nested_fixture import wait_gate, dispatch_worker


class NestedSurvivalOracleTests(unittest.TestCase):
    def test_text_capture_passes_absolute_deadline_without_renewing_it(self):
        captured = SimpleNamespace(args=["native-fixture"], returncode=0, stdout=b"ok", stderr=b"")
        token = object()
        with patch("scripts.delegate_acceptance.private_cli.capture.run_captured_command", return_value=captured) as owner:
            response = run_captured_text_command(["native-fixture"], {}, None, capture_root=token, deadline=123.5)
        owner.assert_called_once_with(["native-fixture"], {}, None, capture_root=token, deadline=123.5)
        self.assertEqual(response.stdout, "ok")

    def test_record_wait_distinguishes_absent_from_malformed_existing(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "reply.json"
            entered = threading.Event()

            def publish():
                entered.wait(timeout=1)
                write_json(path, {"nonce": "fresh"})

            writer = threading.Thread(target=publish)
            writer.start()
            try:
                entered.set()
                self.assertEqual(await_record(path, time.monotonic() + 1), {"nonce": "fresh"})
            finally:
                entered.set()
                writer.join(timeout=1)
                self.assertFalse(writer.is_alive())
            path.write_text("not JSON")
            with self.assertRaises(AcceptanceFailure):
                await_record(path, time.monotonic() + 1)

    def test_fenced_leader_acknowledges_but_stays_alive_until_child_cleanup_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fence = root / "fence"
            fence.touch()
            finished = threading.Event()
            results = []

            def observe(_configuration, _capture, pid, output):
                write_json(output, {"pid": pid, "present": True, "process_start": "linux:11"})

            def leader():
                try:
                    results.append(wait_gate(root / "paused", fence, time.time_ns() + 2_000_000_000,
                                              {"nonce": "case-nonce"}, None))
                finally:
                    finished.set()

            with patch("scripts.delegate_acceptance.private_cli.nested_fixture.native_observation", side_effect=observe):
                worker = threading.Thread(target=leader)
                worker.start()
                try:
                    acknowledgement = await_record(root / "fenced.json", time.monotonic() + 1)
                    self.assertEqual(acknowledgement["nonce"], "case-nonce")
                    self.assertFalse(finished.is_set())
                finally:
                    (root / "leader-release").touch()
                    worker.join(timeout=1)
                    self.assertFalse(worker.is_alive())
            self.assertEqual(results, [False])

    def assert_fenced_dispatch(self, *, after_first):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            configuration = {"controls": directory, "nonce": "case-nonce", "delegate": "delegate",
                             "worker_state": "state", "workspace": directory, "w1_brief": "one.md",
                             "w2_brief": "two.md", "w1": "a" * 32, "w2": "b" * 32}
            results, failures = [], []

            def observe(_configuration, _capture, pid, output):
                write_json(output, {"pid": pid, "present": True, "process_start": "fixture-start"})

            def complete_first(*args, **kwargs):
                (root / "fence").touch()

            def creator():
                try:
                    if after_first:
                        results.append(dispatch_worker(configuration, None, "w1"))
                    results.append(dispatch_worker(configuration, None, "w2" if after_first else "w1"))
                except BaseException as error:
                    failures.append(error)

            if not after_first:
                (root / "fence").touch()
            with patch("scripts.delegate_acceptance.private_cli.nested_fixture.native_observation", side_effect=observe), \
                    patch("scripts.delegate_acceptance.private_cli.nested_fixture.run_json",
                          side_effect=complete_first) as invoke:
                worker = threading.Thread(target=creator)
                worker.start()
                try:
                    ack = await_record(root / "fenced.json", time.monotonic() + 2)
                    self.assertEqual((ack["pid"], ack["process_start"], ack["nonce"]),
                                     (os.getpid(), "fixture-start", "case-nonce"))
                    self.assertEqual(invoke.call_count, int(after_first))
                    self.assertFalse((root / (("w2" if after_first else "w1") + "-intent.json")).exists())
                finally:
                    (root / "leader-release").touch()
                    worker.join(timeout=2)
                    self.assertFalse(worker.is_alive())
            self.assertEqual(failures, [])
            self.assertEqual(results, [True, False] if after_first else [False])

    def test_fence_before_first_dispatch_refuses_invocation_and_binds_ack(self):
        self.assert_fenced_dispatch(after_first=False)

    def test_fence_between_worker_dispatches_refuses_second_and_binds_ack(self):
        self.assert_fenced_dispatch(after_first=True)

    def test_repeated_fence_keeps_original_request_for_live_acknowledged_creator(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            controls = root / "controls"
            controls.mkdir()
            task = root / "tasks" / ("a" * 32)
            task.mkdir(parents=True)
            (task / "provider.start").touch()
            response = {"task_id": "a" * 32, "root_id": "b" * 32}
            case = SimpleNamespace(controls=SimpleNamespace(directory=controls), a=root,
                                   configuration={"nonce": "same-case"}, responses={"leader": response},
                                   match_launch=Mock(return_value={"pid": 123}))
            ack = {"pid": 123, "process_start": "native-start", "nonce": "same-case", "observed_at_unix_ns": 150}
            write_json(controls / "fenced.json", ack)
            with patch("scripts.delegate_acceptance.private_cli.nested_cleanup.time.time_ns", side_effect=[100, 200]) as clock:
                fence_creator(case, time.monotonic() + 1)
                original = (controls / "fence").read_bytes()
                fence_creator(case, time.monotonic() + 1)
            clock.assert_called_once()
            self.assertEqual((controls / "fence").read_bytes(), original)
            self.assertEqual(_read_control_record(controls / "fence", "request")["requested_at_unix_ns"], 100)

    def test_guardian_owner_probe_never_creates_a_lock_or_claims_a_busy_owner_free(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owner.lock"
            with self.assertRaises(FileNotFoundError):
                ordinary_lifetime_owner_live(path)
            with path.open("xb") as owner:
                fcntl.flock(owner, fcntl.LOCK_EX | fcntl.LOCK_NB)
                self.assertTrue(ordinary_lifetime_owner_live(path))
            self.assertFalse(ordinary_lifetime_owner_live(path))

    def test_removed_row_only_passes_with_independent_native_closure_and_admission(self):
        receipt = {"root_id": "a" * 32, "task_id": "b" * 32, "numeric_task_id": 0,
                   "label": "delegate:" + "a" * 32 + ":" + "b" * 32}
        closed = {"scope_disposed": True, "owner_released": True, "termination": {"scope_empty": True}}
        self.assertEqual(queue_ended_evidence({"tasks": {}}, receipt, closed)["state"],
                         "removed-after-canonical-native-closure")
        with self.assertRaises(AcceptanceFailure):
            queue_ended_evidence({"tasks": {}}, receipt, {**closed, "owner_released": False})
        row = {"id": 0, "label": receipt["label"], "status": {"Running": {}}}
        self.assertIsNone(queue_ended_evidence({"tasks": {"0": row}}, receipt, closed))
        row["status"] = {"Done": {}}
        self.assertEqual(queue_ended_evidence({"tasks": {"0": row}}, receipt, closed)["state"], "Done")
        row["id"] = 1
        with self.assertRaises(AcceptanceFailure):
            queue_ended_evidence({"tasks": {"1": row}}, receipt, closed)

    def test_nonce_rejects_old_response_and_pid_reuse(self):
        launch = {"pid": 123, "process_start": "linux:11"}
        reply = {"nonce": "fresh", **launch, "observed_at_unix_ns": 101}
        require_challenge(reply, "fresh", launch, 100, 200, same_identity=True)
        for change in ({"nonce": "old"}, {"process_start": "linux:12"}, {"pid": True},
                       {"observed_at_unix_ns": 99}, {"observed_at_unix_ns": 201}):
            with self.subTest(change=change), self.assertRaises(AcceptanceFailure):
                require_challenge({**reply, **change}, "fresh", launch, 100, 200,
                                  same_identity=False if "process_start" in change else True)

    def test_backend_does_not_accept_cooperative_as_exact(self):
        require_backend({"scope_kind": "cgroup-v2", "assurance": "exact"}, "cgroup-v2")
        with self.assertRaises(AcceptanceFailure):
            require_backend({"scope_kind": "process-group", "assurance": "cooperative"}, "cgroup-v2")

    def test_margin_is_not_an_automatic_retry_or_budget_extension(self):
        require_setup_margin(31_000_000_000, 1_000_000_000)
        with self.assertRaisesRegex(AcceptanceFailure, "setup_inconclusive"):
            require_setup_margin(31_000_000_000, 1_000_000_001)

    def test_descendant_self_exit_cannot_pass_containment(self):
        before = {"pid": 123, "process_start": "linux:11", "present": True}
        after = {"pid": 123, "present": False}
        require_descendant_absent(before, after, 101, 100, False, same_identity=False)
        for live, observed, released in ((before, 101, False), (after, 101, True),
                                         (after, 60_000_000_100, False)):
            with self.subTest(observed=observed, released=released), self.assertRaises(AcceptanceFailure):
                require_descendant_absent(before, live, observed, 100, released, same_identity=live is before)

    def test_queued_admission_time_is_not_execution_start(self):
        intent = {"created_at": "2026-11-02T12:00:00Z", "deadline": "2026-11-02T12:09:59Z"}
        require_worker_deadline(intent, 0, 0)
        for deadline in ("2026-11-02T12:01:00Z", "2026-11-02T12:10:01Z"):
            with self.subTest(deadline=deadline), self.assertRaises(AcceptanceFailure):
                require_worker_deadline({**intent, "deadline": deadline}, 0, 0)

    def test_cleanup_hook_precedes_readiness_and_shares_deadline(self):
        lifetime = FixtureLifetime()
        events = []
        lifetime.prepare_cleanup = lambda deadline: events.append(("prepare", deadline))
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            config.touch()
            lifetime.private_config = config
            lifetime.managed_state_roots.append((Path(directory), ["a" * 32]))
            with patch("scripts.delegate_acceptance.private_cli.finalization.await_primary_readiness",
                       side_effect=lambda *args, **kwargs: events.append(("readiness", args[3]))):
                _, ready, deadline = release_fixture_workers(SimpleNamespace(scenario="nested-survival"),
                                                             [], lifetime, False)
        self.assertTrue(ready)
        self.assertEqual(events, [("prepare", deadline), ("readiness", deadline)])

    def test_failed_cleanup_hook_blocks_parent_readiness(self):
        lifetime = FixtureLifetime()
        lifetime.prepare_cleanup = lambda deadline: (_ for _ in ()).throw(RuntimeError("child unresolved"))
        errors = []
        _, ready, _ = release_fixture_workers(SimpleNamespace(scenario="nested-survival"), errors, lifetime, False)
        self.assertFalse(ready)
        self.assertIn("child unresolved", str(errors[0]))

    def test_capture_borrow_fences_replaced_directory_without_taking_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = DirectoryOwner()
            token = owner.create("captures", Path(directory) / "captures", Path(directory))
            try:
                with borrow_capture_root(token.directory, token.identity) as borrowed:
                    borrowed.assert_live()
                self.assertTrue(borrowed.consumed)
                token.assert_live()
                with self.assertRaisesRegex(RuntimeError, "identity changed"):
                    with borrow_capture_root(token.directory, (0, 0)):
                        self.fail("changed capture pin accepted")
            finally:
                rollback_owned_directories(owner.transfer())
