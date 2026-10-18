"""GitHub transport uses the canonical bounded owner without logging secrets."""

import json
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient, positive_id


class GitHubTransportTests(unittest.TestCase):
    def test_binary_downloads_use_the_endpoint_media_type_and_preserve_bytes(self):
        client = GitHubClient({"commands": []}, {})
        payload = b"PK\x03\x04\xff\x00\n"
        for artifact in (False, True):
            with self.subTest(artifact=artifact):
                with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                           return_value=BoundedCommandResult(0, payload)) as run:
                    if artifact:
                        actual = client.artifact_archive("owner/repo", 17, limit=1024)
                    else:
                        actual = client.bytes("repos/owner/repo/releases/assets/17", limit=1024)
                self.assertEqual(actual, payload)
                argv = run.call_args.args[0]
                accept = ("application/vnd.github+json" if artifact else "application/octet-stream")
                self.assertIn(f"Accept: {accept}", argv)
                endpoint = ("repos/owner/repo/actions/artifacts/17/zip" if artifact
                            else "repos/owner/repo/releases/assets/17")
                self.assertEqual(argv[-1], endpoint)
                self.assertEqual(run.call_args.kwargs["max_output"], 1024)

    def test_json_request_is_bounded_and_records_only_safe_transport_facts(self):
        receipt = {"commands": []}
        env = {"GH_TOKEN": "private-canary"}
        client = GitHubClient(receipt, env)
        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   return_value=BoundedCommandResult(0, b'{"workflow_run_id":17}')) as run:
            value = client.json("repos/owner/repo/actions/workflows/proof.yml/dispatches",
                                method="POST", fields={"ref": "main", "inputs[tag]": "v1.2.3"})
        self.assertEqual(value, {"workflow_run_id": 17})
        self.assertEqual(run.call_args.args[1], env)
        self.assertEqual(run.call_args.kwargs["timeout"], 120)
        self.assertNotIn("private-canary", repr(run.call_args.args[0]))
        self.assertNotIn("private-canary", repr(receipt))
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)

    def test_scalar_or_malformed_response_is_rejected(self):
        for data in (b'null', b'42', b'{bad'):
            with self.subTest(data=data):
                client = GitHubClient({"commands": []}, {})
                with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                           return_value=BoundedCommandResult(0, data)):
                    with self.assertRaisesRegex(VerificationError, "github_response_invalid"):
                        client.json("repos/owner/repo")

    def test_id_does_not_accept_boolean_or_string(self):
        for value in (True, "17", 0, -1):
            with self.subTest(value=value):
                with self.assertRaisesRegex(VerificationError, "github_id_invalid"):
                    positive_id(value, "test")


class GitHubReleaseLookupTests(unittest.TestCase):
    def setUp(self):
        self.client = GitHubClient({"commands": []}, {})
        self.release = {"id": 17, "tag_name": "v1.2.3", "draft": True,
                        "prerelease": False}
        self.endpoint = "repos/owner/repo/releases?per_page=30&page=1"

    def test_draft_and_published_releases_use_listing_not_published_only_lookup(self):
        for draft in (True, False):
            with self.subTest(draft=draft):
                self.release["draft"] = draft
                with patch.object(self.client, "json", return_value=[self.release]) as request:
                    self.assertEqual(self.client.release("owner/repo", "v1.2.3"), self.release)
                request.assert_called_once_with(self.endpoint, limit=1 << 20)

    def test_release_on_later_page_is_found(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        with patch.object(self.client, "json",
                          side_effect=[[unrelated] * 30, [self.release]]) as request:
            self.assertEqual(self.client.release("owner/repo", "v1.2.3"), self.release)
        self.assertEqual(request.call_count, 2)
        self.assertEqual(request.call_args.args[0],
                         "repos/owner/repo/releases?per_page=30&page=2")

    def test_exact_tag_is_required(self):
        rows = [{"id": 18, "tag_name": "v1.2.30"}]
        with patch.object(self.client, "json", return_value=rows):
            with self.assertRaisesRegex(VerificationError, "github_release_not_found"):
                self.client.release("owner/repo", "v1.2.3")

    def test_duplicate_match_across_pages_is_ambiguous(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        first = [self.release] + [unrelated] * 29
        with patch.object(self.client, "json", side_effect=[first, [self.release]]):
            with self.assertRaisesRegex(VerificationError, "github_release_ambiguous"):
                self.client.release("owner/repo", "v1.2.3")

    def test_malformed_page_is_rejected(self):
        for rows in ({"releases": []}, [self.release] * 31, [None]):
            with self.subTest(rows=rows):
                with patch.object(self.client, "json", return_value=rows):
                    with self.assertRaises(VerificationError):
                        self.client.release("owner/repo", "v1.2.3")

    def test_match_requires_numeric_release_identity(self):
        for release_id in (True, "17", 0, -1, None):
            with self.subTest(release_id=release_id):
                row = {**self.release, "id": release_id}
                with patch.object(self.client, "json", return_value=[row]):
                    with self.assertRaisesRegex(VerificationError, "github_id_invalid"):
                        self.client.release("owner/repo", "v1.2.3")

    def test_pagination_exhaustion_cannot_accept_an_early_match(self):
        unrelated = {"id": 18, "tag_name": "v9.9.9"}
        first = [self.release] + [unrelated] * 29
        pages = [first] + [[unrelated] * 30] * 99
        with patch.object(self.client, "json", side_effect=pages) as request:
            with self.assertRaisesRegex(VerificationError, "github_releases_pagination_incomplete"):
                self.client.release("owner/repo", "v1.2.3")
        self.assertEqual(request.call_count, 100)

    def test_transport_error_is_reported_without_fallback(self):
        error = VerificationError("github_request_failed", "github-api")
        with patch.object(self.client, "json", side_effect=error) as request:
            with self.assertRaisesRegex(VerificationError, "github_request_failed"):
                self.client.release("owner/repo", "v1.2.3")
        request.assert_called_once_with(self.endpoint, limit=1 << 20)

    def test_realistic_metadata_history_fits_the_transport_page_budget(self):
        releases = [{"id": index + 1, "tag_name": f"v0.0.{index}",
                     "body": "x" * 11292} for index in range(95)]
        releases[0].update(self.release)
        observed = []

        def bounded(argv, _env, *, max_output, **_options):
            endpoint = next(value for value in argv if value.startswith("repos/"))
            fields = dict(value.split("=") for value in endpoint.split("?", 1)[1].split("&"))
            count, page = int(fields["per_page"]), int(fields["page"])
            rows = releases[(page - 1) * count:page * count]
            data = json.dumps(rows).encode()
            if len(data) > max_output:
                raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
            observed.append((len(rows), len(data)))
            return BoundedCommandResult(0, data)

        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   side_effect=bounded):
            self.assertEqual(self.client.release("owner/repo", "v1.2.3"), releases[0])
        self.assertEqual([count for count, _ in observed], [30, 30, 30, 5])
        self.assertTrue(all(size <= 1 << 20 for _, size in observed))

    def test_oversized_page_remains_an_explicit_transport_failure(self):
        error = BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   side_effect=error) as request:
            with self.assertRaisesRegex(VerificationError, "command_output_exceeded"):
                self.client.release("owner/repo", "v1.2.3")
        request.assert_called_once()
        self.assertEqual(request.call_args.kwargs["max_output"], 1 << 20)


if __name__ == "__main__":
    unittest.main()
