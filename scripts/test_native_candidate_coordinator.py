"""The candidate target of the native coordinator, composed end to end over finite owners.

Each test counts native Starts through the canonical process owner and reads
the store through the real client. The fixture is described in
``test_native_candidate_support``.
"""

from __future__ import annotations

import contextlib
from dataclasses import replace
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_release.receipt import lock_name
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import commands, source
from scripts.delegate_acceptance.release_verification.candidate_attempts import claim_sha256, parse_claim
from scripts.test_native_candidate_support import MODEL, CandidateCase, git, harness_checkout


CLAIMED = ("claim", "native", "project", "record", "cleanup")


class SuccessTests(CandidateCase):
    def test_a_fresh_claim_launches_once_and_records_a_resolved_proof(self) -> None:
        receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason),
                         ("LOCAL_ONLY", "candidate_native_pass_recorded_not_readiness_authority"))
        self.assertEqual(self.starts, 1)
        self.assertEqual([receipt.stage(name).state for name in CLAIMED], ["complete"] * 5)
        self.assertEqual(receipt.stage("claim").result["disposition"], "confirmed")
        self.assertEqual(receipt.stage("record").result["disposition"], "confirmed")
        self.assertEqual(len(self.github.writes("PATCH")), 2)
        self.assertEqual(self.github.downloads, 1)
        snapshot = self.snapshot()
        claim = parse_claim(receipt.stage("claim").intent["claim"].encode())
        self.assertEqual(snapshot.history.claims, (claim,))
        self.assertEqual([proof.proof_sha256 for proof in snapshot.history.proofs],
                         [receipt.stage("project").result["proof_sha256"]])
        self.assertEqual(receipt.stage("claim").intent["claim_sha256"], claim_sha256(claim))
        self.assertFalse((receipt.invocation.root / "runner").exists())
        self.assertTrue(receipt.invocation.proof.is_file())

    def test_the_claim_binds_the_observed_harness_platform_binaries_and_models(self) -> None:
        receipt = self.execute()
        claim = parse_claim(receipt.stage("claim").intent["claim"].encode())
        facts = receipt.stage("preflight").result
        self.assertEqual((claim.harness_revision, claim.harness_sha256),
                         (facts["harness_revision"], facts["harness_sha256"]))
        self.assertEqual(claim.platform, self.platform())
        self.assertEqual(claim.binaries.as_mapping(), receipt.stage("archive").result["binaries"])
        self.assertEqual((claim.opencode_model, claim.ccr_model, claim.agy_model), (MODEL, MODEL, MODEL))
        self.assertEqual(claim.invocation_id, receipt.invocation.invocation_id)
        self.assertNotEqual(claim.harness_sha256, json.loads(self.manifest)["build"]["harness_sha256"])

    def test_only_the_native_tag_input_is_removed_and_the_source_revision_is_the_claims(self) -> None:
        with patch.dict(os.environ, {"TAG": "user-own-tag", "DELEGATE_ACCEPTANCE_TAG": "v9.9.9"}):
            receipt = self.execute()
        self.assertEqual(receipt.status, "LOCAL_ONLY")
        note = self.environment_note()
        self.assertEqual(note, {"TAG": "user-own-tag", "DELEGATE_ACCEPTANCE_TAG": None,
                                "DELEGATE_ACCEPTANCE_SOURCE_REVISION": receipt.stage("preflight").result[
                                    "harness_revision"], "OPENCODE_LIVE_MODEL": MODEL})
        self.assertEqual(os.environ.get("DELEGATE_ACCEPTANCE_TAG"), None)

    def test_a_blocked_matrix_is_recorded_and_cleaned_but_is_never_readiness(self) -> None:
        self.mode = "blocked"
        receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason),
                         ("LOCAL_ONLY", "candidate_native_blocked_recorded_not_readiness_authority"))
        self.assertEqual(receipt.stage("cleanup").state, "complete")


class RecordedOutcomeTests(CandidateCase):
    def test_a_valid_fail_proof_is_recorded_and_every_private_file_is_retained(self) -> None:
        self.mode = "fail"
        receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason), ("FAIL", "candidate_native_fail_recorded_evidence_retained"))
        self.assertEqual(receipt.stage("record").result["disposition"], "confirmed")
        self.assertEqual(receipt.stage("cleanup").state, "pending")
        self.assertTrue((receipt.invocation.root / "runner" / "native-processes").is_dir())
        self.assertEqual(self.snapshot().history.proofs[0].proof_sha256,
                         receipt.stage("project").result["proof_sha256"])

    def test_an_unresolved_proof_is_recorded_incomplete_and_retained(self) -> None:
        self.mode = "unresolved"
        receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason),
                         ("INCOMPLETE", "candidate_native_unresolved_recorded_evidence_retained"))
        self.assertEqual(receipt.stage("project").result["failed_count"], 0)
        self.assertTrue(receipt.stage("project").result["unresolved_case_ids"])
        self.assertEqual(receipt.stage("cleanup").state, "pending")
        self.assertEqual(len(self.snapshot().history.proofs), 1)
        self.assertTrue((receipt.invocation.root / "runner").is_dir())


class PreClaimRefusalTests(CandidateCase):
    def assertBlockedBeforeClaim(self, receipt, reason: str) -> None:
        self.assertEqual((receipt.status, receipt.reason), ("BLOCKED", reason))
        self.assertEqual([receipt.stage(name).state for name in CLAIMED], ["pending"] * 5)
        self.assertEqual((self.starts, self.github.writes("POST"), self.github.writes("PATCH")), (0, [], []))

    def test_a_locator_with_the_same_candidate_id_but_another_run_is_refused_before_download(self) -> None:
        other = replace(self.locator, run_id=self.locator.run_id + 1)
        self.assertEqual(other.candidate_id, self.locator.candidate_id)
        receipt = self.execute(self.create_receipt(self.request(other), "2" * 32))
        self.assertBlockedBeforeClaim(receipt, "candidate_locator_mismatch")
        self.assertEqual(self.github.downloads, 0)

    def test_a_changed_container_is_refused_by_the_loader_before_any_claim(self) -> None:
        self.github.container = self.github.container[:-1] + bytes([self.github.container[-1] ^ 1])
        receipt = self.execute()
        self.assertBlockedBeforeClaim(receipt, "candidate_container_mismatch")
        self.assertEqual(receipt.stage("download").state, "intended")

    def edit_harness_file(self) -> Path:
        """Append a fixture-only line to one fingerprinted harness file; the test restores it."""
        edited = self.checkout / "scripts" / "native_acceptance_evidence.py"
        original = edited.read_bytes()
        self.addCleanup(self.restore_harness_file, edited, original)
        edited.write_bytes(original + b"\n# fixture-only change\n")
        return edited

    def restore_harness_file(self, edited: Path, original: bytes) -> None:
        self.require_resolved("harness file restoration")
        edited.write_bytes(original)

    def test_an_uncommitted_native_harness_edit_is_refused_before_download(self) -> None:
        self.edit_harness_file()
        receipt = self.execute()
        self.assertBlockedBeforeClaim(receipt, "candidate_native_harness_not_committed")
        self.assertEqual(self.github.calls, [])

    def test_a_replace_ref_cannot_make_a_commit_hold_the_executing_bytes(self) -> None:
        # This test's own checkout: HEAD commits other bytes than the executing
        # ones, and a replace ref swaps that blob for the executing bytes.
        self.checkout = harness_checkout(self.case)
        relative = "scripts/native_acceptance_evidence.py"
        target = self.checkout / relative
        executing = target.read_bytes()
        target.write_bytes(executing + b"\n# committed fixture-only change\n")
        git(self.case, self.checkout, "commit", "--quiet", "--no-verify", "--all", "--message=other bytes")
        committed = git(self.case, self.checkout, "rev-parse", f"HEAD:{relative}").decode().strip()
        target.write_bytes(executing)
        replacement = git(self.case, self.checkout, "hash-object", "-w", "--", relative).decode().strip()
        git(self.case, self.checkout, "replace", committed, replacement)
        receipt = self.execute(self.create_receipt(self.request(), "4" * 32))
        self.assertBlockedBeforeClaim(receipt, "candidate_native_harness_not_committed")
        self.assertEqual((self.github.calls, self.github.downloads), ([], 0))

    def test_a_harness_file_changed_during_the_observation_is_refused(self) -> None:
        fingerprint = source.fingerprint_sources

        def fingerprint_then_edit(root: Path, seeds: tuple[str, ...]) -> dict[str, object]:
            observed = fingerprint(root, seeds)
            self.edit_harness_file()
            return observed

        with patch.object(source, "fingerprint_sources", side_effect=fingerprint_then_edit):
            receipt = self.execute()
        self.assertBlockedBeforeClaim(receipt, "native_source_changed_during_preflight")
        self.assertEqual(self.github.calls, [])

    def test_an_unproven_preflight_git_closure_reaches_both_owners_before_conversion(self) -> None:
        # A dedicated harness owner: this zero-start control must never retain the shared class tree.
        self.launches: list[tuple[object, ...]] = []
        self.zero_start = False
        self.tree = FixtureTree.open(prefix="native-candidate-preflight", label="native candidate preflight fixture")
        self.addCleanup(self.remove_after_zero_starts)
        self.checkout = harness_checkout(self.tree)
        self.receipt = self.create_receipt(self.request(), "6" * 32)
        edited = self.checkout / "scripts" / "native_acceptance_evidence.py"
        original = edited.read_bytes()
        edited.write_bytes(original + b"\n# fixture-only change\n")
        failure = BoundedCommandError(CommandFailure.TIMEOUT)
        with patch.object(commands, "run_bounded_command", side_effect=failure) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                receipt = self.execute()
            finally:
                self.zero_start = owner.call_count == 1 and not self.launches
        self.assertEqual((self.case.unresolved, self.tree.unresolved), (failure, failure))
        # Its closure is unknown, so this is never a definite refusal.
        self.assertEqual((receipt.status, receipt.reason), ("INCOMPLETE", "command_timeout"))
        self.assertEqual([receipt.stage(name).state for name in CLAIMED], ["pending"] * 5)
        self.assertEqual((receipt.stage("preflight").state, self.starts), ("intended", 0))
        self.assertEqual((self.launches, self.github.calls), ([], []))
        with self.assertRaisesRegex(RuntimeError, "no proven closure"):
            self.restore_harness_file(edited, original)
        self.assertNotEqual(edited.read_bytes(), original)

    def refuse_launch(self, *arguments: object, **_options: object) -> None:
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a closure control")

    def remove_after_zero_starts(self) -> None:
        """Dispose both owners; a retained one is removed only after the zero-start proof."""
        report = io.StringIO()
        with contextlib.redirect_stderr(report):
            retained = [tree for tree in (self.case, self.tree) if not tree.dispose()]
        if retained and not self.zero_start:
            raise self.failureException(f"a child may have started: {report.getvalue()}")
        for tree in retained:
            tree.remove_retained()

    def test_an_invocation_for_another_platform_is_refused(self) -> None:
        os_name = "Darwin" if self.platform()[0] == "Linux" else "Linux"
        receipt = self.create_receipt(self.request(), "3" * 32)
        invocation = replace(receipt.invocation, platform_os=os_name)
        with patch.object(receipt, "invocation", invocation):
            self.execute(receipt)
        stored = type(receipt).load(receipt.path)
        self.assertEqual((stored.status, stored.reason), ("BLOCKED", "candidate_platform_changed"))
        self.assertEqual(self.starts, 0)

    def test_a_history_that_needs_closure_facts_refuses_with_none_supplied(self) -> None:
        claim = self.foreign_claim()
        self.put_claim(claim)
        self.put_closure_triage(claim)
        receipt = self.execute()
        self.assertBlockedBeforeClaim(receipt, "store_closure_unverified")
        self.assertEqual(self.github.downloads, 0)


class ClaimRefusalTests(CandidateCase):
    def assertClaimRefused(self, receipt, code: str) -> dict[str, object]:
        self.assertEqual((receipt.status, receipt.reason), ("BLOCKED", f"candidate_claim_refused:{code}"))
        claim = receipt.stage("claim")
        self.assertEqual((claim.state, claim.result["disposition"], claim.result["code"]),
                         ("complete", "refused", code))
        self.assertEqual((self.starts, self.github.writes("POST"), self.github.writes("PATCH")), (0, [], []))
        self.assertEqual(receipt.stage("native").state, "pending")
        return claim.result

    def test_an_existing_claim_of_this_platform_is_refused_before_any_write(self) -> None:
        self.put_claim(self.foreign_claim())
        self.assertEqual(self.assertClaimRefused(self.execute(), "store_candidate_tree_invalid")["findings"], [])

    def test_a_policy_denial_records_its_exact_findings_and_nothing_starts(self) -> None:
        receipt = self.execute(self.create_receipt(replace(self.request(), attempt=2), "5" * 32))
        result = self.assertClaimRefused(receipt, "store_claim_denied")
        self.assertEqual(result["findings"], [{"code": "claim_attempt_stale", "subject": "2"}])

    def test_a_second_invocation_of_the_same_attempt_shares_its_lock_and_is_refused(self) -> None:
        first = self.execute()
        self.assertEqual((first.status, self.starts), ("LOCAL_ONLY", 1))
        second = self.create_receipt(self.request(), "4" * 32)
        self.assertEqual(lock_name(second.invocation.request), lock_name(first.invocation.request))
        result = self.execute(second)
        self.assertEqual((result.status, result.reason),
                         ("BLOCKED", "candidate_claim_refused:store_candidate_tree_invalid"))
        self.assertEqual(self.starts, 1)
        self.assertEqual(len(self.snapshot().history.claims), 1)

    def test_reentering_execute_on_a_used_receipt_never_launches(self) -> None:
        self.assertEqual(self.execute().status, "LOCAL_ONLY")
        again = self.execute(type(self.receipt).load(self.receipt.path))
        self.assertEqual((again.status, again.reason),
                         ("INCOMPLETE", "candidate_preflight_incomplete_no_rerun"))
        self.assertEqual(self.starts, 1)

    def test_an_unexpected_refusal_never_marks_a_claimed_run_blocked(self) -> None:
        with patch("scripts.delegate_acceptance.native_release.candidate.project_candidate",
                   side_effect=ReleaseError("fixture_projection_refused")):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason), ("INCOMPLETE", "fixture_projection_refused"))
        self.assertEqual(self.starts, 1)


if __name__ == "__main__":
    unittest.main()
