"""Historical compatibility and upgrade resumption; no native claims or providers."""

from copy import deepcopy
import json
import os
from pathlib import Path
import tempfile
import stat
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.files import digest
from scripts.acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.acceptance.inspections.tests.fixtures import InspectionFixture, ROOT, TASK
from scripts.acceptance.processes.contracts import BoundedCommandResult
from scripts.acceptance.private_cli.fingerprints import REPOSITORY
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.acceptance.private_cli.scheduler.cases import SchedulerCase
from scripts.acceptance.private_cli.scheduler.occurrences import json_object
from scripts.acceptance.private_cli.scheduler import historical_state, recovery_chain, releases


class HistoricalRecoveryTests(unittest.TestCase):
    def test_rebase_preserves_historical_omissions_and_canonical_integrity_chain(self):
        source = REPOSITORY / historical_state.FIXTURE_PATH
        manifest = json.loads((source / "manifest.json").read_text())
        terminal = unstarted = 0
        for case in manifest["cases"]:
            records = {path.name: json.loads(path.read_text()) for path in (source / case["name"]).glob("*.json")}
            original = deepcopy(records)
            changed = historical_state.rebased_records(records, "a" * 32)
            self.assertEqual(records, original)
            self.assertEqual(set(changed), set(original))
            for name in changed:
                self.assertEqual(set(changed[name]), set(original[name]))
            request = changed["request.json"]
            self.assertEqual(request["root_id"], "a" * 32)
            self.assertEqual(request["task"]["root_id"], "a" * 32)
            self.assertEqual(set(request["binding"]), set(case["binding_fields"]))
            self.assertEqual(set(request["binding"]["supervisor"]), set(case["supervisor_fields"]))
            self.assertEqual(request["deadline"], original["request.json"]["deadline"])
            self.assertEqual(request["task_sha256"], historical_state.record_digest(request["task"]))
            for record in changed.values():
                if "request_sha256" in record:
                    self.assertEqual(record["request_sha256"], historical_state.record_digest(request))
            if "completion.json" in changed:
                terminal += 1
                self.assertEqual(changed["completion.json"]["result_sha256"], historical_state.record_digest(changed["result.json"]))
                self.assertEqual(changed["worker-observation.json"]["completion_sha256"], historical_state.record_digest(changed["completion.json"]))
            else:
                unstarted += 1
        self.assertEqual((terminal, unstarted), (5, 2))

    def test_owned_seed_retains_exact_records_and_detects_rewrite(self):
        owner = DirectoryOwner()
        namespace_owner = DirectoryOwner()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            supervisor = root / ".supervisor"
            supervisor.mkdir(mode=0o700)
            for name in ("transition.lock", "bootstrap.lock"):
                historical_state.write_private_record(supervisor / name, b"")
            tokens = []
            try:
                expected = historical_state.seed(REPOSITORY, root, "a" * 32, owner, tokens,
                                                 namespace_owner=namespace_owner)
                self.assertEqual(len(tokens), 7)
                for token in tokens:
                    for path in token.directory.iterdir():
                        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
                # The namespace belongs to the fixture lifetime, so synthetic
                # retirement cannot remove later production inspection records.
                ordinary = root / "inspections" / "ordinary-production-record"
                ordinary.write_text("preserve through synthetic retirement")
                historical_state.require_preserved(root, expected)
                first = root / "inspections" / next(iter(expected))
                first.write_bytes(b"rewritten")
                with self.assertRaisesRegex(AcceptanceFailure, "rewrote or removed"):
                    historical_state.require_preserved(root, expected)
                owner.rollback()
                self.assertEqual(ordinary.read_text(), "preserve through synthetic retirement")
            finally:
                owner.rollback()
                owner.release()
                namespace_owner.rollback()
                namespace_owner.release()

    def test_private_record_creation_does_not_depend_on_inherited_umask(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / ".operation.lock"
            previous = os.umask(0)
            try:
                historical_state.write_private_record(path, b"")
            finally:
                os.umask(previous)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_official_recovery_release_is_separate_from_schema_compatibility_baselines(self):
        release = releases.RECOVERY_RELEASE
        self.assertEqual((release.version, release.schema, release.commit),
                         ("0.7.0", 2, "3321d341cf0e993864e2ab6a0a0534910bc8586b"))
        self.assertEqual([item.version for item in releases.RELEASES], ["0.4.2", "0.5.1"])
        self.assertEqual(release.asset("Darwin", "arm64"), (
            "delegation-layer_0.7.0_Darwin_arm64.tar.gz",
            "bdff48d1e9df1ce728a49a620a2c2075c8fa46991caf4d290fe8f8a3d64a6fbe"))


class RecoveryResumptionTests(unittest.TestCase):
    operation = "upgrade-v1:" + "a" * 32

    def response(self, phase="draining", *, complete=False, operation=None, action=None):
        return {"supervisor_control": {"state": "migrated" if complete else "blocked",
                "next_action": action or ("retry_dispatch" if complete else "repeat_supervisor_ensure_owner"),
                "upgrade": {"operation_id": operation or self.operation, "phase": phase, "completed": complete}}}

    def case(self, responses):
        return SimpleNamespace(command=Mock(side_effect=responses), remaining=lambda: 2)

    def test_separate_invocations_resume_same_operation_through_all_progress_phases(self):
        case = self.case([self.response(), self.response("authorized"), self.response("migrating"), self.response(complete=True)])
        self.assertEqual(recovery_chain.advance(case, "native", self.operation), self.operation)
        self.assertEqual(case.command.call_count, 4)
        for call in case.command.call_args_list:
            self.assertEqual(call.args[1:], (["supervisor", "ensure-owner"], (0, 3)))

    def test_hard_observation_fault_is_never_reclassified_as_repeat(self):
        case = self.case([self.response(action="stop_and_report")])
        with self.assertRaisesRegex(AcceptanceFailure, "did not request a repeat"):
            recovery_chain.advance(case, "fault", self.operation)
        self.assertEqual(case.command.call_count, 1)

    def test_interrupted_drain_cannot_replace_operation(self):
        case = self.case([self.response(operation="upgrade-v1:" + "b" * 32)])
        with self.assertRaisesRegex(AcceptanceFailure, "replaced its operation"):
            recovery_chain.advance(case, "interrupted", self.operation)

    def test_default_task_target_names_existing_inspection_and_explicit_kind(self):
        operation = "b" * 32
        fact = {
            "resource_kind": "inspection", "resource_id": operation,
            "fact_kind": "target_kind", "name": "inspection",
            "reason_code": "inspection_requires_kind", "next_action": "settle_with_kind_inspection",
        }
        response = {"error": "inspection requires explicit kind", "control_fact": fact}
        case = self.case([response])
        recovery_chain.require_inspection_target_error(case, operation)
        self.assertEqual(case.command.call_args.args[1:], (["settle", operation, "--prepare"], (1,)))

        wrong_fact = {**fact, "resource_kind": "task"}
        wrong = {"error": "inspection requires explicit kind", "control_fact": wrong_fact}
        with self.assertRaisesRegex(AcceptanceFailure, "default-task mismatch"):
            recovery_chain.require_inspection_target_error(self.case([wrong]), operation)

    def test_unknown_native_closure_retains_synthetic_records(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            inspections = state / "inspections"
            inspections.mkdir()
            owner = DirectoryOwner()
            token = owner.create("owned synthetic journal", inspections / ("b" * 32), inspections)
            (token.directory / "request.json").write_text("{}")
            case = Mock(spec=recovery_chain.SchedulerCase)
            case.fixture = SimpleNamespace(state=state)
            try:
                with patch.object(recovery_chain, "close_actual_resources", side_effect=RuntimeError("native closure unknown")):
                    with self.assertRaisesRegex(RuntimeError, "native closure unknown"):
                        recovery_chain.retire_synthetic(case, owner, [token], {})
                self.assertTrue((token.directory / "request.json").exists())
                token.assert_live()
            finally:
                owner.rollback()
                owner.release()

    def test_c_installs_native_pair_in_place_and_wires_actual_selected_daemon_marker(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            lifetime = FixtureLifetime(base=root)
            fixture = FixtureInstallation(
                delegate_binary=root / "a/delegate", runner_binary=root / "a/delegate-run",
                upgraded_delegate_binary=root / "b/delegate", upgraded_runner_binary=root / "b/delegate-run",
                pueue_binary=root / "tools/pueue", pueued_binary=root / "tools/pueued",
                workspace=root / "workspace", provider_bin=root / "provider", brief=root / "brief.md",
                upgraded_install=root / "b", old_daemon_marker=root / "a-marker", upgraded_daemon_marker=root / "b-marker",
                state=root / "state", waiter_directory=root / "waiters", lease_nonce="fixture",
            )
            case = SchedulerCase(fixture, lifetime, root / "responses", Mock(), Mock(), 1, upgraded=True)

            def package(owner, _source, package_root, symlinked, *, version):
                self.assertTrue(symlinked)
                self.assertEqual(version, "0.7.1-upgrade-c")
                binary = owner.create("test package bin", package_root / "bin", package_root).directory
                libexec = owner.create("test package libexec", package_root / "libexec", package_root).directory
                return binary, binary, libexec

            def archive(destination):
                for role in ("pueue", "pueued"):
                    path = destination / role
                    path.write_text("owned fixture program source\n")
                    path.chmod(0o700)

            try:
                with patch.object(recovery_chain, "install_package_layout", side_effect=package), \
                        patch.object(recovery_chain, "install_supervisor_pair", side_effect=archive) as install:
                    recovery_chain.select_second_candidate(case, REPOSITORY)
                selected = root / "second-candidate/libexec"
                install.assert_called_once_with(selected / "native")
                self.assertEqual(lifetime.upgraded_libexec, selected)
                marker = case.fixture.upgraded_daemon_marker
                self.assertEqual(marker, root / "second-candidate-daemon-starts")
                self.assertFalse(marker.exists(), "installation forged a daemon-start marker")
                for role in ("pueue", "pueued"):
                    wrapper = (selected / role).read_text()
                    self.assertIn(str(selected / "native" / role), wrapper)
                    self.assertTrue(os.access(selected / role, os.X_OK))
                self.assertIn(str(marker), (selected / "pueued").read_text())
                supervisor = root / "state/.supervisor"
                supervisor.mkdir(parents=True)
                (supervisor / "supervisor.identity.json").write_text(json.dumps({"schema_version": 1, "binding": {
                    "client_executable": str(selected / "pueue"), "daemon_executable": str(selected / "pueued"),
                }}))
                (supervisor / "daemon.identity.json").write_text(json.dumps({"executable": str(selected / "pueued")}))
                self.assertEqual(assert_state_supervisor_pair(root / "state", selected), selected)
            finally:
                lifetime.ownership.rollback()
                lifetime.ownership.release()

    def test_failed_owned_native_helper_retains_bounded_exit_and_diagnostics(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            responses = root / "responses"
            responses.mkdir()
            case = Mock(spec=recovery_chain.SchedulerCase)
            case.responses = responses
            case.lifetime = SimpleNamespace(base=root, env={}, attempted_task_ids=[])
            case.fixture = SimpleNamespace(state=root / "state", workspace=root / "workspace")
            owner = DirectoryOwner()
            results = [BoundedCommandResult(0, b""),
                       BoundedCommandResult(7, b"", b"construct cooperative fixture journal: rejected binding")]
            try:
                with patch.object(recovery_chain, "run_bounded_command", side_effect=results):
                    with self.assertRaisesRegex(AcceptanceFailure, "native-upgrade-fixture.json"):
                        recovery_chain.native_seed(case, REPOSITORY, owner, [])
                evidence = json.loads((responses / "native-upgrade-fixture.json").read_text())
                self.assertEqual(evidence["exit_code"], 7)
                self.assertEqual(evidence["stderr"], results[1].stderr.decode())
                self.assertEqual(evidence["stderr_bytes"], len(results[1].stderr))
                self.assertNotIn("environment", evidence)
            finally:
                owner.release()

    def test_chain_task_closure_does_not_consume_unclosed_inspection_as_root_closure(self):
        fixture = InspectionFixture(self)
        inspection = fixture.inspection_records(managed_exit_proof=True)
        directory = fixture.durable_ordinary_admission(runner_ownership="managed")
        fixture.managed_ordinary_lifetime(directory)
        # Fixture construction only: retain a deliberately incomplete synthetic
        # inspection beside the complete task. No native process exists here.
        (inspection / "lifetime-termination.json").unlink()
        incomplete = {path.name: path.read_bytes() for path in inspection.iterdir()}
        responses = fixture.base / "responses"
        responses.mkdir()
        case = Mock(spec=SchedulerCase)
        case.fixture = SimpleNamespace(state=fixture.state, workspace=fixture.base)
        case.lifetime = FixtureLifetime(base=fixture.base, attempted_task_ids=[TASK])
        case.responses, case.root_id, case.command_count = responses, ROOT, 0
        case.remaining.return_value = 5
        observed = {"root_id": ROOT, "task_id": TASK, "closed": True, "kind": "registered-native",
                    "reference_sha256": digest(directory / "lifetime/worker-wait.json")}
        with patch.object(recovery_chain, "run_bounded_command", return_value=BoundedCommandResult(0, json.dumps(observed).encode())):
            selected = recovery_chain.observe_chain_task_closure(case, fixture.state, (TASK,))
        closure = json_object(json_object(selected["tasks"])[TASK])
        self.assertEqual(json_object(closure["worker_wait"])["result"], "success")
        self.assertTrue(json_object(closure["termination"])["scope_empty"])
        self.assertEqual(incomplete, {path.name: path.read_bytes() for path in inspection.iterdir()})
        self.assertIsNone(case.lifetime.closure_reader)
        with self.assertRaises(AcceptanceFailure):
            validate_managed_lifetime_closure(fixture.state, (TASK,))
        with patch.object(recovery_chain, "run_bounded_command") as process:
            with self.assertRaisesRegex(AcceptanceFailure, "unowned root or task"):
                recovery_chain.observe_chain_task_closure(case, fixture.state, ("c" * 32,))
            process.assert_not_called()


if __name__ == "__main__":
    unittest.main()
