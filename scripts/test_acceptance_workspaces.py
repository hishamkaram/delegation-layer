"""Deterministic policy and evidence tests for the managed-workspace driver."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import os
import subprocess
import tempfile
import unittest
from scripts.acceptance_tests.record_fixtures import array_at, object_at
from unittest import mock

import acceptance_workspaces as driver
from scripts.delegate_acceptance.private_cli import supervisor as private_supervisor
import scripts.delegate_acceptance.workspaces.scenarios.application as scenario_application
import scripts.delegate_acceptance.workspaces.scenarios.capacity as scenario_capacity
import scripts.delegate_acceptance.workspaces.scenarios.lifecycle as scenario_lifecycle
import scripts.delegate_acceptance.workspaces.scenarios.operations as scenario_operations
import scripts.delegate_acceptance.workspaces.scenarios.workspace_operations as scenario_workspace_operations
from scripts.acceptance_supervisor_common import DirectoryOwner, Processes


TASK_ID = "a" * 32
WORKSPACE_ID = "b" * 32
OPERATION_ID = "c" * 32
SUBJECT_SHA = "d" * 64
BASE_OID = "1" * 40
HEAD_OID = "2" * 40
TREE_OID = "3" * 40
TERMINATION_SHA = "4" * 64
WAIT_SHA = "5" * 64


class RecordFixtureTraversalTests(unittest.TestCase):
    def test_traversal_returns_original_containers_for_mutation(self):
        record: dict[str, object] = {"value": 1}
        items = [record]
        root = {"items": items}
        self.assertIs(array_at(root, "items"), items)
        self.assertIs(object_at(root, "items", -1), record)
        object_at(root, "items", 0)["value"] = "changed"
        self.assertEqual(record["value"], "changed")

    def test_malformed_fixture_paths_are_explicit_failures(self):
        for root, path in (({}, ("missing",)), ({"items": []}, ("items", 0)),
                           ({"items": [{}]}, ("items", True)), ([], ("field",)),
                           ({1: {}}, ("field",)), ({"items": 1}, ("items", 0))):
            with self.subTest(root=root, path=path), self.assertRaises(AssertionError):
                object_at(root, *path)
        with self.assertRaises(AssertionError):
            array_at({"items": {}}, "items")
        with self.assertRaises(AssertionError):
            object_at({"items": []}, "items")


class ManagedWorkspaceDriverTests(unittest.TestCase):
    def test_root_closure_waits_for_capture_after_provider_publication(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        proof = {"tasks": {TASK_ID: {}}, "workspace": {OPERATION_ID: {}}}
        pending = driver.private_cli.AcceptanceFailure(
            "workspace operation lifetime owner is still active: " + OPERATION_ID)
        with (
            mock.patch.object(private_supervisor, "authorize_managed_lifetime_closure",
                              side_effect=[pending, proof]) as authorize,
            mock.patch.object(driver.private_cli.time, "sleep") as wait,
        ):
            self.assertEqual(driver._prove_lifetimes_closed(context, [TASK_ID]), proof)
        self.assertEqual(authorize.call_args_list, [
            mock.call(context.state, (TASK_ID,)), mock.call(context.state, (TASK_ID,)),
        ])
        wait.assert_called_once()

    def test_root_closure_deadline_retains_the_validation_failure(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        fault = driver.private_cli.AcceptanceFailure("capture identity mismatch")
        with (
            mock.patch.object(private_supervisor, "authorize_managed_lifetime_closure",
                              side_effect=fault),
            mock.patch.object(driver.private_cli.time, "monotonic",
                              side_effect=[0, driver.WAIT_SECONDS]),
            mock.patch.object(driver.private_cli.time, "sleep") as wait,
        ):
            with self.assertRaisesRegex(RuntimeError, "closure remained incomplete.*capture identity mismatch") as raised:
                driver._prove_lifetimes_closed(context, [TASK_ID])
        self.assertIs(raised.exception.__cause__, fault)
        wait.assert_not_called()

    def test_capacity_waits_for_provider_fifo_after_preflight_launch(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            task = {"task_id": TASK_ID, "workspace_id": WORKSPACE_ID, "root_id": "e" * 32}
            preflight = {"task_id": TASK_ID, "pid": 101, "path": base / "preflight.fifo"}
            provider_path = base / "provider.fifo"
            provider = {"task_id": TASK_ID, "pid": 102, "path": provider_path}
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.gate_path, context.state = base / "gate", base / "state"
            context.gates, context.cohorts = [], []
            context.waiter_directory, context.lease_nonce = base, "f" * 32
            published = False

            def specifications(*_arguments):
                return [provider if published else preflight]

            def publish_provider(_delay):
                nonlocal published
                os.mkfifo(provider_path, 0o600)
                published = True

            def acquire(candidates, _nonce):
                self.assertEqual(candidates, [provider])
                self.assertTrue(provider_path.exists())
                raise RuntimeError("expected provider cohort reached")

            with (
                mock.patch.object(driver.private_cli.LockedReleaseGate, "acquire"),
                mock.patch.object(scenario_capacity, "_dispatch_capacity_one", return_value=task),
                mock.patch.object(scenario_capacity, "_adopt_workspace_container"),
                mock.patch.object(
                    scenario_capacity.private_cli_capacity_records,
                    "capacity_lease_specifications", side_effect=specifications,
                ),
                mock.patch.object(driver.time, "sleep", side_effect=publish_provider),
                mock.patch.object(driver.private_cli.LiveFIFOLeaseCohort, "acquire", side_effect=acquire) as cohort,
            ):
                with self.assertRaisesRegex(RuntimeError, "expected provider cohort reached"):
                    driver._run_capacity(context, 1, 1)
                cohort.assert_called_once()

    def test_workspace_watch_keeps_transport_budget_for_accepted_scheduling(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")

        def delayed_control(_context, arguments, *, timeout, **_options):
            # A valid accepted scheduling transaction takes longer than the
            # requested observation window, while remaining finitely bounded.
            if timeout < 20:
                raise subprocess.TimeoutExpired(arguments, timeout)
            return 0, {"workspace": {"id": WORKSPACE_ID, "state": "retained",
                                     "path": "/checkout"}}

        with mock.patch.object(scenario_operations, "_control", side_effect=delayed_control):
            response = driver._wait_workspace(context, WORKSPACE_ID, timeout=5)
        self.assertEqual(object_at(response, "workspace")["state"], "retained")

    def test_capacity_failure_retains_completed_native_cohort_proof(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/missing-proof-root/state")
        context.source_baseline = {}
        context.native_proofs = []
        proof = {"scenario": "capacity", "counted_task_count": 200,
                 "lease_probe": "authenticated-native-cohort"}

        def failed_cleanup(_context, _count, _workers):
            context.native_proofs.append(proof)
            raise RuntimeError("cleanup observation failed")

        with tempfile.TemporaryDirectory() as temporary:
            receipt = Path(temporary) / "receipt.json"
            finalizer = driver.ReceiptFinalizer(receipt, [], {})
            with (
                mock.patch.object(scenario_application, "build_source_manifest", return_value={}),
                mock.patch.object(scenario_application, "_run_capacity", side_effect=failed_cleanup),
            ):
                with self.assertRaisesRegex(RuntimeError, "cleanup observation failed"):
                    driver._run(mock.Mock(scenario="capacity", parallel_tasks=200,
                                          dispatch_workers=16), finalizer, context)
            finalizer.finalize("failed", "cleanup observation failed", {"authorized": False})
            recorded = json.loads(receipt.read_text())
            self.assertEqual(recorded["verdict"], "failed")
            self.assertEqual(recorded["details"]["native_proofs"], [proof])

    def test_default_roundtrip_paths_are_separate_for_each_capture(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.base, context.source, context.state = base, base / "source", base / "state"
            context.owner = DirectoryOwner()
            captures: list[dict[str, object]] = [{"operation_id": operation, "snapshot_ref": "refs/delegate/" + operation,
                         "tree_oid": TREE_OID} for operation in ("a" * 32, "b" * 32)]

            def git_result(_context, arguments, _cwd, *, label):
                if arguments[0] == "clone":
                    (Path(arguments[-1]) / driver.FIXTURE_FILE).write_text("fixture workspace edit\n")
                return ((TREE_OID + "\n").encode() if label == "capture-roundtrip-tree" else b"")

            try:
                with (
                    mock.patch.object(scenario_workspace_operations, "verify_capture_receipt", side_effect=captures),
                    mock.patch.object(scenario_workspace_operations, "_git", return_value=(0, b"")),
                    mock.patch.object(scenario_workspace_operations, "_git_ok", side_effect=git_result),
                ):
                    for capture in captures:
                        self.assertEqual(driver._capture_roundtrip(context, capture), capture)
                for operation in ("a" * 32, "b" * 32):
                    self.assertTrue((base / ("capture-roundtrip-" + operation)).is_dir())
            finally:
                context.owner.rollback()

    def test_admission_does_not_watch_workspace_before_live_lease_proof(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state, context.source = Path("/state"), Path("/source")
        context.workspace_budget = "5m"
        context.source_baseline = {"head": BASE_OID}
        context.command_lock = mock.MagicMock()
        context.attempted_task_ids = []
        predecessor = {"task_id": TASK_ID, "root_id": "e" * 32}
        response = {
            "task_id": TASK_ID, "root_id": "e" * 32, "parent_task_id": TASK_ID,
            "task_record": "created", "admission": "admitted",
            "workspace": {"id": WORKSPACE_ID, "state": "busy"},
        }
        with (
            mock.patch.object(driver.uuid, "uuid4", return_value=mock.Mock(hex=TASK_ID)),
            mock.patch.object(scenario_operations, "_control", return_value=(0, response)),
            mock.patch.object(scenario_operations, "_wait_workspace") as observe,
        ):
            first = driver._dispatch(context, Path("/brief"), task_budget="2s", fresh=True)
            followup = driver._continue(
                context, predecessor, brief=Path("/followup"), task_budget="2s")
        observe.assert_not_called()
        self.assertEqual(first["workspace"], response)
        self.assertEqual(followup["workspace"], response)

    def test_lifecycle_failure_keeps_completed_scenario_evidence(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/missing-proof-root/state")
        context.source_baseline = {"head": BASE_OID}
        context.native_proofs = []
        finalizer = mock.Mock(spec=driver.ReceiptFinalizer)
        success = {"release": {"state": "released"}, "forget": {"state": "forgotten"}}
        with (
            mock.patch.object(scenario_application, "build_source_manifest", return_value={}),
            mock.patch.object(scenario_application, "_run_success", return_value=success),
            mock.patch.object(scenario_application, "_run_timeout", side_effect=RuntimeError("timeout proof failed")),
        ):
            with self.assertRaisesRegex(RuntimeError, "timeout proof failed"):
                driver._run(mock.Mock(scenario="lifecycle"), finalizer, context)
        self.assertIn(mock.call(lifecycle=success), finalizer.set_details.call_args_list)

    def test_exact_ref_absence_is_distinct_from_lookup_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            source = root / "source"
            source.mkdir()
            owner = DirectoryOwner()
            environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
            environment.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
            try:
                processes = Processes(root / "captures", environment=environment, owner=owner,
                                      environment_keys_only=True)
                git = driver._git_executable()
                processes.run("init", [git, "init", "--quiet", source], root)
                tree = processes.run("tree", [git, "-C", source, "mktree"], root, stdin=b"").output().decode().strip()
                reference = "refs/delegate/test-capture"
                processes.run("ref", [git, "-C", source, "update-ref", reference, tree], root)
                context = mock.Mock(spec=driver.WorkspaceContext)
                context.source, context.processes = source, processes
                self.assertEqual(driver._verify_ref(context, reference, "present", (0,)), 0)
                processes.run("remove", [git, "-C", source, "update-ref", "-d", reference, tree], root)
                self.assertEqual(driver._verify_ref(context, reference, "absent", (1,)), 1)
                (source / ".git" / reference).write_text("corrupt ref\n")
                with self.assertRaises(RuntimeError):
                    driver._verify_ref(context, reference, "corrupt", (1,))
                self.assertEqual(processes.pending(), [])
            finally:
                owner.rollback()

    def test_fixture_session_marker_parses_first_turn_and_exact_resume(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            provider_bin, waiters = root / "bin", root / "waiters"
            provider_bin.mkdir()
            waiters.mkdir()
            marker = root / "session.json"
            driver._install_workspace_fixture(provider_bin, root / "gate", waiters, "nonce", marker)
            owner = DirectoryOwner()
            try:
                processes = Processes(root / "captures", environment=dict(os.environ), owner=owner,
                                      environment_keys_only=True)
                for index, resumed in enumerate((False, True)):
                    arguments = [str(provider_bin / "pi"), "--mode", "json"]
                    if resumed:
                        arguments.extend(["--session", driver.FIXTURE_SESSION])
                    process = processes.run(str(index), arguments, root, stdin=b"session probe\n")
                    self.assertEqual(json.loads(marker.read_text()), {
                        "schema_version": 1, "session": driver.FIXTURE_SESSION, "resumed": resumed,
                    })
                    events = [json.loads(line) for line in process.output().splitlines()]
                    self.assertEqual(events[0], {"type": "session", "id": driver.FIXTURE_SESSION})
                self.assertEqual(processes.pending(), [])
            finally:
                owner.rollback()

    def test_reused_success_cannot_fabricate_capture_verdict(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        context.brief_success = Path("/brief-success")
        context.brief_success_reuse = Path("/brief-success-reuse")
        context.command_lock = mock.MagicMock()
        context.attempted_task_ids = []
        first = {
            "task_id": TASK_ID,
            "workspace_id": WORKSPACE_ID,
            "root_id": "e" * 32,
        }
        reused = {
            "task_id": "f" * 32,
            "workspace_id": WORKSPACE_ID,
            "root_id": "e" * 32,
        }
        with (
            mock.patch.object(scenario_lifecycle, "_dispatch", side_effect=[first, reused]),
            mock.patch.object(scenario_lifecycle, "_adopt_workspace_container"),
            mock.patch.object(scenario_lifecycle, "_await_task", return_value={"status": "done"}),
            mock.patch.object(scenario_lifecycle, "_collect_task", return_value={"status": "collected"}),
            mock.patch.object(
                scenario_lifecycle,
                "_wait_capture",
                side_effect=[
                    ({"workspace": {"state": "retained"}}, {"status": "captured"}),
                    RuntimeError("reused capture receipt unavailable"),
                ],
            ) as wait_capture,
            mock.patch.object(
                scenario_lifecycle, "read_json", return_value={"verdict": "committed"},
            ),
            mock.patch.object(
                scenario_lifecycle, "_prove_lifetimes_closed", return_value={"closed": True},
            ),
            mock.patch.object(
                scenario_lifecycle,
                "_provider_session",
                return_value={"provider": "pi:json", "conversation_id": driver.FIXTURE_SESSION},
            ),
            mock.patch.object(scenario_lifecycle, "_capture_roundtrip", return_value={
                "snapshot_ref": "ref",
                "operation_id": "a" * 32,
                "subject": {"id": TASK_ID},
            }),
            mock.patch.object(scenario_lifecycle, "verify_task_turn", return_value={
                "snapshot_ref": "ref", "subject": {"kind": "task", "id": TASK_ID},
            }),
        ):
            with self.assertRaisesRegex(RuntimeError, "reused capture receipt unavailable"):
                driver._run_success(context)
        self.assertEqual(wait_capture.call_count, 2)

    def test_continue_admits_once_and_observes_status_first(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        context.environment = {}
        context.command_lock = mock.MagicMock()
        context.attempted_task_ids = []
        predecessor = {
            "task_id": TASK_ID,
            "workspace_id": WORKSPACE_ID,
            "root_id": "e" * 32,
        }
        successor_id = "f" * 32
        events: list[str] = []
        continue_response = {
            "task_id": successor_id,
            "task_record": "created",
            "admission": "admitted",
            "parent_task_id": TASK_ID,
            "root_id": predecessor["root_id"],
            "workspace": {"id": WORKSPACE_ID, "state": "busy"},
        }

        def control(_context, _args, **kwargs):
            events.append(kwargs["label"])
            if kwargs["label"] == "continue":
                return 0, continue_response
            if kwargs["label"] == "first-status":
                return 0, {"task_id": successor_id, "status": "queued"}
            raise AssertionError("unexpected control: " + kwargs["label"])

        with (
            mock.patch.object(scenario_operations, "_control", side_effect=control) as control_mock,
            mock.patch.object(
                scenario_operations,
                "_wait_workspace",
                return_value={"root_id": predecessor["root_id"],
                              "workspace": {"id": WORKSPACE_ID, "state": "retained",
                                            "path": "/workspace"}},
            ),
            mock.patch.object(
                driver.uuid, "uuid4", return_value=mock.Mock(hex="1" * 32),
            ) as generated,
        ):
            successor = driver._continue(
                context, predecessor, brief=Path("/resume-brief"),
                task_budget=driver.TIMEOUT_CONTINUE_TASK_BUDGET,
            )
            generated.assert_not_called()

        self.assertEqual(events, ["continue", "first-status"])
        self.assertEqual(context.attempted_task_ids, [successor_id])
        context.add_task.assert_called_once_with(successor_id)
        context.add_workspace.assert_called_once_with(WORKSPACE_ID)
        continue_args = control_mock.call_args_list[0].args[1]
        self.assertEqual(
            continue_args,
            [
                "--root", "/state", "continue", "--task", TASK_ID,
                "--brief", "/resume-brief", "--budget",
                driver.TIMEOUT_CONTINUE_TASK_BUDGET, "--json",
            ],
        )
        self.assertEqual(successor["parent_task_id"], TASK_ID)
        self.assertEqual(successor["workspace_id"], WORKSPACE_ID)

    def test_continue_failure_is_retained_without_task_bookkeeping_or_retry(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        context.environment = {}
        context.command_lock = mock.MagicMock()
        context.attempted_task_ids = []
        predecessor = {
            "task_id": TASK_ID,
            "workspace_id": WORKSPACE_ID,
            "root_id": "e" * 32,
        }
        successor_id = "f" * 32
        response = {
            "task_id": successor_id,
            "task_record": "not_created",
            "admission": "rejected",
            "failure": {
                "code": "continuation_unavailable",
                "next_action": "stop_and_report",
            },
        }
        with (
            mock.patch.object(scenario_operations, "_control", return_value=(1, response)) as control_mock,
            mock.patch.object(
                driver.uuid, "uuid4", return_value=mock.Mock(hex=successor_id),
            ),
        ):
            with self.assertRaisesRegex(RuntimeError, "did not durably create task"):
                driver._continue(
                    context, predecessor, brief=Path("/resume-brief"),
                    task_budget=driver.TIMEOUT_CONTINUE_TASK_BUDGET,
                )
        self.assertEqual(control_mock.call_count, 1)
        context.add_task.assert_not_called()
        self.assertEqual(context.attempted_task_ids, [successor_id])

    def test_continuation_binding_proves_prior_session_cwd_and_workspace(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            root_id = "e" * 32
            predecessor_id = TASK_ID
            successor_id = "f" * 32
            workspace_path = "/owned/managed-workspace"
            (state / "root.json").write_text(json.dumps({"root_id": root_id}))
            for task_id in (predecessor_id, successor_id):
                directory = state / "tasks" / task_id
                directory.mkdir(parents=True)
                record = {
                    "task_id": task_id,
                    "root_id": root_id,
                    "provider": "pi:json",
                    "canonical_cwd": workspace_path,
                    "workspace": {"id": WORKSPACE_ID},
                }
                if task_id == successor_id:
                    record["prior_session"] = {
                        "provider": "pi:json",
                        "conversation_id": driver.FIXTURE_SESSION,
                        "predecessor_task_id": predecessor_id,
                    }
                (directory / "task.json").write_text(json.dumps(record))
                (directory / "provider.ref.json").write_text(json.dumps({
                    "root_id": root_id,
                    "task_id": task_id,
                    "provider": "pi:json",
                    "conversation_id": driver.FIXTURE_SESSION,
                }))
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.state = state
            predecessor = {
                "task_id": predecessor_id,
                "workspace_id": WORKSPACE_ID,
                "root_id": root_id,
            }
            successor = {
                "task_id": successor_id,
                "workspace_id": WORKSPACE_ID,
                "root_id": root_id,
                "workspace": {
                    "workspace": {"id": WORKSPACE_ID, "path": workspace_path},
                },
            }
            evidence = driver._validate_continuation_binding(
                context, predecessor, successor, driver.FIXTURE_SESSION,
            )
            self.assertEqual(object_at(evidence, "prior_session")["predecessor_task_id"], predecessor_id)
            self.assertEqual(evidence["canonical_cwd"], workspace_path)
            self.assertEqual(evidence["workspace_id"], WORKSPACE_ID)

    def test_main_keeps_context_when_scenario_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.cleanup.return_value = {
                "authorized": False, "decision": "failure-evidence-retained",
                "retained_owned_directories": ["owned-fixture"],
            }
            with (
                mock.patch.object(scenario_application, "_make_context", return_value=(context, {})),
                mock.patch.object(scenario_application, "_run", side_effect=RuntimeError("injected scenario failure")),
                mock.patch.object(driver.sys.stdout, "write"),
            ):
                result = driver.main([
                    "--source-root", str(scenario_application.REPOSITORY),
                    "--output", str(output),
                ])
            self.assertEqual(result, 1)
            context.cleanup.assert_called_once_with("failed")
            receipt = json.loads(output.read_text())
            self.assertEqual(object_at(receipt, "cleanup")["retained_owned_directories"], ["owned-fixture"])

    def test_success_verdict_requires_provider_outcome_and_capture(self):
        status = {"status": "done"}
        committed: dict[str, object] = {"verdict": "committed"}
        self.assertEqual(
            driver.classify_task_verdict(
                status, committed, "success",
                lifetime_closed=True, capture_status="captured",
            )["verdict"],
            "passed",
        )
        self.assertEqual(
            driver.classify_task_verdict(
                status, {"verdict": "rejected"}, "success",
                lifetime_closed=True, capture_status="captured",
            )["verdict"],
            "failed",
        )
        self.assertEqual(
            driver.classify_task_verdict(
                status, committed, "success",
                lifetime_closed=True, capture_status="unavailable",
            )["reason"],
            "workspace-capture-not-captured",
        )

    def test_timeout_verdict_keeps_outcome_authority_separate(self):
        self.assertEqual(
            driver.classify_task_verdict(
                {"status": "timed_out"},
                {"verdict": "rejected"},
                "timeout",
                lifetime_closed=True,
                capture_status="captured",
            )["verdict"],
            "passed",
        )
        self.assertEqual(
            driver.classify_task_verdict(
                {"status": "done"},
                {"verdict": "committed"},
                "timeout",
                lifetime_closed=True,
                capture_status="captured",
            )["reason"],
            "task-was-not-authoritatively-timed-out",
        )

    def test_timeout_without_publication_requires_native_continuation_and_closure(self):
        status = {
            "status": "timed_out", "task_id": TASK_ID, "publication": "unknown",
            "continuation": {
                "resumable": True, "mode": "native", "predecessor_task_id": TASK_ID,
            },
        }
        result = driver.classify_task_verdict(
            status, None, "timeout", lifetime_closed=True, capture_status="captured")
        self.assertEqual(result["verdict"], "passed")
        self.assertEqual(result["reason"],
                         "native-timeout-and-capture-proven-without-provider-publication")
        self.assertEqual(driver.classify_task_verdict(
            status, None, "timeout", lifetime_closed=False,
            capture_status="captured")["reason"], "native-lifetime-not-closed")
        status["continuation"]["predecessor_task_id"] = "f" * 32
        self.assertEqual(driver.classify_task_verdict(
            status, None, "timeout", lifetime_closed=True,
            capture_status="captured")["reason"], "timeout-continuation-not-proven")
        self.assertEqual(driver.classify_task_verdict(
            status, None, "success", lifetime_closed=True,
            capture_status="captured")["reason"], "outcome-verdict-missing")

    def test_continuation_snapshot_preserves_absent_predecessor_publication(self):
        with tempfile.TemporaryDirectory() as temporary:
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.state = Path(temporary)
            directory = context.state / "tasks" / TASK_ID
            directory.mkdir(parents=True)
            for name in ("task.json", "meta.json", "brief.md", "provider.started.json",
                         "provider.ref.json"):
                (directory / name).write_bytes(b"immutable fixture\n")
            snapshot = driver._immutable_task_snapshot(context, TASK_ID)
            self.assertIsNone(snapshot["outcome.json"])
            self.assertIsNone(snapshot["provider.exit"])
            driver._assert_immutable_task_snapshot(context, TASK_ID, snapshot)
            (directory / "outcome.json").write_bytes(b"invented predecessor outcome\n")
            with self.assertRaisesRegex(RuntimeError, "created an absent predecessor record"):
                driver._assert_immutable_task_snapshot(context, TASK_ID, snapshot)

    def test_exit_three_is_pending_only_with_watch_evidence(self):
        self.assertEqual(
            driver.classify_control_response(
                3, {"workspace": {"state": "provisioning",
                                  "next_action": "watch_workspace"}},
            ),
            "pending",
        )
        self.assertEqual(
            driver.classify_control_response(
                3, {"workspace": {"state": "retained"}},
            ),
            "failed",
        )
        self.assertEqual(
            driver.classify_control_response(
                0, {"workspace": {"state": "retained"}},
            ),
            "ready",
        )

    def test_action_retry_is_distinct_from_an_accepted_pending_action(self):
        workspace = {
            "state": "busy", "reason_code": "requested_action_not_authorized",
            "next_action": "retry_release",
        }
        self.assertEqual(driver.classify_control_response(3, {"workspace": workspace}),
                         "not-authorized")
        workspace["operation_id"] = OPERATION_ID
        self.assertEqual(driver.classify_control_response(3, {"workspace": workspace}),
                         "failed")

    def test_negative_acknowledgement_waits_then_retries_exact_mutation(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        arguments = ["workspace", "release", WORKSPACE_ID, "--json"]
        rejected = {"workspace": {
            "id": WORKSPACE_ID, "state": "busy",
            "reason_code": "requested_action_not_authorized", "next_action": "retry_release",
        }}
        accepted = {"workspace": {
            "id": WORKSPACE_ID, "state": "cleanup_pending", "next_action": "watch_workspace",
        }}
        with (
            mock.patch.object(scenario_workspace_operations, "_control", side_effect=[(3, rejected), (3, accepted)]) as control,
            mock.patch.object(scenario_workspace_operations, "_wait_workspace") as observe,
        ):
            self.assertEqual(driver._workspace_mutation(
                context, arguments, action="release", workspace_id=WORKSPACE_ID,
                label="workspace-release", task_id=TASK_ID,
            ), (3, accepted))
        self.assertEqual(control.call_count, 2)
        self.assertEqual([call.args[1] for call in control.call_args_list], [arguments, arguments])
        observe.assert_called_once()
        self.assertEqual(observe.call_args.kwargs["state"], "retained")
        self.assertNotIn("operation_id", observe.call_args.kwargs)

    def test_accepted_helperless_forget_is_observed_without_repeating_mutation(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        pending = {"workspace": {
            "id": WORKSPACE_ID, "state": "forgetting", "next_action": "watch_workspace",
        }}
        finished = {"workspace": {"id": WORKSPACE_ID, "state": "forgotten"}}
        with (
            mock.patch.object(scenario_workspace_operations, "_control", return_value=(3, pending)) as control,
            mock.patch.object(scenario_workspace_operations, "_wait_workspace", return_value=finished) as observe,
        ):
            result = driver._forget_workspace(context, {"workspace_id": WORKSPACE_ID, "task_id": TASK_ID})
        self.assertEqual(result["forget_state"], "forgotten")
        self.assertEqual(control.call_count, 1)
        observe.assert_called_once()
        self.assertIsNone(observe.call_args.kwargs["operation_id"])
        self.assertEqual(observe.call_args.kwargs["state"], "forgotten")

    def test_exec_waits_for_recapture_after_command_success(self):
        context = mock.Mock(spec=driver.WorkspaceContext)
        context.state = Path("/state")
        pending = {"workspace": {
            "id": WORKSPACE_ID, "state": "busy", "next_action": "watch_workspace",
            "operation_id": OPERATION_ID,
        }}
        finished = {"workspace": {
            "id": WORKSPACE_ID, "state": "retained", "operation_id": OPERATION_ID,
            "operation": {"operation_id": OPERATION_ID, "status": "succeeded", "command_exit": 0},
        }}
        with (
            mock.patch.object(scenario_workspace_operations, "_control", return_value=(3, pending)),
            mock.patch.object(scenario_workspace_operations, "_wait_workspace", return_value=finished) as observe,
        ):
            driver._workspace_exec(context, {"workspace_id": WORKSPACE_ID, "task_id": TASK_ID})
        self.assertEqual(observe.call_args.kwargs["state"], "retained")

    def _main(self, run, cleanup, *, setup_cleanup=None):
        """Drive the real main with a canonical cleanup result; return exit, receipt, finalize calls."""
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            context = mock.Mock(spec=driver.WorkspaceContext)
            context.cleanup.return_value = cleanup
            make = mock.Mock(return_value=(context, {}))
            if setup_cleanup is not None:
                failure = driver.AcceptanceSetupFailure(
                    driver.AcceptanceBlocked("fixture-unavailable"), driver.DirectoryOwner(), mock.Mock())
                failure.cleanup = mock.Mock(return_value=setup_cleanup)
                make = mock.Mock(side_effect=failure)
            finalize = driver.ReceiptFinalizer.finalize
            with (
                mock.patch.object(scenario_application, "_make_context", make),
                mock.patch.object(scenario_application, "_run", side_effect=run),
                mock.patch.object(driver.ReceiptFinalizer, "finalize", autospec=True,
                                  side_effect=finalize) as finalized,
                mock.patch.object(driver.sys.stdout, "write"),
            ):
                result = driver.main([
                    "--source-root", str(scenario_application.REPOSITORY), "--output", str(output),
                ])
            return result, json.loads(output.read_text()), [call.args[1:3] for call in finalized.call_args_list]

    @staticmethod
    def _passed(*_arguments):
        return "passed", "managed workspace fixture acceptance passed"

    @staticmethod
    def _failed(*_arguments):
        raise RuntimeError("injected scenario failure")

    @staticmethod
    def _blocked(*_arguments):
        raise driver.AcceptanceBlocked("fixture-unavailable")

    def _refusals(self):
        retained = {"retained_owned_directories": ["owned-fixture"]}
        policy = {
            "lifetime": driver.cleanup_decision("passed", lifetime_closed=False, daemon_closed=True,
                                                task_count=1, pending_process_count=0),
            "daemon": driver.cleanup_decision("passed", lifetime_closed=True, daemon_closed=False,
                                              task_count=1, pending_process_count=0),
            "processes": driver.cleanup_decision("passed", lifetime_closed=True, daemon_closed=True,
                                                 task_count=1, pending_process_count=1),
        }
        refused = [dict(authorized=allowed, decision=decision, **retained)
                   for allowed, decision in policy.values()]
        return refused + [dict(authorized=False, decision="cleanup-failed-evidence-retained",
                               error="removal failed", **retained)]

    def test_returned_cleanup_refusal_fails_a_passing_run_with_its_retained_evidence(self):
        refusals = self._refusals()
        self.assertEqual({item["decision"] for item in refusals}, {
            "managed-lifetime-closure-unproven", "private-daemon-closure-unproven",
            "process-observation-pending", "cleanup-failed-evidence-retained"})
        for cleanup in refusals:
            with self.subTest(cleanup["decision"]):
                code, receipt, calls = self._main(self._passed, cleanup)
                self.assertEqual(code, 1)
                self.assertEqual((receipt["verdict"], receipt["reason"]),
                                 ("failed", "cleanup-refused:" + cleanup["decision"]))
                self.assertEqual(receipt["cleanup"], cleanup)
                self.assertEqual(calls[-1][0], "failed")

    def test_cleanup_without_a_true_authorization_never_passes(self):
        for cleanup in ({}, {"decision": "x-y"}, {"authorized": "yes", "decision": "d"},
                        {"authorized": 1, "decision": "d"}, {"authorized": None, "decision": 7},
                        {"authorized": False, "decision": "Bad Decision/../"}):
            with self.subTest(cleanup):
                code, receipt, _calls = self._main(self._passed, cleanup)
                self.assertEqual(code, 1)
                self.assertEqual(receipt["verdict"], "failed")
                self.assertTrue(receipt["reason"].startswith("cleanup-refused:"))
        self.assertEqual(receipt["reason"], "cleanup-refused:unrecorded")

    def test_authorized_cleanup_keeps_a_pass_and_marks_the_preliminary_receipt_nonfinal(self):
        cleanup = {"authorized": True, "decision": "passed-with-closed-managed-lifetimes"}
        code, receipt, calls = self._main(self._passed, cleanup)
        self.assertEqual((code, receipt["verdict"], receipt["cleanup"]), (0, "passed", cleanup))
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0][0], "failed")
        self.assertEqual(calls[1], ("passed", "managed workspace fixture acceptance passed"))

    def test_cleanup_refusal_preserves_a_genuine_failure_reason(self):
        for cleanup in (self._refusals()[0], {"authorized": True, "decision": "d"}):
            code, receipt, _calls = self._main(self._failed, cleanup)
            self.assertEqual(code, 1)
            self.assertEqual(receipt["reason"], "RuntimeError: injected scenario failure")

    def test_blocked_run_is_neutral_only_when_cleanup_is_authorized(self):
        authorized = {"authorized": True, "decision": "blocked-before-task-or-daemon"}
        code, receipt, _calls = self._main(self._blocked, authorized)
        self.assertEqual((code, receipt["verdict"], receipt["reason"]), (3, "BLOCKED", "fixture-unavailable"))
        code, receipt, _calls = self._main(self._blocked, self._refusals()[0])
        self.assertEqual((code, receipt["verdict"]), (1, "failed"))
        self.assertEqual(receipt["reason"], "cleanup-refused:managed-lifetime-closure-unproven")

    def test_setup_failure_block_is_neutral_only_when_its_cleanup_is_authorized(self):
        unused = {"authorized": False, "decision": "unused"}
        removed = {"authorized": True, "decision": "setup-failure-exact-root-removed",
                   "removed_owned_directories": ["setup"]}
        code, receipt, _calls = self._main(self._passed, unused, setup_cleanup=removed)
        self.assertEqual((code, receipt["verdict"], receipt["cleanup"]), (3, "BLOCKED", removed))
        retained = {"authorized": False, "decision": "setup-cleanup-failed-evidence-retained",
                    "error": "removal failed", "retained_owned_directories": ["setup"]}
        code, receipt, _calls = self._main(self._passed, unused, setup_cleanup=retained)
        self.assertEqual((code, receipt["verdict"], receipt["cleanup"]), (1, "failed", retained))
        self.assertEqual(receipt["reason"], "cleanup-refused:setup-cleanup-failed-evidence-retained")

    def test_capture_verification_binds_artifact_bytes_and_ref(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            root_id = "e" * 32
            (state / "root.json").write_text(json.dumps({"root_id": root_id}))
            artifact_root = state / "workspace-operations" / OPERATION_ID / "artifacts"
            artifact_root.mkdir(parents=True)
            patch = b"diff --git a/managed b/managed\n"
            patch_path = artifact_root / "diff.patch"
            patch_path.write_bytes(patch)
            name_status = b"A\x00managed\x00"
            (artifact_root / "name-status.z").write_bytes(name_status)
            capture: dict[str, object] = {
                "status": "captured",
                "workspace_id": WORKSPACE_ID,
                "operation_id": OPERATION_ID,
                "termination_sha256": TERMINATION_SHA,
                "worker_wait_sha256": WAIT_SHA,
                "candidate": {
                    "workspace_id": WORKSPACE_ID,
                    "operation_id": OPERATION_ID,
                    "subject": {"kind": "task", "id": TASK_ID, "sha256": SUBJECT_SHA},
                    "base_oid": BASE_OID,
                    "observed_head": HEAD_OID,
                    "tree_oid": TREE_OID,
                    "snapshot_ref": "refs/delegate/" + root_id + "/workspaces/" + WORKSPACE_ID + "/captures/" + OPERATION_ID,
                    "object_format": "sha1",
                },
                "artifacts": [{
                    "name": "diff.patch",
                    "length": len(patch),
                    "sha256": hashlib.sha256(patch).hexdigest(),
                }, {
                    "name": "name-status.z",
                    "length": len(name_status),
                    "sha256": hashlib.sha256(name_status).hexdigest(),
                }],
            }
            evidence = driver.verify_capture_receipt(state, capture)
            self.assertEqual(evidence["operation_id"], OPERATION_ID)
            self.assertEqual(object_at(evidence, "artifacts", 0)["sha256"],
                             hashlib.sha256(patch).hexdigest())
            artifact = object_at(capture, "artifacts", 0)
            length = artifact["length"]
            if type(length) is not int:
                self.fail("fixture artifact length is not an integer")
            artifact["length"] = length + 1
            with self.assertRaisesRegex(RuntimeError, "artifact digest"):
                driver.verify_capture_receipt(state, capture)

    def test_capture_turn_artifacts_follow_the_turn_observation(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            root_id = "e" * 32
            (state / "root.json").write_text(json.dumps({"root_id": root_id}))
            artifact_root = state / "workspace-operations" / OPERATION_ID / "artifacts"
            artifact_root.mkdir(parents=True)
            files = {"diff.patch": b"diff --git a/managed b/managed\n",
                     "name-status.z": b"A\x00managed\x00", "turn-diff.patch": b"",
                     "turn-name-status.z": b"A\x00current-turn\x00"}
            descriptors = []
            for name, data in files.items():
                (artifact_root / name).write_bytes(data)
                descriptors.append({"name": name, "length": len(data), "sha256": hashlib.sha256(data).hexdigest()})

            def capture(turn, artifacts):
                candidate = {
                    "workspace_id": WORKSPACE_ID, "operation_id": OPERATION_ID,
                    "subject": {"kind": "task", "id": TASK_ID, "sha256": SUBJECT_SHA},
                    "base_oid": BASE_OID, "observed_head": HEAD_OID, "tree_oid": TREE_OID,
                    "snapshot_ref": "refs/delegate/" + root_id + "/workspaces/" + WORKSPACE_ID + "/captures/" + OPERATION_ID,
                    "object_format": "sha1",
                }
                if turn is not None:
                    candidate["turn"] = turn
                return {"status": "captured", "workspace_id": WORKSPACE_ID, "operation_id": OPERATION_ID,
                        "termination_sha256": TERMINATION_SHA, "worker_wait_sha256": WAIT_SHA,
                        "candidate": candidate, "artifacts": artifacts}

            observed = {"status": "observed", "content": "changed", "head": "unchanged", "index": "unchanged"}
            unavailable = {"status": "unavailable", "reason_code": "baseline_unavailable"}
            evidence = driver.verify_capture_receipt(state, capture(observed, descriptors))
            self.assertEqual({item["name"] for item in evidence["artifacts"]}, set(files))
            driver.verify_capture_receipt(state, capture(unavailable, descriptors[:2]))
            driver.verify_capture_receipt(state, capture(None, descriptors[:2]))
            for turn, artifacts in ((observed, descriptors[:3]), (unavailable, descriptors), (None, descriptors)):
                with self.assertRaisesRegex(RuntimeError, "turn artifacts"):
                    driver.verify_capture_receipt(state, capture(turn, artifacts))
            with self.assertRaisesRegex(RuntimeError, "duplicated"):
                driver.verify_capture_receipt(state, capture(observed, descriptors + descriptors[2:3]))

    def test_failure_finalizer_writes_bounded_receipt_and_retains_on_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            finalizer = driver.ReceiptFinalizer(
                output, ["acceptance_workspaces.py", "--scenario", "lifecycle"],
                {"driver": Path(driver.__file__)},
            )
            finalizer.set_source_manifest({"status": "complete", "files": []})
            finalizer.set_details(failure={"message": "fixture failed"})
            finalizer.finalize(
                "failed",
                "failure-evidence-retained",
                {
                    "authorized": False,
                    "decision": "failure-evidence-retained",
                    "retained_owned_directories": [".dl-workspaces-example"],
                },
            )
            receipt = json.loads(output.read_text())
            self.assertEqual(receipt["verdict"], "failed")
            self.assertEqual(object_at(receipt, "cleanup")["authorized"], False)
            self.assertEqual(object_at(receipt, "cleanup")["decision"], "failure-evidence-retained")
            self.assertLess(output.stat().st_size, driver.MAX_RECEIPT_BYTES)
            self.assertEqual(finalizer.write_count, 1)

    def test_partial_setup_cleanup_uses_exact_owner_after_receipt_phase(self):
        with tempfile.TemporaryDirectory() as temporary:
            parent = Path(temporary)
            base = parent / ".dl-setup"
            base.mkdir(mode=0o700)
            owner = driver.DirectoryOwner()
            root_token = owner.adopt_created("setup root", base, parent)
            owner.create("setup child", base / "child", base)
            failure = driver.AcceptanceSetupFailure(
                driver.AcceptanceBlocked("fixture-unavailable"), owner, root_token,
            )
            cleanup = failure.cleanup()
            self.assertEqual(cleanup["authorized"], True)
            self.assertFalse(base.exists())

    def test_cleanup_retention_policy(self):
        self.assertEqual(
            driver.cleanup_decision(
                "passed", lifetime_closed=True, daemon_closed=True,
                task_count=2, pending_process_count=0,
            ),
            (True, "passed-with-closed-managed-lifetimes"),
        )
        self.assertEqual(
            driver.cleanup_decision(
                "failed", lifetime_closed=True, daemon_closed=True,
                task_count=2, pending_process_count=0,
            ),
            (False, "failure-evidence-retained"),
        )
        self.assertEqual(
            driver.cleanup_decision(
                "BLOCKED", lifetime_closed=True, daemon_closed=True,
                task_count=0, pending_process_count=0,
            ),
            (True, "blocked-before-task-or-daemon"),
        )
        self.assertEqual(
            driver.cleanup_decision(
                "failed", lifetime_closed=True, daemon_closed=True,
                task_count=2, pending_process_count=1,
            ),
            (False, "process-observation-pending"),
        )


if __name__ == "__main__":
    unittest.main()

