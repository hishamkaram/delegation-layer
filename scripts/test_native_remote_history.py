"""Composed promotion-history fixtures include prior attempts and pagination."""

from __future__ import annotations

import io
import json
import unittest
from unittest.mock import call, create_autospec
import zipfile

from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.native_history import (
    classify_native_history, proof_title,
)
from scripts.delegate_acceptance.release_verification.native_remote_history import (
    history_reports, matching_runs,
)
from scripts.delegate_acceptance.release_verification.native_reports import ReportScope


SCOPE = ReportScope(7, "v1.2.3", "c" * 40)


def report_archive(run: int, attempt: int, verdict: str) -> bytes:
    data = {"schema_version": 1, "kind": "native-proof-validation", "repository_id": 7,
            "tag": SCOPE.tag, "harness_revision": SCOPE.harness_revision, "run_id": run,
            "run_attempt": attempt, "proof_sha256": "a" * 64,
            "admission": "admitted", "verdict": verdict}
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w") as bundle:
        bundle.writestr("report.json", json.dumps(data))
    return buffer.getvalue()


class NativeRemoteHistoryTests(unittest.TestCase):
    def test_prior_failed_attempt_is_loaded_and_blocks_later_success(self):
        client = create_autospec(GitHubClient, instance=True)
        title = proof_title(SCOPE.tag, "b" * 32, "a" * 64)

        def response(endpoint: str, **_options):
            if "/workflows/" in endpoint:
                return {"total_count": 1, "workflow_runs": [
                    {"id": 2, "run_attempt": 2, "display_title": title},
                ]}
            if "/artifacts?" in endpoint:
                return {"artifacts": [
                    {"id": 11, "name": "native-proof-validation-2-1", "expired": False},
                    {"id": 12, "name": "native-proof-validation-2-2", "expired": False},
                ]}
            attempt = int(endpoint.rsplit("/", 1)[1])
            return {"id": 2, "run_attempt": attempt, "head_sha": SCOPE.harness_revision,
                    "head_branch": "main", "event": "workflow_dispatch",
                    "status": "completed", "conclusion": "success",
                    "run_started_at": f"2026-10-17T12:00:0{attempt}Z"}

        client.json.side_effect = response
        client.artifact_archive.side_effect = [report_archive(2, 1, "fail"), report_archive(2, 2, "pass")]
        reports = history_reports(client, "owner/repo", SCOPE)
        self.assertEqual(client.artifact_archive.call_args_list, [
            call("owner/repo", 11, limit=64 << 10),
            call("owner/repo", 12, limit=64 << 10),
        ])
        client.bytes.assert_not_called()
        result = classify_native_history(
            tuple(item.observation for item in reports), complete=True, selected_run_id=2,
        )
        self.assertFalse(result.allowed)
        self.assertEqual(result.blocking_attempts, ((2, 1),))

    def test_runs_are_paginated_and_bound_fails_closed(self):
        client = create_autospec(GitHubClient, instance=True)
        others = [{"id": index, "display_title": "another tag"} for index in range(1, 101)]
        target = {"id": 101, "display_title": proof_title(SCOPE.tag, "b" * 32, "a" * 64)}
        client.json.side_effect = [
            {"total_count": 101, "workflow_runs": others},
            {"total_count": 101, "workflow_runs": [target]},
        ]
        self.assertEqual(matching_runs(client, "owner/repo", SCOPE), (target,))
        self.assertIn("page=2", client.json.call_args.args[0])
        client.json.side_effect = None
        client.json.return_value = {"total_count": 1001, "workflow_runs": []}
        with self.assertRaisesRegex(VerificationError, "native_history_incomplete"):
            matching_runs(client, "owner/repo", SCOPE)

    def test_partial_or_duplicate_page_cannot_claim_complete_history(self):
        client = create_autospec(GitHubClient, instance=True)
        item = {"id": 1, "display_title": "other"}
        for rows in ([item], [item, item]):
            with self.subTest(rows=rows):
                client.json.return_value = {"total_count": 2, "workflow_runs": rows}
                with self.assertRaisesRegex(VerificationError, "native_history_incomplete"):
                    matching_runs(client, "owner/repo", SCOPE)


if __name__ == "__main__":
    unittest.main()
