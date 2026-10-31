"""Publication trusts complete GitHub job history and the artifact's actual producer chain."""

from copy import deepcopy
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
    return {"id": identity, "name": name, "run_attempt": attempt, "run_id": RUN_ID,
            "head_sha": COMMIT, "status": "completed", "conclusion": "success", **changes}


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
