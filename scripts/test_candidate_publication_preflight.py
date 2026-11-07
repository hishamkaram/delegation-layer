"""Publication trusts complete GitHub job history and the artifact's actual producer chain."""

from copy import deepcopy
from datetime import UTC, datetime, timedelta, timezone
import json
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.release_verification import candidate_publication_preflight as preflight
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_artifact_support import COMMIT, ENDPOINTS, REPOSITORY, RUN_ID, TREE, CandidateArtifactCase
from scripts.test_release_github_transport import ObservedGitHubClient


def trusted_metadata(replies):
    replies["repository"]["private"] = False
    replies["attempt"].update(head_branch="main", run_attempt=3, event="push")
    for name in ("repository", "head_repository"):
        replies["attempt"][name]["full_name"] = REPOSITORY


def job(name, attempt=2, identity=1, **changes):
    started = datetime(2026, 1, 1, tzinfo=UTC) + timedelta(hours=attempt)
    finished = started + timedelta(minutes=1)
    return {"id": identity, "name": name, "run_attempt": attempt, "run_id": RUN_ID,
            "head_sha": COMMIT, "status": "completed", "conclusion": "success",
            "created_at": (started - timedelta(seconds=2)).isoformat(),
            "started_at": started.isoformat(), "completed_at": finished.isoformat(),
            "runner_id": 7, "runner_name": "fixture-runner",
            "steps": [{"name": "Prove the candidate", "number": 1, "status": "completed",
                       "conclusion": "success", "started_at": started.isoformat(),
                       "completed_at": finished.isoformat()}], **changes}


def carried(row, attempt=3, identity=100):
    """Observed GitHub rerun shape: new record, unchanged completed execution."""
    return {**deepcopy(row), "id": identity, "run_attempt": attempt,
            "created_at": datetime(2026, 1, 1, attempt, tzinfo=UTC).isoformat(),
            "runner_group_id": None}


def jobs():
    return [job(name, identity=index + 1) for index, name in enumerate(preflight.REQUIRED_JOBS)]


def jobs_endpoint(page=1):
    return f"repos/{REPOSITORY}/actions/runs/{RUN_ID}/jobs?filter=all&per_page=100&page={page}"


class PreflightTests(CandidateArtifactCase):
    def setUp(self):
        super().setUp()
        served = self.serve(change=trusted_metadata)
        self.artifact = self.load(served.locator)
        self.routes = json.loads(self.routes_path.read_text())
        self.routes.update({
            jobs_endpoint(): {"json": {"total_count": len(jobs()), "jobs": jobs()}},
            f"repos/{REPOSITORY}/commits/{COMMIT}": {
                "json": {"sha": COMMIT, "commit": {"tree": {"sha": TREE}}}},
            f"repos/{REPOSITORY}/branches/main": {"json": {"commit": {"sha": COMMIT}}},
        })
        self.log_path.unlink()

    def run_preflight(self):
        self.routes_path.write_text(json.dumps(self.routes))
        client = ObservedGitHubClient({"commands": []}, {
            "PATH": str(self.root / "bin"), "FIXTURE_ROUTES": str(self.routes_path),
            "FIXTURE_LOG": str(self.log_path)}, self.tree)
        preflight.require_publishable(client, self.artifact)

    def refuse(self, code):
        with self.assertRaises(VerificationError) as caught:
            self.run_preflight()
        self.assertEqual(caught.exception.code, code)
        self.assert_only_github_children()

    def set_jobs(self, rows):
        self.routes[jobs_endpoint()] = {"json": {"total_count": len(rows), "jobs": rows}}

    def test_carried_successful_jobs_are_valid_in_a_later_successful_attempt(self):
        self.run_preflight()
        self.assertIn(jobs_endpoint(), self.requested())
        self.assertFalse(any("/attempts/" in endpoint for endpoint in self.requested()))

    def test_copied_successes_keep_the_original_artifact_publishable(self):
        original = jobs()
        copies = [carried(row, identity=100 + index) for index, row in enumerate(original)]
        # The failed macOS job actually reruns; all other successes are copied.
        mac = preflight.CHECK_JOBS[1]
        copies = [row if row["name"] != mac else job(mac, 3, row["id"]) for row in copies]
        self.set_jobs([*copies, *reversed(original)])
        self.run_preflight()

    def test_copied_old_installs_do_not_prove_a_new_producer(self):
        original = [job(name, 1, 100 + index) for index, name in enumerate(preflight.REQUIRED_JOBS)]
        current = [job(preflight.PRODUCER_JOB, 2, 1)]
        current += [carried(row, identity=200 + index) for index, row in enumerate(original)
                    if row["name"] != preflight.PRODUCER_JOB]
        self.set_jobs([*original, *current])
        self.refuse("distribution_install_predates_producer")

    def test_old_producer_copy_cannot_hide_an_intervening_rebuild(self):
        original = job(preflight.PRODUCER_JOB, 1, 90)
        self.set_jobs([*jobs(), original, carried(original)])
        self.refuse("distribution_job_provenance_unproven")

    def test_copy_requires_an_unambiguous_original_execution(self):
        original = jobs()
        copies = [carried(row, identity=100 + index) for index, row in enumerate(original)]
        self.set_jobs(copies)
        self.refuse("distribution_job_provenance_unproven")
        self.set_jobs([*original, *copies, job(preflight.PRODUCER_JOB, 2, 90)])
        self.refuse("distribution_job_ambiguous")

    def test_carry_identity_requires_matching_execution_steps(self):
        original = jobs()
        copy = carried(original[0])
        copy["steps"][0]["name"] = "A different execution"
        self.set_jobs([*original, copy])
        self.refuse("distribution_job_provenance_unproven")

    def test_real_rebuild_can_reuse_the_same_or_null_runner(self):
        for runner in (7, None):
            with self.subTest(runner=runner):
                original = jobs()
                original[0]["runner_id"] = runner
                rebuilt = job(preflight.PRODUCER_JOB, 3, 90, runner_id=runner)
                self.set_jobs([*original, rebuilt])
                self.refuse("distribution_artifact_superseded")

    def test_copies_allow_attempt_gaps_and_nullable_runner_metadata(self):
        self.routes[ENDPOINTS["attempt"]]["json"]["run_attempt"] = 4
        original = jobs()
        for row in original:
            row.update(runner_id=None, runner_name=None)
        copies = [carried(row, 4, 100 + index) for index, row in enumerate(original)]
        self.set_jobs([*original, *copies])
        self.run_preflight()

    def test_execution_times_compare_instants_and_ignore_additive_metadata(self):
        original = jobs()
        copies = [carried(row, identity=100 + index) for index, row in enumerate(original)]
        for row in copies:
            for key in ("started_at", "completed_at"):
                row[key] = datetime.fromisoformat(row[key]).astimezone(
                    timezone(timedelta(hours=2))).isoformat()
            for step in row["steps"]:
                step["new_api_field"] = "additive"
                for key in ("started_at", "completed_at"):
                    step[key] = step[key].replace("+00:00", "Z")
        self.set_jobs([*original, *copies])
        self.run_preflight()

    def test_older_cancelled_history_does_not_block_a_later_original(self):
        self.set_jobs([job(preflight.PRODUCER_JOB, 1, 90, status="completed",
                           conclusion="cancelled", created_at=None, started_at=None,
                           completed_at=None, steps=None), *jobs()])
        self.run_preflight()

    def test_copy_cannot_hide_an_intervening_failed_rebuild(self):
        original = job(preflight.PRODUCER_JOB, 1, 90)
        rebuilt = job(preflight.PRODUCER_JOB, 2, 1, conclusion="failure")
        self.set_jobs([*[row for row in jobs() if row["name"] != preflight.PRODUCER_JOB],
                       original, rebuilt, carried(original)])
        self.refuse("distribution_job_provenance_unproven")

    def test_incomplete_or_inconsistent_copy_evidence_refuses(self):
        for changes in ({"steps": None}, {"steps": []}, {"completed_at": None},
                        {"started_at": "invalid"}, {"created_at": "2026-01-01T03:00:00"},
                        {"created_at": "2026-01-01T02:00:01Z"},
                        {"created_at": "2026-01-01T02:01:00Z"}):
            with self.subTest(changes=changes):
                original = jobs()
                self.set_jobs([*original, {**carried(original[0]), **changes}])
                self.refuse("distribution_job_provenance_unproven")

    def test_installs_may_reexecute_after_a_carried_producer(self):
        self.set_jobs([*jobs(), *[job(name, 3, 100 + index)
                                  for index, name in enumerate(preflight.INSTALL_JOBS)]])
        self.run_preflight()

    def test_nullable_step_metadata_is_compared_without_new_requirements(self):
        original = jobs()
        for row in original:
            row["steps"][0].update(conclusion=None, started_at=None, completed_at=None)
        self.set_jobs([*original, *[carried(row, identity=100 + index)
                                   for index, row in enumerate(original)]])
        self.run_preflight()

    def test_malformed_step_identity_cannot_establish_a_copy(self):
        for changes in ({"number": True}, {"name": None}, {"status": None},
                        {"started_at": "not a timestamp"}):
            with self.subTest(changes=changes):
                original = jobs()
                copy = carried(original[0])
                copy["steps"][0].update(changes)
                self.set_jobs([*original, copy])
                self.refuse("distribution_job_provenance_unproven")

    def test_explicit_main_dispatch_is_a_trusted_producer_event(self):
        self.routes[ENDPOINTS["attempt"]]["json"]["event"] = "workflow_dispatch"
        self.run_preflight()

    def test_light_runs_with_skipped_required_checks_cannot_authorize_publication(self):
        for name in preflight.CHECK_JOBS:
            with self.subTest(name=name):
                self.set_jobs([job(row["name"], identity=index + 1,
                                   conclusion="skipped" if row["name"] == name else "success")
                               for index, row in enumerate(jobs())])
                self.refuse("distribution_job_not_successful")

    def test_history_is_paginated_in_full_before_selecting_latest_executions(self):
        rows = [job("unrelated", identity=1000 + index) for index in range(95)]
        rows += [job(preflight.INSTALL_JOBS[0], 1, 800, conclusion="failure"), *jobs()]
        self.routes[jobs_endpoint()] = {"json": {"total_count": len(rows), "jobs": rows[:100]}}
        self.routes[jobs_endpoint(2)] = {"json": {"total_count": len(rows), "jobs": rows[100:]}}
        self.run_preflight()
        self.assertIn(jobs_endpoint(2), self.requested())

    def test_latest_failed_or_running_job_cannot_be_hidden_by_an_old_success(self):
        for state in ({"conclusion": "failure"}, {"status": "in_progress", "conclusion": None}):
            with self.subTest(state=state):
                self.set_jobs([*jobs(), job(preflight.INSTALL_JOBS[0], 3, 90, **state)])
                self.refuse("distribution_job_not_successful")

    def test_a_newer_producer_supersedes_the_selected_artifact(self):
        self.set_jobs([*jobs(), job(preflight.PRODUCER_JOB, 3, 90)])
        self.refuse("distribution_artifact_superseded")

    def test_an_install_from_before_the_selected_producer_is_unproven(self):
        rows = jobs()
        rows[1]["run_attempt"] = 1
        self.set_jobs(rows)
        self.refuse("distribution_install_predates_producer")

    def test_every_one_of_the_six_channels_and_existing_checks_is_required(self):
        for name in preflight.REQUIRED_JOBS:
            with self.subTest(name=name):
                self.set_jobs([row for row in jobs() if row["name"] != name])
                self.refuse("distribution_job_missing")

    def test_duplicate_selected_executions_and_foreign_job_sources_refuse(self):
        self.set_jobs([*jobs(), job(preflight.PRODUCER_JOB, 2, 90)])
        self.refuse("distribution_job_ambiguous")
        for changes in ({"head_sha": "f" * 40}, {"run_id": RUN_ID + 1}, {"run_attempt": 4}):
            with self.subTest(changes=changes):
                rows = jobs()
                rows[1].update(changes)
                self.set_jobs(rows)
                self.refuse("distribution_job_source_mismatch")

    def test_missing_pages_duplicate_ids_and_changed_counts_refuse(self):
        rows = [job("unrelated", identity=1000 + index) for index in range(100)]
        self.routes[jobs_endpoint()] = {"json": {"total_count": 101, "jobs": rows[:99]}}
        self.refuse("distribution_jobs_incomplete")
        self.routes[jobs_endpoint()] = {"json": {"total_count": 101, "jobs": rows}}
        self.routes[jobs_endpoint(2)] = {"json": {"total_count": 102, "jobs": [rows[0]]}}
        self.refuse("distribution_jobs_invalid")
        self.routes[jobs_endpoint(2)] = {"json": {"total_count": 101, "jobs": [rows[0]]}}
        self.refuse("distribution_jobs_invalid")

    def test_run_must_be_the_same_repository_main_workflow_and_successful_latest_state(self):
        original = deepcopy(self.routes[ENDPOINTS["attempt"]]["json"])
        for changes in ({"head_branch": "task"}, {"event": "pull_request"}, {"head_sha": "f" * 40},
                        {"path": ".github/workflows/release.yml"}, {"repository": {"id": 7}}):
            with self.subTest(changes=changes):
                self.routes[ENDPOINTS["attempt"]]["json"] = {**original, **changes}
                self.refuse("distribution_source_untrusted")
        self.routes[ENDPOINTS["attempt"]]["json"] = {**original, "conclusion": "failure"}
        self.refuse("distribution_ci_not_successful")

    def test_private_repository_and_source_tree_mismatch_refuse(self):
        self.routes[ENDPOINTS["repository"]]["json"]["private"] = True
        self.refuse("publication_repository_not_public")
        self.routes[ENDPOINTS["repository"]]["json"]["private"] = False
        self.routes[f"repos/{REPOSITORY}/commits/{COMMIT}"]["json"]["commit"]["tree"]["sha"] = "f" * 40
        self.refuse("distribution_source_mismatch")

    def test_source_can_be_an_ancestor_of_current_main_but_never_divergent(self):
        newer = "f" * 40
        self.routes[f"repos/{REPOSITORY}/branches/main"]["json"]["commit"]["sha"] = newer
        endpoint = f"repos/{REPOSITORY}/compare/{COMMIT}...{newer}"
        self.routes[endpoint] = {"json": {"status": "ahead", "merge_base_commit": {"sha": COMMIT}}}
        self.run_preflight()
        self.routes[endpoint]["json"]["status"] = "diverged"
        self.refuse("distribution_source_not_on_main")

    def test_unrelated_repository_metadata_does_not_change_the_validated_run(self):
        real = ObservedGitHubClient.json
        calls = 0

        def changed(client, endpoint, *args, **kwargs):
            nonlocal calls
            record = real(client, endpoint, *args, **kwargs)
            if endpoint == ENDPOINTS["attempt"]:
                calls += 1
                record["repository"]["watchers_count"] = calls
            return record
        with patch.object(ObservedGitHubClient, "json", changed):
            self.run_preflight()
        self.assertEqual(calls, 2)

    def test_a_run_changing_during_observation_refuses(self):
        real = preflight._run
        calls = 0

        def changed(client, artifact):
            nonlocal calls
            record = real(client, artifact)
            calls += 1
            return record if calls == 1 else record + 1
        with patch.object(preflight, "_run", changed):
            self.refuse("distribution_ci_changed")


if __name__ == "__main__":
    unittest.main()
