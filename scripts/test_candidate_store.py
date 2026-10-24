"""Raw candidate store I/O: one immutable head, targeted history, refusal on doubt."""

from __future__ import annotations

import contextlib
import hashlib
import inspect
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.release_verification import candidate_store, github
from scripts.delegate_acceptance.release_verification.candidate_history import (
    MAX_PUBLICATION_BYTES, PublicationAuthorization, encode_publication,
)
from scripts.delegate_acceptance.release_verification.candidate_store import (
    _append_transaction, load_raw_candidate,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendAmbiguous, AppendConfirmed, RawCandidateSnapshot, RawTransaction, StoreBoundary, StoredBlob,
    StoreLocation, TransitionKind, TreeEntry, classify_transition, git_tree_sha,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha
from scripts.test_candidate_store_support import (
    NOT_FOUND, REPOSITORY, TIMEOUT, FakeGitHub, Fault, Store,
)


X = hashlib.sha256(b"candidate x").hexdigest()
Y = hashlib.sha256(b"candidate y").hexdigest()
REVIEW = f"review.{'1' * 64}.json"
# Blob contents and the candidate subtree SHA the GitHub API returned in the R3 run.
R3_ONE, R3_SUBTREE_A = b"synthetic version one\n", "9b36de21d0459ab59d6e3febbf0af94a1531db01"


def freeze_bytes() -> bytes:
    return encode_publication(PublicationAuthorization(
        candidate_id=X, attempt=1, evidence_parent="1" * 40, source_commit="2" * 40, source_tree="3" * 40,
        proof_sha256s=(Y,), install_sha256s=(Y,), triage_sha256s=(), review_sha256=Y))


def record(name: str, data: bytes) -> StoredBlob:
    return StoredBlob(TreeEntry(name, "100644", "blob", git_blob_sha(data)), data)


class StoreCase(unittest.TestCase):
    repository = REPOSITORY

    def setUp(self):
        self.github = FakeGitHub(repository=self.repository)
        self.store = Store(self.github)
        self.receipt: dict = {"commands": []}
        self.client = GitHubClient(self.receipt, {})
        transport = patch.object(github, "run_bounded_command", self.github)
        transport.start()
        self.addCleanup(transport.stop)

    def put(self, candidate: str, name: str, data: bytes, mode: str = "100644") -> None:
        self.store.put(f"candidates/{candidate}/{name}", data, mode)

    def register(self, candidate: str = X, manifest: bytes = b"manifest") -> str:
        self.put(candidate, "manifest.json", manifest)
        self.put(candidate, "locator.json", b"locator")
        return self.store.step("register " + candidate[:8])

    def load(self, candidate: str = X) -> RawCandidateSnapshot:
        return load_raw_candidate(self.client, self.store.location, candidate)

    def assertRefused(self, code: str, candidate: str = X) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.load(candidate)
        self.assertEqual(caught.exception.code, code)

    def requests(self, method: str = "GET", needle: str = "") -> list[str]:
        return [endpoint for verb, endpoint in self.github.calls if verb == method and needle in endpoint]


class RawValueTests(StoreCase):
    def test_fixture_objects_match_github_server_objects(self):
        self.assertEqual(self.github.build({"manifest.json": ("100644", self.github.blob(R3_ONE))}), R3_SUBTREE_A)

    def test_stored_blob_is_exact_bounded_and_codec_backed(self):
        blob = record("manifest.json", b"bytes")
        self.assertEqual(blob.sha256, hashlib.sha256(b"bytes").hexdigest())
        with self.assertRaisesRegex(VerificationError, "store_blob_mismatch"):
            StoredBlob(blob.entry, b"other")
        with self.assertRaisesRegex(VerificationError, "store_blob_too_large"):
            record("attempt-0001.claim.Linux-amd64.json", b"x" * (1 << 20))
        freeze = freeze_bytes()
        self.assertEqual(record("publication.json", freeze).data, freeze)
        self.assertEqual(len(record("publication.json", b" " * MAX_PUBLICATION_BYTES).data), MAX_PUBLICATION_BYTES)
        with self.assertRaisesRegex(VerificationError, "store_blob_too_large"):
            record("publication.json", b" " * (MAX_PUBLICATION_BYTES + 1))
        with self.assertRaises(VerificationError):
            StoredBlob(blob.entry, bytearray(b"bytes"))

    def test_transaction_carries_exactly_the_added_blobs(self):
        manifest, locator = record("manifest.json", b"m"), record("locator.json", b"l")
        transition = classify_transition(frozenset(), frozenset({manifest.entry, locator.entry}))
        tree = git_tree_sha(transition.added)
        RawTransaction("1" * 40, "2" * 40, None, tree, transition, (locator, manifest))
        for blobs in ((manifest, locator), (locator,), (locator, manifest, manifest)):
            with self.subTest(blobs=len(blobs)):
                with self.assertRaisesRegex(VerificationError, "store_transaction_invalid"):
                    RawTransaction("1" * 40, "2" * 40, None, tree, transition, blobs)

    def test_snapshot_chain_must_recompute_every_directory_tree(self):
        manifest, locator = record("manifest.json", b"m"), record("locator.json", b"l")
        transition = classify_transition(frozenset(), frozenset({manifest.entry, locator.entry}))
        tree = git_tree_sha(transition.added)
        boundary = StoreBoundary(42, (7,))
        good = RawTransaction("1" * 40, "2" * 40, None, tree, transition, (locator, manifest))
        RawCandidateSnapshot(self.store.location, X, "1" * 40, "3" * 40, boundary, (good,))
        for parent_tree, child_tree in ((tree, "4" * 40), (None, "4" * 40)):
            with self.subTest(parent_tree=parent_tree):
                bad = RawTransaction("1" * 40, "2" * 40, parent_tree, child_tree, transition, (locator, manifest))
                with self.assertRaisesRegex(VerificationError, "store_snapshot_invalid"):
                    RawCandidateSnapshot(self.store.location, X, "1" * 40, "3" * 40, boundary, (bad,))
        with self.assertRaisesRegex(VerificationError, "store_snapshot_invalid"):
            RawCandidateSnapshot(self.store.location, X, "1" * 40, "3" * 40, boundary, (good, good))
        with self.assertRaisesRegex(VerificationError, "store_candidate_id_invalid"):
            RawCandidateSnapshot(self.store.location, "x", "1" * 40, "3" * 40, boundary, ())

    def test_boundary_requires_positive_sorted_ruleset_identities(self):
        for repository_id, rulesets in ((42, ()), (0, (7,)), (True, (7,)), (42, (8, 7)), (42, (7, 7)),
                                        (42, [7])):
            with self.subTest(repository_id=repository_id, rulesets=rulesets):
                with self.assertRaisesRegex(VerificationError, "store_boundary_invalid"):
                    StoreBoundary(repository_id, rulesets)


class AbsenceTests(StoreCase):
    def test_complete_empty_history_behind_positive_root_checks_is_absence(self):
        self.register(Y)
        snapshot = self.load()
        self.assertEqual((snapshot.head, snapshot.transactions, snapshot.entries), (self.store.head, (), frozenset()))
        self.assertEqual(snapshot.boundary, StoreBoundary(42, (7,)))
        self.assertEqual(self.requests("GET", "/git/blobs/"), [])
        candidates = self.github.resolve(f"{self.store.head}:candidates")
        self.assertEqual(self.requests("GET", candidates), [], "no read lists every candidate")

    def test_absence_is_refused_when_the_root_could_fake_it(self):
        other = self.github.blob(b"other")
        for label, files in (("candidates blob", {"candidates": ("100644", other)}),
                             ("extra root entry", {"notes.json": ("100644", other)}),
                             ("candidates symlink", {"candidates": ("120000", other)})):
            with self.subTest(label=label):
                self.setUp()
                self.store.files.update(files)
                self.store.step(label)
                self.assertRefused("store_root_invalid")

    def test_no_failed_read_is_ever_absence(self):
        for needle, fault in (("/commits?", NOT_FOUND), ("/commits?", None), ("/git/ref/", NOT_FOUND),
                              ("/rules/branches/", NOT_FOUND), ("/git/trees/", NOT_FOUND),
                              ("/git/commits/", NOT_FOUND)):
            with self.subTest(needle=needle, fault=fault):
                self.setUp()
                self.register(Y)
                kind = Fault("GET", needle, result=fault, error=None if fault else TIMEOUT)
                self.github.faults.append(kind)
                with self.assertRaises(VerificationError) as caught:
                    self.load()
                expected = "github_request_failed" if fault else "command_timeout"
                self.assertEqual(caught.exception.code, expected)


class HistoryTests(StoreCase):
    def build_history(self) -> list[str]:
        """X appends interleaved with Y commits on one branch; returns X's commits, oldest first."""
        commits = [self.register(X)]
        self.register(Y)
        self.put(X, "attempt-0001.intent.json", b"intent")
        commits.append(self.store.step("x intent"))
        self.put(Y, "attempt-0001.intent.json", b"y intent")
        self.store.step("y intent")
        self.store.step("unrelated empty commit")
        self.put(X, "attempt-0001.claim.Linux-amd64.json", b"claim")
        commits.append(self.store.step("x claim"))
        self.put(X, REVIEW, b"locator")
        commits.append(self.store.step("x review reuses identical bytes"))
        self.put(Y, REVIEW, b"y review")
        self.store.step("y review")
        return commits

    def test_paged_history_is_replayed_by_parent_edges(self):
        commits = self.build_history()
        with patch.object(candidate_store, "HISTORY_PAGE_SIZE", 2):
            snapshot = self.load()
        self.assertEqual([item.commit for item in snapshot.transactions], commits)
        self.assertEqual([item.transition.kind for item in snapshot.transactions],
                         [TransitionKind.REGISTER, TransitionKind.INTENT, TransitionKind.CLAIM,
                          TransitionKind.REVIEW])
        self.assertEqual(snapshot.transactions[0].parent, self.store.genesis)
        self.assertIsNone(snapshot.transactions[0].parent_tree)
        self.assertEqual(self.github.resolve(f"{self.store.head}:candidates/{X}"), snapshot.transactions[-1].tree)
        self.assertEqual({blob.entry.name: blob.data for item in snapshot.transactions for blob in item.blobs}[REVIEW],
                         b"locator")
        self.assertEqual(len(self.requests("GET", "/commits?")), 4, "X pages 1-3 (last empty) plus marker")
        fetched = self.requests("GET", "/git/blobs/")
        self.assertEqual(len(fetched), len(set(fetched)), "each distinct blob is fetched once per load")
        self.assertEqual(len(fetched), 4)

    def test_each_load_reads_its_own_bytes_without_a_persisted_cache(self):
        self.build_history()
        self.load()
        first = len(self.requests("GET", "/git/blobs/"))
        self.load()
        self.assertEqual(len(self.requests("GET", "/git/blobs/")), 2 * first)

    def test_candidate_bound_counts_only_this_candidates_commits(self):
        self.register(X)
        self.register(Y)
        for index in range(1, 6):
            self.put(Y, f"attempt-{index:04d}.intent.json", b"y%d" % index)
            self.store.step("y")
        with patch.object(candidate_store, "MAX_CANDIDATE_COMMITS", 2), \
                patch.object(candidate_store, "HISTORY_PAGE_SIZE", 2):
            self.assertEqual(len(self.load().transactions), 1)
            self.assertRefused("store_history_too_long", Y)

    def test_reordered_duplicate_or_overfull_listing_refuses(self):
        commits = self.build_history()
        listings = {
            "store_history_discontinuous": [{"sha": sha, "parents": [{"sha": self.github.commits[sha].parents[0]}]}
                                            for sha in (commits[1], commits[3], commits[2], commits[0])],
            "store_history_duplicate": [{"sha": commits[0], "parents": [{"sha": self.store.genesis}]}] * 2,
            "store_history_invalid": [{"sha": commits[0], "parents": [{"sha": self.store.genesis}]}] * 101,
        }
        for code, rows in listings.items():
            with self.subTest(code=code):
                self.github.calls.clear()
                body = BoundedCommandResult(0, json.dumps(rows).encode())
                self.github.faults.append(Fault("GET", f"path=candidates%2F{X}", result=body))
                with self.assertRaises(VerificationError) as caught:
                    self.load()
                self.assertIn(caught.exception.code, {code, "store_entry_rewritten", "store_transition_invalid"}
                              if code == "store_history_discontinuous" else {code})

    def test_a_listing_that_omits_commits_breaks_tree_continuity(self):
        register = self.register(X)
        self.put(X, "attempt-0001.intent.json", b"intent")
        hidden = self.store.step("intent, later deleted")
        del self.store.files[f"candidates/{X}/attempt-0001.intent.json"]
        self.put(X, "attempt-0001.claim.Linux-amd64.json", b"claim")
        claim = self.store.step("delete intent and add a claim")
        self.register(Y)

        def listed(*commits: str) -> Fault:
            rows = [{"sha": sha, "parents": [{"sha": self.github.commits[sha].parents[0]}]}
                    for sha in reversed(commits)]
            return Fault("GET", f"path=candidates%2F{X}", result=BoundedCommandResult(0, json.dumps(rows).encode()))

        for label, fault in (("middle", listed(register, claim)), ("newest", listed(register, hidden))):
            with self.subTest(omitted=label):
                self.github.faults[:] = [fault]
                self.assertRefused("store_history_discontinuous")

    def test_merge_in_candidate_history_refuses(self):
        self.register(X)
        side = self.store.head
        self.put(X, "attempt-0001.intent.json", b"intent")
        self.store.step("merge", parents=(side, self.store.genesis))
        self.assertRefused("store_commit_not_linear")

    def test_unrelated_malformed_sibling_does_not_block_a_valid_candidate(self):
        for code, name, mode in (("store_entry_name_invalid", "bad name.json", "100644"),
                                 ("store_entry_invalid", "manifest.json", "100755")):
            with self.subTest(code=code):
                self.setUp()
                self.register(X)
                self.put(Y, name, b"bad", mode)
                self.store.step("malformed y")
                self.assertEqual(len(self.load().transactions), 1)
                self.assertRefused(code, Y)


class TamperTests(StoreCase):
    def test_genesis_root_must_hold_the_marker_alone(self):
        self.store.put("extra.json", b"extra")
        self.store.genesis = self.github.commit(self.github.build(self.store.files), (), "genesis with extra")
        self.store.head = self.store.genesis
        del self.store.files["extra.json"]
        self.register(X)
        self.assertRefused("store_genesis_mismatch")

    def test_marker_or_genesis_rewrites_refuse(self):
        def rewrite(store):
            store.put("STORE.json", b"changed")

        def delete_restore(store):
            del store.files["STORE.json"]
            store.step("delete marker")
            store.files["STORE.json"] = ("100644", store.marker)

        for label, change in (("rewrite", rewrite), ("delete-restore", delete_restore)):
            with self.subTest(label=label):
                self.setUp()
                self.register(X)
                change(self.store)
                self.store.step(label)
                self.assertRefused("store_genesis_mismatch")
        self.setUp()
        self.register(X)
        location = self.store.location
        for genesis in (self.store.head, "1" * 40):
            with self.subTest(genesis=genesis):
                other = StoreLocation(location.repository, location.branch, genesis, location.marker_blob)
                with self.assertRaisesRegex(VerificationError, "store_genesis_mismatch"):
                    load_raw_candidate(self.client, other, X)

    def test_rewrite_mode_change_rename_or_delete_then_restore_refuses(self):
        def rewrite_restore():
            self.put(X, "locator.json", b"rewritten")
            self.store.step("rewrite")
            self.put(X, "locator.json", b"locator")

        def mode_restore():
            self.put(X, "locator.json", b"locator", "100755")
            self.store.step("mode")
            self.put(X, "locator.json", b"locator")

        def rename():
            self.store.files[f"candidates/{X}/{REVIEW}"] = self.store.files.pop(f"candidates/{X}/locator.json")

        def delete_restore():
            saved = {path: value for path, value in self.store.files.items() if path.startswith("candidates/")}
            for path in saved:
                del self.store.files[path]
            self.store.step("delete")
            self.store.files.update(saved)

        for change, codes in ((rewrite_restore, {"store_entry_rewritten"}), (mode_restore, {"store_entry_invalid"}),
                              (rename, {"store_entry_rewritten"}), (delete_restore, {"github_request_failed"})):
            with self.subTest(change=change.__name__):
                self.setUp()
                self.register(X)
                change()
                self.store.step(change.__name__)
                with self.assertRaises(VerificationError) as caught:
                    self.load()
                self.assertIn(caught.exception.code, codes)

    def test_root_of_the_first_parent_must_pass_the_root_check(self):
        self.store.put("candidates", b"not a directory")
        self.store.step("candidates blob")
        del self.store.files["candidates"]
        self.register(X)
        self.assertRefused("store_root_invalid")

    def test_object_identity_size_and_truncation_mismatches_refuse(self):
        self.register(X)
        manifest = self.github.blob(b"manifest")
        directory = self.github.resolve(f"{self.store.head}:candidates/{X}")
        replies = {
            "store_blob_mismatch": ("/git/blobs/" + manifest,
                                    {"sha": manifest, "size": 8, "content": "bWFuaWZlc1Q=", "encoding": "base64"}),
            "store_blob_invalid": ("/git/blobs/" + manifest,
                                   {"sha": manifest, "size": 9, "content": "bWFuaWZlc3Q=", "encoding": "base64"}),
            "store_tree_mismatch": (f"%3Acandidates%2F{X}",
                                    {"sha": directory, "truncated": False, "tree": [
                                        {"path": "manifest.json", "mode": "100644", "type": "blob", "sha": manifest}]}),
            "store_tree_truncated": (f"%3Acandidates%2F{X}", {**self.github.tree_reply(directory), "truncated": True}),
        }
        for code, (needle, reply) in replies.items():
            with self.subTest(code=code):
                body = BoundedCommandResult(0, json.dumps(reply).encode())
                self.github.faults.append(Fault("GET", needle, result=body))
                self.assertRefused(code)
                self.github.faults.clear()


class PublicationBytesTests(StoreCase):
    """A stored publication is read as bounded bytes; the snapshot never becomes publication authority."""

    def freeze(self, data: bytes, candidate: str = X) -> str:
        self.register(candidate)
        self.put(candidate, "publication.json", data)
        return self.store.step("freeze " + candidate[:8])

    def test_canonical_publication_bytes_load_as_a_freeze_transaction_fetched_once(self):
        data = freeze_bytes()
        commit = self.freeze(data)
        snapshot = self.load()
        self.assertIsInstance(snapshot, RawCandidateSnapshot)
        freeze = snapshot.transactions[-1]
        self.assertEqual((freeze.commit, freeze.transition.kind), (commit, TransitionKind.FREEZE))
        self.assertEqual({blob.entry.name: blob.data for blob in freeze.blobs}, {"publication.json": data})
        self.assertEqual(len([endpoint for endpoint in self.requests("GET", "/git/blobs/")
                              if git_blob_sha(data) in endpoint]), 1)

    def test_the_owner_limit_loads_and_one_more_byte_refuses(self):
        exact = b" " * MAX_PUBLICATION_BYTES
        self.freeze(exact)
        self.assertEqual(self.load().transactions[-1].blobs[0].data, exact)
        self.freeze(exact + b" ", Y)
        self.assertRefused("store_blob_too_large", Y)

    def test_publication_json_that_is_not_a_freeze_record_is_still_only_bounded_bytes(self):
        # The raw reader checks identity and size, as for every family. It does not parse
        # the publication codec, so a loaded blob proves nothing about the freeze record.
        self.freeze(b"{}")
        self.assertEqual(self.load().transactions[-1].blobs[0].data, b"{}")

    def test_the_snapshot_carries_no_publication_or_readiness_decision(self):
        self.freeze(freeze_bytes())
        snapshot = self.load()
        self.assertEqual({field for field in snapshot.__dataclass_fields__},
                         {"location", "candidate_id", "head", "head_tree", "boundary", "transactions"})
        self.assertEqual({field for field in type(snapshot.transactions[-1]).__dataclass_fields__},
                         {"commit", "parent", "parent_tree", "tree", "transition", "blobs"})


class BoundaryTests(StoreCase):
    def test_repository_identity_and_no_bypass_append_only_rules_are_required(self):
        def renamed(github):
            github.repository_reply["full_name"] = "other/store"

        def missing_rule(github):
            github.rules.pop()

        def foreign_source(github):
            github.rules[0]["ruleset_source_type"] = "Organization"

        def mutator(key, value):
            return lambda github: github.rulesets[7].update({key: value})

        def hidden_bypass(github):
            del github.rulesets[7]["bypass_actors"]

        cases = {"store_repository_mismatch": [renamed],
                 "store_branch_rules_missing": [
                     missing_rule, foreign_source, hidden_bypass, mutator("enforcement", "evaluate"),
                     mutator("bypass_actors", [{"actor_id": 1, "actor_type": "RepositoryRole",
                                                "bypass_mode": "always"}]),
                     mutator("target", "tag")]}
        for code, changes in cases.items():
            for change in changes:
                with self.subTest(change=getattr(change, "__name__", code)):
                    self.setUp()
                    change(self.github)
                    self.assertRefused(code)


class WriterCase(StoreCase):
    """The fixture tree owns the writer's request files and observes its transport failures.

    No real child exists here: every launch is refused at the canonical process
    owner, and only that zero-launch oracle permits removing a retained tree.
    """

    def setUp(self):
        super().setUp()
        refusal = AssertionError("no child may launch in a synthetic transport test")
        launches = patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal))
        self.launches = launches.start()
        self.addCleanup(launches.stop)
        self.tree = FixtureTree.open(prefix="candidate-store-test", label="candidate store test fixture")
        self.addCleanup(self.settle, self.tree, self.launches)
        self.work = self.tree.root
        self.observed: list[VerificationError] = []

    def settle(self, tree: FixtureTree, launches: Mock) -> None:
        with contextlib.redirect_stderr(io.StringIO()):
            removed = tree.dispose()
        if not removed and tree.root.exists():
            self.assertEqual(launches.call_count, 0)
            tree.remove_retained()

    def observe(self, error: VerificationError) -> None:
        """The caller's observation: record what arrived, then let the tree owner classify it."""
        self.observed.append(error)
        self.tree.observe(error)

    def append(self, snapshot, *blobs: StoredBlob):
        return _append_transaction(self.client, self.store.location, snapshot, blobs, work_directory=self.work,
                                   observe_failure=self.observe)

    def registration(self) -> tuple[StoredBlob, StoredBlob]:
        return record("manifest.json", b"manifest"), record("locator.json", b"locator")


class WriterTests(WriterCase):
    def test_confirmed_append_is_one_nonced_commit_on_the_read_head(self):
        self.register(Y)
        snapshot = self.load()
        self.github.calls.clear()
        outcome = self.append(snapshot, *self.registration())
        self.assertIsInstance(outcome, AppendConfirmed)
        self.assertEqual((outcome.parent, outcome.kind), (snapshot.head, TransitionKind.REGISTER))
        self.assertEqual(outcome.record_sha256s, tuple(sorted(blob.sha256 for blob in self.registration())))
        self.assertEqual(self.github.refs[self.github.branch], outcome.commit)
        self.assertRegex(self.github.commits[outcome.commit].message,
                         rf"\AAppend register to development candidate {X}\n\nStore-Nonce: [0-9a-f]{{64}}\Z")
        self.assertEqual(len(self.requests("PATCH")), 1)
        self.assertEqual(self.requests("GET", "/git/ref/"), [], "an acknowledgement is not read back")
        after = self.load()
        self.assertEqual([item.commit for item in after.transactions], [outcome.commit])
        self.assertEqual(len(self.load(Y).transactions), 1, "the sibling directory is preserved")
        self.assertEqual(len(list(self.work.iterdir())), 5, "request bodies stay with the caller")

    def test_identical_appends_from_one_head_get_distinct_commits_and_one_winner(self):
        snapshot = self.load()
        first = self.append(snapshot, *self.registration())
        second = self.append(snapshot, *self.registration())
        self.assertIsInstance(first, AppendConfirmed)
        self.assertIsInstance(second, AppendAmbiguous)
        self.assertNotEqual(first.commit, second.proposed)
        self.assertNotEqual(self.github.commits[first.commit].message, self.github.commits[second.proposed].message)
        self.assertEqual((second.failure, len(self.requests("PATCH"))), ("github_request_failed", 2))
        self.assertEqual(self.github.refs[self.github.branch], first.commit)

    def test_lost_or_mismatched_acknowledgement_is_ambiguous_and_never_retried(self):
        faults = {
            "github_request_failed": Fault("PATCH", "/git/refs/", result=NOT_FOUND, apply=True),
            "command_timeout": Fault("PATCH", "/git/refs/", apply=True,
                                     error=BoundedCommandError(CommandFailure.TIMEOUT)),
            "store_append_ack_mismatch": Fault("PATCH", "/git/refs/", apply=True, result=BoundedCommandResult(
                0, b'{"ref":"refs/heads/development-evidence","object":{"sha":"' + b"1" * 40 + b'"}}')),
        }
        for code, fault in faults.items():
            with self.subTest(code=code):
                self.setUp()
                snapshot = self.load()
                self.github.faults.append(fault)
                self.github.calls.clear()
                outcome = self.append(snapshot, *self.registration())
                self.assertIsInstance(outcome, AppendAmbiguous)
                self.assertEqual((outcome.failure, outcome.parent), (code, snapshot.head))
                self.assertEqual(self.github.refs[self.github.branch], outcome.proposed, "the update did land")
                self.assertEqual(self.github.calls[-1][0], "PATCH", "no readback or retry follows the update")
                self.assertEqual(len(self.requests("PATCH")), 1)
                self.assertEqual(len(list(self.work.iterdir())), 5, "request bodies are retained")
                acknowledged = code == "store_append_ack_mismatch"
                self.assertEqual([error.code for error in self.observed], [] if acknowledged else [code])
                self.assertEqual(self.tree.unresolved is not None, code == "command_timeout")

    def test_unknown_wait_on_the_update_reaches_the_lifetime_owner_with_its_cause(self):
        snapshot = self.load()
        cause = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        self.github.faults.append(Fault("PATCH", "/git/refs/", error=cause, apply=True))
        raised: list[VerificationError] = []
        send = self.client.json_body

        def transport(*arguments, **options):
            try:
                return send(*arguments, **options)
            except VerificationError as error:
                raised.append(error)
                raise

        self.github.calls.clear()
        with patch.object(self.client, "json_body", transport):
            outcome = _append_transaction(self.client, self.store.location, snapshot, self.registration(),
                                          work_directory=self.work, observe_failure=self.tree.observe)
        self.assertIsInstance(outcome, AppendAmbiguous)
        self.assertEqual(outcome.failure, "command_termination_timeout")
        self.assertEqual(len(raised), 1)
        self.assertIs(self.tree.unresolved, raised[0], "the owner holds the transport's own exception")
        self.assertIs(raised[0].__cause__, cause)
        self.assertIsNone(self.receipt["commands"][-1]["exit_code"], "the child's Wait is not proven")
        self.assertEqual(self.requests("PATCH"),
                         [f"repos/{self.github.repository}/git/refs/heads/{self.github.branch}"])
        self.assertEqual(self.github.calls[-1][0], "PATCH", "no readback or retry follows the update")
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(self.tree.dispose())
        self.assertIn(f"(command_termination_timeout): {self.work}", stderr.getvalue())
        self.assertEqual(len(list(self.work.iterdir())), 5, "the owner retains every request body")
        self.assertEqual(self.launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(self.work.exists())

    def test_acknowledgements_are_not_failures_to_observe(self):
        self.assertIsInstance(self.append(self.load(), *self.registration()), AppendConfirmed)
        self.github.faults.append(Fault("PATCH", "/git/refs/", apply=True, result=BoundedCommandResult(
            0, b'{"ref":"refs/heads/development-evidence","object":{"sha":"' + b"1" * 40 + b'"}}')))
        intent = record("attempt-0001.intent.json", b"intent")
        self.assertIsInstance(self.append(self.load(), intent), AppendAmbiguous)
        self.assertEqual((self.observed, self.tree.unresolved), ([], None))

    def test_failures_before_the_update_raise_unobserved_and_unchanged(self):
        for method, needle in (("POST", "/git/blobs"), ("POST", "/git/trees"), ("POST", "/git/commits"),
                               ("GET", "/git/trees/")):
            with self.subTest(method=method, needle=needle):
                self.setUp()
                snapshot = self.load()
                cause = BoundedCommandError(CommandFailure.TIMEOUT)
                self.github.faults.append(Fault(method, needle, error=cause))
                with self.assertRaisesRegex(VerificationError, "command_timeout") as caught:
                    self.append(snapshot, *self.registration())
                self.assertIs(caught.exception.__cause__, cause)
                self.assertEqual(self.observed, [], "the raising caller observes it, not the writer")
                self.assertEqual(self.requests("PATCH"), [])
                self.assertEqual(self.github.refs[self.github.branch], snapshot.head)
                self.tree.observe(caught.exception)
                self.assertIs(self.tree.unresolved, caught.exception)

    def test_proposed_root_must_keep_the_store_layout(self):
        snapshot = self.load()
        manifest, locator = self.registration()
        files = dict(self.store.files)
        files.update({f"candidates/{X}/{blob.entry.name}": ("100644", self.github.blob(blob.data))
                      for blob in (manifest, locator)})
        files["extra.json"] = ("100644", self.github.blob(b"extra"))
        reply = BoundedCommandResult(0, json.dumps(self.github.tree_reply(self.github.build(files))).encode())
        self.github.faults.append(Fault("POST", "/git/trees", result=reply))
        with self.assertRaisesRegex(VerificationError, "store_root_invalid"):
            self.append(snapshot, manifest, locator)
        self.assertEqual(self.requests("PATCH"), [])

    def test_created_objects_must_match_the_proposed_identity(self):
        snapshot = self.load()
        wrong = BoundedCommandResult(0, b'{"sha":"' + b"1" * 40 + b'"}')
        for method, needle in (("POST", "/git/blobs"), ("POST", "/git/trees"), ("POST", "/git/commits")):
            with self.subTest(needle=needle):
                self.github.faults[:] = [Fault(method, needle, result=wrong)]
                with self.assertRaisesRegex(VerificationError, "store_append_object_mismatch|store_tree_"):
                    self.append(snapshot, *self.registration())
                self.assertEqual(self.requests("PATCH"), [])

    def test_a_created_commit_reporting_another_message_refuses_before_the_update(self):
        snapshot = self.load()
        write = self.github.write
        for label, alter in (("terminal LF", lambda message: message + "\n"),
                             ("other nonce", lambda message: message[:-1] + ("0" if message[-1] != "0" else "1"))):
            def reply(method: str, route: str, body: dict, alter=alter) -> object:
                created = write(method, route, body)
                return {**created, "message": alter(created["message"])} if route == "/git/commits" else created

            with self.subTest(altered=label), patch.object(self.github, "write", reply):
                with self.assertRaisesRegex(VerificationError, "store_append_object_mismatch"):
                    self.append(snapshot, *self.registration())
                self.assertEqual(self.requests("PATCH"), [])
                self.assertEqual(self.github.refs[self.github.branch], snapshot.head)

    def test_additions_that_are_not_one_new_group_refuse_before_any_request(self):
        self.register(X)
        snapshot = self.load()
        manifest, locator = self.registration()
        other = Store(FakeGitHub(repository="owner/other"))
        cases = {"store_transition_invalid": (snapshot, (locator,)),
                 "store_candidate_tree_invalid": (snapshot, (record("manifest.json", b"replacement"),)),
                 "store_request_invalid": (snapshot, ())}
        self.github.calls.clear()
        for code, (value, blobs) in cases.items():
            with self.subTest(code=code):
                with self.assertRaisesRegex(VerificationError, code):
                    self.append(value, *blobs)
        with self.assertRaisesRegex(VerificationError, "store_request_invalid"):
            _append_transaction(self.client, other.location, snapshot, (manifest,), work_directory=self.work,
                                observe_failure=self.observe)
        with self.assertRaisesRegex(VerificationError, "store_request_invalid"):
            _append_transaction(self.client, self.store.location, snapshot, (manifest,),
                                work_directory=self.work, observe_failure=None)
        self.assertEqual(self.github.calls, [])

    def test_the_only_public_append_admits_on_a_head_it_reads_itself(self):
        public = {name for name, value in vars(candidate_store).items()
                  if inspect.isfunction(value) and value.__module__ == candidate_store.__name__
                  and not name.startswith("_")}
        self.assertEqual(public, {"load_raw_candidate", "load_candidate", "append", "load_candidate_history",
                                  "load_candidate_at_head", "load_candidate_at_branch_head"})
        parameters = inspect.signature(candidate_store.append).parameters
        self.assertEqual(list(parameters), ["client", "location", "candidate_id", "additions", "closure_resolutions",
                                            "work_directory", "observe_failure"])
        self.assertFalse({"launch", "approved", "ready"} & set(AppendConfirmed.__dataclass_fields__))


if __name__ == "__main__":
    unittest.main()
