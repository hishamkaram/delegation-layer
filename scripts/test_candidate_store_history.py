"""Candidate store reads at a supplied or once-resolved head: the strict reader's history, no boundary, no authority.

The store is the in-memory ``FakeGitHub`` behind the real ``GitHubClient``;
it proves request flow and refusals, not GitHub itself. The existing reader
and tamper cases rerun below through the history reader at the head the
branch names, and must refuse exactly as the strict reader does. The branch
head observation resolves that head once and then reads exactly what the
supplied-head read reads there.
"""

from __future__ import annotations

from dataclasses import replace
import inspect
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_store as store_cases
from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.release_verification import candidate_store
from scripts.delegate_acceptance.release_verification.candidate_attempts import encode_review
from scripts.delegate_acceptance.release_verification.candidate_history import encode_publication
from scripts.delegate_acceptance.release_verification.candidate_install_contract import InstallChannel
from scripts.delegate_acceptance.release_verification.candidate_readiness import authorize_publication
from scripts.delegate_acceptance.release_verification.candidate_store import (
    _append_transaction, load_candidate_at_branch_head, load_candidate_at_head, load_candidate_history,
    load_raw_candidate,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendConfirmed, CandidateHistoryAtHead, EntryFamily, RawCandidateSnapshot, candidate_path,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import CandidateSnapshot, admit_append
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_store_history_contract import at_head
from scripts.test_candidate_store_semantic import (
    IO_STAGE, REPLAY_STAGE, SemanticCase, blobs, digest_file, name, refusal,
)
from scripts.test_candidate_store_replay import DARWIN, LINUX, Scenario, other_candidate
from scripts.test_candidate_store_support import BRANCH, NOT_FOUND, REPOSITORY, RULESET_ID, TIMEOUT, Fault


X, Y = store_cases.X, store_cases.Y
BASE = f"repos/{REPOSITORY}"
STRICT_PREFIX = [f"{BASE}/git/ref/heads/{BRANCH}", BASE, f"{BASE}/rules/branches/{BRANCH}?per_page=100",
                 f"{BASE}/rulesets/{RULESET_ID}"]


def boundary_free(endpoint: str) -> bool:
    return not any(part in endpoint for part in ("/git/ref", "/rules/", "/rulesets/"))


class AtBranchHead(store_cases.StoreCase):
    """Loads through the history reader at the head the branch names now, as a caller that pinned it."""

    def branch_head(self) -> str:
        return self.github.refs[self.github.branch]

    def load(self, candidate: str = X) -> CandidateHistoryAtHead:
        return load_candidate_history(self.client, self.store.location, candidate, head=self.branch_head())


class HistoryReaderTests(AtBranchHead, store_cases.HistoryTests):
    pass


class HistoryTamperTests(AtBranchHead, store_cases.TamperTests):
    pass


class HistoryRequestTests(AtBranchHead):
    def build(self) -> None:
        self.register(X)
        self.register(Y)
        self.put(X, "attempt-0001.intent.json", b"intent")
        self.store.step("x intent")

    def strict(self, candidate: str = X) -> RawCandidateSnapshot:
        return load_raw_candidate(self.client, self.store.location, candidate)

    def test_history_reads_are_the_strict_reads_without_branch_rule_or_ruleset_requests(self):
        self.build()
        strict = self.strict()
        strict_calls = list(self.github.calls)
        self.github.calls.clear()
        self.receipt["commands"].clear()
        history = self.load()
        self.assertEqual([endpoint for _, endpoint in strict_calls[:4]], STRICT_PREFIX)
        self.assertEqual(self.github.calls, [call for call in strict_calls if boundary_free(call[1])])
        self.assertEqual({method for method, _ in self.github.calls}, {"GET"})
        self.assertEqual(self.github.calls[0], ("GET", BASE))
        self.assertFalse([record for record in self.receipt["commands"] if "body_bytes" in record])
        self.assertEqual(history, at_head(strict))

    def test_a_pinned_head_keeps_its_history_after_the_branch_advances(self):
        self.build()
        pinned, before = self.branch_head(), self.strict()
        self.put(X, "attempt-0001.claim.Linux-amd64.json", b"claim")
        advanced = self.store.step("x claim")
        self.put(Y, "attempt-0001.intent.json", b"y intent")
        self.store.step("y intent")
        history = load_candidate_history(self.client, self.store.location, X, head=pinned)
        self.assertEqual(history, at_head(before))
        self.assertEqual((history.head, len(history.transactions)), (pinned, 2))
        after = self.strict()
        self.assertEqual((after.head, [item.commit for item in after.transactions][-1]),
                         (self.branch_head(), advanced))

    def test_the_supplied_head_is_not_authenticated_as_the_branch_head(self):
        """A commit no protected branch names, descending from the genesis, still reads structurally."""
        self.build()
        self.put(X, "attempt-0001.claim.Linux-amd64.json", b"claim")
        unprotected = self.github.commit(self.github.build(self.store.files), (self.branch_head(),), "elsewhere")
        history = load_candidate_history(self.client, self.store.location, X, head=unprotected)
        self.assertEqual((history.head, history.transactions[-1].commit), (unprotected, unprotected))
        self.assertEqual(len(self.strict().transactions), 2)

    def test_the_boundary_is_not_read_while_the_strict_load_still_refuses_it(self):
        def missing_rule(github):
            github.rules.pop()

        def hidden_bypass(github):
            del github.rulesets[RULESET_ID]["bypass_actors"]

        def bypass_actor(github):
            github.rulesets[RULESET_ID]["bypass_actors"] = [{"actor_id": 1, "actor_type": "RepositoryRole",
                                                            "bypass_mode": "always"}]

        for change in (missing_rule, hidden_bypass, bypass_actor):
            with self.subTest(change=change.__name__):
                self.setUp()
                self.build()
                expected = self.load()
                change(self.github)
                self.assertEqual(self.load(), expected)
                with self.assertRaises(VerificationError) as caught:
                    self.strict()
                self.assertEqual(caught.exception.code, "store_branch_rules_missing")
                self.assertEqual({method for method, _ in self.github.calls}, {"GET"})

    def test_the_repository_identity_is_still_required(self):
        self.build()
        self.github.repository_reply["full_name"] = "other/store"
        self.github.calls.clear()
        with self.assertRaises(VerificationError) as caught:
            self.load()
        self.assertEqual(caught.exception.code, "store_repository_mismatch")
        self.assertEqual(self.github.calls, [("GET", BASE)])
        with self.assertRaises(VerificationError) as strict:
            self.strict()
        self.assertEqual(strict.exception.code, "store_repository_mismatch")

    def test_malformed_requests_refuse_before_any_read(self):
        self.build()
        location, head = self.store.location, self.branch_head()
        cases = [("store_head_invalid", (self.client, location, X), value)
                 for value in ("", head.upper(), head[:-1], head + "0", None, 7)]
        cases += [("store_candidate_id_invalid", (self.client, location, "x"), head),
                  ("store_request_invalid", (None, location, X), head),
                  ("store_request_invalid", (self.client, None, X), head)]
        self.github.calls.clear()
        for code, arguments, value in cases:
            with self.subTest(code=code, head=value), self.assertRaises(VerificationError) as caught:
                load_candidate_history(*arguments, head=value)
            self.assertEqual(caught.exception.code, code)
        self.assertEqual(self.github.calls, [])

    def test_no_failed_read_is_ever_absence(self):
        for needle, fault in (("/commits?", NOT_FOUND), ("/commits?", None), ("/git/trees/", NOT_FOUND),
                              ("/git/commits/", NOT_FOUND), ("/git/blobs/", NOT_FOUND), (BASE, NOT_FOUND)):
            for loader in (self.load, self.strict):
                with self.subTest(needle=needle, fault=fault, loader=loader.__name__):
                    self.setUp()
                    self.build()
                    self.github.faults.append(Fault("GET", needle, result=fault, error=None if fault else TIMEOUT))
                    with self.assertRaises(VerificationError) as caught:
                        loader()
                    self.assertEqual(caught.exception.code, "github_request_failed" if fault else "command_timeout")


class HistoryWriterTests(store_cases.WriterCase):
    def test_admission_and_the_writer_refuse_a_history_before_any_request(self):
        self.register(Y)
        history = load_candidate_history(self.client, self.store.location, X,
                                         head=self.github.refs[self.github.branch])
        manifest, locator = self.registration()
        self.github.calls.clear()
        with self.assertRaises(VerificationError) as admitted:
            admit_append(history, (manifest, locator), review_provenance=(), closure_resolutions=(),
                         installer_sources=())
        self.assertEqual((admitted.exception.code, admitted.exception.stage),
                         ("store_snapshot_invalid", REPLAY_STAGE))
        with self.assertRaises(VerificationError) as written:
            _append_transaction(self.client, self.store.location, history, (manifest, locator),
                                work_directory=self.work, observe_failure=self.observe)
        self.assertEqual((written.exception.code, written.exception.stage), ("store_request_invalid", IO_STAGE))
        self.assertEqual((self.github.calls, self.observed, list(self.work.iterdir())), ([], [], []))
        snapshot = self.load()
        self.assertEqual(at_head(snapshot), history)
        self.assertIsInstance(self.append(snapshot, manifest, locator), AppendConfirmed)

    def test_the_public_reads_take_no_caller_provenance(self):
        public = {name for name, value in vars(candidate_store).items()
                  if inspect.isfunction(value) and value.__module__ == candidate_store.__name__
                  and not name.startswith("_")}
        self.assertEqual(public, {"load_raw_candidate", "load_candidate", "load_candidate_history",
                                  "load_candidate_at_head", "load_candidate_at_branch_head", "append"})
        keyword = inspect.Parameter.KEYWORD_ONLY
        for function, expected in ((load_candidate_history, ["head"]),
                                   (load_candidate_at_head, ["head", "closure_resolutions"]),
                                   (load_candidate_at_branch_head, ["closure_resolutions"])):
            parameters = inspect.signature(function).parameters
            with self.subTest(function=function.__name__):
                self.assertEqual(list(parameters), ["client", "location", "candidate_id", *expected])
                self.assertEqual([name for name, item in parameters.items() if item.kind is keyword], expected)


class ComposedAtHeadTests(SemanticCase):
    """The composed read at a head observes introductions and sources through the real I/O owner."""

    def at_head(self, head: str | None = None, closures=()):
        return load_candidate_at_head(self.client, self.store.location, self.candidate.candidate_id,
                                      head=head or self.github.refs[self.github.branch], closure_resolutions=closures)

    def reviewed(self) -> tuple[AppendConfirmed, AppendConfirmed]:
        self.confirmed(self.registration_files())
        install = self.confirmed(self.install_files(LINUX, InstallChannel.SHELL))
        self.confirmed(self.intent_files())
        return install, self.confirmed(digest_file(EntryFamily.REVIEW, encode_review(fx.review_record(self.candidate))))

    def assertBothRefuse(self, expected: tuple[str, str], closures=()) -> None:
        for read in (lambda: self.loaded(closures), lambda: self.at_head(closures=closures)):
            with self.assertRaises(VerificationError) as caught:
                read()
            self.assertEqual(refusal(caught.exception), expected)

    def test_the_read_at_the_current_head_equals_the_strict_load(self):
        _, review = self.reviewed()
        data, fact = self.triage()
        triage = self.confirmed(self.triage_files(data, fact), (fact,))
        strict = self.loaded((fact,))
        self.github.calls.clear()
        snapshot = self.at_head(closures=(fact,))
        self.assertEqual(snapshot, strict)
        self.assertEqual(snapshot.head, triage.commit)
        self.assertEqual(snapshot.reviews[0].provenance.transaction_commit, review.commit)
        self.assertEqual(snapshot.history.triages[0].provenance.transaction_commit, triage.commit)
        self.assertEqual(len(self.source_reads()), 1)
        self.assertTrue(all(method == "GET" and boundary_free(endpoint) for method, endpoint in self.github.calls))

    def test_a_pinned_head_replays_its_own_facts_after_the_branch_advances(self):
        _, review = self.reviewed()
        before = self.loaded()
        later = self.confirmed(digest_file(EntryFamily.REVIEW, encode_review(
            fx.review_record(self.candidate, head="9" * 40))))
        self.assertEqual(self.at_head(review.commit), before)
        self.assertEqual([item.provenance.transaction_commit for item in self.at_head().reviews],
                         [review.commit, later.commit])

    def test_missing_closure_and_installer_source_facts_refuse_as_in_the_strict_load(self):
        self.reviewed()
        data, fact = self.triage()
        self.confirmed(self.triage_files(data, fact), (fact,))
        foreign = replace(fact, invocation_id=fx.invocation_id(1, DARWIN))
        self.assertBothRefuse(("store_closure_unverified", REPLAY_STAGE))
        self.assertBothRefuse(("store_closure_resolution_unused", REPLAY_STAGE), (fact, foreign))
        self.put_source(fx.COMMIT, b"#!/bin/sh\necho different installer\n")
        self.assertBothRefuse(("store_installer_source_invalid", REPLAY_STAGE), (fact,))
        self.github.source(fx.COMMIT, {"other.sh": ("100755", self.github.blob(fx.INSTALL_SH))})
        self.assertBothRefuse(("store_installer_source_missing", IO_STAGE), (fact,))

    def test_an_absent_candidate_reads_as_none_and_refuses_supplied_facts(self):
        self.assertIsNone(self.at_head())
        _, fact = self.triage()
        with self.assertRaises(VerificationError) as caught:
            self.at_head(closures=(fact,))
        self.assertEqual(refusal(caught.exception), ("store_trusted_fact_unused", REPLAY_STAGE))

class BranchHeadTests(SemanticCase):
    """The observation resolves H once, replays there, and names the commit that actually introduced the freeze."""

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        s = Scenario(cls.candidate)
        darwin, linux = s.claim(DARWIN), s.claim(LINUX)
        cls.ready = [s.register(), s.intent(), s.claim_file(darwin), s.claim_file(linux),
                     s.proof_file(darwin, s.zip(darwin)), s.proof_file(linux, s.zip(linux)),
                     *(s.install(platform, channel)[0] for platform in (DARWIN, LINUX) for channel in InstallChannel),
                     s.digest_file(EntryFamily.REVIEW, s.review())]
        cls.later_review = s.digest_file(EntryFamily.REVIEW, s.review(unresolved_findings=("finding-1",)))
        cls.other = Scenario(other_candidate())

    def setUp(self):
        super().setUp()
        self.base = f"repos/{self.github.repository}"
        self.ref = f"{self.base}/git/ref/heads/{self.github.branch}"

    def at_branch_head(self, closures=()) -> CandidateSnapshot | None:
        return load_candidate_at_branch_head(self.client, self.store.location, self.candidate.candidate_id,
                                             closure_resolutions=closures)

    def at(self, head: str) -> CandidateSnapshot | None:
        return load_candidate_at_head(self.client, self.store.location, self.candidate.candidate_id, head=head,
                                      closure_resolutions=())

    def frozen(self) -> AppendConfirmed:
        """Append the ready history and its canonical freeze record through the public writer."""
        for files in self.ready:
            self.confirmed(files)
        snapshot = self.loaded()
        source = self.candidate.manifest.source
        record = authorize_publication(candidate=self.candidate, observed=fx.observed(source.commit, source.tree),
                                       scope=snapshot.history.intents[-1].scope, history=snapshot.history,
                                       review=snapshot.reviews[0], evidence_parent=snapshot.head)
        return self.confirmed({name(EntryFamily.PUBLICATION): encode_publication(record)})

    def advance_unrelated(self) -> str:
        """Register another candidate through the public writer; the branch head moves past the freeze."""
        outcome = candidate_store.append(self.client, self.store.location, self.other.id, blobs(self.other.register()),
                                         closure_resolutions=(), work_directory=self.work,
                                         observe_failure=self.observe)
        self.assertIsInstance(outcome, AppendConfirmed)
        return outcome.commit

    def assertRefusedAfter(self, code: str, endpoints: list[str]) -> None:
        self.github.calls.clear()
        with self.assertRaises(VerificationError) as caught:
            self.at_branch_head()
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(self.github.calls, [("GET", endpoint) for endpoint in endpoints])

    def test_a_freeze_at_h_is_f_and_h_is_the_only_extra_read(self):
        freeze = self.frozen()
        self.github.calls.clear()
        snapshot = self.at_branch_head()
        observed_calls = list(self.github.calls)
        self.github.calls.clear()
        self.assertEqual(self.at(freeze.commit), snapshot)
        self.assertEqual(observed_calls, [("GET", self.ref), *self.github.calls])
        self.assertTrue(all(boundary_free(endpoint) for _, endpoint in observed_calls[1:]))
        self.assertEqual((snapshot.head, snapshot.freeze_commit), (freeze.commit, freeze.commit))
        self.assertEqual(snapshot.history.publication.evidence_parent, freeze.parent)
        self.assertEqual(self.loaded(), snapshot)

    def test_an_unrelated_candidate_advancing_h_keeps_the_actual_freeze_commit(self):
        freeze = self.frozen()
        head = self.advance_unrelated()
        snapshot = self.at_branch_head()
        parent = snapshot.history.publication.evidence_parent
        self.assertEqual((snapshot.head, snapshot.freeze_commit, parent), (head, freeze.commit, freeze.parent))
        # H holds the same publication bytes as F, and neither H nor F's parent is the freeze commit.
        path = f"{candidate_path(self.candidate.candidate_id)}/{name(EntryFamily.PUBLICATION)}"
        self.assertEqual(self.github.path_identity(head, path), self.github.path_identity(freeze.commit, path))
        self.assertIsNone(self.github.path_identity(parent, path))
        self.assertNotIn(snapshot.freeze_commit, {head, parent})
        self.assertEqual(self.loaded(), snapshot)
        pinned = self.at(freeze.commit)
        self.assertEqual((pinned.head, pinned.freeze_commit, pinned.history), (freeze.commit, freeze.commit,
                                                                              snapshot.history))

    def test_an_unfrozen_candidate_has_no_freeze_commit(self):
        for files in self.ready:
            self.confirmed(files)
        snapshot = self.at_branch_head()
        self.assertEqual((snapshot.history.publication, snapshot.freeze_commit), (None, None))
        self.assertEqual(self.loaded(), snapshot)

    def test_a_raw_candidate_write_after_the_freeze_refuses_at_the_current_head(self):
        freeze = self.frozen()
        files = self.github.flatten(self.github.commits[freeze.commit].tree)
        ((entry, data),) = self.later_review.items()
        files[f"{candidate_path(self.candidate.candidate_id)}/{entry}"] = ("100644", self.github.blob(data))
        raw_write = self.github.commit(self.github.build(files), (freeze.commit,), "raw write")
        self.github.refs[self.github.branch] = raw_write
        for read in (self.at_branch_head, self.loaded):
            with self.subTest(read=read.__name__), self.assertRaises(VerificationError) as caught:
                read()
            self.assertEqual(refusal(caught.exception), ("store_candidate_frozen", REPLAY_STAGE))
        self.assertEqual(self.at(freeze.commit).freeze_commit, freeze.commit)

    def test_the_ref_is_read_once_and_every_later_read_stays_at_that_head(self):
        freeze = self.frozen()
        moved = self.advance_unrelated()
        self.github.refs[self.github.branch] = freeze.commit
        reply = self.github.ref_reply

        def moving() -> dict[str, object]:
            value = reply()
            self.github.refs[self.github.branch] = moved
            return value

        self.github.ref_reply = moving
        self.github.calls.clear()
        snapshot = self.at_branch_head()
        endpoints = [endpoint for _, endpoint in self.github.calls]
        self.assertEqual((snapshot.head, snapshot.freeze_commit), (freeze.commit, freeze.commit))
        self.assertEqual([endpoint for endpoint in endpoints if "/git/ref" in endpoint], [self.ref])
        self.assertEqual([endpoint for endpoint in endpoints if moved in endpoint], [])
        pinned = (f"sha={freeze.commit}&", f"/git/trees/{freeze.commit}%3A", f"/git/commits/{freeze.commit}")
        for needle in pinned:
            with self.subTest(needle=needle):
                self.assertTrue(any(needle in endpoint for endpoint in endpoints))
        self.assertEqual((self.github.refs[self.github.branch], self.loaded().head), (moved, moved))

    def test_hidden_ruleset_fields_do_not_block_the_observation_while_the_strict_load_refuses(self):
        self.frozen()
        expected = self.at_branch_head()
        rules = [dict(rule) for rule in self.github.rules]
        rulesets = {key: dict(value) for key, value in self.github.rulesets.items()}

        def hidden_bypass(github):
            del github.rulesets[RULESET_ID]["bypass_actors"]

        def hidden_enforcement(github):
            del github.rulesets[RULESET_ID]["enforcement"]

        def hidden_rule_source(github):
            del github.rules[0]["ruleset_source"]

        for change in (hidden_bypass, hidden_enforcement, hidden_rule_source):
            with self.subTest(change=change.__name__):
                self.github.rules = [dict(rule) for rule in rules]
                self.github.rulesets = {key: dict(value) for key, value in rulesets.items()}
                change(self.github)
                self.github.calls.clear()
                self.assertEqual(self.at_branch_head(), expected)
                self.assertEqual([endpoint for _, endpoint in self.github.calls if "/rules" in endpoint], [])
                with self.assertRaises(VerificationError) as caught:
                    self.loaded()
                self.assertEqual(caught.exception.code, "store_branch_rules_missing")

    def test_malformed_requests_refuse_before_any_read(self):
        location, candidate = self.store.location, self.candidate.candidate_id
        cases = [("store_request_invalid", (None, location, candidate)),
                 ("store_request_invalid", (self.client, None, candidate)),
                 ("store_candidate_id_invalid", (self.client, location, "x")),
                 ("store_candidate_id_invalid", (self.client, location, None))]
        self.github.calls.clear()
        for code, arguments in cases:
            with self.subTest(code=code, arguments=arguments), self.assertRaises(VerificationError) as caught:
                load_candidate_at_branch_head(*arguments, closure_resolutions=())
            self.assertEqual(caught.exception.code, code)
        self.assertEqual(self.github.calls, [])

    def test_ref_and_repository_refusals_stop_before_any_history_read(self):
        head, ref = self.github.refs[self.github.branch], f"refs/heads/{self.github.branch}"
        replies = {"not a commit": {"ref": ref, "object": {"type": "tag", "sha": head}},
                   "another ref": {"ref": "refs/heads/other", "object": {"type": "commit", "sha": head}},
                   "not an object id": {"ref": ref, "object": {"type": "commit", "sha": head[:-1]}}}
        for label, value in replies.items():
            with self.subTest(reply=label):
                self.github.ref_reply = lambda value=value: value
                self.assertRefusedAfter("store_head_invalid", [self.ref])
        del self.github.ref_reply
        self.github.faults.append(Fault("GET", "/git/ref/", result=NOT_FOUND))
        self.assertRefusedAfter("github_request_failed", [self.ref])
        self.github.repository_reply["full_name"] = "other/store"
        self.assertRefusedAfter("store_repository_mismatch", [self.ref, self.base])

    def test_absence_and_failed_reads_are_those_of_the_supplied_head_read(self):
        self.assertIsNone(self.at_branch_head())
        self.advance_unrelated()
        self.assertIsNone(self.at_branch_head())
        _, fact = self.triage()
        with self.assertRaises(VerificationError) as unused:
            self.at_branch_head((fact,))
        self.assertEqual(refusal(unused.exception), ("store_trusted_fact_unused", REPLAY_STAGE))
        self.confirmed(self.registration_files())
        head = self.github.refs[self.github.branch]
        for needle in ("/commits?", "/git/trees/", "/git/commits/", "/git/blobs/"):
            for fault in (Fault("GET", needle, result=NOT_FOUND), Fault("GET", needle, error=TIMEOUT)):
                codes = []
                for read in (self.at_branch_head, lambda: self.at(head)):
                    self.github.faults.append(replace(fault))
                    with self.assertRaises(VerificationError) as caught:
                        read()
                    codes.append(refusal(caught.exception))
                with self.subTest(needle=needle, fault=fault.error or fault.result):
                    self.assertEqual(codes[0], codes[1])
                    self.assertIn(codes[0][0], {"github_request_failed", "command_timeout"})
        self.assertEqual(self.at_branch_head(), self.at(head))


if __name__ == "__main__":
    unittest.main()
