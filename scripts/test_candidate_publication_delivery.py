"""Hosted publication loads a real ZIP, checks actual CI history, and delivers its exact bytes."""

import contextlib
import io
import json
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts import publish_development_candidate as cli
from scripts.delegate_acceptance.release_verification import candidate_publication_delivery as delivery
from scripts.delegate_acceptance.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_ports import default_ports
from scripts.test_candidate_artifact_support import (
    ARTIFACT_ID, COMMIT, DOWNLOAD, ENDPOINTS, REPOSITORY, RUN_ID, TREE, CandidateArtifactCase,
)
from scripts import test_candidate_publication_preflight as ci_fixture
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_release_github_transport import ObservedGitHubClient
from scripts.delegate_acceptance.release_verification import github


SECRETS = {"GH_TOKEN": "github-private-canary", "HOMEBREW_TAP_TOKEN": "tap-private-canary",
           "NPM_TOKEN": "npm-private-canary"}


def environment(**changes):
    return {"GITHUB_REPOSITORY": REPOSITORY, "GITHUB_REF": "refs/heads/main",
            "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_RUN_ID": "7001", "GITHUB_RUN_ATTEMPT": "2",
            "GITHUB_SHA": COMMIT,
            "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/release.yml@refs/heads/main",
            "PATH": os.environ["PATH"], **SECRETS, **changes}


class PublicationTests(CandidateArtifactCase):
    def setUp(self):
        super().setUp()
        self.served = self.serve(change=ci_fixture.trusted_metadata)
        routes = json.loads(self.routes_path.read_text())
        routes.update({
            ci_fixture.jobs_endpoint(): {"json": {"total_count": len(ci_fixture.jobs()), "jobs": ci_fixture.jobs()}},
            f"repos/{REPOSITORY}/commits/{COMMIT}": {
                "json": {"sha": COMMIT, "commit": {"tree": {"sha": TREE}}}},
            f"repos/{REPOSITORY}/branches/main": {"json": {"commit": {"sha": COMMIT}}},
        })
        self.routes_path.write_text(json.dumps(routes))
        self.checkout = self.root / "checkout"
        self.checkout.mkdir()
        self.events = []
        self.receipts = []
        self.environments = []

    def request(self, *, resume=False, name="output"):
        return delivery.HostedPublication(artifact_id=ARTIFACT_ID, resume_published=resume,
            run=delivery.hosted_run(environment()), output_dir=self.root / name, checkout=self.checkout)

    def client(self, receipt, env):
        self.receipts.append(receipt)
        self.environments.append(env)
        return ObservedGitHubClient(receipt, {**env, "PATH": str(self.root / "bin"),
            "FIXTURE_ROUTES": str(self.routes_path), "FIXTURE_LOG": str(self.log_path)}, self.tree)

    def assert_payload(self, artifact):
        for name, data in {**self.served.files, MANIFEST_PATH: self.served.manifest}.items():
            self.assertEqual((artifact.payload / name).read_bytes(), data)
        self.assertEqual(artifact.container.path.read_bytes(), self.served.container.read_bytes())

    @contextlib.contextmanager
    def scripted_run_replies(self, states):
        """Vary synthetic metadata after real transport reads, never hosted GitHub state."""
        real = ObservedGitHubClient.json
        seen = []

        def observed(client, endpoint, *args, **kwargs):
            record = real(client, endpoint, *args, **kwargs)
            if endpoint == ENDPOINTS["attempt"]:
                self.assertIsInstance(record, dict)
                self.assertLess(len(seen), len(states), "unexpected extra CI run observation")
                record.update(states[len(seen)])
                seen.append(record.copy())
            return record

        with patch.object(ObservedGitHubClient, "json", observed):
            yield seen

    def assert_only_publication_reads(self):
        allowed = {*ENDPOINTS.values(), DOWNLOAD, ci_fixture.jobs_endpoint(),
                   f"repos/{REPOSITORY}/commits/{COMMIT}", f"repos/{REPOSITORY}/branches/main"}
        calls = [json.loads(line) for line in self.log_path.read_text().splitlines()]
        self.assertTrue(calls)
        for call in calls:
            self.assertEqual(call["argv"][:3], ["api", "--method", "GET"])
            self.assertIn(call["endpoint"], allowed)

    def publish(self, *, fail=None, resume=False, name="output"):
        request = self.request(resume=resume, name=name)

        def prepare(subject, artifact, **options):
            self.assert_payload(artifact)
            self.assertEqual(subject.manifest, artifact.manifest)
            self.assertEqual(options["resume_published"], resume)
            self.events.append("prepare")
            if fail == "prepare":
                raise VerificationError("release_conflict", "prepare")
            return SimpleNamespace(release_id=501)

        def published(subject, prepared, **options):
            self.events.append("github")
            receipt = options["client"].receipt
            receipt["commands"].append({"name": "github-api", "method": "PATCH", "endpoint": "releases/501"})
            if fail == "github":
                raise VerificationError("github_write_unconfirmed", "publish")
            return prepared

        def npm(artifact, **options):
            self.events.append("npm")
            self.assert_payload(artifact)
            self.assertEqual(options["resume_published"], resume)
            if fail == "npm":
                raise VerificationError("npm_conflict", "npm")
            return SimpleNamespace(version=artifact.manifest.version, integrity="sha512-fixture")

        def tap(artifact, **options):
            self.events.append("tap")
            self.assert_payload(artifact)
            self.assertEqual(options["client"].environment["GH_TOKEN"], SECRETS["HOMEBREW_TAP_TOKEN"])
            if fail == "tap":
                raise VerificationError("tap_conflict", "tap")
            return "f" * 40

        with patch.object(delivery, "GitHubClient", self.client), \
                patch.object(delivery, "prepare_release_assets", prepare), \
                patch.object(delivery, "publish_prepared_release", published), \
                patch.object(delivery, "deliver_npm", npm), patch.object(delivery, "deliver_tap", tap), \
                contextlib.redirect_stderr(io.StringIO()):
            code = self.tree.observe_operation(lambda: delivery.publish_candidate(
                request, environment=environment(), ports=default_ports()))
        receipt = json.loads((request.output_dir / delivery.RECEIPT_NAME).read_text())
        return code, receipt, request

    def test_real_zip_and_full_ci_history_precede_exact_byte_delivery_and_cleanup(self):
        code, receipt, request = self.publish()
        self.assertEqual(code, 0)
        self.assertEqual(self.events, ["prepare", "github", "npm", "tap"])
        self.assertEqual((receipt["verdict"], receipt["publication"]["status"]), ("pass", "delivered"))
        self.assertTrue(receipt["publication"]["first_mutation_started"])
        self.assertFalse((request.output_dir / delivery.WORKSPACE_NAME).exists())
        self.assertIn(ci_fixture.jobs_endpoint(), self.requested())
        for secret in SECRETS.values():
            self.assertNotIn(secret, json.dumps(receipt))

    def test_ci_refusal_starts_no_destination_and_retains_input_bytes(self):
        routes = json.loads(self.routes_path.read_text())
        routes[ENDPOINTS["attempt"]]["json"]["conclusion"] = "failure"
        self.routes_path.write_text(json.dumps(routes))
        code, receipt, request = self.publish()
        self.assertEqual((code, self.events), (1, []))
        self.assertFalse(receipt["publication"]["first_mutation_started"])
        self.assertEqual(receipt["failure"]["code"], "distribution_ci_not_successful")
        self.assertTrue((request.output_dir / delivery.WORKSPACE_NAME).is_dir())

    def test_changing_run_titles_preserve_exact_artifact_delivery(self):
        titles = ("Initial CI title", "Renamed CI title", "Latest CI title")
        states = tuple({"display_title": title} for title in titles)
        with self.scripted_run_replies(states) as seen:
            code, receipt, request = self.publish()
        self.assertEqual([row["display_title"] for row in seen], list(titles))
        self.assertEqual(code, 0)
        self.assertEqual(self.events, ["prepare", "github", "npm", "tap"])
        self.assertEqual((receipt["verdict"], receipt["publication"]["status"]), ("pass", "delivered"))
        artifact = receipt["publication"]["artifact"]
        self.assertEqual((artifact["artifact"]["id"], artifact["attempt"]["run_id"], artifact["source_commit"]),
                         (ARTIFACT_ID, RUN_ID, COMMIT))
        self.assertFalse((request.output_dir / delivery.WORKSPACE_NAME).exists())
        self.assert_only_publication_reads()

    def test_changing_titles_cannot_hide_ci_failure_or_source_mismatch(self):
        cases = (
            ("failed", {"conclusion": "failure"}, "distribution_ci_not_successful"),
            ("running", {"status": "in_progress", "conclusion": None}, "distribution_ci_not_successful"),
            ("unknown-status", {"status": "unknown"}, "distribution_ci_not_successful"),
            ("malformed-status", {"status": []}, "distribution_ci_not_successful"),
            ("missing-run", {"id": None}, "distribution_source_untrusted"),
            ("foreign-run", {"id": RUN_ID + 1}, "distribution_source_untrusted"),
            ("source", {"head_sha": "f" * 40}, "distribution_source_untrusted"),
            ("workflow", {"workflow_id": 7}, "distribution_source_untrusted"),
            ("branch", {"head_branch": "task"}, "distribution_source_untrusted"),
            ("event", {"event": "pull_request"}, "distribution_source_untrusted"),
            ("path", {"path": ".github/workflows/release.yml"}, "distribution_source_untrusted"),
            ("repository", {"repository": {"id": 7}}, "distribution_source_untrusted"),
            ("head-repository", {"head_repository": {"id": 7}}, "distribution_source_untrusted"),
        )
        for read in (2, 3):
            for name, changes, expected in cases:
                with self.subTest(read=read, case=name):
                    self.events.clear()
                    self.log_path.unlink(missing_ok=True)
                    states = tuple({"display_title": f"CI title {index}",
                                    **(changes if index == read else {})} for index in (1, 2, 3))
                    with self.scripted_run_replies(states) as seen:
                        code, receipt, request = self.publish(name=f"refused-{read}-{name}")
                    self.assertEqual(len(seen), read)
                    self.assertNotEqual(seen[0]["display_title"], seen[-1]["display_title"])
                    self.assertEqual((code, self.events), (1, []))
                    self.assertEqual(receipt["failure"]["code"], expected)
                    self.assertFalse(receipt["publication"]["first_mutation_started"])
                    payload = request.output_dir / delivery.WORKSPACE_NAME / "payload"
                    self.assertEqual((payload / MANIFEST_PATH).read_bytes(), self.served.manifest)
                    self.assert_only_publication_reads()

    def test_each_conflict_stops_later_destinations_and_retains_workspace(self):
        for fail, expected in (("prepare", ["prepare"]), ("github", ["prepare", "github"]),
                               ("npm", ["prepare", "github", "npm"]), ("tap", ["prepare", "github", "npm", "tap"])):
            with self.subTest(fail=fail):
                self.events.clear()
                code, receipt, request = self.publish(fail=fail, name=fail)
                self.assertEqual((code, self.events), (1, expected))
                self.assertEqual(receipt["publication"]["status"], "partial")
                self.assertEqual(receipt["publication"]["first_mutation_started"], fail != "prepare")
                self.assertTrue((request.output_dir / delivery.WORKSPACE_NAME).is_dir())

    def test_explicit_resume_reuses_the_same_artifact_with_no_npm_secret(self):
        request = self.request(resume=True)
        config = self.root / "config"
        env = environment()
        del env["NPM_TOKEN"]
        isolated = delivery._environments(env, config, resume_published=True)
        self.assertNotIn("NPM_TOKEN", isolated.npm)
        self.assertEqual(isolated.npm_user_config, b"")
        code, receipt, _ = self.publish(resume=True)
        self.assertEqual(code, 0)
        self.assertEqual(receipt["publication"]["artifact_id"], ARTIFACT_ID)

    def test_unproven_download_child_keeps_workspace_and_original_error(self):
        fault = ChildFault(lambda argv: argv[:1] == ["gh"])
        with patch.object(github, "run_bounded_command", fault):
            code, receipt, request = self.publish()
        self.assertEqual(code, 1)
        self.assertEqual(receipt["failure"]["code"], "command_timeout")
        self.assertEqual(self.events, [])
        self.assertIsNotNone(self.tree.unresolved)
        settle_retained(self, self.tree, fault,
                        (request.output_dir / delivery.WORKSPACE_NAME, request.output_dir / delivery.RECEIPT_NAME))


class BoundaryTests(unittest.TestCase):
    def test_hosted_context_requires_trusted_main_but_accepts_later_attempts(self):
        self.assertEqual(delivery.hosted_run(environment()).run_attempt, 2)
        for key, value in (("GITHUB_REF", "refs/heads/task"), ("GITHUB_EVENT_NAME", "pull_request"),
                           ("GITHUB_REPOSITORY", "fork/repo"), ("GITHUB_RUN_ATTEMPT", "0"),
                           ("GITHUB_WORKFLOW_REF", REPOSITORY + "/.github/workflows/other.yml@refs/heads/main")):
            with self.subTest(key=key), self.assertRaises(VerificationError):
                delivery.hosted_run(environment(**{key: value}))

    def test_credentials_are_separated_and_missing_credentials_refuse(self):
        result = delivery._environments(environment(), Path("/config"), resume_published=False)
        self.assertEqual(result.github["GH_TOKEN"], SECRETS["GH_TOKEN"])
        self.assertEqual(result.tap["GH_TOKEN"], SECRETS["HOMEBREW_TAP_TOKEN"])
        self.assertEqual(result.npm["NPM_TOKEN"], SECRETS["NPM_TOKEN"])
        self.assertNotIn("GH_TOKEN", result.npm)
        self.assertNotIn("NPM_TOKEN", result.github)
        for name in SECRETS:
            env = environment()
            del env[name]
            with self.subTest(name=name), self.assertRaises(VerificationError):
                delivery._environments(env, Path("/config"), resume_published=False)

    def test_invalid_cli_inputs_refuse_before_the_output_owner_runs(self):
        for changes in ({"ARTIFACT_ID": ""}, {"ARTIFACT_ID": "0"}, {"ARTIFACT_ID": "x"},
                        {"RESUME_PUBLISHED": "yes"}):
            with self.subTest(changes=changes), patch.dict(os.environ, environment(
                    **{"ARTIFACT_ID": str(ARTIFACT_ID), "RESUME_PUBLISHED": "false", **changes})), \
                    patch.object(cli, "publish_candidate") as publisher, \
                    contextlib.redirect_stderr(io.StringIO()) as error:
                self.assertEqual(cli.main(["--output-dir", "/output"]), 1)
                self.assertIn("publication_input_invalid", error.getvalue())
                publisher.assert_not_called()

    def test_cli_parses_only_real_artifact_identity_and_boolean_resume(self):
        with patch.dict(os.environ, environment(ARTIFACT_ID=str(ARTIFACT_ID), RESUME_PUBLISHED="true")), \
                patch.object(cli, "publish_candidate", return_value=0) as publisher:
            self.assertEqual(cli.main(["--output-dir", "/output"]), 0)
        request = publisher.call_args.args[0]
        self.assertEqual((request.artifact_id, request.resume_published), (ARTIFACT_ID, True))


if __name__ == "__main__":
    unittest.main()
