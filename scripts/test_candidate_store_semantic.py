"""Public semantic candidate store I/O: a fresh verified head, observed facts, canonical replay.

The store is the in-memory ``FakeGitHub`` behind the real ``GitHubClient``. It
proves the composed request flow and its refusals, not GitHub itself. Every
record comes from its canonical encoder; closure facts are explicit trusted
test inputs that stand in for the native lifetime owner.
"""

from __future__ import annotations

import base64
from dataclasses import replace
import json
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_store, candidate_store_replay
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    ReviewProvenance, claim_sha256, encode_claim, encode_intent, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ClosureResolution, ResourceDisposition, encode_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_history import encode_publication, encode_triage
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    PROOF_STAGE, InstallChannel, InstallProof, SourceFileBinding, encode_install_proof, encode_install_witness,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import encode_locator, encode_manifest
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    SourceObservation, authorize_publication, decide_platform_claim,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendAmbiguous, AppendConfirmed, AppendOutcome, EntryFamily, EntryName, StoredBlob, TransitionKind,
    TreeEntry, entry_name,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import CandidateSnapshot, ReplayDenied
from scripts.delegate_acceptance.release_verification.contract import MAX_SOURCE_FILE, VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha
from scripts.test_candidate_store import WriterCase
from scripts.test_candidate_store_replay import DARWIN, LINUX, Scenario
from scripts.test_candidate_store_support import NOT_FOUND, Fault


IO_STAGE, REPLAY_STAGE = candidate_store.STAGE, candidate_store_replay.STAGE
FOREIGN_SOURCE = "f" * 40
Files = dict[str, bytes]


def name(family: EntryFamily, **identity: object) -> str:
    return entry_name(EntryName(family, **identity))


def digest_file(family: EntryFamily, data: bytes) -> Files:
    return {name(family, digest=sha(data)): data}


def blobs(files: Files) -> tuple[StoredBlob, ...]:
    return tuple(StoredBlob(TreeEntry(path, "100644", "blob", git_blob_sha(data)), data)
                 for path, data in sorted(files.items()))


def refusal(error: VerificationError) -> tuple[str, str]:
    return error.code, error.stage


class SemanticCase(WriterCase):
    """A store in the candidate's own repository whose source commit holds the shell installer."""

    repository = fx.REPOSITORY
    candidate = fx.bound()

    def setUp(self):
        super().setUp()
        self.put_source(fx.COMMIT, fx.INSTALL_SH)

    def put_source(self, commit: str, data: bytes, mode: str = "100755") -> str:
        """Store ``data`` as the commit's root install.sh; return its blob identity."""
        blob = self.github.blob(data)
        self.github.source(commit, {"install.sh": (mode, blob), "README.md": ("100644", self.github.blob(b"r"))})
        return blob

    def append_files(self, files: Files, closures: tuple[ClosureResolution, ...] = ()) -> AppendOutcome:
        return candidate_store.append(self.client, self.store.location, self.candidate.candidate_id, blobs(files),
                                      closure_resolutions=closures, work_directory=self.work,
                                      observe_failure=self.observe)

    def confirmed(self, files: Files, closures: tuple[ClosureResolution, ...] = ()) -> AppendConfirmed:
        outcome = self.append_files(files, closures)
        self.assertIsInstance(outcome, AppendConfirmed)
        self.assertEqual(self.github.commits[outcome.commit].parents, (outcome.parent,))
        return outcome

    def assertRefusedBeforeWrite(self, expected: tuple[str, str], files: Files,
                                 closures: tuple[ClosureResolution, ...] = ()) -> VerificationError:
        head = self.github.refs[self.github.branch]
        self.github.calls.clear()
        with self.assertRaises(VerificationError) as caught:
            self.append_files(files, closures)
        self.assertEqual(refusal(caught.exception), expected)
        self.assertEqual(self.requests("POST") + self.requests("PATCH"), [])
        self.assertEqual(self.github.refs[self.github.branch], head)
        return caught.exception

    def loaded(self, closures: tuple[ClosureResolution, ...] = ()) -> CandidateSnapshot | None:
        return candidate_store.load_candidate(self.client, self.store.location, self.candidate.candidate_id,
                                              closure_resolutions=closures)

    def source_reads(self, commit: str = fx.COMMIT) -> list[str]:
        return self.requests("GET", f"/git/commits/{commit}")

    def registration_files(self) -> Files:
        return {name(EntryFamily.LOCATOR): encode_locator(self.candidate.locator),
                name(EntryFamily.MANIFEST): encode_manifest(self.candidate.manifest)}

    def intent_files(self) -> Files:
        return {name(EntryFamily.INTENT, attempt=1): encode_intent(fx.intent(self.candidate))}

    def install_files(self, platform: tuple[str, str], channel: InstallChannel,
                      proof: InstallProof | None = None) -> Files:
        value = proof or fx.install(self.candidate, platform=platform, channel=channel)
        witness = fx.witness(self.candidate, platform=platform, channel=channel)
        role = dict(platform=platform, channel=channel)
        return {name(EntryFamily.INSTALL_PROOF, **role): encode_install_proof(value),
                name(EntryFamily.INSTALL_WITNESS, **role): encode_install_witness(witness)}

    def triage(self) -> tuple[bytes, ClosureResolution]:
        """An attempt-1 triage whose unproven Linux claim the native owner closed."""
        claim = fx.claim(self.candidate, attempt=1, platform=LINUX)
        fact = fx.resolution(ClaimSubject(claim_sha256(claim)), invocation=claim.invocation_id,
                             resources=((NATIVE_CHILD_RESOURCE, ResourceDisposition.WAITED),))
        record = fx.triage(self.candidate, attempt=1, prior_proofs=(), explained=(), prior_claims=(claim,),
                           closure_refs=(fx.reference(fact),)).record
        return encode_triage(record), fact

    def triage_files(self, data: bytes, fact: ClosureResolution) -> Files:
        return {**digest_file(EntryFamily.TRIAGE, data),
                **digest_file(EntryFamily.CLOSURE, encode_closure_evidence(fact))}


class ProvenanceTests(SemanticCase):
    def test_public_appends_load_with_their_actual_introducing_commits(self):
        registered = self.confirmed(self.registration_files())
        self.assertEqual((registered.parent, registered.kind), (self.store.genesis, TransitionKind.REGISTER))
        self.confirmed(self.intent_files())
        review_bytes = encode_review(fx.review_record(self.candidate))
        review = self.confirmed(digest_file(EntryFamily.REVIEW, review_bytes))
        triage_bytes, fact = self.triage()
        triage = self.confirmed(self.triage_files(triage_bytes, fact), (fact,))
        self.assertEqual((triage.parent, triage.kind), (review.commit, TransitionKind.TRIAGE))
        snapshot = self.loaded((fact,))
        self.assertEqual((snapshot.head, snapshot.candidate), (triage.commit, self.candidate))
        self.assertEqual([(item.record, item.provenance) for item in snapshot.reviews],
                         [(fx.review_record(self.candidate),
                           ReviewProvenance(artifact_sha256=sha(review_bytes), reviewer=fx.REVIEWER,
                                            transaction_commit=review.commit))])
        stored = snapshot.history.triages[0]
        self.assertEqual((sha(encode_triage(stored.record)), stored.provenance, stored.resolutions),
                         (sha(triage_bytes), ReviewProvenance(artifact_sha256=sha(triage_bytes),
                                                              reviewer=fx.REVIEWER,
                                                              transaction_commit=triage.commit), (fact,)))

    def test_an_absent_candidate_loads_as_none_and_refuses_supplied_facts(self):
        self.assertIsNone(self.loaded())
        _, fact = self.triage()
        with self.assertRaises(VerificationError) as caught:
            self.loaded((fact,))
        self.assertEqual(refusal(caught.exception), ("store_trusted_fact_unused", REPLAY_STAGE))

    def test_only_the_native_owners_closure_facts_admit_and_replay_a_closure(self):
        self.confirmed(self.registration_files())
        self.confirmed(self.intent_files())
        self.confirmed(digest_file(EntryFamily.REVIEW, encode_review(fx.review_record(self.candidate))))
        data, fact = self.triage()
        files = self.triage_files(data, fact)
        foreign = replace(fact, invocation_id=fx.invocation_id(1, DARWIN))
        self.assertRefusedBeforeWrite(("store_closure_unverified", REPLAY_STAGE), files)
        self.assertRefusedBeforeWrite(("store_closure_unverified", REPLAY_STAGE), files, (foreign,))
        self.assertRefusedBeforeWrite(("store_closure_resolution_unused", REPLAY_STAGE), files, (fact, foreign))
        self.confirmed(files, (fact,))
        for closures, code in (((), "store_closure_unverified"), ((fact, foreign), "store_closure_resolution_unused")):
            with self.subTest(closures=len(closures)), self.assertRaises(VerificationError) as caught:
                self.loaded(closures)
            self.assertEqual(refusal(caught.exception), (code, REPLAY_STAGE))
        later = digest_file(EntryFamily.REVIEW, encode_review(fx.review_record(self.candidate, head="9" * 40)))
        self.assertRefusedBeforeWrite(("store_closure_unverified", REPLAY_STAGE), later)
        self.confirmed(later, (fact,))
        self.assertEqual(len(self.loaded((fact,)).reviews), 2)


class AdmissionTests(SemanticCase):
    def test_policy_denials_raise_unchanged_with_their_exact_findings_before_any_write(self):
        self.assertRefusedBeforeWrite(("store_registration_missing", REPLAY_STAGE), self.intent_files())
        self.confirmed(self.registration_files())
        claim = fx.claim(self.candidate, attempt=1, platform=LINUX)
        expected = decide_platform_claim(candidate=self.candidate, history=self.loaded().history, claim=claim)
        self.assertFalse(expected.allowed)
        error = self.assertRefusedBeforeWrite(
            ("store_claim_denied", REPLAY_STAGE),
            {name(EntryFamily.CLAIM, attempt=1, platform=LINUX): encode_claim(claim)})
        self.assertIs(type(error), ReplayDenied)
        self.assertEqual(error.findings, expected.findings)

    def test_a_malformed_request_refuses_before_any_read(self):
        self.github.calls.clear()
        for additions in (list(blobs(self.registration_files())), ("manifest.json",)):
            with self.subTest(additions=type(additions).__name__), self.assertRaises(VerificationError) as caught:
                candidate_store.append(self.client, self.store.location, self.candidate.candidate_id, additions,
                                       closure_resolutions=(), work_directory=self.work,
                                       observe_failure=self.observe)
            self.assertEqual(refusal(caught.exception), ("store_request_invalid", IO_STAGE))
        self.assertEqual(self.github.calls, [])

    def test_an_unknown_ref_update_is_observed_once_and_never_read_back_or_retried(self):
        registered = self.confirmed(self.registration_files())
        review = digest_file(EntryFamily.REVIEW, encode_review(fx.review_record(self.candidate)))
        timeout = BoundedCommandError(CommandFailure.TIMEOUT)
        self.github.faults.append(Fault("PATCH", "/git/refs/", error=timeout, apply=True))
        self.github.calls.clear()
        outcome = self.append_files(review)
        self.assertIsInstance(outcome, AppendAmbiguous)
        self.assertEqual((outcome.parent, outcome.failure, outcome.record_sha256s),
                         (registered.commit, "command_timeout", tuple(sha(data) for data in review.values())))
        self.assertEqual(self.github.commits[outcome.proposed].message,
                         f"Append review to development candidate {self.candidate.candidate_id}\n\n"
                         f"Store-Nonce: {outcome.nonce}")
        self.assertEqual([error.code for error in self.observed], ["command_timeout"])
        self.assertIs(self.tree.unresolved, self.observed[0])
        self.assertIs(self.observed[0].__cause__, timeout)
        self.assertEqual((len(self.requests("PATCH")), self.github.calls[-1][0]), (1, "PATCH"))
        self.assertEqual(self.launches.call_count, 0)

    def test_the_requested_commit_message_needs_no_reply_normalization(self):
        """GitHub replies without a terminal LF; the requested message has none to lose, so it is exact."""
        tree = self.github.commits[self.store.genesis].tree
        self.assertEqual(self.github.commit_reply(self.github.commit(tree, (), "model\n"))["message"], "model")
        self.github.calls.clear()
        outcome = self.append_files(self.registration_files())
        self.assertIsInstance(outcome, AppendConfirmed)
        self.assertEqual(len(self.requests("PATCH")), 1)
        [body] = self.work.glob("store-append-*-commit.json")
        requested = json.loads(body.read_bytes())["message"]
        self.assertRegex(requested, rf"\AAppend register to development candidate {self.candidate.candidate_id}"
                                    r"\n\nStore-Nonce: [0-9a-f]{64}\Z")
        replied = self.github.commit_reply(outcome.commit)["message"]
        self.assertEqual((replied, self.github.commits[outcome.commit].message), (requested, requested))


class InstallerSourceTests(SemanticCase):
    def linux_shell(self) -> Files:
        return self.install_files(LINUX, InstallChannel.SHELL)

    def test_one_read_per_distinct_binding_and_none_for_artifact_installers(self):
        self.confirmed(self.registration_files())
        self.confirmed(self.install_files(LINUX, InstallChannel.NPM))
        self.loaded()
        self.assertEqual(self.source_reads(), [])
        self.confirmed(self.linux_shell())
        self.github.calls.clear()
        self.confirmed(self.install_files(DARWIN, InstallChannel.SHELL))
        self.assertEqual(len(self.source_reads()), 1, "the stored and proposed shell installs share one read")
        self.github.calls.clear()
        snapshot = self.loaded()
        self.assertEqual((len(self.source_reads()), len(snapshot.history.installs)), (1, 3))
        self.assertEqual(len(self.requests("GET", f"/git/blobs/{git_blob_sha(fx.INSTALL_SH)}")), 1)

    def test_regular_files_of_either_mode_are_the_installer_bytes(self):
        for mode in ("100644", "100755"):
            with self.subTest(mode=mode):
                self.setUp()
                self.put_source(fx.COMMIT, fx.INSTALL_SH, mode)
                self.confirmed(self.registration_files())
                self.confirmed(self.linux_shell())
                self.assertEqual(len(self.loaded().history.installs), 1)

    def test_a_link_submodule_directory_or_absent_entry_is_not_an_installer(self):
        target = self.github.blob(fx.INSTALL_SH)
        sources = {"store_installer_source_invalid": (
            {"install.sh": ("120000", target)}, {"install.sh": ("160000", "1" * 40)},
            {"install.sh/inner": ("100755", target)}),
            "store_installer_source_missing": ({"other.sh": ("100755", target)},)}
        for code, variants in sources.items():
            for files in variants:
                with self.subTest(files=files):
                    self.setUp()
                    self.github.source(fx.COMMIT, files)
                    self.confirmed(self.registration_files())
                    self.assertRefusedBeforeWrite((code, IO_STAGE), self.linux_shell())

    def test_failed_or_malformed_source_reads_refuse_with_their_existing_codes(self):
        blob = git_blob_sha(fx.INSTALL_SH)
        root = self.github.commits[fx.COMMIT].tree
        other = base64.b64encode(fx.INSTALL_SH.replace(b"candidate", b"Candidate")).decode()
        replies = {
            ("store_installer_source_invalid", IO_STAGE): (f"/git/blobs/{blob}", {
                "sha": blob, "size": len(fx.INSTALL_SH), "content": other, "encoding": "base64"}),
            ("store_blob_invalid", IO_STAGE): (f"/git/blobs/{blob}", {
                "sha": blob, "size": len(fx.INSTALL_SH), "content": "not base64!", "encoding": "base64"}),
            ("store_commit_invalid", IO_STAGE): (f"/git/commits/{fx.COMMIT}", {
                **self.github.commit_reply(fx.COMMIT), "sha": "1" * 40}),
            ("store_tree_truncated", IO_STAGE): (f"/git/trees/{root}", {
                **self.github.tree_reply(root), "truncated": True}),
            ("store_tree_mismatch", IO_STAGE): (f"/git/trees/{root}", {
                **self.github.tree_reply(root), "tree": self.github.tree_reply(root)["tree"][:1]}),
            ("github_request_failed", "github-api"): (f"/git/commits/{fx.COMMIT}", None),
        }
        self.assertEqual(len(base64.b64decode(other)), len(fx.INSTALL_SH))
        self.confirmed(self.registration_files())
        for expected, (needle, reply) in replies.items():
            with self.subTest(code=expected[0]):
                body = NOT_FOUND if reply is None else BoundedCommandResult(0, json.dumps(reply).encode())
                self.github.faults[:] = [Fault("GET", needle, result=body)]
                self.assertRefusedBeforeWrite(expected, self.linux_shell())

    def test_the_source_file_bound_is_exact(self):
        exact = b"#" * MAX_SOURCE_FILE
        for data, accepted in ((exact, True), (exact + b"#", False)):
            with self.subTest(size=len(data)):
                self.put_source(fx.COMMIT, data)
                binding = SourceFileBinding(source_commit=fx.COMMIT, repo_path="install.sh", sha256=sha(data))
                if accepted:
                    self.assertEqual(candidate_store._installer_source(self.client, self.store.location, binding),
                                     data)
                    continue
                with self.assertRaises(VerificationError) as caught:
                    candidate_store._installer_source(self.client, self.store.location, binding)
                self.assertEqual(refusal(caught.exception), ("store_installer_source_invalid", IO_STAGE))

    def test_only_the_root_installer_path_is_read(self):
        self.github.calls.clear()
        binding = SourceFileBinding(source_commit=fx.COMMIT, repo_path="scripts/install.sh",
                                    sha256=sha(fx.INSTALL_SH))
        with self.assertRaises(VerificationError) as caught:
            candidate_store._installer_source(self.client, self.store.location, binding)
        self.assertEqual((refusal(caught.exception), self.github.calls),
                         (("store_installer_source_invalid", IO_STAGE), []))

    def test_the_binding_commit_is_read_and_the_canonical_policy_judges_it(self):
        self.confirmed(self.registration_files())
        foreign_bytes = fx.INSTALL_SH.replace(b"candidate", b"foreign")
        self.put_source(FOREIGN_SOURCE, foreign_bytes)
        proof = fx.install(self.candidate, platform=LINUX, channel=InstallChannel.SHELL)
        foreign = replace(proof, installer=replace(proof.installer, source_commit=FOREIGN_SOURCE,
                                                   sha256=sha(foreign_bytes)))
        self.assertRefusedBeforeWrite(("install_source_commit_mismatch", PROOF_STAGE),
                                      self.install_files(LINUX, InstallChannel.SHELL, foreign))
        self.assertEqual((len(self.source_reads(FOREIGN_SOURCE)), self.source_reads()), (1, []))
        self.put_source(fx.COMMIT, b"#!/bin/sh\necho different installer\n")
        self.assertRefusedBeforeWrite(("store_installer_source_invalid", REPLAY_STAGE), self.linux_shell())


class FreezeTests(SemanticCase):
    """A ready history appended through the public writer freezes on the head the store actually read."""

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        s = cls.scenario = Scenario(cls.candidate)
        darwin, linux = s.claim(DARWIN), s.claim(LINUX)
        cls.ready = [s.register(), s.intent(), s.claim_file(darwin), s.claim_file(linux),
                     s.proof_file(darwin, s.zip(darwin)), s.proof_file(linux, s.zip(linux)),
                     *(s.install(platform, channel)[0] for platform in (DARWIN, LINUX) for channel in InstallChannel),
                     s.digest_file(EntryFamily.REVIEW, s.review())]

    def test_the_freeze_binds_its_actual_parent_and_loads_with_actual_provenance(self):
        outcomes = [self.confirmed(files) for files in self.ready]
        head = outcomes[-1].commit
        snapshot = self.loaded()
        self.assertEqual(snapshot.reviews[0].provenance.transaction_commit, head)
        source = self.candidate.manifest.source
        record = authorize_publication(
            candidate=self.candidate, observed=SourceObservation(head_commit=source.commit, tree=source.tree),
            scope=snapshot.history.intents[-1].scope, history=snapshot.history, review=snapshot.reviews[0],
            evidence_parent=head)
        stale = replace(record, evidence_parent=outcomes[-2].commit)
        self.assertRefusedBeforeWrite(("store_publication_mismatch", REPLAY_STAGE),
                                      {name(EntryFamily.PUBLICATION): encode_publication(stale)})
        freeze = self.confirmed({name(EntryFamily.PUBLICATION): encode_publication(record)})
        self.assertEqual((freeze.parent, freeze.kind), (head, TransitionKind.FREEZE))
        self.github.calls.clear()
        frozen = self.loaded()
        self.assertEqual((frozen.head, frozen.history.publication), (freeze.commit, record))
        self.assertEqual(frozen.reviews, snapshot.reviews)
        self.assertEqual(len(self.source_reads()), 1, "both shell installs share one source read")
        later = self.scenario.review(unresolved_findings=("finding-1",))
        self.assertRefusedBeforeWrite(("store_candidate_frozen", REPLAY_STAGE), digest_file(EntryFamily.REVIEW, later))


if __name__ == "__main__":
    unittest.main()
