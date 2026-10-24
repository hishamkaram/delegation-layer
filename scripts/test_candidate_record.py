"""The explicit maintainer record command over the production store, replay and a real Git source.

The store is the in-memory ``FakeGitHub`` behind the real ``GitHubClient`` and
the production ``candidate_store`` reader, writer and replay; it proves the
composed request flow, not GitHub itself. The freeze observes a real
temporary Git repository. Every expected finding comes from the canonical
policy over the facts the store reads back.
"""

from __future__ import annotations

import contextlib
from dataclasses import replace
import errno
import io
from pathlib import Path
import os
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import record_candidate
from scripts import test_candidate_record_support as record_support
from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import (
    candidate_record, candidate_record_receipt, candidate_store_replay, commands,
)
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    claim_sha256, encode_intent, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ResourceDisposition,
)
from scripts.delegate_acceptance.release_verification.candidate_history import encode_triage
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    InstallChannel, parse_install_proof, read_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import encode_locator, encode_manifest
from scripts.delegate_acceptance.release_verification.candidate_readiness import decide_launch, evaluate_attempt
from scripts.delegate_acceptance.release_verification.candidate_store_contract import TransitionKind
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_record_support import DARWIN, LINUX, PLANNED, RecordCase, findings
from scripts.test_candidate_store_support import BRANCH, REFUSED, Fault

REPLAY_STAGE = candidate_store_replay.STAGE


class RegisterTests(RecordCase):
    def test_register_appends_the_bound_pair_once_and_requests_no_artifact(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        reported = self.assertConfirmed(self.register(locator, manifest), TransitionKind.REGISTER, self.store.genesis)
        self.assertEqual(reported["record_sha256s"], sorted((sha(locator), sha(manifest))))
        self.assertEqual(self.document()["inputs"], {"locator_sha256": sha(locator), "manifest_sha256": sha(manifest),
                                                     "candidate_id": self.records.id})
        # Every request is a store Git database, branch or ruleset request: nothing reads an artifact.
        self.assertTrue(all(endpoint.startswith(f"repos/{fx.REPOSITORY}") for _, endpoint in self.github.calls))
        self.assertFalse([endpoint for _, endpoint in self.github.calls if "/actions" in endpoint])
        self.assertEqual(self.git_commands(), [])
        snapshot = self.readback()
        self.assertEqual((snapshot.head, snapshot.candidate), (reported["commit"], self.records.candidate))

    def test_unbound_or_noncanonical_documents_refuse_before_any_request(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        other = encode_manifest(fx.manifest("0.1.38"))
        cases = {
            "manifest of another digest": (locator, other, "candidate_manifest_digest_mismatch"),
            "noncanonical locator": (locator.replace(b",", b", "), manifest, "candidate_record_not_canonical"),
            "noncanonical manifest": (locator, manifest + b" ", "candidate_manifest_digest_mismatch"),
        }
        for label, (locator_data, manifest_data, code) in cases.items():
            with self.subTest(label):
                self.github.calls.clear()
                self.assertRefusedBeforeWrite(self.register(locator_data, manifest_data), code, self.store.genesis)
                self.assertEqual(self.github.calls, [])
                # Only bytes actually read carry a digest; no candidate was derived from them.
                self.assertEqual(self.document()["inputs"], {"locator_sha256": sha(locator_data),
                                                             "manifest_sha256": sha(manifest_data)})

    def test_a_foreign_repository_locator_is_refused_by_canonical_admission(self):
        locator = replace(self.records.candidate.locator, repository="another/repository")
        result = self.register(encode_locator(locator), encode_manifest(self.records.candidate.manifest))
        reported = self.assertRefusedBeforeWrite(result, "store_candidate_repository_mismatch", self.store.genesis)
        self.assertEqual(reported["stage"], REPLAY_STAGE)

    def test_an_unreadable_input_is_reported_without_an_invented_digest(self):
        missing = self.scratch / "absent-manifest.json"
        result = self.run_command("register", "--locator", str(self.input(b"{}")), "--manifest", str(missing))
        self.assertRefusedBeforeWrite(result, "manifest_input_unreadable", self.store.genesis)
        self.assertEqual(self.document()["inputs"], {"locator_sha256": sha(b"{}")})
        self.assertEqual(self.github.calls, [])


class IntentTests(RecordCase):
    def setUp(self):
        super().setUp()
        self.registered = self.seed(self.records.register())

    def test_the_intent_is_stored_exactly_as_written(self):
        value = self.records.intent()
        data = encode_intent(value)
        reported = self.assertConfirmed(self.intent(data), TransitionKind.INTENT, self.registered)
        self.assertEqual(reported["record_sha256s"], [sha(data)])
        self.assertEqual(self.stored(reported["commit"], "attempt-0001.intent.json"), data)
        self.assertEqual(self.readback().history.intents, (value,))

    def test_canonical_launch_denials_refuse_with_their_exact_findings_before_any_write(self):
        snapshot = self.readback()
        cases = {
            "wrong attempt": self.records.intent(attempt=2),
            "missing required platform": self.records.intent(platforms=(LINUX,)),
            "foreign source": self.records.intent(source_commit=self.later),
        }
        for label, value in cases.items():
            with self.subTest(label):
                self.github.calls.clear()
                reported = self.assertRefusedBeforeWrite(self.intent(encode_intent(value)), "store_launch_denied",
                                                         self.registered)
                expected = decide_launch(candidate=snapshot.candidate, history=snapshot.history, intent=value)
                self.assertFalse(expected.allowed)
                self.assertEqual((reported["stage"], reported["findings"]), (REPLAY_STAGE, findings(expected)))

    def test_noncanonical_intent_bytes_refuse_before_any_request(self):
        data = encode_intent(self.records.intent()).replace(b",", b", ")
        self.assertRefusedBeforeWrite(self.intent(data), "candidate_record_not_canonical", self.registered)
        self.assertEqual(self.github.calls, [])


class TriageTests(RecordCase):
    """Attempt 1 claimed both platforms and proved neither; the triage cites a closure nobody supplies."""

    def setUp(self):
        super().setUp()
        records = self.records
        self.claims = tuple(records.claim(platform) for platform in PLANNED)
        self.head = self.seed(records.register(), records.intent_file(records.intent()),
                              *(records.claim_file(claim) for claim in self.claims))

    def triage_record(self):
        snapshot = self.readback()
        prior = evaluate_attempt(snapshot.candidate.manifest, snapshot.history, 1)
        linux = next(claim for claim in self.claims if claim.platform == LINUX)
        fact = fx.resolution(ClaimSubject(claim_sha256(linux)), invocation=linux.invocation_id,
                             resources=((NATIVE_CHILD_RESOURCE, ResourceDisposition.WAITED),))
        approval = fx.approval(attempt=2, platforms=PLANNED, source=self.records.source.commit)
        return fx.triage(self.records.candidate, attempt=1, prior_proofs=(), explained=prior.findings,
                         approved=approval, prior_claims=self.claims, closure_refs=(fx.reference(fact),)).record

    def test_the_triage_is_stored_unchanged_and_its_closure_stays_unresolved_for_the_next_intent(self):
        record = self.triage_record()
        data = encode_triage(record)
        reported = self.assertConfirmed(self.triage(data), TransitionKind.TRIAGE, self.head)
        # Only the triage itself: no closure record accompanies it.
        self.assertEqual(reported["record_sha256s"], [sha(data)])
        self.assertEqual(self.stored(reported["commit"], f"triage.{sha(data)}.json"), data)
        snapshot = self.readback()
        stored = snapshot.history.triages
        self.assertEqual([(item.record, item.resolutions, item.provenance.transaction_commit) for item in stored],
                         [(record, (), reported["commit"])])
        replacement = self.records.intent(attempt=2, triage_sha256=sha(data))
        expected = decide_launch(candidate=snapshot.candidate, history=snapshot.history, intent=replacement)
        self.github.calls.clear()
        self.work = self.owned("work-again")
        refused = self.assertRefusedBeforeWrite(self.intent(encode_intent(replacement)), "store_launch_denied",
                                                reported["commit"])
        self.assertEqual(refused["findings"], findings(expected))
        # The canonical policy distinguishes a cited fact nobody supplied from a claim no triage cites.
        closure = {(item["code"], item["subject"].split("/")[0]) for item in refused["findings"]
                   if item["code"].startswith("closure_")}
        self.assertEqual(closure, {("closure_resolution_unverified", "Linux"), ("closure_resolution_missing", "Darwin")})

    def test_noncanonical_triage_bytes_refuse_before_any_request(self):
        data = encode_triage(self.triage_record()) + b"\n"
        self.github.calls.clear()
        self.assertRefusedBeforeWrite(self.triage(data), "candidate_record_not_canonical", self.head)
        self.assertEqual(self.github.calls, [])


class InstallTests(RecordCase):
    def setUp(self):
        super().setUp()
        records = self.records
        self.head = self.seed(records.register(), records.intent_file(records.intent()))

    def test_the_paired_install_is_admitted_against_the_actual_bound_installer_source(self):
        proof, witness = self.records.install(LINUX, InstallChannel.SHELL)
        reported = self.assertConfirmed(self.install(proof, witness), TransitionKind.INSTALL, self.head)
        self.assertEqual(reported["record_sha256s"], sorted((sha(proof), sha(witness))))
        self.assertEqual(self.stored(reported["commit"], "install.Linux-amd64.shell.witness.json"), witness)
        # Canonical admission itself read the installer at the manifest's source commit.
        self.assertIn(("GET", f"repos/{fx.REPOSITORY}/git/commits/{self.records.source.commit}"), self.github.calls)
        self.assertEqual(self.git_commands(), [])
        self.assertEqual(self.readback().history.installs, (parse_install_proof(proof),))

    def test_a_changed_witness_byte_refuses_with_the_canonical_reader_finding(self):
        proof, witness = self.records.install(DARWIN, InstallChannel.NPM)
        changed = witness.replace(b'"tag"', b'"tah"', 1)
        self.assertNotEqual(changed, witness)
        with self.assertRaises(VerificationError) as canonical:
            read_install_proof(proof, changed, manifest=self.records.candidate.manifest, installer_source=None,
                               repository=fx.REPOSITORY)
        reported = self.assertRefusedBeforeWrite(self.install(proof, changed), canonical.exception.code, self.head)
        self.assertEqual(reported["stage"], canonical.exception.stage)

    def test_a_missing_witness_refuses_before_any_request(self):
        proof, _ = self.records.install(DARWIN, InstallChannel.NPM)
        result = self.run_command("install", "--proof", str(self.input(proof)),
                                  "--witness", str(self.scratch / "absent-witness.json"))
        self.assertRefusedBeforeWrite(result, "witness_input_unreadable", self.head)
        self.assertEqual(self.document()["inputs"], {"proof_sha256": sha(proof)})
        self.assertEqual(self.github.calls, [])


class CommandTests(RecordCase):
    """The command boundary: one new receipt, sanitized outcomes and exit codes."""

    def test_entrypoint_is_the_record_command(self):
        self.assertIs(record_candidate.main, candidate_record.main)

    def test_every_command_refuses_an_existing_receipt_before_any_read(self):
        records = self.records
        claim = records.claim(LINUX)
        triage = fx.triage(records.candidate, attempt=1, prior_proofs=(), explained=(), prior_claims=(claim,)).record
        proof, witness = records.install(LINUX, InstallChannel.SHELL)
        self.seed(records.register(), records.intent_file(records.intent()), records.claim_file(claim),
                  records.review_file(records.review()))
        # Every input is canonical and readable, so a command that read or requested anything before
        # reserving its receipt would reach the store or the checkout.
        argvs = {
            "register": ["--locator", str(self.input(encode_locator(records.candidate.locator))),
                         "--manifest", str(self.input(encode_manifest(records.candidate.manifest)))],
            "intent": ["--intent", str(self.input(encode_intent(records.intent())))],
            "triage": ["--triage", str(self.input(encode_triage(triage)))],
            "install": ["--proof", str(self.input(proof)), "--witness", str(self.input(witness))],
            "freeze": ["--candidate-id", records.id, "--review-sha256", sha(encode_review(records.review())),
                       "--repository-root", str(self.source.path)],
        }
        for label, arguments in argvs.items():
            with self.subTest(label), patch.object(commands, "run_bounded_command") as child:
                self.github.calls.clear()
                receipt = self.scratch / f"existing-{label}.json"
                receipt.write_bytes(b"prior observation\n")
                code, reported, _ = self.run_command(label, *arguments, receipt=receipt)
                self.assertEqual((code, reported["code"]), (1, "record_receipt_unavailable"))
                self.assertEqual(receipt.read_bytes(), b"prior observation\n")
                self.assertEqual((self.github.calls, child.call_count), ([], 0))

    def test_receipt_write_fault_after_confirmation_fails_without_relabelling(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        with patch.object(candidate_record_receipt, "write_all", side_effect=OSError(errno.EIO, "fixture fault")):
            code, reported, _ = self.register(locator, manifest)
        self.assertEqual(code, 1)
        self.assertEqual((reported["disposition"], reported["receipt_error"]),
                         ("confirmed", "record_receipt_write_failed"))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])
        self.assertEqual(self.readback().head, reported["commit"])

    def test_receipt_sync_fault_after_confirmation_fails_without_relabelling(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        def fsync(descriptor: int) -> None:
            raise OSError(errno.EIO, "fixture fault")

        with patch.object(candidate_record_receipt, "os", SimpleNamespace(fsync=fsync, close=os.close)):
            code, reported, _ = self.register(locator, manifest)
        self.assertEqual((code, reported["disposition"], reported["receipt_error"]),
                         (1, "confirmed", "record_receipt_write_failed"))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])

    def test_receipt_close_fault_after_confirmation_keeps_the_commit_and_fails(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        with self.receipt_close_fault() as closed:
            code, reported, _ = self.register(locator, manifest)
        self.assertEqual((code, len(closed)), (1, 1))
        self.assertEqual((reported["disposition"], reported["receipt_error"]),
                         ("confirmed", "record_receipt_write_failed"))
        self.assertEqual((self.github.refs[BRANCH], self.patches(), self.calls_after_patch()),
                         (reported["commit"], 1, []))
        # The bytes were written and synced before the close failed.
        self.assertEqual(self.document()["outcome"]["commit"], reported["commit"])

    def test_receipt_close_fault_after_an_ambiguous_append_still_reports_it(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        self.github.faults.append(Fault("PATCH", f"/git/refs/heads/{BRANCH}", result=REFUSED, apply=True))
        with self.receipt_close_fault() as closed:
            code, reported, _ = self.register(locator, manifest)
        self.assertEqual((code, len(closed)), (3, 1))
        self.assertEqual((reported["disposition"], reported["failure"], reported["receipt_error"]),
                         ("ambiguous", "github_request_failed", "record_receipt_write_failed"))
        self.assertEqual((self.github.refs[BRANCH], self.patches(), self.calls_after_patch()),
                         (reported["proposed"], 1, []))
        self.assertEqual(self.document()["outcome"], {key: reported[key] for key in (
            "disposition", "parent", "proposed", "nonce", "record_sha256s", "failure")})

    def test_receipt_close_fault_never_masks_an_error_the_record_raises(self):
        locator, manifest = encode_locator(self.records.candidate.locator), encode_manifest(self.records.candidate.manifest)
        with self.receipt_close_fault() as closed, patch.object(candidate_record, "append", return_value=object()), \
                self.assertRaises(VerificationError) as caught:
            self.register(locator, manifest)
        self.assertEqual((caught.exception.code, len(closed)), ("store_append_outcome_invalid", 1))
        self.assertEqual(caught.exception.__notes__, ["record_receipt_write_failed: the receipt close failed"])

    def test_usage_errors_exit_2_without_a_receipt(self):
        receipt = self.scratch / "usage-receipt.json"
        location = self.store.location
        common = ["--store-repository", location.repository, "--store-branch", location.branch,
                  "--store-genesis-commit", location.genesis_commit, "--store-marker-blob", location.marker_blob,
                  "--work-dir", str(self.work), "--receipt", str(receipt)]
        cases = {"no command": common, "no witness": ["install", "--proof", "p.json", *common],
                 "short review digest": ["freeze", "--candidate-id", self.records.id, "--review-sha256", "0" * 63,
                                         "--repository-root", str(self.source.path), *common]}
        for label, argv in cases.items():
            with self.subTest(label), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit) as caught:
                candidate_record.main(argv)
            self.assertEqual(caught.exception.code, 2)
            self.assertFalse(receipt.exists())
            self.assertEqual(self.github.calls, [])


class HeadObservationLifetimeTests(RecordCase):
    def test_an_original_unconverted_head_failure_retains_the_tree_until_zero_launches_are_proven(self):
        tree = FixtureTree.open(prefix="candidate-record-head-fault", label="candidate record head fault")
        self.addCleanup(tree.dispose)
        # This test's own owner: its work directory, input and receipt, and its children's faults.
        self.tree = tree
        self.work, self.scratch = self.owned("work"), self.owned("scratch")
        review = self.records.review()
        head = self.seed(self.records.register(), self.records.review_file(review))
        injected = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        observed: list[Path] = []

        def unconverted(receipt: dict, environment: dict[str, str], *, repository: Path) -> tuple[str, str]:
            observed.append(repository)
            raise injected

        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with patch.object(record_support, "head_tree", unconverted), \
                patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches, \
                self.assertRaises(BoundedCommandError) as caught:
            self.freeze(sha(encode_review(review)))
        self.assertIs(caught.exception, injected)
        self.assertEqual(observed, [self.source.path])
        self.assertIs(tree.unresolved, injected)
        self.assertEqual((self.writes(), self.github.refs[BRANCH]), ([], head))
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(tree.dispose())
        self.assertIn(f"(command_termination_timeout): {tree.root}", stderr.getvalue())
        self.assertTrue(self.work.is_dir() and self.scratch.is_dir())
        self.assertEqual(launches.call_count, 0)
        tree.remove_retained()
        self.assertFalse(tree.root.exists())


if __name__ == "__main__":
    unittest.main()
