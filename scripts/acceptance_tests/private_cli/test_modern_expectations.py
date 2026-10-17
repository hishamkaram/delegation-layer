"""Migration response expectations and candidate collection are exact controls."""

from pathlib import Path
from copy import deepcopy
import json
import unittest
from unittest.mock import MagicMock, patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.delegate_acceptance.private_cli import contention_controls as controls
from scripts.delegate_acceptance.private_cli.contention_policy import (
    migration_expectation, require_control_response, require_migration_response,
)
from scripts.delegate_acceptance.evidence.records import parse_json_output
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort


class ModernMigrationExpectationTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)
        self.fixture.minimal_task()

    def test_healthy_baseline_writer_succeeds_while_candidate_reader_holds_shared_lease(self):
        result = controls.prove_contention_round(
            processes=self.fixture.processes, reader=self.fixture.candidate.delegate, writer=self.fixture.baseline,
            state=self.fixture.state, controls=self.fixture.controls, label="healthy-current", task_id=self.fixture.task_id,
            immutable_content=lambda: {"task": (self.fixture.state / "tasks" / self.fixture.task_id / "meta.json").read_bytes()},
            assert_live=lambda: None,
        )
        self.assertFalse(result["active_work_refused"])
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_candidate_collects_both_existing_tasks_using_the_one_task_ledger(self):
        tasks = self.fixture.task_operations()
        tasks.tasks.update({"modern-baseline": self.fixture.task_id, "modern-candidate": "c" * 32})
        observed = []
        for name, installation in (("modern-baseline", self.fixture.baseline), ("modern-candidate", self.fixture.candidate)):
            directory = tasks.state / "tasks" / tasks.tasks[name]
            if not directory.exists():
                self.fixture.owner.create("candidate task", directory, directory.parent)
            runner = directory / "managed-runner"
            runner.write_bytes(installation.runner.read_bytes())
            (directory / "meta.json").write_text(json.dumps({"runner_ownership": "managed", "runner_executable": str(runner)}))

        def wait_for_original_runner(_name, task_id):
            self.assertEqual(tasks.delegate, self.fixture.candidate.delegate)
            return tasks._ordinary_runner_executable(task_id)

        def collect(_name, task_id):
            observed.append((tasks.delegate, tasks.runner, task_id))
            return {"task_id": task_id}

        with (
            patch.object(tasks, "wait_task", side_effect=wait_for_original_runner) as wait,
            patch.object(tasks, "collect", side_effect=collect),
            patch.object(controls, "verify_collected_outcome") as verify,
        ):
            controls.collect_modern_tasks(tasks, self.fixture.baseline, self.fixture.candidate)
        self.assertEqual(observed, [
            (self.fixture.candidate.delegate, installation.runner, task_id)
            for installation, task_id in zip((self.fixture.baseline, self.fixture.candidate), tasks.tasks.values(),
                strict=True)
        ])
        self.assertEqual(wait.call_count, 2)
        self.assertEqual(verify.call_count, 2)
        for call, task_id in zip(verify.call_args_list, tasks.tasks.values(), strict=True):
            self.assertEqual(call.args, ({"task_id": task_id}, Path(tasks.state) / "tasks" / task_id, "committed"))

    def test_prepared_old_reader_is_a_separate_negative_control_candidate_observation_stays_strict(self):
        self.fixture.prepare_migration()
        state, task_id = self.fixture.state, self.fixture.task_id
        negative = self.fixture.processes.run(
            "baseline-postprepared-observation-negative-control",
            [self.fixture.baseline.delegate, "--root", state, "status", task_id, "--json"],
            self.fixture.root.directory, expected=1, timeout=10,
        )
        response = parse_json_output(negative, "known baseline fixture negative control")
        self.assertEqual(response["error"], "private supervisor migration is required: published private supervisor transition is incomplete")
        with self.assertRaisesRegex(RuntimeError, "operational error"):
            require_control_response(response, self.fixture.root_id, task_id=task_id)
        tasks = self.fixture.task_operations()
        tasks.root_id = self.fixture.root_id
        tasks.tasks["modern-candidate"] = task_id
        result = controls.prove_candidate_observation(
            tasks, self.fixture.candidate, self.fixture.controls,
            lambda: {"meta": (state / "tasks" / task_id / "meta.json").read_bytes()}, lambda: None,
        )
        self.assertIsNotNone(result["migration_facts"]["migration"])
        self.assertIsNotNone(result["migration_facts"]["alias"])
        self.assertEqual(result["response"]["task_id"], task_id)
        self.assertEqual(result["immutable_content_before"], result["immutable_content_after"])
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_wrong_busy_payload_or_exit_never_passes_and_all_controls_close(self):
        for changes, error in (
            ({"writer_exit": 0}, "unexpected exit"),
            ({"writer_control": {"reason_code": "unknown"}}, "contract"),
            ({"writer_control": {"active_work": "none"}}, "contract"),
            ({"writer_error": "an unrelated runtime error"}, "unexpected error"),
        ):
            with self.subTest(changes=changes):
                fixture = ModernControlFixture(self)
                fixture.minimal_task()
                fixture.write_options(**changes)
                with self.assertRaisesRegex(RuntimeError, error):
                    controls.prove_contention_round(
                        processes=fixture.processes, reader=fixture.baseline.delegate, writer=fixture.candidate,
                        state=fixture.state,
                        controls=fixture.controls, label="bad-response", task_id=fixture.task_id, immutable_content=lambda: {},
                        assert_live=lambda: None,
                    )
                self.assertTrue(fixture.processes.finished_naturally())

    def test_candidate_warm_admission_cannot_replace_the_published_daemon_pid(self):
        self.fixture.write_options(candidate_admission_pid=424243)
        tasks = self.fixture.task_operations()
        fixture = self.fixture.modern_input()
        with self.assertRaisesRegex(RuntimeError, "daemon PID"):
            controls.dispatch_modern_pair(tasks, self.fixture.baseline, self.fixture.candidate, fixture, fixture.brief)
        self.assertEqual(len(tasks.dispatch_attempts), 2)
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_both_lock_directions_precede_candidate_read_of_prepared_intent(self):
        self.fixture.write_options(prepare_intent=True)
        tasks = self.fixture.task_operations()
        fixture = self.fixture.modern_input()
        responses = controls.dispatch_modern_pair(
            tasks, self.fixture.baseline, self.fixture.candidate, fixture, fixture.brief
        )
        cohort = MagicMock(spec=LiveFIFOLeaseCohort)
        with (
            patch.object(controls, "capacity_lease_specifications", return_value=[]),
            patch.object(controls, "prove_lifetime_owner_cohort", return_value={"fixture_only": True}),
        ):
            _owner, _active, rounds = controls.run_live_contention(
                tasks, self.fixture.baseline, self.fixture.candidate, fixture, responses, cohort
            )
        self.assertEqual([(r["reader"], r["writer"]) for r in rounds], [
            (str(self.fixture.candidate.delegate), str(self.fixture.baseline.delegate)),
            (str(self.fixture.baseline.delegate), str(self.fixture.candidate.delegate)),
        ])
        self.assertEqual([r["migration_expectation"]["case"] for r in rounds],
                         ["healthy_same_generation", "active_provenance_change"])
        observation = rounds[1]["candidate_post_migration_observation"]
        self.assertIsNotNone(observation["migration_facts"]["migration"])
        self.assertIsNotNone(observation["migration_facts"]["alias"])
        self.assertEqual(observation["response"]["task_id"], tasks.tasks["modern-candidate"])
        self.assertEqual(cohort.assert_live.call_count, 8)
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_daemon_provenance_and_pid_drift_are_rejected_with_joined_controls(self):
        for record in ("daemon.identity.json", "supervisor.identity.json", "run/pueue.pid"):
            with self.subTest(record=record):
                fixture = ModernControlFixture(self)
                fixture.minimal_task()
                fixture.write_options(mutate_daemon=record)
                with self.assertRaisesRegex(RuntimeError, "immutable|PID record"):
                    controls.prove_contention_round(
                        processes=fixture.processes, reader=fixture.baseline.delegate, writer=fixture.candidate,
                        state=fixture.state,
                        controls=fixture.controls, label="daemon-drift", task_id=fixture.task_id, immutable_content=lambda: {},
                        assert_live=lambda: None,
                    )
                self.assertTrue(fixture.processes.finished_naturally())


class ModernMigrationFactPolicyTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)

    def facts(self, writer=None):
        return controls.observe_migration(self.fixture.state, writer or self.fixture.candidate)

    def test_identity_and_phase_select_exact_noop_busy_and_final_migration_contracts(self):
        current = migration_expectation(self.facts(self.fixture.baseline), active=True)
        self.assertEqual((current.exit_code, current.state, current.active_work), (0, "migrated", "present"))
        changed = migration_expectation(self.facts(), active=True)
        self.assertEqual((changed.exit_code, changed.state, changed.reason_code), (1, "blocked", "active_work"))
        self.fixture.prepare_migration()
        prepared = self.facts()
        self.assertIsNotNone(prepared["alias"])
        self.assertEqual(migration_expectation(prepared, active=True), changed)
        final = migration_expectation(prepared, active=False)
        self.assertEqual((final.exit_code, final.state, final.migration_phase), (0, "migrated", "complete"))
        prepared["migration"] = None
        self.assertEqual(migration_expectation(prepared, active=True), changed)
        self.assertEqual(migration_expectation(prepared, active=False), final)

    def test_same_bytes_at_another_daemon_path_still_require_provenance_change(self):
        facts = self.facts(self.fixture.baseline)
        facts["writer"]["executable"] += "-different-path"
        self.assertEqual(migration_expectation(facts, active=True).case, "active_provenance_change")

    def test_rejects_postshutdown_or_mismatched_prepared_intent_and_old_writer_retarget(self):
        self.fixture.prepare_migration()
        for field, value in (("legacy_shutdown_requested", True), ("legacy_shutdown_dispatched", True),
                             ("legacy_stopped", True), ("legacy_pid", 123), ("legacy_process_start", "start"),
                             ("legacy_shutdown_requested", 1)):
            facts = self.facts()
            facts["migration"][field] = value
            with self.subTest(field=field, value=value), self.assertRaisesRegex(RuntimeError, "phase"):
                migration_expectation(facts, active=True)
        for role in ("legacy", "successor"):
            facts = self.facts()
            facts["migration"][role]["sha256"] = "f" * 64
            with self.subTest(role=role), self.assertRaises(RuntimeError):
                migration_expectation(facts, active=True)
        with self.assertRaisesRegex(RuntimeError, "negative control"):
            migration_expectation(self.facts(self.fixture.baseline), active=True)

    def test_rejects_wrong_alias_or_publication_facts_instead_of_waiving_migration_errors(self):
        self.fixture.prepare_migration()
        original = self.facts()
        for path, value in (
            (("alias", "supervisor", "endpoint"), "unix:/another"),
            (("alias", "config_sha256"), "f" * 64),
            (("alias", "supervisor", "daemon_sha256"), "f" * 64),
            (("alias", "schema_version"), True),
            (("daemon", "sha256"), "f" * 64),
            (("unsettled_records",), ["identity.pending.json"]),
            (("alias",), None),
        ):
            facts = deepcopy(original)
            owner = facts
            for key in path[:-1]:
                owner = owner[key]
            owner[path[-1]] = value
            with self.subTest(path=path), self.assertRaises(RuntimeError):
                migration_expectation(facts, active=True)
        original["migration"] = None
        with self.assertRaisesRegex(RuntimeError, "negative control"):
            migration_expectation({**original, "writer": original["daemon"]}, active=True)

    def test_busy_response_must_match_exact_reason_phase_action_and_public_error(self):
        expected = migration_expectation(self.facts(), active=True)
        response = {"schema_version": 1, "command": "supervisor", "root_id": self.fixture.root_id,
                    "supervisor_control": {"action": "migrate", "state": "blocked", "active_work": "present",
                                           "reason_code": "active_work", "next_action": "wait_for_legacy_work"},
                    "error": "Legacy supervisor has active work. Wait for queued or running work before migrating."}
        require_migration_response(response, self.fixture.root_id, expected)
        for field, value in (("state", "migrated"), ("action", "status"), ("active_work", "none"),
                             ("reason_code", "unknown"), ("next_action", "none"), ("migration_phase", "complete")):
            changed = deepcopy(response)
            changed["supervisor_control"][field] = value
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                require_migration_response(changed, self.fixture.root_id, expected)
        for field, value in (("error", "unknown error"), ("error", None), ("schema_version", True), ("command", "status")):
            with self.subTest(field=field, value=value), self.assertRaises(RuntimeError):
                require_migration_response({**response, field: value}, self.fixture.root_id, expected)

    def test_noop_response_cannot_be_reported_as_busy_or_contain_an_operational_error(self):
        expected = migration_expectation(self.facts(self.fixture.baseline), active=True)
        response = {"schema_version": 1, "command": "supervisor", "root_id": self.fixture.root_id,
                    "supervisor_control": {"action": "migrate", "state": "migrated", "active_work": "present",
                                           "migration_phase": "complete", "next_action": "retry_dispatch"}}
        require_migration_response(response, self.fixture.root_id, expected)
        with self.assertRaisesRegex(RuntimeError, "unexpected error"):
            require_migration_response({**response, "error": "runtime failed"}, self.fixture.root_id, expected)
        response["supervisor_control"]["state"] = "blocked"
        with self.assertRaisesRegex(RuntimeError, "migrated"):
            require_migration_response(response, self.fixture.root_id, expected)
