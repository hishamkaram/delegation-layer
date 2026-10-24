"""The source-review writer and its maintainer command over a real Git source and the fake store.

The source is a real temporary Git repository read by the canonical observer.
The store is the in-memory ``FakeGitHub`` behind the real ``GitHubClient``; it
proves the composed request flow, not GitHub itself. Every record comes from
its canonical encoder. Constants are test inputs, not evidence about the real
repository, a provider or a reviewer.
"""

from __future__ import annotations

import contextlib
from dataclasses import replace
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

from scripts import record_candidate_review
from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.contract import canonical_json
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.release_verification import (
    candidate_record_receipt, candidate_review_record, candidate_store, candidate_store_contract,
    candidate_store_replay, commands, github,
)
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    PROVIDER_FAMILIES, ChangeScope, MatrixScope, ReviewedSource, ReviewProvenance, SourceReview, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_build_source import child_environment
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ResourceDisposition,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    SourceIdentity, encode_locator, encode_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    SourceObservation, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.candidate_review_record import record_source_review
from scripts.delegate_acceptance.release_verification.candidate_source_inventory import (
    REGISTRY_PATH, ObservedSourceChange, reviewed_change_scope,
)
from scripts.delegate_acceptance.release_verification.candidate_source_observation import observe_source_change
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendAmbiguous, AppendConfirmed, TransitionKind,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.test_candidate_source_support import ChildFault, SourceRepository, settle_retained
from scripts.test_candidate_store_support import BRANCH, REFUSED, FakeGitHub, Fault, Store


FRAGMENT = "agents/_data/subsystems/fixture.json"
REGISTRY = json.dumps({"version": 1, "module_files": [FRAGMENT]}).encode() + b"\n"
BASE_FILES = {"keep.txt": b"keep\n", REGISTRY_PATH: REGISTRY, FRAGMENT: b"[]\n"}
# Base to head edits one file and adds one: a two-path inventory, written out.
HEAD_FILES = {"keep.txt": b"kept and changed\n", "added.txt": b"added\n"}
HEAD_CHANGED_PATHS = 2
RATIONALE = "Fixture review: only shared release tooling changed."
SHARED = ChangeScope(matrix=MatrixScope.SHARED, changed_providers=(), provider_imports=())
WRITE_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})
TOKEN = "fixture-token-" + "z" * 24
# Inherited by any Git child that leaked the caller environment, where it would break every read.
LEAKED_GIT = {"GIT_DIR": "/nonexistent/fixture-git-dir", "GIT_CONFIG_PARAMETERS": "'fixture.broken"}


def observe_source(tree: FixtureTree, receipt: dict, environment: dict[str, str], *, repository: Path,
                   source: SourceIdentity) -> ObservedSourceChange:
    """The canonical observer; its original child failure reaches the tree before any caller converts it."""
    return tree.observe_operation(
        lambda: observe_source_change(receipt, environment, repository=repository, source=source))


class ReviewCase(unittest.TestCase):
    """One registered candidate whose manifest pins the fixture head; the tree owns every child."""

    tree: FixtureTree
    source: SourceRepository
    foreign: SourceRepository
    base: str
    head: str
    later: str
    review: SourceReview

    @classmethod
    def setUpClass(cls):
        cls.tree = FixtureTree.open(prefix="candidate-review-record", label="candidate review record fixture")
        cls.addClassCleanup(cls.tree.dispose)
        cls.source = SourceRepository(cls.tree, "source")
        cls.base = cls.source.commit(BASE_FILES, "base")
        cls.head = cls.source.commit(HEAD_FILES, "head")
        cls.later = cls.source.commit({"later.txt": b"later\n"}, "later")
        # A repository holding none of the source objects.
        cls.foreign = SourceRepository(cls.tree, "foreign")
        cls.foreign.commit({"foreign.txt": b"foreign\n"}, "foreign")
        cls.manifest = replace(fx.manifest(), source=SourceIdentity(
            commit=cls.head, tree=cls.source.tree_of(cls.head), base_commit=cls.base))
        cls.locator = fx.locator_for(cls.manifest)
        observed = observe_source(cls.tree, {"commands": []}, child_environment(os.environ),
                                  repository=cls.source.path, source=cls.manifest.source)
        cls.review = SourceReview(
            candidate_id=cls.locator.candidate_id, head_commit=cls.head,
            tree=cls.manifest.source.tree, reviewer=fx.REVIEWER, unresolved_findings=(),
            reviewed_scope=reviewed_change_scope(observed, scope=SHARED, rationale=RATIONALE))

    def setUp(self):
        self.github = FakeGitHub(repository=fx.REPOSITORY)
        self.store = Store(self.github)
        transport = patch.object(github, "run_bounded_command", self.github)
        transport.start()
        self.addCleanup(transport.stop)
        # Read at call time, so a test that owns a separate tree receives its own children's faults.
        observer = patch.object(candidate_review_record, "observe_source_change",
                                lambda receipt, environment, **sources: observe_source(
                                    self.tree, receipt, environment, **sources))
        observer.start()
        self.addCleanup(observer.stop)
        self.runner = run_bounded_command
        source_runner = patch.object(commands, "run_bounded_command", self.run_child)
        source_runner.start()
        self.addCleanup(source_runner.stop)
        self.client = GitHubClient({"commands": []}, {})
        directory = f"candidates/{self.locator.candidate_id}"
        self.store.put(f"{directory}/manifest.json", encode_manifest(self.manifest))
        self.store.put(f"{directory}/locator.json", encode_locator(self.locator))
        self.registered = self.store.step("register")
        self.work = self.owned("work")
        self.observed: list[VerificationError] = []

    def run_child(self, argv: list[str], environment: dict[str, str], **options: object) -> BoundedCommandResult:
        """The source children's runner; its original failure reaches the tree before the command converts it."""
        return self.tree.observe_operation(lambda: self.runner(argv, environment, **options))

    def owned(self, label: str) -> Path:
        """A new directory inside the fixture tree, which owns and disposes of it."""
        path = self.tree.root / f"{label}-{secrets.token_hex(6)}"
        path.mkdir()
        return path

    def observe(self, error: VerificationError) -> None:
        """The caller's observation: record what arrived, then let the tree owner classify it."""
        self.observed.append(error)
        self.tree.observe(error)

    def record(self, review: object, *, closures: tuple = (), repository: Path | None = None):
        return self.tree.observe_operation(lambda: record_source_review(
            self.client, self.store.location, review, repository=repository or self.source.path,
            environment=os.environ, work_directory=self.work, closure_resolutions=closures,
            observe_failure=self.observe))

    def writes(self) -> list[tuple[str, str]]:
        return [call for call in self.github.calls if call[0] in WRITE_METHODS]

    def git_commands(self) -> list[str]:
        return [item["name"] for item in self.client.receipt["commands"] if item["name"].startswith("git-")]

    def assertRefusedBeforeMutation(self, code: str, review: object, **options) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.record(review, **options)
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(self.writes(), [])
        self.assertEqual(self.github.refs[BRANCH], self.registered)
        self.assertEqual(list(self.work.iterdir()), [])
        return caught.exception

    def stored_review(self, commit: str, review: SourceReview) -> bytes:
        files = self.github.flatten(self.github.commits[commit].tree)
        path = f"candidates/{self.locator.candidate_id}/review.{sha(encode_review(review))}.json"
        return self.github.blobs[files[path][1]]

    def readback(self):
        return candidate_store.load_candidate(self.client, self.store.location, self.locator.candidate_id,
                                              closure_resolutions=())

    def calls_after_patch(self) -> list[tuple[str, str]]:
        index = next(i for i, (method, _) in enumerate(self.github.calls) if method == "PATCH")
        return self.github.calls[index + 1:]


class WriterTests(ReviewCase):
    def test_confirmed_review_binds_observed_source_and_reads_back_with_provenance(self):
        closures = ()
        with patch.object(candidate_review_record, "append", wraps=candidate_store.append) as appended, \
                patch.object(candidate_review_record, "load_candidate",
                             wraps=candidate_store.load_candidate) as loaded:
            outcome = self.record(self.review, closures=closures)
        digest = sha(encode_review(self.review))
        self.assertIsInstance(outcome, AppendConfirmed)
        self.assertEqual((outcome.parent, outcome.kind, outcome.record_sha256s),
                         (self.registered, TransitionKind.REVIEW, (digest,)))
        self.assertEqual(self.github.refs[BRANCH], outcome.commit)
        self.assertEqual(appended.call_count, 1)
        self.assertIs(appended.call_args.kwargs["closure_resolutions"], closures)
        self.assertIs(loaded.call_args.kwargs["closure_resolutions"], closures)
        self.assertEqual([method for method, _ in self.github.calls].count("PATCH"), 1)
        self.assertEqual(self.observed, [])
        self.assertEqual(self.review.reviewed_scope.changed_path_count, HEAD_CHANGED_PATHS)
        self.assertIn("git-source-changed-paths", self.git_commands())
        self.assertEqual(self.stored_review(outcome.commit, self.review), encode_review(self.review))
        snapshot = self.readback()
        provenance = ReviewProvenance(artifact_sha256=digest, reviewer=fx.REVIEWER,
                                      transaction_commit=outcome.commit)
        self.assertEqual(snapshot.reviews, (ReviewedSource(record=self.review, provenance=provenance),))
        source = self.manifest.source
        readiness = evaluate_readiness(candidate=snapshot.candidate,
                                       observed=SourceObservation(head_commit=source.commit, tree=source.tree),
                                       scope=SHARED, history=snapshot.history, review=snapshot.reviews[0])
        codes = {finding.code for finding in readiness.findings}
        self.assertFalse(readiness.ready)
        self.assertEqual({code for code in codes if code.startswith("review_")}, set())
        self.assertIn("attempt_missing", codes)

    def test_submitted_scope_and_findings_are_stored_unchanged_without_approval(self):
        affected = ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS, changed_providers=(PROVIDER_FAMILIES[0],),
                               provider_imports=())
        review = replace(self.review, unresolved_findings=("fixture-finding",),
                         reviewed_scope=replace(self.review.reviewed_scope, scope=affected))
        outcome = self.record(review)
        self.assertIsInstance(outcome, AppendConfirmed)
        self.assertEqual(self.stored_review(outcome.commit, review), encode_review(review))
        snapshot = self.readback()
        self.assertEqual([item.record for item in snapshot.reviews], [review])
        source = self.manifest.source
        for scope, expected in ((affected, {"review_findings_unresolved"}),
                                (SHARED, {"review_findings_unresolved", "review_scope_mismatch"})):
            with self.subTest(scope=scope.matrix):
                readiness = evaluate_readiness(
                    candidate=snapshot.candidate, observed=SourceObservation(source.commit, source.tree),
                    scope=scope, history=snapshot.history, review=snapshot.reviews[0])
                self.assertFalse(readiness.ready)
                self.assertEqual({item.code for item in readiness.findings if item.code.startswith("review_")},
                                 expected)

    def test_stale_or_wrong_bindings_refuse_before_any_store_mutation(self):
        source, reviewed = self.source, self.review.reviewed_scope
        cases = {
            "stale head": (replace(self.review, head_commit=self.later), "review_binding_mismatch"),
            "wrong tree": (replace(self.review, tree=source.tree_of(self.later)), "review_binding_mismatch"),
            "wrong base": (replace(self.review, reviewed_scope=replace(reviewed, base_commit=self.head)),
                           "review_baseline_mismatch"),
            "wrong base tree": (replace(self.review, reviewed_scope=replace(
                reviewed, base_tree=source.tree_of(self.head))), "review_baseline_mismatch"),
            "changed count": (replace(self.review, reviewed_scope=replace(
                reviewed, changed_path_count=HEAD_CHANGED_PATHS + 1)), "review_change_mismatch"),
            "changed inventory": (replace(self.review, reviewed_scope=replace(
                reviewed, changed_paths_sha256="0" * 64)), "review_change_mismatch"),
            "registry": (replace(self.review, reviewed_scope=replace(
                reviewed, subsystem_registry_sha256="1" * 64)), "review_change_mismatch"),
        }
        for label, (review, code) in cases.items():
            with self.subTest(label):
                self.github.calls.clear()
                self.client.receipt["commands"].clear()
                self.assertRefusedBeforeMutation(code, review)
                self.assertIn("git-source-changed-paths", self.git_commands())

    def test_repository_without_the_manifest_source_refuses_before_any_store_mutation(self):
        error = self.assertRefusedBeforeMutation("command_failed", self.review, repository=self.foreign.path)
        self.assertEqual(error.stage, "git-source-commit")

    def test_unregistered_candidate_refuses_without_git(self):
        self.assertRefusedBeforeMutation("candidate_not_registered", replace(self.review, candidate_id="9" * 64))
        self.assertEqual(self.git_commands(), [])

    def test_malformed_review_value_refuses_before_store_or_git(self):
        self.assertRefusedBeforeMutation("candidate_review_invalid", encode_review(self.review))
        self.assertEqual(self.github.calls, [])
        self.assertEqual(self.client.receipt["commands"], [])

    def test_unavailable_closure_facts_refuse_honestly_before_git(self):
        fact = fx.resolution(ClaimSubject("1" * 64), invocation=fx.invocation_id(1, ("linux", "amd64")),
                             resources=((NATIVE_CHILD_RESOURCE, ResourceDisposition.WAITED),))
        error = self.assertRefusedBeforeMutation("store_closure_resolution_unused", self.review,
                                                 closures=(fact,))
        self.assertEqual(error.stage, candidate_store_replay.STAGE)
        self.assertEqual(self.git_commands(), [])

    def test_duplicate_review_is_refused_by_canonical_replay(self):
        first = self.record(self.review)
        self.assertIsInstance(first, AppendConfirmed)
        self.registered = first.commit
        self.github.calls.clear()
        self.work = self.owned("work-again")
        error = self.assertRefusedBeforeMutation("store_transition_invalid", self.review)
        self.assertEqual(error.stage, candidate_store_contract.STAGE)

    def test_failed_ref_update_is_ambiguous_observed_once_and_never_retried_or_read_back(self):
        self.github.faults.append(Fault("PATCH", f"/git/refs/heads/{BRANCH}", result=REFUSED, apply=True))
        outcome = self.record(self.review)
        self.assertIsInstance(outcome, AppendAmbiguous)
        self.assertEqual((outcome.parent, outcome.failure), (self.registered, "github_request_failed"))
        self.assertEqual([error.code for error in self.observed], ["github_request_failed"])
        self.assertEqual([method for method, _ in self.github.calls].count("PATCH"), 1)
        self.assertEqual(self.calls_after_patch(), [])
        # The update landed, yet only the lost acknowledgement is reported: nothing promotes it.
        self.assertEqual(self.github.refs[BRANCH], outcome.proposed)
        self.assertTrue(any(path.name.endswith("-ref.json") for path in self.work.iterdir()))

    def test_mismatched_acknowledgement_is_ambiguous_without_the_failure_callback(self):
        reply = json.dumps({"ref": "refs/heads/other", "object": {"sha": "0" * 40}}).encode()
        self.github.faults.append(Fault("PATCH", f"/git/refs/heads/{BRANCH}",
                                        result=BoundedCommandResult(0, reply), apply=True))
        outcome = self.record(self.review)
        self.assertIsInstance(outcome, AppendAmbiguous)
        self.assertEqual(outcome.failure, "store_append_ack_mismatch")
        self.assertEqual(self.observed, [])
        self.assertEqual([method for method, _ in self.github.calls].count("PATCH"), 1)
        self.assertEqual(self.calls_after_patch(), [])


class CommandTests(ReviewCase):
    """The maintainer command: one new receipt, sanitized outcomes and exit codes."""

    def setUp(self):
        super().setUp()
        self.scratch = self.owned("scratch")
        self.environments: list[dict[str, str]] = []

        def transport(argv: list[str], environment: dict[str, str], **options: object) -> BoundedCommandResult:
            self.environments.append(dict(environment))
            return self.github(argv, environment, **options)

        recorded = patch.object(github, "run_bounded_command", transport)
        recorded.start()
        self.addCleanup(recorded.stop)

    def argv(self, review: Path, receipt: Path) -> list[str]:
        location = self.store.location
        return ["--review-input", str(review), "--repository-root", str(self.source.path),
                "--store-repository", location.repository, "--store-branch", location.branch,
                "--store-genesis-commit", location.genesis_commit, "--store-marker-blob", location.marker_blob,
                "--work-dir", str(self.work), "--receipt", str(receipt)]

    def run_command(self, data: bytes | None = None, *, receipt: Path | None = None,
                    argv: list[str] | None = None) -> tuple[int, dict, str]:
        review = self.scratch / f"review-{secrets.token_hex(4)}.json"
        review.write_bytes(encode_review(self.review) if data is None else data)
        self.receipt = receipt or self.scratch / f"receipt-{secrets.token_hex(4)}.json"
        stdout, stderr = io.StringIO(), io.StringIO()
        leaked = {"GH_TOKEN": TOKEN, "GITHUB_TOKEN": TOKEN, **LEAKED_GIT}
        with patch.dict(os.environ, leaked), contextlib.redirect_stdout(stdout), \
                contextlib.redirect_stderr(stderr):
            code = candidate_review_record.main(argv or self.argv(review, self.receipt))
        self.assertNotIn(TOKEN, stdout.getvalue() + stderr.getvalue())
        return code, json.loads(stdout.getvalue()), stderr.getvalue()

    @contextlib.contextmanager
    def receipt_close_fault(self):
        """Fail only the reserved receipt's close, after really closing it; no other descriptor is touched.

        Yields every close the receipt owner attempted, so a test proves one close and no leak.
        """
        real_create, reserved, closed = candidate_record_receipt.create_new_file, [], []

        def create(path, code, *, stage):
            reserved.append(real_create(path, code, stage=stage))
            return reserved[-1]

        def close(descriptor):
            closed.append(descriptor)
            os.close(descriptor)
            if descriptor in reserved:
                raise OSError(errno.EIO, "fixture close fault")

        with patch.object(candidate_record_receipt, "create_new_file", create), \
                patch.object(candidate_record_receipt, "os", SimpleNamespace(fsync=os.fsync, close=close)):
            yield closed
        self.assertEqual(closed, reserved)
        with self.assertRaises(OSError):
            os.fstat(reserved[0])

    def receipt_document(self) -> dict:
        text = self.receipt.read_text()
        self.assertNotIn(TOKEN, text)
        return json.loads(text)

    def test_entrypoint_is_the_writer_command(self):
        self.assertIs(record_candidate_review.main, candidate_review_record.main)

    def test_command_confirms_the_review_and_writes_one_new_receipt(self):
        code, reported, _ = self.run_command()
        self.assertEqual(code, 0)
        self.assertEqual((reported["disposition"], reported["receipt_error"]), ("confirmed", None))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])
        document = self.receipt_document()
        self.assertEqual((document["kind"], document["outcome"]["commit"]),
                         (candidate_review_record.RECEIPT_KIND, reported["commit"]))
        reviewed = self.review.reviewed_scope
        self.assertEqual(document["review"], {
            "candidate_id": self.review.candidate_id, "review_sha256": sha(encode_review(self.review)),
            "reviewer": fx.REVIEWER, "head_commit": self.review.head_commit, "tree": self.review.tree,
            "base_commit": reviewed.base_commit, "base_tree": reviewed.base_tree,
            "changed_path_count": reviewed.changed_path_count,
            "changed_paths_sha256": reviewed.changed_paths_sha256,
            "subsystem_registry_sha256": reviewed.subsystem_registry_sha256})
        names = {item["name"] for item in document["commands"]}
        self.assertTrue({"github-api", "git-source-changed-paths"} <= names)
        self.assertTrue(self.environments)
        for environment in self.environments:
            self.assertFalse({"GH_TOKEN", "GITHUB_TOKEN"} & set(environment))
        self.assertEqual([item.provenance.transaction_commit for item in self.readback().reviews],
                         [reported["commit"]])

    def test_command_refuses_an_existing_receipt_before_any_read(self):
        receipt = self.scratch / "existing-receipt.json"
        receipt.write_bytes(b"prior observation\n")
        code, reported, _ = self.run_command(receipt=receipt)
        self.assertEqual((code, reported["code"]), (1, "review_receipt_unavailable"))
        self.assertEqual(receipt.read_bytes(), b"prior observation\n")
        self.assertEqual(self.github.calls, [])

    def test_command_refuses_malformed_review_input_before_git_or_store(self):
        canonical = encode_review(self.review)
        record = json.loads(canonical)
        non_ascii = {**record["reviewed_scope"], "rationale": "Fixture caf\N{LATIN SMALL LETTER E WITH ACUTE} review"}
        cases = {
            "not json": (b"{not json", "candidate_record_invalid"),
            "not canonical": (canonical.replace(b",", b", "), "candidate_record_not_canonical"),
            "non-ascii rationale": (canonical_json({**record, "reviewed_scope": non_ascii}),
                                    "candidate_review_invalid"),
            "unknown field": (canonical_json({**record, "approved": True}), "candidate_record_fields_invalid"),
        }
        for label, (data, expected) in cases.items():
            with self.subTest(label):
                code, reported, _ = self.run_command(data)
                self.assertEqual((code, reported["disposition"], reported["code"]), (1, "refused", expected))
                document = self.receipt_document()
                self.assertEqual((document["review"], document["commands"]), (None, []))
                self.assertEqual(self.github.calls, [])

    def test_command_reports_canonical_refusal_findings_unchanged(self):
        self.assertEqual(self.run_command()[0], 0)
        head = self.github.refs[BRANCH]
        self.github.calls.clear()
        self.work = self.owned("work-again")
        code, reported, _ = self.run_command()
        self.assertEqual((code, reported["disposition"], reported["code"]), (1, "refused", "store_transition_invalid"))
        self.assertEqual(self.receipt_document()["outcome"], {key: reported[key] for key in (
            "disposition", "code", "stage", "findings")})
        self.assertEqual(self.writes(), [])
        self.assertEqual(self.github.refs[BRANCH], head)

    def test_command_reports_an_ambiguous_append_and_keeps_the_work_directory(self):
        self.github.faults.append(Fault("PATCH", f"/git/refs/heads/{BRANCH}", result=REFUSED, apply=True))
        code, reported, stderr = self.run_command()
        self.assertEqual((code, reported["disposition"], reported["failure"]), (3, "ambiguous", "github_request_failed"))
        self.assertEqual(self.receipt_document()["outcome"]["proposed"], reported["proposed"])
        self.assertIn("store append unresolved: github_request_failed", stderr)
        self.assertEqual(self.calls_after_patch(), [])
        self.assertTrue(any(path.name.endswith("-ref.json") for path in self.work.iterdir()))

    def test_receipt_write_fault_after_confirmation_fails_without_relabelling(self):
        with patch.object(candidate_record_receipt, "write_all", side_effect=OSError(errno.EIO, "fixture fault")):
            code, reported, _ = self.run_command()
        self.assertEqual(code, 1)
        self.assertEqual((reported["disposition"], reported["receipt_error"]),
                         ("confirmed", "review_receipt_write_failed"))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])

    def test_receipt_close_fault_after_confirmation_keeps_the_commit_and_fails(self):
        with self.receipt_close_fault() as closed:
            code, reported, _ = self.run_command()
        self.assertEqual((code, len(closed)), (1, 1))
        self.assertEqual((reported["disposition"], reported["receipt_error"]),
                         ("confirmed", "review_receipt_write_failed"))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])
        self.assertEqual([method for method, _ in self.github.calls].count("PATCH"), 1)
        self.assertEqual(self.calls_after_patch(), [])
        # The bytes were written and synced before the close failed.
        self.assertEqual(self.receipt_document()["outcome"]["commit"], reported["commit"])

    def test_receipt_close_fault_after_an_ambiguous_append_still_reports_it(self):
        self.github.faults.append(Fault("PATCH", f"/git/refs/heads/{BRANCH}", result=REFUSED, apply=True))
        with self.receipt_close_fault() as closed:
            code, reported, _ = self.run_command()
        self.assertEqual((code, len(closed)), (3, 1))
        self.assertEqual((reported["disposition"], reported["failure"], reported["receipt_error"]),
                         ("ambiguous", "github_request_failed", "review_receipt_write_failed"))
        self.assertEqual(self.github.refs[BRANCH], reported["proposed"])
        self.assertEqual([method for method, _ in self.github.calls].count("PATCH"), 1)
        self.assertEqual(self.calls_after_patch(), [])
        self.assertEqual(self.receipt_document()["outcome"], {key: reported[key] for key in (
            "disposition", "parent", "proposed", "nonce", "record_sha256s", "failure")})

    def test_converted_observer_timeout_still_retains_the_tree(self):
        tree = FixtureTree.open(prefix="candidate-review-command-fault", label="candidate review command fault")
        self.addCleanup(tree.dispose)
        # This test's own owner: its work directory, input and receipt, and its children's faults.
        self.tree = tree
        self.work, self.scratch = self.owned("work"), self.owned("scratch")
        fault = ChildFault(lambda argv: argv[1:3] == ["rev-parse", "--verify"])
        with patch.object(commands, "run_bounded_command", fault):
            code, reported, _ = self.run_command()
        self.assertEqual((code, reported["disposition"], reported["code"], reported["stage"]),
                         (1, "refused", "command_timeout", "git-source-commit"))
        self.assertIsInstance(tree.unresolved, VerificationError)
        self.assertEqual((tree.unresolved.code, tree.unresolved.stage), ("command_timeout", "git-source-commit"))
        self.assertEqual(self.receipt_document()["outcome"]["code"], "command_timeout")
        self.assertEqual((self.writes(), fault.launched), ([], 0))
        settle_retained(self, tree, fault, (self.work, self.scratch, self.receipt))

    def test_original_runner_timeout_reaches_the_tree_before_the_command_converts_it(self):
        tree = FixtureTree.open(prefix="candidate-review-runner-fault", label="candidate review runner fault")
        self.addCleanup(tree.dispose)
        self.tree = tree
        self.work, self.scratch = self.owned("work"), self.owned("scratch")
        fault = ChildFault(lambda argv: argv[1:3] == ["rev-parse", "--verify"])
        self.runner = fault
        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches:
            code, reported, _ = self.run_command()
        self.assertEqual((code, reported["code"], reported["stage"]), (1, "command_timeout", "git-source-commit"))
        self.assertIsInstance(tree.unresolved, BoundedCommandError)
        self.assertIs(tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual((self.writes(), fault.launched, launches.call_count), ([], 0, 0))
        settle_retained(self, tree, fault, (self.work, self.scratch, self.receipt))

    def test_usage_error_exits_2_without_a_receipt(self):
        review, receipt = self.scratch / "unused.json", self.scratch / "usage-receipt.json"
        argv = self.argv(review, receipt)
        index = argv.index("--work-dir")
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            candidate_review_record.main(argv[:index] + argv[index + 2:])
        self.assertEqual(caught.exception.code, 2)
        self.assertFalse(receipt.exists())
        self.assertEqual(self.github.calls, [])



class SetupLifetimeTests(unittest.TestCase):
    def test_setup_observer_timeout_retains_the_class_tree_with_the_original_error(self):
        class Faulted(ReviewCase):
            """Runs the real class setup; unittest never collects this local class."""

        self.addCleanup(Faulted.doClassCleanups)
        fault = ChildFault(lambda argv: argv[1:3] == ["rev-parse", "--verify"])
        with patch.object(commands, "run_bounded_command", fault), self.assertRaises(VerificationError) as caught:
            Faulted.setUpClass()
        tree = Faulted.tree
        self.assertEqual((caught.exception.code, caught.exception.stage), ("command_timeout", "git-source-commit"))
        self.assertIs(tree.unresolved, caught.exception)
        with contextlib.redirect_stderr(io.StringIO()):
            Faulted.doClassCleanups()
        self.assertEqual(Faulted.tearDown_exceptions, [])
        self.assertGreater(fault.launched, 0)
        settle_retained(self, tree, fault, (Faulted.source.path, Faulted.foreign.path))


if __name__ == "__main__":
    unittest.main()
