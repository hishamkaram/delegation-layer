"""The read-only readiness check over the production store and replay, observing a real Git checkout.

Every history is committed to the in-memory ``FakeGitHub`` store as one
transaction per canonical group. Every verdict and finding expected here
comes from ``evaluate_readiness`` over the facts the store reads back; the
check must report it unchanged, append nothing and need no pull request.
"""

from __future__ import annotations

import contextlib
import errno
import io
import json
import os
from pathlib import Path
import secrets
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as proof_fixture
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_record, candidate_record_receipt, commands
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    PROVIDER_FAMILIES, BlockReason, CellStatus, ChangeScope, MatrixScope, PlatformClaim,
    candidate_proof_expectation, encode_review, scope_record,
)
from scripts.delegate_acceptance.release_verification.candidate_build_source import require_clean
from scripts.delegate_acceptance.release_verification.candidate_history import FindingRef
from scripts.delegate_acceptance.release_verification.candidate_install_contract import InstallChannel
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    STAGE as READINESS_STAGE, Readiness, SourceObservation, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import EntryFamily, TransitionKind
from scripts.delegate_acceptance.release_verification.candidate_store_replay import CandidateSnapshot
from scripts.test_candidate_record_support import (
    DARWIN, LEAKED_GIT, LINUX, PLANNED, TOKEN, Files, RecordCase, findings, name,
)
from scripts.test_candidate_source_support import ChildFault, SourceRepository, settle_retained
from scripts.test_candidate_store_support import BRANCH


def verdicts(readiness: Readiness) -> list[dict[str, object]]:
    """The receipt form of the canonical per-attempt verdicts."""
    return [{"attempt": item.attempt, "state": item.state.value,
             "findings": [{"code": finding.code, "subject": finding.subject} for finding in item.findings]}
            for item in readiness.attempt_verdicts]


class CheckCase(RecordCase):
    """Runs ``check`` exactly as a maintainer would: no work directory and no pull request."""

    def setUp(self):
        super().setUp()
        # Read at call time, so a test that owns a separate tree receives its own children's faults.
        observer = patch.object(candidate_record, "require_clean", self.observe_clean)
        observer.start()
        self.addCleanup(observer.stop)

    def observe_clean(self, receipt: dict, environment: dict[str, str], *, repository: Path, code: str) -> None:
        """The canonical clean check; its original child failure reaches the tree before the command converts it."""
        self.tree.observe_operation(lambda: require_clean(receipt, environment, repository=repository, code=code))

    def check(self, review_sha256: str, *, repository: Path | None = None, candidate_id: str | None = None,
              receipt: Path | None = None) -> tuple[int, dict, str]:
        self.receipt = receipt or self.scratch / f"receipt-{secrets.token_hex(4)}.json"
        location = self.store.location
        argv = ["check", "--candidate-id", candidate_id or self.records.id, "--review-sha256", review_sha256,
                "--repository-root", str(repository or self.source.path),
                "--store-repository", location.repository, "--store-branch", location.branch,
                "--store-genesis-commit", location.genesis_commit, "--store-marker-blob", location.marker_blob,
                "--receipt", str(self.receipt)]
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.dict(os.environ, {"GH_TOKEN": TOKEN, "GITHUB_TOKEN": TOKEN, **LEAKED_GIT}), \
                contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = candidate_record.main(argv)
        self.assertNotIn(TOKEN, stdout.getvalue() + stderr.getvalue())
        return code, json.loads(stdout.getvalue()), stderr.getvalue()

    def canonical(self, review_sha256: str, observed: SourceObservation | None = None) -> Readiness:
        """The canonical verdict over the read-back facts, for the recorded source unless ``observed`` is given."""
        snapshot = self.readback()
        review = next(item for item in snapshot.reviews if item.sha256 == review_sha256)
        source = self.records.source
        return evaluate_readiness(
            candidate=snapshot.candidate, observed=observed or SourceObservation(head_commit=source.commit,
                                                                                 tree=source.tree),
            scope=review.record.reviewed_scope.scope, history=snapshot.history, review=review)

    def assertObservedOnly(self, reported: dict, head: str) -> None:
        """Nothing was written, the branch stayed at ``head`` and the receipt holds the printed outcome."""
        self.assertEqual((self.writes(), self.github.refs[BRANCH]), ([], head))
        self.assertEqual(list(self.work.iterdir()), [])
        document = self.document()
        self.assertEqual((document["kind"], document["command"]),
                         ("delegation-layer-candidate-readiness-check", "check"))
        self.assertEqual(document["outcome"], {key: value for key, value in reported.items()
                                               if key not in {"receipt", "receipt_error"}})
        # No pull request, commit status or Actions request: only the store's Git database and its ruleset.
        self.assertFalse([endpoint for _, endpoint in self.github.calls
                          if any(part in endpoint for part in ("/pulls", "/statuses", "/actions"))])

    def assertNotReady(self, result: tuple[int, dict, str], expected: Readiness, head: str) -> dict:
        code, reported, _ = result
        self.assertFalse(expected.ready)
        self.assertTrue(expected.findings)
        self.assertEqual((code, reported["disposition"], reported["code"], reported["stage"]),
                         (1, "refused", "publication_not_ready", READINESS_STAGE))
        self.assertEqual((reported["ready"], reported["findings"], reported["attempt_verdicts"]),
                         (False, findings(expected), verdicts(expected)))
        self.assertObservedOnly(reported, head)
        return reported


def without(groups: list[Files], *entries: str) -> list[Files]:
    """The groups less every one that holds any of ``entries``."""
    return [files for files in groups if not set(entries) & set(files)]


class CheckTests(CheckCase):
    def ready(self) -> tuple[str, str]:
        return self.seed(*self.records.ready()), sha(encode_review(self.records.review()))

    def test_a_ready_candidate_reports_the_canonical_verdict_and_appends_nothing(self):
        head, review_sha256 = self.ready()
        expected = self.canonical(review_sha256)
        self.assertTrue(expected.ready)
        source = self.records.source
        code, reported, _ = self.check(review_sha256)
        self.assertEqual((code, reported["disposition"], reported["receipt_error"]), (0, "confirmed", None))
        self.assertEqual((reported["kind"], reported["ready"], reported["findings"], reported["attempt_verdicts"]),
                         ("readiness-check", True, [], verdicts(expected)))
        self.assertEqual(reported["scope"], scope_record(self.records.review().reviewed_scope.scope))
        self.assertNotIn("commit", reported)
        self.assertIn("appended nothing", reported["notice"])
        self.assertEqual(self.document()["inputs"], {
            "candidate_id": self.records.id, "review_sha256": review_sha256, "store_head": head,
            "observed_head_commit": source.commit, "observed_tree": source.tree})
        self.assertEqual(self.git_commands(), ["git-head", "git-tree", "git-status"])
        self.assertObservedOnly(reported, head)
        # The store is unchanged, so the canonical freeze of the same facts still appends on the same head.
        self.assertConfirmed(self.freeze(review_sha256), TransitionKind.FREEZE, head)

    def test_a_missing_native_proof_is_not_ready_with_the_canonical_findings(self):
        head = self.seed(*without(self.records.ready(), name(EntryFamily.PROOF, attempt=1, platform=LINUX)))
        review_sha256 = sha(encode_review(self.records.review()))
        self.assertNotReady(self.check(review_sha256), self.canonical(review_sha256), head)

    def test_a_missing_install_is_not_ready_and_its_finding_is_never_filtered(self):
        entry = name(EntryFamily.INSTALL_PROOF, platform=LINUX, channel=InstallChannel.SHELL)
        head = self.seed(*without(self.records.ready(), entry))
        review_sha256 = sha(encode_review(self.records.review()))
        expected = self.canonical(review_sha256)
        self.assertNotReady(self.check(review_sha256), expected, head)
        self.assertEqual(findings(expected), [{"code": "install_proof_missing", "subject": "Linux/amd64/shell"}])

    def test_a_checkout_at_another_commit_is_not_ready(self):
        head, review_sha256 = self.ready()
        observed = SourceObservation(head_commit=self.later, tree=self.moved.tree_of(self.later))
        expected = self.canonical(review_sha256, observed)
        self.assertEqual({item.code for item in expected.findings}, {"source_changed"})
        self.assertNotReady(self.check(review_sha256, repository=self.moved.path), expected, head)
        self.assertEqual(self.document()["inputs"]["observed_tree"], observed.tree)

    def test_a_dirty_checkout_at_the_recorded_commit_is_refused_before_readiness(self):
        head, review_sha256 = self.ready()
        dirty = SourceRepository(self.tree, f"dirty-{secrets.token_hex(6)}", clone_of=self.source)
        self.assertEqual(dirty.git("rev-parse", "HEAD"), self.records.source.commit)
        (dirty.path / "untracked.txt").write_bytes(b"not committed\n")
        code, reported, _ = self.check(review_sha256, repository=dirty.path)
        self.assertEqual((code, reported["disposition"], reported["code"], reported["findings"]),
                         (1, "refused", "check_checkout_not_clean", []))
        self.assertNotIn("ready", reported)
        self.assertEqual(self.git_commands(), ["git-head", "git-tree", "git-status"])
        self.assertObservedOnly(reported, head)

    def test_an_unknown_review_or_candidate_refuses_before_observing_the_checkout(self):
        head, review_sha256 = self.ready()
        cases = {"unknown review": ("0" * 64, None, "freeze_review_missing"),
                 "unregistered candidate": (review_sha256, "8" * 64, "candidate_not_registered")}
        for label, (digest, candidate_id, expected) in cases.items():
            with self.subTest(label):
                self.github.calls.clear()
                code, reported, _ = self.check(digest, candidate_id=candidate_id)
                self.assertEqual((code, reported["disposition"], reported["code"]), (1, "refused", expected))
                self.assertEqual(self.git_commands(), [])
                self.assertObservedOnly(reported, head)

    def test_an_existing_receipt_is_refused_before_any_read(self):
        _, review_sha256 = self.ready()
        receipt = self.scratch / "existing-check.json"
        receipt.write_bytes(b"prior observation\n")
        self.github.calls.clear()
        with patch.object(commands, "run_bounded_command") as child:
            code, reported, _ = self.check(review_sha256, receipt=receipt)
        self.assertEqual((code, reported["code"]), (1, "record_receipt_unavailable"))
        self.assertEqual(receipt.read_bytes(), b"prior observation\n")
        self.assertEqual((self.github.calls, child.call_count), ([], 0))

    def test_a_receipt_fault_never_lets_a_ready_check_pass(self):
        _, review_sha256 = self.ready()

        def fsync(descriptor: int) -> None:
            raise OSError(errno.EIO, "fixture fault")

        faults = {
            "write": lambda: patch.object(candidate_record_receipt, "write_all",
                                          side_effect=OSError(errno.EIO, "fixture fault")),
            "sync": lambda: patch.object(candidate_record_receipt, "os", SimpleNamespace(fsync=fsync, close=os.close)),
            "close": self.receipt_close_fault,
        }
        for label, fault in faults.items():
            with self.subTest(label):
                with fault():
                    code, reported, _ = self.check(review_sha256)
                self.assertEqual((code, reported["disposition"], reported["ready"], reported["receipt_error"]),
                                 (1, "confirmed", True, "record_receipt_write_failed"))

    def test_usage_errors_exit_2_without_a_receipt(self):
        receipt = self.scratch / "usage-receipt.json"
        location = self.store.location
        store = ["--store-repository", location.repository, "--store-branch", location.branch,
                 "--store-genesis-commit", location.genesis_commit, "--store-marker-blob", location.marker_blob,
                 "--receipt", str(receipt)]
        reviewed = ["--candidate-id", self.records.id, "--review-sha256", "0" * 64,
                    "--repository-root", str(self.source.path)]
        cases = {"check with a work directory": ["check", *reviewed, *store, "--work-dir", str(self.work)],
                 "check without a checkout": ["check", *reviewed[:4], *store],
                 "check with a short digest": ["check", *reviewed[:3], "0" * 63, *reviewed[4:], *store],
                 "freeze without a work directory": ["freeze", *reviewed, *store]}
        for label, argv in cases.items():
            with self.subTest(label), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit) as caught:
                candidate_record.main(argv)
            self.assertEqual(caught.exception.code, 2)
            self.assertFalse(receipt.exists())
            self.assertEqual(self.github.calls, [])


class CoverageTests(CheckCase):
    """Attempt 1's Linux proof fails one Claude case; canonical readiness blocks it in every reviewed scope."""

    failing: proof_fixture.ReleaseProof

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        with contextlib.redirect_stderr(io.StringIO()):
            cls.failing = proof_fixture.release_proof(failed=True)

    def history(self, change: ChangeScope) -> tuple[str, str]:
        records = self.records
        claims = [records.claim(platform) for platform in PLANNED]
        proofs = [records.proof_file(claim) if claim.platform == DARWIN else self.failing_proof(claim)
                  for claim in claims]
        review = records.review(change)
        head = self.seed(records.register(), records.intent_file(records.intent(scope=change)),
                         *(records.claim_file(claim) for claim in claims), *proofs,
                         *(records.install_file(platform, channel) for platform in PLANNED
                           for channel in InstallChannel),
                         records.review_file(review))
        return head, sha(encode_review(review))

    def failing_proof(self, claim: PlatformClaim) -> Files:
        candidate = self.records.candidate
        facts = candidate_proof_expectation(candidate.manifest, candidate.locator, claim)
        return {name(EntryFamily.PROOF, attempt=claim.attempt, platform=claim.platform):
                proof_fixture.candidate_zip(self.failing, facts)}

    def assertBlockedByTheFailedCell(self, change: ChangeScope) -> None:
        head, review_sha256 = self.history(change)
        expected = self.canonical(review_sha256)
        self.assertNotReady(self.check(review_sha256), expected, head)
        failed = FindingRef(code="cell_failed", subject="Linux/amd64/case-02-claude-print-workspace-write-write")
        self.assertIn(failed, expected.findings)

    def test_the_failed_cell_blocks_a_scope_that_affects_its_provider(self):
        self.assertBlockedByTheFailedCell(ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS,
                                                      changed_providers=("claude",), provider_imports=()))

    def test_the_failed_cell_blocks_even_a_scope_that_does_not_require_it(self):
        others = tuple(family for family in PROVIDER_FAMILIES if family != "claude")
        self.assertBlockedByTheFailedCell(ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS,
                                                      changed_providers=others[:1], provider_imports=()))

    def test_the_failed_cell_blocks_the_final_version_full_matrix(self):
        self.assertBlockedByTheFailedCell(ChangeScope(matrix=MatrixScope.FINAL_VERSION, changed_providers=(),
                                                      provider_imports=()))


class BlockedVerdictForwardingTests(CheckCase):
    """UNIT command forwarding of BLOCKED cells: typed, prevalidated facts at the strict store port.

    Only ``load_candidate`` returns the canonical ``candidate_fixtures`` snapshot and only ``head_tree``
    reports that candidate's recorded source; the real command, receipt, review selection, clean check
    of a real checkout and ``evaluate_readiness`` run unchanged. Producing BLOCKED native proof ZIPs
    would copy compact-receipt policy, so no real ZIP store composition is claimed here.
    """

    CODEX_WRITE = fx.case_id("codex:exec", "write")

    def check_blocked(self, change: ChangeScope, reason: BlockReason) -> tuple[tuple[int, dict, str], Readiness]:
        candidate = fx.bound()
        cells = fx.cells(overrides={self.CODEX_WRITE: (CellStatus.BLOCKED, reason)})
        history = fx.history(candidate, intents=(fx.intent(candidate, change=change),),
                             proof_records=tuple(fx.proof(candidate, attempt=1, platform=platform, results=cells)
                                                 for platform in PLATFORMS))
        review = fx.review(candidate, change=change)
        snapshot = CandidateSnapshot(head=self.github.refs[BRANCH], candidate=candidate, history=history,
                                     reviews=(review,), freeze_commit=None)
        load = Mock(return_value=snapshot)
        with patch.object(candidate_record, "load_candidate", load), \
                patch.object(candidate_record, "head_tree", Mock(return_value=(fx.COMMIT, fx.TREE))):
            result = self.check(review.sha256, candidate_id=candidate.candidate_id)
        load.assert_called_once()
        self.assertEqual((load.call_args.args[1:], load.call_args.kwargs),
                         ((self.store.location, candidate.candidate_id), {"closure_resolutions": ()}))
        self.assertEqual(self.git_commands(), ["git-status"])
        self.assertEqual(self.document()["inputs"]["store_head"], snapshot.head)
        expected = evaluate_readiness(candidate=candidate, observed=fx.observed(), scope=change, history=history,
                                      review=review)
        return result, expected

    def assertBlocked(self, change: ChangeScope, reason: BlockReason, code: str) -> None:
        result, expected = self.check_blocked(change, reason)
        subjects = [f"{os_name}/{arch}/{self.CODEX_WRITE}" for os_name, arch in PLATFORMS]
        self.assertEqual(findings(expected), sorted(({"code": code, "subject": subject} for subject in subjects),
                                                    key=lambda item: item["subject"]))
        self.assertNotReady(result, expected, self.github.refs[BRANCH])

    def test_an_affected_blocked_cell_is_not_ready_whatever_its_reason(self):
        self.assertBlocked(fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("codex",)), BlockReason.AUTH_UNAVAILABLE,
                           "affected_cell_blocked")

    def test_an_unaffected_unavailable_block_is_neutral_in_the_final_version_matrix(self):
        for reason in (BlockReason.AUTH_UNAVAILABLE, BlockReason.EXECUTABLE_UNAVAILABLE):
            with self.subTest(reason=reason):
                (code, reported, _), expected = self.check_blocked(fx.scope(MatrixScope.FINAL_VERSION), reason)
                self.assertTrue(expected.ready, expected.findings)
                self.assertEqual((code, reported["disposition"], reported["ready"], reported["findings"]),
                                 (0, "confirmed", True, []))
                self.assertEqual([item["state"] for item in reported["attempt_verdicts"]], ["PASS"])
                self.assertEqual(reported["attempt_verdicts"], verdicts(expected))
                self.assertObservedOnly(reported, self.github.refs[BRANCH])

    def test_an_unclassified_block_is_not_ready_in_the_final_version_matrix(self):
        self.assertBlocked(fx.scope(MatrixScope.FINAL_VERSION), BlockReason.UNCLASSIFIED, "cell_blocked")


class CheckLifetimeTests(CheckCase):
    def own_tree(self, label: str) -> FixtureTree:
        tree = FixtureTree.open(prefix=f"candidate-record-check-{label}", label=f"candidate record check {label}")
        self.addCleanup(tree.dispose)
        # This test's own owner: its work directory, input and receipt, and its children's faults.
        self.tree = tree
        self.work, self.scratch = self.owned("work"), self.owned("scratch")
        return tree

    def test_an_original_unconverted_status_failure_retains_the_tree_until_zero_launches_are_proven(self):
        tree = self.own_tree("status-fault")
        head = self.seed(*self.records.ready())
        injected = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        observed: list[Path] = []

        def unconverted(receipt: dict, environment: dict[str, str], *, repository: Path, code: str) -> None:
            observed.append(repository)
            raise injected

        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with patch(f"{__name__}.require_clean", unconverted), \
                patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches, \
                patch.object(candidate_record, "head_tree", lambda *_, **__: (self.records.source.commit,
                                                                               self.records.source.tree)), \
                self.assertRaises(BoundedCommandError) as caught:
            self.check(sha(encode_review(self.records.review())))
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

    def test_a_converted_status_timeout_is_refused_and_still_retains_the_tree(self):
        tree = self.own_tree("status-timeout")
        head = self.seed(*self.records.ready())
        fault = ChildFault(lambda argv: argv[1:2] == ["status"])
        with patch.object(commands, "run_bounded_command", fault):
            code, reported, _ = self.check(sha(encode_review(self.records.review())))
        self.assertEqual((code, reported["disposition"], reported["code"], reported["stage"]),
                         (1, "refused", "command_timeout", "git-status"))
        self.assertEqual((tree.unresolved.code, tree.unresolved.stage), ("command_timeout", "git-status"))
        self.assertObservedOnly(reported, head)
        settle_retained(self, tree, fault, (self.work, self.scratch, self.receipt))


if __name__ == "__main__":
    unittest.main()
