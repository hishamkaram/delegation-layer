"""A proof asset is admitted only after matching trusted GitHub identities."""

import hashlib
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.native_submission import (
    authenticate_submission, require_public_repository,
)


class NativeSubmissionTests(unittest.TestCase):
    def setUp(self):
        self.client = GitHubClient({"commands": []}, {})
        self.data = b"proof fixture"
        self.repository = {"full_name": "owner/repo", "id": 7,
                           "private": False, "visibility": "public"}
        self.branch = {"protected": True, "commit": {"sha": "c" * 40}}
        self.asset = {"id": 42, "name": "native-proof-Linux-amd64-" + "b" * 32 + ".zip",
                      "size": len(self.data), "uploader": {"id": 9, "login": "maintainer"}}
        self.permission = {"permission": "write", "user": {"id": 9}}

    def response(self, endpoint, **_options):
        if endpoint == "repos/owner/repo":
            return self.repository
        if endpoint.endswith("branches/main"):
            return self.branch
        if "/releases/tags/" in endpoint:
            return {"id": 8, "tag_name": "v1.2.3", "prerelease": False}
        if "/assets?" in endpoint:
            return [self.asset]
        return self.permission

    def invoke(self, *, digest=None, sender_id=9):
        with (patch.object(self.client, "json", side_effect=self.response),
              patch.object(self.client, "bytes", return_value=self.data),
              patch("scripts.delegate_acceptance.release_verification.native_submission."
                    "resolve_release_identity", return_value={"commit_sha": "d" * 40})):
            return authenticate_submission(
                self.client, "owner/repo", "v1.2.3", asset_id=42,
                proof_sha256=digest or hashlib.sha256(self.data).hexdigest(),
                harness_revision="c" * 40, sender_id=sender_id, max_proof_bytes=1024,
            )

    def test_matching_asset_is_bound_to_tag_current_main_and_sender(self):
        identity, data = self.invoke()
        self.assertEqual(identity.repository_id, 7)
        self.assertEqual(identity.release_revision, "d" * 40)
        self.assertEqual(identity.sender_id, 9)
        self.assertEqual(data, self.data)

    def test_wrong_uploader_is_not_authorized(self):
        with self.assertRaisesRegex(VerificationError, "native_uploader_sender_mismatch"):
            self.invoke(sender_id=10)

    def test_login_cannot_substitute_another_numeric_identity(self):
        self.permission["user"] = {"id": 10}
        with self.assertRaisesRegex(VerificationError, "native_uploader_not_authorized"):
            self.invoke()

    def test_read_only_uploader_cannot_attest(self):
        self.permission["permission"] = "read"
        with self.assertRaisesRegex(VerificationError, "native_uploader_not_authorized"):
            self.invoke()

    def test_wrong_digest_is_not_admitted(self):
        with self.assertRaisesRegex(VerificationError, "native_proof_digest_mismatch"):
            self.invoke(digest="a" * 64)

    def test_unprotected_or_moved_main_is_not_admitted(self):
        self.branch["protected"] = False
        with self.assertRaisesRegex(VerificationError, "native_harness_not_current_protected_main"):
            self.invoke()
        self.branch.update(protected=True, commit={"sha": "e" * 40})
        with self.assertRaisesRegex(VerificationError, "native_harness_not_current_protected_main"):
            self.invoke()

    def test_asset_from_another_release_is_not_admitted(self):
        self.asset["id"] = 43
        with self.assertRaisesRegex(VerificationError, "native_proof_asset_not_in_release"):
            self.invoke()

    def test_public_precheck_cannot_mask_private_installation_with_token(self):
        with patch.object(self.client, "json", return_value=self.repository):
            require_public_repository(self.client, "owner/repo")
            self.repository.update(private=True, visibility="private")
            with self.assertRaisesRegex(VerificationError, "anonymous_installation_requires_public"):
                require_public_repository(self.client, "owner/repo")


if __name__ == "__main__":
    unittest.main()
