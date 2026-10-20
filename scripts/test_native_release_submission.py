"""Lost upload/dispatch responses cannot create duplicate remote mutations."""

from __future__ import annotations

from unittest.mock import patch
import unittest

from scripts.delegate_acceptance.native_release import watching
from scripts.delegate_acceptance.native_release.contract import ReleaseError, WORKFLOW
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.native_release.submission import (
    dispatch, reconcile_asset, upload, validate_run,
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
            ("path", {"path": f"{bare}@secret-ref"}, 81),
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


if __name__ == "__main__":
    unittest.main()
