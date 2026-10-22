"""Promotion gate regressions use the real history and submission policies."""

from __future__ import annotations

import argparse
import io
import json
import unittest
from unittest.mock import patch
import zipfile

from scripts import validate_native_release as native_cli
from scripts.delegate_acceptance.native_evidence.public_contract import ProofExpectation
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.native_submission import SubmissionIdentity
from scripts.delegate_acceptance.release_verification.native_hosted import validate_promotion
from scripts.test_native_release_support import (
    FakeGitHub, HEAD, PAYLOAD, PROOF_HASH, RELEASE, REPO, SOURCE, TAG, ReleaseFixture,
)


HOSTED = "scripts.delegate_acceptance.release_verification.native_hosted."
SUBMISSION = "scripts.delegate_acceptance.release_verification.native_submission."
GITHUB_JSON = "scripts.delegate_acceptance.release_verification.github.GitHubClient.json"
PROOF_ASSET_ID = 71
SENDER_ID = 61
REPORT_MODEL = "vendor/report-model"
REPORT_CCR_MODEL = "ccr/exact-alias"


class PromotionGitHub(FakeGitHub):
    """Existing release fake with the read ports used by hosted promotion."""

    def __init__(self, receipt):
        super().__init__(receipt)
        self.private = False
        self.permission = "write"
        self.proof_bytes = PAYLOAD
        self.artifact_rows: dict[int, list[dict[str, object]]] = {}
        self.artifact_bytes: dict[int, bytes] = {}
        self.attempt_facts: dict[tuple[int, int], dict[str, object]] = {}
        self.asset_reads = 0
        self.history_reads = 0
        asset = super().asset(asset_id=PROOF_ASSET_ID)
        asset["uploader"] = {"id": SENDER_ID, "login": "maintainer"}
        self.assets = [asset]

    def add_run(self, run_id: int, attempts: int) -> None:
        run = super().run(run_id=run_id)
        run["run_attempt"] = attempts
        self.runs.append(run)

    @staticmethod
    def report_archive(report: dict[str, object]) -> bytes:
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w") as bundle:
            bundle.writestr("report.json", json.dumps(report))
        return buffer.getvalue()

    def add_attempt(
        self, run_id: int, attempt: int, *, admission: str, verdict: str,
        stage: str, started_at: str, conclusion: str | None = "success",
        status: str = "completed", include_report: bool = True,
        opencode_model: str = REPORT_MODEL,
        ccr_model: str | None = REPORT_CCR_MODEL,
    ) -> None:
        self.attempt_facts[(run_id, attempt)] = {
            "id": run_id, "run_attempt": attempt, "head_sha": HEAD,
            "head_branch": "main", "event": "workflow_dispatch",
            "run_started_at": started_at, "status": status,
            "conclusion": conclusion,
        }
        if not include_report:
            return
        report: dict[str, object] = {
            "schema_version": 1, "kind": "native-proof-validation",
            "repository_id": 41, "tag": TAG, "harness_revision": HEAD,
            "run_id": run_id, "run_attempt": attempt,
            "proof_sha256": PROOF_HASH, "proof_asset_id": PROOF_ASSET_ID,
            "sender_id": SENDER_ID, "opencode_model": opencode_model,
            "ccr_model": ccr_model, "admission": admission,
            "verdict": verdict, "stage": stage,
        }
        artifact_id = run_id * 100 + attempt
        self.artifact_rows.setdefault(run_id, []).append({
            "id": artifact_id,
            "name": f"native-proof-validation-{run_id}-{attempt}",
            "expired": False,
        })
        self.artifact_bytes[artifact_id] = self.report_archive(report)

    def json(
        self, endpoint: str, *, method: str = "GET",
        fields: dict[str, str] | None = None, limit: int = 64 << 10,
    ) -> dict[str, object] | list[object]:
        self.calls.append((method, endpoint, fields))
        if endpoint == f"repos/{REPO}":
            return {"id": 41, "full_name": REPO, "private": self.private,
                    "visibility": "private" if self.private else "public"}
        if endpoint == f"repos/{REPO}/branches/main":
            return {"protected": True, "commit": {"sha": HEAD}}
        if endpoint == f"repos/{REPO}/releases?per_page=30&page=1":
            return [{"id": 51, "tag_name": TAG, "prerelease": False}]
        if "/releases/51/assets?" in endpoint:
            return self.assets
        if endpoint == f"repos/{REPO}/collaborators/maintainer/permission":
            return {"permission": self.permission, "user": {"id": SENDER_ID}}
        if "/workflows/release-native-acceptance.yml/runs?" in endpoint:
            page = int(endpoint.rsplit("page=", 1)[1])
            rows = self.run_pages.get(page, self.runs if page == 1 else [])
            return {"total_count": self.total_runs or len(self.runs),
                    "workflow_runs": rows}
        if "/actions/runs/" in endpoint and "/attempts/" in endpoint:
            tail = endpoint.split("/actions/runs/", 1)[1]
            run_id = int(tail.split("/", 1)[0])
            attempt = int(tail.split("/attempts/", 1)[1])
            return self.attempt_facts[(run_id, attempt)]
        if "/actions/runs/" in endpoint and "/artifacts?" in endpoint:
            tail = endpoint.split("/actions/runs/", 1)[1]
            run_id = int(tail.split("/", 1)[0])
            return {"artifacts": self.artifact_rows.get(run_id, [])}
        raise AssertionError(f"unexpected GitHub request: {method} {endpoint}")

    def artifact_archive(self, repo: str, artifact_id: int, *, limit: int) -> bytes:
        if repo != REPO:
            raise AssertionError(f"unexpected GitHub repository: {repo}")
        self.history_reads += 1
        return self.artifact_bytes[artifact_id]

    def bytes(self, endpoint: str, *, limit: int) -> bytes:
        if endpoint.endswith(f"/releases/assets/{PROOF_ASSET_ID}"):
            self.asset_reads += 1
            return self.proof_bytes
        raise AssertionError(f"unexpected GitHub bytes request: {endpoint}")


class NativePromotionTests(ReleaseFixture):
    def setUp(self) -> None:
        super().setUp()
        self.client = PromotionGitHub(self.receipt)
        self.expectation = self.make_expectation(REPORT_MODEL, REPORT_CCR_MODEL)
        self.proof_result = {"status": "PASS", "case_results_verdict": "PASS"}
        self.auth_modes: list[str] = []

    @staticmethod
    def make_expectation(opencode_model: str, ccr_model: str | None) -> ProofExpectation:
        return ProofExpectation(
            repo=REPO, tag=TAG, release_revision=RELEASE, harness_revision=HEAD,
            platform_os="Linux", platform_arch="amd64",
            opencode_model=opencode_model, ccr_model=ccr_model,
            source_harness_sha256=SOURCE, archive_sha256="d" * 64,
            delegate_sha256="e" * 64, runner_sha256="f" * 64,
            pueue_sha256="1" * 64, pueued_sha256="2" * 64,
        )

    def seed_passing_attempt(self) -> None:
        self.client.add_run(82, 1)
        self.client.add_attempt(
            82, 1, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:01Z",
        )

    def seed_admitted_failure_before_selected_pass(self) -> None:
        self.client.add_run(81, 1)
        self.client.add_run(82, 1)
        self.client.add_attempt(
            81, 1, admission="admitted", verdict="fail", stage="validate-native-facts",
            started_at="2026-10-17T12:00:01Z", conclusion="failure",
        )
        self.client.add_attempt(
            82, 1, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:02Z",
        )

    def arguments(self, run_id: int = 82) -> argparse.Namespace:
        # Deliberately omits github_auth: older callers must keep hosted auth.
        return argparse.Namespace(
            repo=REPO, source_revision=HEAD, tag=TAG,
            native_validation_run_id=run_id, work_dir=str(self.base),
        )

    def environment(self, *, auth_mode: str = "environment") -> dict[str, str]:
        self.auth_modes.append(auth_mode)
        return {}

    def invoke(self, run_id: int = 82, *, args: argparse.Namespace | None = None,
               argv: list[str] | None = None) -> int:
        with (
            patch(HOSTED + "GitHubClient", return_value=self.client),
            patch(HOSTED + "gh_environment", self.environment),
            patch(SUBMISSION + "resolve_release_identity",
                  return_value={"commit_sha": RELEASE}),
            patch(HOSTED + "archive_expectation", return_value=self.expectation) as archive,
            patch(HOSTED + "inspect_public_proof",
                  return_value={"platform": {"os": "Linux", "arch": "amd64"}}),
            patch(HOSTED + "validate_public_proof", return_value=self.proof_result) as validator,
        ):
            self.archive_mock = archive
            self.proof_validator_mock = validator
            if argv is not None:
                return native_cli.main(argv)
            return validate_promotion(args or self.arguments(run_id))

    def test_selected_latest_attempt_is_used_even_when_run_list_is_unsorted(self):
        self.client.add_run(83, 1)
        self.client.add_run(82, 2)
        self.client.add_run(81, 1)
        self.client.add_attempt(
            81, 1, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:01Z",
        )
        self.client.add_attempt(
            82, 1, admission="not_admitted", verdict="rejected", stage="authenticate",
            started_at="2026-10-17T12:00:02Z", opencode_model="stale/model",
        )
        self.client.add_attempt(
            82, 2, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:03Z", opencode_model="vendor/exact-selected",
            ccr_model="ccr/exact-selected",
        )
        self.client.add_attempt(
            83, 1, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:04Z", opencode_model="newer/run-model",
        )
        selected = self.make_expectation("vendor/exact-selected", "ccr/exact-selected")
        self.expectation = selected

        self.assertEqual(self.invoke(82), 0)

        self.assertEqual(self.archive_mock.call_count, 1)
        identity = self.archive_mock.call_args.args[1]
        self.assertEqual(identity, SubmissionIdentity(
            repository=REPO, repository_id=41, release_revision=RELEASE,
            harness_revision=HEAD, sender_id=SENDER_ID, asset_id=PROOF_ASSET_ID,
            proof_sha256=PROOF_HASH, asset_name=self.receipt.invocation.proof_name,
        ))
        self.assertEqual(self.archive_mock.call_args.kwargs, {
            "tag": TAG, "platform_os": "Linux", "platform_arch": "amd64",
            "root": self.base, "opencode_model": "vendor/exact-selected",
            "ccr_model": "ccr/exact-selected",
        })
        self.proof_validator_mock.assert_called_once_with(PAYLOAD, selected)

    def test_running_or_failed_workflow_attempts_block_promotion(self):
        self.seed_passing_attempt()
        facts = self.client.attempt_facts[(82, 1)]
        for status, conclusion in (("in_progress", None), ("completed", "failure")):
            with self.subTest(status=status, conclusion=conclusion):
                facts.update(status=status, conclusion=conclusion)
                with self.assertRaisesRegex(
                    VerificationError, "native_validation_run_not_successful",
                ):
                    self.invoke()

    def test_report_stage_must_be_complete(self):
        self.client.add_run(82, 1)
        self.client.add_attempt(
            82, 1, admission="admitted", verdict="pass", stage="validate-history",
            started_at="2026-10-17T12:00:01Z",
        )
        with self.assertRaisesRegex(
            VerificationError, "native_validation_run_not_successful",
        ):
            self.invoke()

    def test_missing_attempt_report_blocks_promotion(self):
        self.client.add_run(82, 1)
        self.client.add_attempt(
            82, 1, admission="unknown", verdict="incomplete", stage="inspect-envelope",
            started_at="2026-10-17T12:00:01Z", include_report=False,
        )
        with self.assertRaisesRegex(VerificationError, "native_history_blocked"):
            self.invoke()
        self.assertEqual(self.client.asset_reads, 0)

    def test_admitted_failure_in_another_run_blocks_selected_pass(self):
        self.seed_admitted_failure_before_selected_pass()
        with self.assertRaisesRegex(VerificationError, "native_history_blocked"):
            self.invoke()
        self.assertEqual(self.client.asset_reads, 0)

    def test_admitted_incomplete_attempt_still_blocks_passing_attempt_two(self):
        self.client.add_run(82, 2)
        self.client.add_attempt(
            82, 1, admission="admitted", verdict="incomplete",
            stage="validate-native-facts", started_at="2026-10-17T12:00:01Z",
            conclusion="failure",
        )
        self.client.add_attempt(
            82, 2, admission="admitted", verdict="pass", stage="complete",
            started_at="2026-10-17T12:00:02Z",
        )
        with self.assertRaisesRegex(VerificationError, "native_history_blocked"):
            self.invoke()
        self.assertEqual(self.client.asset_reads, 0)

    def test_private_repository_fails_before_history_or_proof_side_effects(self):
        self.seed_passing_attempt()
        self.client.private = True
        with self.assertRaisesRegex(
            VerificationError, "anonymous_installation_requires_public_repository",
        ):
            self.invoke()
        self.assertFalse(any("/actions/workflows/" in endpoint
                             for _, endpoint, _ in self.client.calls))
        self.assertEqual(self.client.asset_reads, 0)
        self.archive_mock.assert_not_called()

    def test_revoked_uploader_permission_stops_before_asset_download(self):
        self.seed_passing_attempt()
        self.client.permission = "read"
        with self.assertRaisesRegex(VerificationError, "native_uploader_not_authorized"):
            self.invoke()
        self.assertEqual(self.client.asset_reads, 0)
        self.archive_mock.assert_not_called()

    def test_proof_asset_bytes_must_match_the_reported_digest(self):
        self.seed_passing_attempt()
        self.client.proof_bytes = b"x" * len(PAYLOAD)
        with self.assertRaisesRegex(VerificationError, "native_proof_digest_mismatch"):
            self.invoke()
        self.assertEqual(self.client.asset_reads, 1)
        self.archive_mock.assert_not_called()

    def test_revalidated_proof_verdict_must_pass(self):
        self.seed_passing_attempt()
        self.proof_result = {"status": "FAIL", "case_results_verdict": "PASS"}
        with self.assertRaisesRegex(
            VerificationError, "native_promotion_proof_not_passing",
        ):
            self.invoke()

    def test_promotion_without_auth_field_keeps_hosted_environment_auth(self):
        self.seed_passing_attempt()
        self.assertEqual(self.invoke(), 0)
        self.assertEqual(self.auth_modes, ["environment"])

    def test_promotion_forwards_requested_auth_mode_to_the_environment_owner(self):
        self.seed_passing_attempt()
        args = self.arguments()
        args.github_auth = "local-config"
        self.assertEqual(self.invoke(args=args), 0)
        self.assertEqual(self.auth_modes, ["local-config"])

    def test_unknown_auth_mode_is_rejected_by_the_owner_before_github_reads(self):
        args = self.arguments()
        args.github_auth = "token-extraction"
        with patch(HOSTED + "GitHubClient", return_value=self.client), \
                self.assertRaisesRegex(VerificationError, "github_auth_mode_invalid"):
            validate_promotion(args)
        self.assertEqual(self.client.calls, [])

    def test_local_config_promotion_still_blocks_admitted_failure_before_proof_reads(self):
        self.seed_admitted_failure_before_selected_pass()
        args = self.arguments()
        args.github_auth = "local-config"
        with self.assertRaises(VerificationError) as caught:
            self.invoke(args=args)
        self.assertEqual((caught.exception.code, caught.exception.stage),
                         ("native_history_blocked", "promotion-history"))
        self.assertEqual(self.auth_modes, ["local-config"])
        self.assertEqual(self.client.asset_reads, 0)
        self.archive_mock.assert_not_called()

    def test_local_config_cli_reports_admitted_failure_before_proof_reads(self):
        self.seed_admitted_failure_before_selected_pass()
        argv = ["promotion", "--tag", TAG, "--repo", REPO, "--source-revision", HEAD,
                "--native-validation-run-id", "82", "--work-dir", str(self.base),
                "--github-auth", "local-config"]
        with patch("sys.stderr", new_callable=io.StringIO) as stderr:
            self.assertEqual(self.invoke(argv=argv), 1)
        self.assertEqual(
            stderr.getvalue(),
            "Native proof validation failed: native_history_blocked (promotion-history)\n",
        )
        self.assertEqual(self.auth_modes, ["local-config"])
        self.assertEqual(self.client.asset_reads, 0)
        self.archive_mock.assert_not_called()


class NativeReleaseCliAuthTests(unittest.TestCase):
    PROMOTION = ["promotion", "--tag", TAG, "--repo", REPO, "--source-revision", HEAD,
                 "--native-validation-run-id", "82", "--work-dir", "/work"]
    VALIDATE = ["validate", "--tag", TAG, "--asset-id", str(PROOF_ASSET_ID),
                "--proof-sha256", PROOF_HASH, "--invocation-id", "1" * 32,
                "--opencode-model", REPORT_MODEL, "--report", "report.json"]

    def test_promotion_defaults_to_environment_and_accepts_local_config(self):
        seen: list[str] = []
        with patch.object(native_cli, "validate_promotion",
                          lambda args: seen.append(args.github_auth) or 0):
            self.assertEqual(native_cli.main(self.PROMOTION), 0)
            self.assertEqual(native_cli.main([*self.PROMOTION, "--github-auth", "local-config"]), 0)
        self.assertEqual(seen, ["environment", "local-config"])

    def test_promotion_rejects_unknown_auth_mode_before_validation(self):
        with patch.object(native_cli, "validate_promotion") as promotion, \
                patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                self.assertRaises(SystemExit) as caught:
            native_cli.main([*self.PROMOTION, "--github-auth", "token"])
        self.assertEqual(caught.exception.code, 2)
        self.assertIn("invalid choice: 'token'", stderr.getvalue())
        promotion.assert_not_called()

    def test_hosted_validate_subcommand_rejects_auth_flag(self):
        with patch.object(native_cli, "validate_submission", return_value=0) as submission:
            self.assertEqual(native_cli.main(self.VALIDATE), 0)
            self.assertFalse(hasattr(submission.call_args.args[0], "github_auth"))
            with patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                    self.assertRaises(SystemExit) as caught:
                native_cli.main([*self.VALIDATE, "--github-auth", "local-config"])
        self.assertEqual(caught.exception.code, 2)
        self.assertIn("unrecognized arguments: --github-auth local-config", stderr.getvalue())
        self.assertEqual(submission.call_count, 1)


class PublicPrerequisiteCliTests(unittest.TestCase):
    ARGV = ["public-prerequisite", "--repo", REPO]
    POLICY_CODE = "anonymous_installation_requires_public_repository"
    PREFIX = "Public repository prerequisite failed: "

    def run_cli(self, argv, facts):
        with patch(GITHUB_JSON, **facts) as github, \
                patch(HOSTED + "gh_environment", return_value={}), \
                patch("sys.stderr", new_callable=io.StringIO) as stderr:
            code = native_cli.main(argv)
        return code, stderr.getvalue(), github

    def test_delegates_once_per_run_to_the_sole_policy_with_the_requested_auth_mode(self):
        with patch(HOSTED + "gh_environment", return_value={"marker": "env"}) as auth, \
                patch(HOSTED + "require_public_repository") as policy:
            self.assertEqual(native_cli.main(self.ARGV), 0)
            self.assertEqual(native_cli.main([*self.ARGV, "--github-auth", "local-config"]), 0)
        self.assertEqual([call.kwargs for call in auth.call_args_list],
                         [{"auth_mode": "environment"}, {"auth_mode": "local-config"}])
        self.assertEqual(policy.call_count, 2)
        client, repo = policy.call_args.args
        self.assertEqual((client.receipt, client.environment, repo), ({"commands": []}, {"marker": "env"}, REPO))

    def test_unknown_mode_or_missing_repo_exits_before_any_policy_or_github_read(self):
        for argv in ([*self.ARGV, "--github-auth", "token"], ["public-prerequisite"]):
            with self.subTest(argv=argv), patch(HOSTED + "require_public_repository") as policy, \
                    patch("sys.stderr", new_callable=io.StringIO), \
                    self.assertRaises(SystemExit) as caught:
                native_cli.main(argv)
            self.assertEqual(caught.exception.code, 2)
            policy.assert_not_called()

    def test_public_repository_passes_and_every_other_fact_is_refused_by_the_real_policy(self):
        code, stderr, github = self.run_cli(self.ARGV, {"return_value": {"private": False, "visibility": "public"}})
        self.assertEqual((code, stderr), (0, ""))
        github.assert_called_once_with(f"repos/{REPO}")
        guidance = "Make the repository public, then rerun this release workflow.\n"
        for facts in ({"private": True, "visibility": "private"}, {"private": False, "visibility": "internal"},
                      {"private": True, "visibility": "public"}, {"private": False}, {"visibility": "public"},
                      {}, {"private": "false", "visibility": "public"}):
            with self.subTest(facts=facts):
                code, stderr, _ = self.run_cli(self.ARGV, {"return_value": facts})
                self.assertEqual(code, 1)
                self.assertEqual(stderr, f"{self.PREFIX}{self.POLICY_CODE} (public-release-precheck)\n{guidance}")

    def test_api_and_malformed_failures_render_safe_codes_without_visibility_guidance(self):
        for facts, expected in (
                ({"return_value": []}, "github_object_invalid (public-release-precheck)"),
                ({"side_effect": VerificationError("github_request_failed", "github-api")},
                 "github_request_failed (github-api)")):
            with self.subTest(expected=expected):
                code, stderr, _ = self.run_cli(self.ARGV, facts)
                self.assertEqual((code, stderr), (1, f"{self.PREFIX}{expected}\n"))

    def test_environment_mode_needs_a_token_and_local_config_never_forwards_one(self):
        secret = {"GH_TOKEN": "secret-one", "GITHUB_TOKEN": "secret-two"}
        with patch.dict("os.environ", {}, clear=True), patch(HOSTED + "GitHubClient") as client, \
                patch("sys.stderr", new_callable=io.StringIO) as stderr:
            self.assertEqual(native_cli.main(self.ARGV), 1)
        self.assertEqual(stderr.getvalue(), f"{self.PREFIX}github_auth_unavailable (github-auth)\n")
        client.assert_not_called()
        for mode, forwarded in (("environment", True), ("local-config", False)):
            with self.subTest(mode=mode), patch.dict("os.environ", secret, clear=True), \
                    patch(HOSTED + "GitHubClient") as client, \
                    patch(HOSTED + "require_public_repository"), \
                    patch("sys.stderr", new_callable=io.StringIO) as stderr:
                self.assertEqual(native_cli.main([*self.ARGV, "--github-auth", mode]), 0)
            environment = client.call_args.args[1]
            self.assertEqual("GH_TOKEN" in environment, forwarded)
            self.assertEqual(environment.get("GITHUB_TOKEN") == "secret-two", forwarded)
            self.assertNotIn("secret", stderr.getvalue())

    def test_existing_commands_keep_their_error_format_and_never_reach_the_new_policy(self):
        error = VerificationError("native_example_failure", "example-stage")
        for command, handler, argv in (("validate", "validate_submission", NativeReleaseCliAuthTests.VALIDATE),
                                       ("promotion", "validate_promotion", NativeReleaseCliAuthTests.PROMOTION)):
            with self.subTest(command=command), patch.object(native_cli, handler, side_effect=error), \
                    patch(HOSTED + "require_public_repository") as policy, \
                    patch("sys.stderr", new_callable=io.StringIO) as stderr:
                self.assertEqual(native_cli.main(argv), 1)
            self.assertEqual(stderr.getvalue(), "Native proof validation failed: native_example_failure (example-stage)\n")
            policy.assert_not_called()
        with patch.object(native_cli, "validate_submission") as submission, \
                patch.object(native_cli, "validate_promotion") as promotion, \
                patch(HOSTED + "require_public_repository"), patch(HOSTED + "gh_environment", return_value={}):
            self.assertEqual(native_cli.main(self.ARGV), 0)
        submission.assert_not_called()
        promotion.assert_not_called()


if __name__ == "__main__":
    unittest.main()
