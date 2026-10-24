"""Lost upload/dispatch responses cannot create duplicate remote mutations."""

from __future__ import annotations

from unittest.mock import patch
import unittest

from scripts.delegate_acceptance.native_release import watching
from scripts.delegate_acceptance.native_release.contract import (
    DispatchAck, ReleaseError, WORKFLOW, WORKFLOW_RUN_PATHS, parse_workflow_dispatch_ack,
    trusted_main_workflow_paths,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.native_release.submission import (
    bind_dispatched_run, dispatch, reconcile_asset, upload, validate_run,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_native_release_support import (
    HEAD, PAYLOAD, PROOF_HASH, RELEASE, REPO, TAG, ReleaseFixture,
)


class SubmissionTests(ReleaseFixture):
    def upload_dependencies(self):
        return (patch("scripts.delegate_acceptance.native_release.submission.read_public",
                      return_value=(PAYLOAD, {})),
                patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                      return_value={"commit_sha": RELEASE}))

    def test_lost_upload_response_reconciles_exact_asset_once(self) -> None:
        self.seed_project()
        def upload_effect(*args, **kwargs):
            durable = Receipt.load(self.receipt.path)
            self.assertEqual(durable.stage("upload").state, "intended")
            self.client.assets = [self.client.asset()]
            raise VerificationError("command_timeout", "native-proof-upload")
        public, identity = self.upload_dependencies()
        with public, identity, patch("scripts.delegate_acceptance.native_release.submission.command",
                                     side_effect=upload_effect) as command:
            upload(self.receipt, self.client)
            upload(Receipt.load(self.receipt.path), self.client)
        self.assertEqual(command.call_count, 1)
        self.assertEqual(self.receipt.stage("upload").result["asset_id"], 71)
        self.assertEqual(command.call_args.args[2], [
            "gh", "release", "upload", TAG, str(self.receipt.invocation.proof), "--repo", REPO,
        ])
        self.assertEqual(command.call_args.kwargs["cwd"], self.checkout)

    def test_crash_after_upload_intent_never_retries_missing_upload(self) -> None:
        self.seed_project()
        intent = {"name": self.receipt.invocation.proof_name, **self.receipt.stage("project").result}
        self.receipt.intend("upload", intent)
        recovered = Receipt.load(self.receipt.path)
        public, identity = self.upload_dependencies()
        with public, identity, patch("scripts.delegate_acceptance.native_release.submission.command") as command:
            with self.assertRaisesRegex(ReleaseError, "proof_upload_response_unresolved"):
                upload(recovered, self.client)
        command.assert_not_called()
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "intended")

    def test_asset_full_pagination_rejects_late_duplicate_and_digest_conflict(self) -> None:
        self.seed_project()
        self.client.asset_pages[1] = [self.client.asset(), *({"name": f"other-{n}"} for n in range(99))]
        self.client.asset_pages[2] = [self.client.asset(asset_id=72)]
        with self.assertRaisesRegex(ReleaseError, "proof_asset_name_ambiguous"):
            reconcile_asset(self.receipt, self.client)
        self.client.asset_pages = {}
        self.client.assets = [{**self.client.asset(), "digest": f"sha256:{'0' * 64}"}]
        with self.assertRaisesRegex(ReleaseError, "proof_asset_digest_mismatch"):
            reconcile_asset(self.receipt, self.client)

    def test_shared_asset_listing_keeps_legacy_resumable_identities(self) -> None:
        self.seed_project()
        full = {page: [{"name": f"other-{page}-{n}"} for n in range(100)] for page in range(1, 101)}
        cases = (
            ({1: {"rows": []}}, "release_assets_invalid", "github_release_assets_invalid"),
            ({1: [{"name": "other"}] * 101}, "release_assets_invalid", "github_release_assets_invalid"),
            (full, "release_assets_pagination_incomplete", "github_release_assets_pagination_incomplete"),
        )
        for pages, code, cause in cases:
            with self.subTest(code=code):
                self.client.asset_pages = pages
                with self.assertRaises(ReleaseError) as raised:
                    reconcile_asset(self.receipt, self.client)
                self.assertEqual((raised.exception.code, raised.exception.incomplete), (code, True))
                self.assertIsInstance(raised.exception.__cause__, VerificationError)
                self.assertEqual((raised.exception.__cause__.code, raised.exception.__cause__.stage),
                                 (cause, "github-release-assets"))
        full[100] = [*full[100][:99], self.client.asset()]
        full[100].pop(0)
        self.client.asset_pages = full
        self.assertEqual(reconcile_asset(self.receipt, self.client)["asset_id"], 71)
        self.client.asset_pages = {1: ["row"]}
        with self.assertRaisesRegex(ReleaseError, "release_record_object_invalid"):
            reconcile_asset(self.receipt, self.client)

    def test_unrelated_asset_listing_errors_keep_their_identity(self) -> None:
        self.seed_project()
        for error in (VerificationError("github_request_failed", "github-api"),
                      VerificationError("github_release_assets_invalid", "github-api")):
            with self.subTest(code=error.code, stage=error.stage), \
                    patch.object(self.client, "json", side_effect=error):
                with self.assertRaises(VerificationError) as raised:
                    reconcile_asset(self.receipt, self.client)
                self.assertIs(raised.exception, error)

    def test_asset_uploader_must_match_original_local_actor(self) -> None:
        self.seed_project()
        self.client.assets = [{**self.client.asset(), "uploader": {"id": 999}}]
        with self.assertRaisesRegex(ReleaseError, "proof_uploader_actor_mismatch"):
            reconcile_asset(self.receipt, self.client)

    def test_lost_dispatch_response_uses_exact_run_and_original_models(self) -> None:
        self.seed_upload()
        self.client.lose_dispatch_response = True
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            dispatch(self.receipt, self.client)
            dispatch(Receipt.load(self.receipt.path), self.client)
        self.assertEqual(self.client.posts, 1)
        self.assertEqual(self.receipt.stage("dispatch").result["run_id"], 81)
        post = next(call for call in self.client.calls if call[0] == "POST")
        self.assertEqual(post[2], {
            "ref": "main", "inputs[tag]": TAG, "inputs[proof_asset_id]": "71",
            "inputs[proof_sha256]": PROOF_HASH, "inputs[invocation_id]": "1" * 32,
            "inputs[opencode_model]": "vendor/exact-model", "inputs[ccr_model]": "exact-alias",
        })

    def test_exact_dispatch_ack_completes_without_reconciliation(self) -> None:
        self.seed_upload()
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            dispatch(self.receipt, self.client)
        title = self.client.run()["display_title"]
        self.assertEqual(self.receipt.stage("dispatch").result, {
            "run_id": 81, "html_url": f"https://github.com/{REPO}/actions/runs/81", "title": title})
        endpoints = [(method, endpoint) for method, endpoint, _ in self.client.calls]
        post = endpoints.index(("POST", f"repos/{REPO}/actions/workflows/{WORKFLOW}/dispatches"))
        self.assertEqual([call for call in endpoints[post + 1:] if "/runs?" in call[1]], [],
                         "an exact ACK settles the dispatch without a run listing")

    def test_malformed_dispatch_ack_still_reconciles_the_one_created_run(self) -> None:
        url = f"https://github.com/{REPO}/actions/runs/81"
        api = f"https://api.github.com/repos/{REPO}/actions/runs/81"
        replies = ([], {"workflow_run_id": True, "html_url": url, "run_url": api},
                   {"workflow_run_id": 0, "html_url": url, "run_url": api},
                   {"workflow_run_id": "81", "html_url": url, "run_url": api},
                   {"workflow_run_id": 81, "html_url": "https://github.com/other/repo/actions/runs/81",
                    "run_url": api},
                   {"workflow_run_id": 81, "html_url": url,
                    "run_url": "https://api.github.com/repos/other/repo/actions/runs/81"},
                   {"workflow_run_id": 82, "html_url": url, "run_url": api})
        for reply in replies:
            with self.subTest(reply=reply):
                self.setUp()
                self.seed_upload()
                original = self.client.json
                def malformed(*args, **kwargs):
                    value = original(*args, **kwargs)
                    return reply if kwargs.get("method") == "POST" else value
                with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                           return_value={"commit_sha": RELEASE}), \
                        patch.object(self.client, "json", side_effect=malformed):
                    dispatch(self.receipt, self.client)
                self.assertEqual(self.client.posts, 1)
                self.assertEqual((self.receipt.stage("dispatch").result["run_id"],
                                  self.receipt.stage("dispatch").result["html_url"]), (81, url))
                endpoints = [endpoint for method, endpoint, _ in self.client.calls]
                post = endpoints.index(f"repos/{REPO}/actions/workflows/{WORKFLOW}/dispatches")
                self.assertTrue(any("/runs?" in endpoint for endpoint in endpoints[post + 1:]))

    def test_crash_after_dispatch_intent_only_observes(self) -> None:
        self.seed_upload()
        self.client.lose_dispatch_response = True
        def lose_response(*args, **kwargs):
            if kwargs.get("method") == "POST":
                raise KeyboardInterrupt
            return original(*args, **kwargs)
        original = self.client.json
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}), patch.object(self.client, "json", side_effect=lose_response):
            with self.assertRaises(KeyboardInterrupt):
                dispatch(self.receipt, self.client)
        recovered = Receipt.load(self.receipt.path)
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            with self.assertRaisesRegex(ReleaseError, "workflow_dispatch_response_unresolved"):
                dispatch(recovered, self.client)
        self.assertEqual(self.client.posts, 0)

    def test_reconciliation_pages_all_runs_and_refuses_wrong_head(self) -> None:
        self.seed_upload()
        self.client.total_runs = 101
        self.client.run_pages[1] = [{**self.client.run(run_id=n + 100), "display_title": "other"} for n in range(100)]
        self.client.run_pages[2] = [self.client.run()]
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            dispatch(self.receipt, self.client)
        self.assertEqual(self.client.posts, 0)
        self.assertEqual(self.receipt.stage("dispatch").result["run_id"], 81)
        bad = self.client.run()
        bad["head_sha"] = "f" * len(HEAD)
        self.client.run_pages[2] = [bad]
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            with self.assertRaisesRegex(ReleaseError, "native_validation_run_unbound"):
                dispatch(Receipt.load(self.receipt.path), self.client)
        self.assertEqual(self.client.posts, 0)


    def test_main_qualified_run_path_reconciles_and_watches_one_dispatch(self) -> None:
        self.seed_upload()
        self.client.run_path = f".github/workflows/{WORKFLOW}@main"
        self.client.lose_dispatch_response = True
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            dispatch(self.receipt, self.client)
            dispatch(Receipt.load(self.receipt.path), self.client)
        self.assertEqual((self.client.posts, self.receipt.stage("dispatch").result["run_id"]), (1, 81))
        with self.assertRaisesRegex(ReleaseError, "native_validation_watch_expired"):
            watching.watch(self.receipt, self.client, timeout=0)
        self.assertEqual(self.client.posts, 1)

    def test_run_binding_mismatch_reports_only_fixed_field_names(self) -> None:
        self.seed_upload()
        bare = f".github/workflows/{WORKFLOW}"
        title = self.client.run()["display_title"]
        self.assertEqual(validate_run(self.receipt, self.client.run(), expected_id=81,
                                      expected_title=title)["path"], bare)
        cases = (
            ("id", {}, 82), ("display_title", {"display_title": "secret-title"}, 81),
            ("head_branch", {"head_branch": "secret-branch"}, 81),
            ("head_sha", {"head_sha": "f" * len(HEAD)}, 81), ("event", {"event": "push"}, 81),
            ("path", {"path": ".github/workflows/release.yml@main"}, 81),
            ("path", {"path": f"{bare}@refs/heads/main"}, 81),
            ("path", {"path": f"{bare}@secret-ref"}, 81), ("path", {"path": f"{bare}@main/x"}, 81),
            ("path", {"path": f"{bare}@main "}, 81), ("path", {"path": f"{bare}@main@main"}, 81),
            ("path", {"path": f"secret/{bare}"}, 81), ("path", {"path": [bare]}, 81),
            ("html_url", {"html_url": "https://github.com/secret/fork/actions/runs/81"}, 81),
        )
        for field, change, expected_id in cases:
            with self.subTest(field=field, change=change):
                with self.assertRaises(ReleaseError) as raised:
                    validate_run(self.receipt, {**self.client.run(), **change},
                                 expected_id=expected_id, expected_title=title)
                self.assertEqual((raised.exception.code, raised.exception.incomplete,
                                  raised.exception.mismatched), ("native_validation_run_unbound", True, (field,)))
                self.assertEqual(raised.exception.diagnostic,
                                 f"native_validation_run_unbound:mismatch={field}")
        with self.assertRaises(ReleaseError) as raised:
            validate_run(self.receipt, {**self.client.run(), "event": "push", "path": "secret"},
                         expected_id=81, expected_title=title)
        self.assertEqual(raised.exception.mismatched, ("event", "path"))


class BoundDispatchedRunTests(unittest.TestCase):
    """The canonical run binding with another workflow's trusted paths, title and refusal code."""

    PATHS = trusted_main_workflow_paths("release.yml")
    TITLE = f"Candidate publication {'5' * 32} {'a' * 64}"
    URL = f"https://github.com/{REPO}/actions/runs/81"

    def row(self, **changes: object) -> dict[str, object]:
        return {"id": 81, "display_title": self.TITLE, "head_sha": HEAD, "head_branch": "main",
                "event": "workflow_dispatch", "path": self.PATHS[0], "html_url": self.URL, **changes}

    def bind(self, row: object, **changes: object) -> dict[str, object]:
        expectations: dict[str, object] = {"repository": REPO, "title": self.TITLE, "revision": HEAD,
                                           "paths": self.PATHS, "expected_id": 81, "code": "publication_run_unbound"}
        return bind_dispatched_run(row, **{**expectations, **changes})

    def test_an_exact_run_on_either_trusted_path_is_returned_unchanged(self) -> None:
        for path in self.PATHS:
            with self.subTest(path=path):
                row = self.row(path=path, status="queued")
                self.assertEqual(self.bind(row), row)
        self.assertEqual(self.bind(self.row(id=82, html_url=f"https://github.com/{REPO}/actions/runs/82"),
                                   expected_id=None)["id"], 82)

    def test_each_wrong_binding_refuses_with_the_callers_code_and_fixed_field_names(self) -> None:
        cases = (
            ("id", {"id": 82, "html_url": f"https://github.com/{REPO}/actions/runs/82"}),
            ("display_title", {"display_title": "Native proof v1.2.3"}),
            ("head_sha", {"head_sha": "f" * len(HEAD)}), ("head_branch", {"head_branch": "release"}),
            ("event", {"event": "push"}),
            ("path", {"path": f".github/workflows/{WORKFLOW}"}), ("path", {"path": f"{self.PATHS[0]}@other"}),
            ("html_url", {"html_url": "https://github.com/other/repo/actions/runs/81"}),
        )
        for field, change in cases:
            with self.subTest(field=field, change=change), self.assertRaises(ReleaseError) as raised:
                self.bind(self.row(**change))
            self.assertEqual((raised.exception.code, raised.exception.incomplete, raised.exception.mismatched),
                             ("publication_run_unbound", True, (field,)))

    def test_a_missing_expectation_binds_nothing(self) -> None:
        for field, expectation in (("display_title", "title"), ("head_sha", "revision")):
            with self.subTest(field=field), self.assertRaises(ReleaseError) as raised:
                self.bind(self.row(**{field: None}), **{expectation: None})
            self.assertEqual(raised.exception.mismatched, (field,))

    def test_a_malformed_row_refuses_before_any_binding(self) -> None:
        for row, code in (([], "release_record_object_invalid"), (self.row(id="81"), "release_record_integer_invalid"),
                          (self.row(id=True), "release_record_integer_invalid")):
            with self.subTest(row=row), self.assertRaises(ReleaseError) as raised:
                self.bind(row)
            self.assertEqual(raised.exception.code, code)


class DispatchAckContractTests(unittest.TestCase):
    URL = f"https://github.com/{REPO}/actions/runs/81"
    API = f"https://api.github.com/repos/{REPO}/actions/runs/81"

    def ack(self, **changes: object) -> dict[str, object]:
        return {"workflow_run_id": 81, "html_url": self.URL, "run_url": self.API, **changes}

    def test_an_exact_ack_is_one_frozen_run_identity(self) -> None:
        ack = parse_workflow_dispatch_ack(self.ack(extra="ignored"), REPO)
        self.assertEqual(ack, DispatchAck(run_id=81, html_url=self.URL, run_url=self.API))
        with self.assertRaises(AttributeError):  # dataclasses.FrozenInstanceError
            setattr(ack, "run_id", 82)

    def test_refusals_keep_their_known_codes_and_completeness_in_validation_order(self) -> None:
        other = "other/repo"
        cases = (
            ([], "release_record_object_invalid", False),
            ({1: 81}, "release_record_object_invalid", False),
            (self.ack(workflow_run_id=None), "release_record_integer_invalid", False),
            ({"html_url": self.URL, "run_url": self.API}, "release_record_integer_invalid", False),
            (self.ack(workflow_run_id=True), "release_record_integer_invalid", False),
            (self.ack(workflow_run_id=0), "release_record_integer_invalid", False),
            (self.ack(workflow_run_id="81"), "release_record_integer_invalid", False),
            (self.ack(workflow_run_id=81.0), "release_record_integer_invalid", False),
            (self.ack(workflow_run_id=0, html_url=None), "release_record_integer_invalid", False),
            (self.ack(workflow_run_id=82), "dispatch_response_identity_invalid", True),
            (self.ack(html_url=f"https://github.com/{other}/actions/runs/81"),
             "dispatch_response_identity_invalid", True),
            (self.ack(run_url=f"https://api.github.com/repos/{other}/actions/runs/81"),
             "dispatch_response_identity_invalid", True),
            (self.ack(html_url=f"{self.URL}/"), "dispatch_response_identity_invalid", True),
            (self.ack(run_url=None), "dispatch_response_identity_invalid", True),
            ({"workflow_run_id": 81, "html_url": self.URL}, "dispatch_response_identity_invalid", True),
        )
        for response, code, incomplete in cases:
            with self.subTest(response=response), self.assertRaises(ReleaseError) as raised:
                parse_workflow_dispatch_ack(response, REPO)
            self.assertEqual((raised.exception.code, raised.exception.incomplete), (code, incomplete))
        with self.assertRaisesRegex(ReleaseError, "dispatch_response_identity_invalid"):
            parse_workflow_dispatch_ack(self.ack(), other)


class TrustedMainWorkflowPathTests(unittest.TestCase):
    def test_a_maintained_file_has_only_its_bare_and_main_qualified_paths(self) -> None:
        self.assertEqual(trusted_main_workflow_paths("development-readiness.yml"), (
            ".github/workflows/development-readiness.yml", ".github/workflows/development-readiness.yml@main"))
        self.assertEqual(WORKFLOW_RUN_PATHS, (f".github/workflows/{WORKFLOW}", f".github/workflows/{WORKFLOW}@main"))


if __name__ == "__main__":
    unittest.main()
