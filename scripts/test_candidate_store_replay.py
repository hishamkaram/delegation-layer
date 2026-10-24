"""Semantic replay of a candidate's verified store history through the canonical owners.

Every record here is produced by its canonical encoder, and every proof ZIP by
the canonical candidate proof fixture. The provenance, closure and installer
source facts these tests pass are explicit trusted test inputs; they stand in
for the store I/O edge's verified branch load and the native lifetime owner,
and prove nothing about how production obtains them.
"""

from __future__ import annotations

import ast
from dataclasses import dataclass, fields, replace
import hashlib
import inspect
import json
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as proof_fixture
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
from scripts.delegate_acceptance.release_verification import candidate_store_replay
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    CellStatus, Closure, PlatformClaim, ReviewProvenance, candidate_proof_expectation, claim_sha256,
    encode_claim, encode_intent, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ClosureResolution, ProofSubject, ResourceDisposition,
    encode_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.candidate_history import (
    STAGE as HISTORY_STAGE, PublicationAuthorization, ReviewedTriage, TriageRecord, encode_publication,
    encode_triage,
)
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    InstallChannel, SourceFileBinding, encode_install_proof, encode_install_witness,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    CandidateLocator, encode_locator, encode_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, BoundCandidate, decide_launch, decide_platform_claim, evaluate_attempt,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    MAX_CANDIDATE_COMMITS, STAGE as STORE_STAGE, EntryFamily, EntryName, RawCandidateSnapshot, RawTransaction,
    StoreBoundary, StoredBlob, StoreLocation, TreeEntry, classify_transition, entry_name, git_tree_sha,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import (
    CandidateSnapshot, ReplayDenied, admit_append, replay_candidate,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha


DARWIN, LINUX = ("Darwin", "arm64"), ("Linux", "amd64")
PLANNED = (DARWIN, LINUX)
LOCATION = StoreLocation(repository=fx.REPOSITORY, branch="development-evidence",
                         genesis_commit="1" * 40, marker_blob="2" * 40)
BOUNDARY = StoreBoundary(repository_id=1, ruleset_ids=(7,))
WAITED = ResourceDisposition.WAITED
Files = dict[str, bytes]


def commit(candidate_id: str, index: int) -> str:
    """The deterministic test commit that appends the candidate's ``index``-th group."""
    return hashlib.sha1(f"{candidate_id}:{index}".encode(), usedforsecurity=False).hexdigest()


FOREIGN_COMMIT = commit("another-candidate", 0)


def raw(candidate_id: str, groups: list[Files], *, parents: dict[int, str] | None = None,
        head: str | None = None) -> RawCandidateSnapshot:
    """Build the structural snapshot the I/O reader would verify for these appended groups.

    ``parents`` overrides the actual parent of a transaction, and ``head`` the
    store head, as when another candidate's commit interleaves.
    """
    entries: frozenset[TreeEntry] = frozenset()
    transactions = []
    parent = LOCATION.genesis_commit
    for index, files in enumerate(groups):
        parent = (parents or {}).get(index, parent)
        child = entries | {TreeEntry(name, "100644", "blob", git_blob_sha(data)) for name, data in files.items()}
        transition = classify_transition(entries, child)
        transactions.append(RawTransaction(
            commit=commit(candidate_id, index), parent=parent,
            parent_tree=git_tree_sha(tuple(entries)) if entries else None, tree=git_tree_sha(tuple(child)),
            transition=transition, blobs=tuple(StoredBlob(entry, files[entry.name]) for entry in transition.added)))
        entries, parent = child, commit(candidate_id, index)
    return RawCandidateSnapshot(location=LOCATION, candidate_id=candidate_id, head=head or parent, head_tree="3" * 40,
                                boundary=BOUNDARY, transactions=tuple(transactions))


def additions(files: Files) -> tuple[StoredBlob, ...]:
    """The proposed blobs of one group, as an append request carries them."""
    return tuple(StoredBlob(TreeEntry(name, "100644", "blob", git_blob_sha(data)), data)
                 for name, data in sorted(files.items()))


def other_candidate() -> BoundCandidate:
    value = fx.manifest("0.1.38")
    return BoundCandidate(locator=fx.locator_for(value), manifest=value)


def name(family: EntryFamily, **identity: object) -> str:
    return entry_name(EntryName(family, **identity))


def refusal(error: VerificationError) -> tuple[object, ...]:
    return error.code, error.stage, error.findings if isinstance(error, ReplayDenied) else None


def codes(findings) -> set[str]:
    return {finding.code for finding in findings}


@dataclass(frozen=True)
class Facts:
    """The trusted facts a test supplies for one replay."""

    provenance: tuple[ReviewProvenance, ...] = ()
    closures: tuple[ClosureResolution, ...] = ()
    sources: tuple[tuple[SourceFileBinding, bytes], ...] = ()


class Scenario:
    """Canonical records of one candidate whose claims bind the canonical proof fixture's models and binaries."""

    def __init__(self, candidate: BoundCandidate):
        self.candidate = candidate
        self.id = candidate.candidate_id
        self.passing = proof_fixture.release_proof(failed=False)
        self.failing = proof_fixture.release_proof(failed=True)
        self.native = proof_fixture.expectation(self.passing)
        self.shell_source = (fx.installer(fx.consumed(candidate, platform=LINUX, channel=InstallChannel.SHELL),
                                          InstallChannel.SHELL), fx.INSTALL_SH)

    def register(self, *, locator: CandidateLocator | None = None, manifest: bytes | None = None) -> Files:
        return {name(EntryFamily.LOCATOR): encode_locator(locator or self.candidate.locator),
                name(EntryFamily.MANIFEST): manifest or encode_manifest(self.candidate.manifest)}

    def intent(self, attempt: int = 1, **changes: object) -> Files:
        value = replace(fx.intent(self.candidate, attempt=attempt, platforms=PLANNED,
                                  triage_sha256=changes.pop("triage_sha256", None)), **changes)
        return {name(EntryFamily.INTENT, attempt=attempt): encode_intent(value)}

    def claim(self, platform: tuple[str, str], attempt: int = 1, **changes: object) -> PlatformClaim:
        native = self.native
        return replace(PlatformClaim(
            candidate_id=self.id, attempt=attempt, os=platform[0], arch=platform[1],
            invocation_id=fx.invocation_id(attempt, platform), harness_revision=native.harness_revision,
            harness_sha256=native.harness_sha256, binaries=native.binaries, opencode_model=native.opencode_model,
            ccr_model=native.ccr_model, agy_model=native.agy_model), **changes)

    def claim_file(self, claim: PlatformClaim) -> Files:
        return {name(EntryFamily.CLAIM, attempt=claim.attempt, platform=claim.platform): encode_claim(claim)}

    def zip(self, claim: PlatformClaim, *, failed: bool = False, candidate: BoundCandidate | None = None) -> bytes:
        owner = candidate or self.candidate
        facts = candidate_proof_expectation(owner.manifest, owner.locator, claim)
        return proof_fixture.candidate_zip(self.failing if failed else self.passing, facts)

    def proof_file(self, claim: PlatformClaim, data: bytes) -> Files:
        return {name(EntryFamily.PROOF, attempt=claim.attempt, platform=claim.platform): data}

    def install(self, platform: tuple[str, str], channel: InstallChannel, *,
                named: InstallChannel | None = None) -> tuple[Files, str]:
        """Return the paired canonical install files and the install proof digest."""
        binaries = self.native.binaries
        proof = fx.install(self.candidate, platform=platform, channel=channel, binaries=binaries)
        witness = encode_install_witness(fx.witness(self.candidate, platform=platform, channel=channel,
                                                    binaries=binaries))
        role = dict(platform=platform, channel=named or channel)
        return ({name(EntryFamily.INSTALL_PROOF, **role): encode_install_proof(proof),
                 name(EntryFamily.INSTALL_WITNESS, **role): witness}, proof.sha256)

    def review(self, **changes: object) -> bytes:
        return encode_review(replace(fx.review_record(self.candidate), **changes))

    def digest_file(self, family: EntryFamily, data: bytes) -> Files:
        return {name(family, digest=sha(data)): data}


class ReplayCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.s = Scenario(fx.bound())
        s = cls.s
        cls.darwin, cls.linux = s.claim(DARWIN), s.claim(LINUX)
        cls.darwin_zip, cls.linux_zip = s.zip(cls.darwin), s.zip(cls.linux)
        cls.installs = [s.install(platform, channel) for platform in PLANNED for channel in InstallChannel]
        cls.review_bytes = s.review()
        # The ready history before its freeze: registration, attempt 1, both proofs, installs, review.
        cls.ready = [s.register(), s.intent(), s.claim_file(cls.darwin), s.claim_file(cls.linux),
                     s.proof_file(cls.darwin, cls.darwin_zip), s.proof_file(cls.linux, cls.linux_zip),
                     *(files for files, _ in cls.installs), s.digest_file(EntryFamily.REVIEW, cls.review_bytes)]
        cls.review_index = len(cls.ready) - 1

    def review_provenance(self, index: int | None = None, data: bytes | None = None,
                          **changes: object) -> ReviewProvenance:
        """The trusted introducing-transaction fact of a stored review (test input only)."""
        index = self.review_index if index is None else index
        value = ReviewProvenance(artifact_sha256=sha(data or self.review_bytes), reviewer=fx.REVIEWER,
                                 transaction_commit=commit(self.s.id, index))
        return replace(value, **changes)

    def replayed(self, groups: list[Files], facts: Facts = Facts(), *, candidate_id: str | None = None,
                 parents: dict[int, str] | None = None) -> CandidateSnapshot | None:
        snapshot = raw(candidate_id or self.s.id, groups, parents=parents)
        return replay_candidate(snapshot, review_provenance=facts.provenance, closure_resolutions=facts.closures,
                                installer_sources=facts.sources)

    def admit(self, groups: list[Files], facts: Facts = Facts(), *, candidate_id: str | None = None,
              parents: dict[int, str] | None = None) -> None:
        """Admit the last group on the verified head before it, as the store's append would.

        The head is that group's actual parent in the replayed history. The
        provenance of the proposed group's own commit does not exist yet, so
        it is left out; every other fact is the replay's.
        """
        candidate_id = candidate_id or self.s.id
        last = len(groups) - 1
        prefix = raw(candidate_id, groups[:-1], parents=parents, head=(parents or {}).get(last))
        proposed = commit(candidate_id, last)
        return admit_append(prefix, additions(groups[-1]),
                            review_provenance=tuple(item for item in facts.provenance
                                                    if item.transaction_commit != proposed),
                            closure_resolutions=facts.closures, installer_sources=facts.sources)

    def replay(self, groups: list[Files], facts: Facts = Facts(), **options: object) -> CandidateSnapshot | None:
        """Replay the history, and admit its last group on the history before it with the same facts."""
        snapshot = self.replayed(groups, facts, **options)
        if groups:
            self.assertIsNone(self.admit(groups, facts, **options))
        return snapshot

    def ready_facts(self, *extra: ReviewProvenance) -> Facts:
        return Facts(provenance=(self.review_provenance(), *extra), sources=(self.s.shell_source,))

    def publication(self, **changes: object) -> PublicationAuthorization:
        """The freeze record a writer derives from the ready history's exact facts."""
        source = self.s.candidate.manifest.source
        value = PublicationAuthorization(
            candidate_id=self.s.id, attempt=1, evidence_parent=commit(self.s.id, self.review_index),
            source_commit=source.commit, source_tree=source.tree,
            proof_sha256s=tuple(sorted((sha(self.darwin_zip), sha(self.linux_zip)))),
            install_sha256s=tuple(sorted(digest for _, digest in self.installs)), triage_sha256s=(),
            review_sha256=sha(self.review_bytes))
        return replace(value, **changes)

    def frozen(self, record: PublicationAuthorization | None = None) -> list[Files]:
        return [*self.ready, {name(EntryFamily.PUBLICATION): encode_publication(record or self.publication())}]

    def assertCode(self, code: str, groups: list[Files], facts: Facts = Facts(), *, paired: bool = True,
                   **options: object):
        """Replay refuses with ``code``; when ``paired``, admitting the last group refuses identically."""
        with self.assertRaises(VerificationError) as caught:
            self.replayed(groups, facts, **options)
        self.assertEqual(caught.exception.code, code)
        if paired and groups:
            with self.assertRaises(VerificationError) as admitted:
                self.admit(groups, facts, **options)
            self.assertEqual(refusal(admitted.exception), refusal(caught.exception))
        return caught.exception

    def assertDenied(self, code: str, groups: list[Files], expected, facts: Facts = Facts()) -> None:
        """Replay and admission carry exactly the findings the canonical policy returns on the prior history."""
        for attempt in (self.replayed, self.admit):
            with self.subTest(path=attempt.__name__), self.assertRaises(ReplayDenied) as caught:
                attempt(groups, facts)
            self.assertEqual((caught.exception.code, caught.exception.findings), (code, expected.findings))
        self.assertFalse(expected.allowed)


class PrefixTests(ReplayCase):
    def test_an_empty_verified_history_is_absence(self):
        self.assertIsNone(self.replay([]))
        self.assertCode("store_trusted_fact_unused", [], Facts(sources=(self.s.shell_source,)))

    def test_every_valid_prefix_replays_to_its_facts(self):
        groups = self.frozen()
        for length in range(1, len(groups) + 1):
            prefix = groups[:length]
            uses_shell = length > 6
            facts = Facts(provenance=(self.review_provenance(),) if length > self.review_index else (),
                          sources=(self.s.shell_source,) if uses_shell else ())
            with self.subTest(length=length):
                snapshot = self.replay(prefix, facts)
                self.assertEqual(snapshot.head, commit(self.s.id, length - 1))
                self.assertEqual(snapshot.candidate, self.s.candidate)
                history = snapshot.history
                self.assertEqual(len(history.intents), int(length > 1))
                self.assertEqual(len(history.claims) + len(history.proofs), max(0, min(length - 2, 4)))
                self.assertEqual(len(history.installs), max(0, min(length - 6, 6)))
                self.assertEqual(len(snapshot.reviews), int(length > self.review_index))
                self.assertEqual(history.publication is not None, length == len(groups))

    def test_the_snapshot_holds_facts_and_no_authority(self):
        self.assertEqual({field.name for field in fields(CandidateSnapshot)},
                         {"head", "candidate", "history", "reviews", "freeze_commit"})
        snapshot = self.replay(self.ready, self.ready_facts())
        self.assertEqual(snapshot.history.publication, None)
        self.assertEqual([proof.proof_sha256 for proof in snapshot.history.proofs],
                         [sha(self.darwin_zip), sha(self.linux_zip)])
        self.assertEqual(snapshot.reviews[0].provenance, self.review_provenance())


class RegistrationTests(ReplayCase):
    def test_registration_comes_first(self):
        self.assertCode("store_registration_missing", [self.s.intent()])
        self.assertCode("store_registration_missing", [self.s.digest_file(EntryFamily.REVIEW, self.review_bytes)])

    def test_the_manifest_binds_the_locator_digest_and_the_candidate_path(self):
        other = fx.manifest("0.1.38")
        self.assertCode("candidate_manifest_digest_mismatch",
                        [self.s.register(manifest=encode_manifest(other))])
        foreign = fx.locator_for(other)
        self.assertCode("store_candidate_path_mismatch",
                        [self.s.register(locator=foreign, manifest=encode_manifest(other))])

    def test_the_locator_repository_is_the_store_repository(self):
        locator = replace(self.s.candidate.locator, repository="different-owner/delegation-layer")
        self.assertCode("store_candidate_repository_mismatch", [self.s.register(locator=locator)])

    def test_non_canonical_or_malformed_registration_bytes_refuse(self):
        locator = encode_locator(self.s.candidate.locator)
        files = self.s.register()
        files[name(EntryFamily.LOCATOR)] = locator.replace(b"{", b"{ ", 1)
        self.assertCode("candidate_record_not_canonical", [files])
        files[name(EntryFamily.LOCATOR)] = b"not json"
        self.assertCode("candidate_record_invalid", [files])


class IntentAndClaimTests(ReplayCase):
    def prior(self, groups: list[Files], facts: Facts = Facts()) -> CandidateSnapshot:
        snapshot = self.replay(groups, facts)
        self.assertIsNotNone(snapshot)
        return snapshot

    def test_the_intent_file_name_binds_its_attempt(self):
        files = self.s.intent()
        self.assertCode("store_entry_body_mismatch",
                        [self.s.register(), {name(EntryFamily.INTENT, attempt=2): next(iter(files.values()))}])

    def test_launch_denials_surface_the_exact_policy_findings(self):
        prior = self.prior([self.s.register()])
        for changes in (dict(source_commit="9" * 40), dict(platforms=(LINUX,))):
            files = self.s.intent(**changes)
            intent = replace(fx.intent(self.s.candidate, platforms=PLANNED), **changes)
            with self.subTest(changes=changes):
                expected = decide_launch(candidate=self.s.candidate, history=prior.history, intent=intent)
                self.assertDenied("store_launch_denied", [self.s.register(), files], expected)
        skipped = self.s.intent(2, triage_sha256="0" * 64)
        self.assertDenied("store_launch_denied", [self.s.register(), skipped], decide_launch(
            candidate=self.s.candidate, history=prior.history,
            intent=fx.intent(self.s.candidate, attempt=2, platforms=PLANNED, triage_sha256="0" * 64)))

    def test_claim_file_names_bind_attempt_and_platform(self):
        data = encode_claim(self.darwin)
        for wrong in (dict(attempt=2, platform=DARWIN), dict(attempt=1, platform=LINUX)):
            with self.subTest(wrong=wrong):
                self.assertCode("store_entry_body_mismatch", [self.s.register(), self.s.intent(),
                                                              {name(EntryFamily.CLAIM, **wrong): data}])

    def test_claim_denials_surface_the_exact_policy_findings(self):
        registered = [self.s.register()]
        prior = self.prior(registered)
        self.assertDenied("store_claim_denied", [*registered, self.s.claim_file(self.darwin)],
                          decide_platform_claim(candidate=self.s.candidate, history=prior.history, claim=self.darwin))
        planned = [*registered, self.s.intent(), self.s.claim_file(self.darwin)]
        prior = self.prior(planned)
        reused = self.s.claim(LINUX, invocation_id=self.darwin.invocation_id)
        unplanned = self.s.claim(("Darwin", "amd64"))
        foreign = self.s.claim(LINUX, candidate_id="9" * 64)
        for claim, finding in ((reused, "claim_invocation_reused"), (unplanned, "claim_platform_unplanned"),
                               (foreign, "candidate_mismatch")):
            with self.subTest(finding=finding):
                expected = decide_platform_claim(candidate=self.s.candidate, history=prior.history, claim=claim)
                self.assertIn(finding, codes(expected.findings))
                self.assertDenied("store_claim_denied", [*planned, self.s.claim_file(claim)], expected)

    def test_a_read_back_claim_is_observed_and_never_relaunched(self):
        snapshot = self.prior([self.s.register(), self.s.intent(), self.s.claim_file(self.darwin)])
        self.assertEqual(snapshot.history.claims, (self.darwin,))
        again = decide_platform_claim(candidate=self.s.candidate, history=snapshot.history, claim=self.darwin)
        self.assertEqual((again.allowed, codes(again.findings)), (False, {"claim_exists", "claim_invocation_reused"}))


class ProofTests(ReplayCase):
    def claimed(self) -> list[Files]:
        return [self.s.register(), self.s.intent(), self.s.claim_file(self.darwin), self.s.claim_file(self.linux)]

    def assertProofRefused(self, data: bytes, native_code: str, claim: PlatformClaim | None = None) -> None:
        error = self.assertCode("store_proof_invalid", [*self.claimed(), self.s.proof_file(claim or self.linux, data)])
        self.assertIsInstance(error.__cause__, PublicProofError)
        self.assertEqual(error.__cause__.args, (native_code,))

    def test_the_actual_candidate_zip_becomes_its_platform_proof(self):
        snapshot = self.replay([*self.claimed(), self.s.proof_file(self.linux, self.linux_zip)])
        proof = snapshot.history.proofs[0]
        self.assertEqual(((proof.os, proof.arch), proof.attempt, proof.invocation_id, proof.proof_sha256,
                          proof.binaries, proof.closure),
                         (LINUX, 1, self.linux.invocation_id, sha(self.linux_zip), self.linux.binaries,
                          Closure.COMPLETE))
        self.assertEqual({cell.status for cell in proof.cells}, {CellStatus.PASS})

    def test_a_proof_needs_the_claim_of_its_attempt_and_platform(self):
        self.assertCode("store_proof_unclaimed", [self.s.register(), self.s.intent(), self.s.claim_file(self.darwin),
                                                  self.s.proof_file(self.linux, self.linux_zip)])

    def test_tampered_foreign_and_shape_only_proofs_refuse(self):
        flipped = bytearray(self.linux_zip)
        flipped[len(flipped) // 2] ^= 0x01
        with self.assertRaises(VerificationError) as caught:
            self.replay([*self.claimed(), self.s.proof_file(self.linux, bytes(flipped))])
        self.assertEqual(caught.exception.code, "store_proof_invalid")
        self.assertIsInstance(caught.exception.__cause__, PublicProofError)
        self.assertProofRefused(self.darwin_zip, "candidate_identity_mismatch")
        other = other_candidate()
        foreign = self.s.zip(replace(self.linux, candidate_id=other.candidate_id), candidate=other)
        self.assertProofRefused(foreign, "candidate_identity_mismatch")
        shape_only = encode_record({"candidate_id": self.s.id, "attempt": 1, "os": "Linux", "arch": "amd64",
                                    "proof_sha256": sha(self.linux_zip)}, "test")
        error = self.assertCode("store_proof_invalid", [*self.claimed(), self.s.proof_file(self.linux, shape_only)])
        self.assertIsInstance(error.__cause__, PublicProofError)

    def test_failed_and_incomplete_attempts_stay_in_history(self):
        failing = self.s.zip(self.linux, failed=True)
        incomplete = self.replay([*self.claimed(), self.s.proof_file(self.linux, failing)])
        proof = incomplete.history.proofs[0]
        self.assertEqual(proof.closure, Closure.UNRESOLVED)
        self.assertIn(CellStatus.FAIL, {cell.status for cell in proof.cells})
        manifest = self.s.candidate.manifest
        self.assertEqual(evaluate_attempt(manifest, incomplete.history, 1).state, AttemptState.INCOMPLETE)
        failed = self.replay([*self.claimed(), self.s.proof_file(self.darwin, self.darwin_zip),
                              self.s.proof_file(self.linux, failing)])
        self.assertEqual(evaluate_attempt(manifest, failed.history, 1).state, AttemptState.FAIL)


class InstallTests(ReplayCase):
    def prefix(self) -> list[Files]:
        return [self.s.register()]

    def test_the_canonical_reader_binds_the_exact_installer_source(self):
        files, digest = self.s.install(LINUX, InstallChannel.SHELL)
        snapshot = self.replay([*self.prefix(), files], Facts(sources=(self.s.shell_source,)))
        self.assertEqual([install.sha256 for install in snapshot.history.installs], [digest])
        self.assertCode("store_installer_source_missing", [*self.prefix(), files])
        binding, _ = self.s.shell_source
        self.assertCode("store_installer_source_invalid", [*self.prefix(), files],
                        Facts(sources=((binding, b"#!/bin/sh\necho other\n"),)))

    def test_artifact_installers_take_no_source_and_an_unused_source_refuses(self):
        files, _ = self.s.install(LINUX, InstallChannel.NPM)
        self.assertEqual(len(self.replay([*self.prefix(), files]).history.installs), 1)
        self.assertCode("store_installer_source_unused", [*self.prefix(), files], Facts(sources=(self.s.shell_source,)))

    def test_a_witness_from_another_repository_refuses(self):
        files, _ = self.s.install(LINUX, InstallChannel.NPM)
        proof_name, witness_name = sorted(files)
        witness = json.loads(files[witness_name])
        for request in witness["installs"][0]["requests"]:
            request["url"] = request["url"].replace(fx.REPOSITORY, "different-owner/different-project")
        data = encode_record(witness, "test")
        proof = json.loads(files[proof_name])
        proof["witness_sha256"] = sha(data)
        changed = {proof_name: encode_record(proof, "test"), witness_name: data}
        self.assertCode("install_witness_mismatch", [*self.prefix(), changed])

    def test_install_file_names_bind_platform_and_channel(self):
        files, _ = self.s.install(LINUX, InstallChannel.NPM, named=InstallChannel.BREW)
        self.assertCode("store_entry_body_mismatch", [*self.prefix(), files])


class ProvenanceTests(ReplayCase):
    def reviewed(self, data: bytes | None = None) -> list[Files]:
        return [self.s.register(), self.s.digest_file(EntryFamily.REVIEW, data or self.review_bytes)]

    def test_a_review_needs_the_trusted_fact_of_its_actual_introducing_transaction(self):
        snapshot = self.replay(self.reviewed(), Facts(provenance=(self.review_provenance(1),)))
        self.assertEqual(snapshot.reviews[0].sha256, sha(self.review_bytes))
        # Admission never sees the proposed review's provenance: it does not exist before the append.
        self.assertCode("store_review_provenance_missing", self.reviewed(), paired=False)
        self.assertCode("candidate_review_provenance_mismatch", self.reviewed(),
                        Facts(provenance=(self.review_provenance(1, reviewer="other-reviewer"),)), paired=False)
        foreign = Facts(provenance=(self.review_provenance(0),))
        self.assertCode("store_review_provenance_foreign", self.reviewed(), foreign, paired=False)
        with self.assertRaises(VerificationError) as caught:
            self.admit(self.reviewed(), foreign)
        self.assertEqual(caught.exception.code, "store_review_provenance_unused")

    def test_a_proposed_review_needs_and_accepts_no_prospective_provenance(self):
        prefix = raw(self.s.id, [self.s.register()])
        proposal = additions(self.s.digest_file(EntryFamily.REVIEW, self.review_bytes))
        self.assertIsNone(admit_append(prefix, proposal, review_provenance=(), closure_resolutions=(),
                                       installer_sources=()))
        for introducing in (prefix.head, commit(self.s.id, 1)):
            with self.subTest(introducing=introducing), self.assertRaises(VerificationError) as caught:
                admit_append(prefix, proposal, closure_resolutions=(), installer_sources=(),
                             review_provenance=(self.review_provenance(transaction_commit=introducing),))
            self.assertEqual(caught.exception.code, "store_review_provenance_unused")

    def test_an_unused_or_ambiguous_provenance_fact_refuses(self):
        extra = self.review_provenance(0, data=b"other artifact")
        self.assertCode("store_review_provenance_unused", self.reviewed(),
                        Facts(provenance=(self.review_provenance(1), extra)))
        ambiguous = (self.review_provenance(1), self.review_provenance(0))
        self.assertCode("store_review_provenance_invalid", self.reviewed(), Facts(provenance=ambiguous), paired=False)
        with self.assertRaises(VerificationError) as caught:
            admit_append(raw(self.s.id, [self.s.register()]), additions(self.reviewed()[-1]),
                         review_provenance=ambiguous, closure_resolutions=(), installer_sources=())
        self.assertEqual(caught.exception.code, "store_review_provenance_invalid")

    def test_review_records_bind_their_digest_name_and_candidate(self):
        wrong_name = {name(EntryFamily.REVIEW, digest="0" * 64): self.review_bytes}
        self.assertCode("store_entry_digest_mismatch", [self.s.register(), wrong_name])
        foreign = self.s.review(candidate_id="9" * 64)
        self.assertCode("store_record_candidate_mismatch", self.reviewed(foreign),
                        Facts(provenance=(self.review_provenance(1, data=foreign),)))

    def test_replay_never_builds_provenance_or_closure_facts(self):
        source = Path(candidate_store_replay.__file__).read_text(encoding="utf-8")
        tree = ast.parse(source)
        called = {node.func.id for node in ast.walk(tree)
                  if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)}
        self.assertFalse(called & {"ReviewProvenance", "ClosureResolution", "parse_closure_evidence"})
        imported = {node.module for node in ast.walk(tree) if isinstance(node, ast.ImportFrom)}
        imported |= {alias.name for node in ast.walk(tree) if isinstance(node, ast.Import) for alias in node.names}
        self.assertFalse({module for module in imported if module and module.split(".")[-1] in {
            "github", "subprocess", "os", "pathlib", "secrets", "candidate_store", "socket"}})


class TriageTests(ReplayCase):
    """Attempt 1 leaves a failed Linux proof and an unproven Darwin claim; a reviewed triage replaces it."""

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        s = cls.s
        cls.failing_zip = s.zip(cls.linux, failed=True)
        cls.attempt1 = [s.register(), s.intent(), s.claim_file(cls.darwin), s.claim_file(cls.linux),
                        s.proof_file(cls.linux, cls.failing_zip)]
        prior = replay_candidate(raw(s.id, cls.attempt1), review_provenance=(), closure_resolutions=(),
                                 installer_sources=())
        cls.prior_history = prior.history
        unresolved = prior.history.proofs[0].unresolved_resources
        cls.proof_closure = fx.resolution(ProofSubject(sha(cls.failing_zip)), invocation=cls.linux.invocation_id,
                                          resources=tuple((item, WAITED) for item in unresolved))
        cls.claim_closure = fx.resolution(ClaimSubject(claim_sha256(cls.darwin)), invocation=cls.darwin.invocation_id,
                                          resources=((NATIVE_CHILD_RESOURCE, WAITED),))
        cls.explained = evaluate_attempt(s.candidate.manifest, prior.history, 1).findings
        cls.closures = (cls.proof_closure, cls.claim_closure)

    def triage_bytes(self, *, approved: bool = True, closures: tuple[ClosureResolution, ...] | None = None) -> bytes:
        return encode_triage(self.triage_record(approved=approved, closures=closures))

    def triage_record(self, *, approved: bool = True,
                      closures: tuple[ClosureResolution, ...] | None = None) -> TriageRecord:
        closures = self.closures if closures is None else closures
        return TriageRecord(
            candidate_id=self.s.id, attempt=1,
            prior_claim_sha256s=tuple(sorted((claim_sha256(self.darwin), claim_sha256(self.linux)))),
            prior_proof_sha256s=(sha(self.failing_zip),), explained_findings=self.explained,
            diagnosis_sha256s=fx.DIAGNOSIS, unresolved_product_findings=(), reviewer=fx.REVIEWER,
            closure_resolutions=tuple(sorted((fx.reference(item) for item in closures), key=lambda ref: ref.key)),
            approval=fx.approval(attempt=2, platforms=PLANNED) if approved else None)

    def triage_files(self, data: bytes, stored: tuple[ClosureResolution, ...]) -> Files:
        files = self.s.digest_file(EntryFamily.TRIAGE, data)
        for item in stored:
            files.update(self.s.digest_file(EntryFamily.CLOSURE, encode_closure_evidence(item)))
        return files

    def provenance(self, data: bytes, index: int | None = None) -> ReviewProvenance:
        """The trusted introducing-transaction fact of a stored triage (test input only)."""
        return ReviewProvenance(artifact_sha256=sha(data), reviewer=fx.REVIEWER,
                                transaction_commit=commit(self.s.id, len(self.attempt1) if index is None else index))

    def replaced(self, data: bytes, stored: tuple[ClosureResolution, ...]) -> list[Files]:
        return [*self.attempt1, self.triage_files(data, stored), self.s.intent(2, triage_sha256=sha(data))]

    def test_a_reviewed_triage_with_trusted_closures_permits_the_replacement(self):
        data = self.triage_bytes()
        closures = (self.proof_closure, self.claim_closure)
        snapshot = self.replay(self.replaced(data, closures),
                               Facts(provenance=(self.provenance(data),), closures=closures))
        triage = snapshot.history.triages[0]
        self.assertEqual((triage.sha256, triage.provenance, triage.resolutions),
                         (sha(data), self.provenance(data),
                          tuple(sorted(closures, key=lambda item: item.evidence_sha256))))
        self.assertEqual([intent.attempt for intent in snapshot.history.intents], [1, 2])
        self.assertEqual(len(snapshot.history.proofs), 1)

    def test_missing_forged_or_unreferenced_closure_facts_refuse(self):
        data = self.triage_bytes()
        stored = (self.proof_closure, self.claim_closure)
        groups = [*self.attempt1, self.triage_files(data, stored)]
        provenance = (self.provenance(data),)
        self.assertCode("store_closure_unverified", groups, Facts(provenance=provenance))
        forged = fx.resolution(self.claim_closure.subject, invocation=self.darwin.invocation_id,
                               resources=((NATIVE_CHILD_RESOURCE, ResourceDisposition.NOT_STARTED),))
        self.assertCode("store_closure_unverified", groups,
                        Facts(provenance=provenance, closures=(self.proof_closure, forged)))
        self.assertCode("store_closure_resolution_unused", groups,
                        Facts(provenance=provenance, closures=(*stored, forged)))
        bare = self.triage_bytes(closures=())
        self.assertCode("store_closure_unreferenced", [*self.attempt1, self.triage_files(bare, stored)],
                        Facts(provenance=(self.provenance(bare),), closures=stored))

    def test_a_referenced_but_unstored_closure_is_left_to_the_replacement_policy(self):
        data = self.triage_bytes()
        groups = self.replaced(data, (self.proof_closure,))
        facts = Facts(provenance=(self.provenance(data),), closures=(self.proof_closure,))
        prior = self.replay(groups[:-1], facts)
        self.assertEqual(prior.history.triages[0].resolutions, (self.proof_closure,))
        intent = fx.intent(self.s.candidate, attempt=2, platforms=PLANNED, triage_sha256=sha(data))
        expected = decide_launch(candidate=self.s.candidate, history=prior.history, intent=intent)
        self.assertIn("closure_resolution_unverified", codes(expected.findings))
        self.assertDenied("store_launch_denied", groups, expected, facts)

    def test_an_unapproved_triage_denies_the_replacement_with_policy_findings(self):
        data = self.triage_bytes(approved=False)
        closures = (self.proof_closure, self.claim_closure)
        facts = Facts(provenance=(self.provenance(data),), closures=closures)
        groups = self.replaced(data, closures)
        prior = self.replay(groups[:-1], facts)
        intent = fx.intent(self.s.candidate, attempt=2, platforms=PLANNED, triage_sha256=sha(data))
        expected = decide_launch(candidate=self.s.candidate, history=prior.history, intent=intent)
        self.assertIn("replacement_unapproved", codes(expected.findings))
        self.assertDenied("store_launch_denied", groups, expected, facts)

    def test_a_triage_needs_its_introducing_provenance(self):
        data = self.triage_bytes()
        closures = (self.proof_closure, self.claim_closure)
        groups = [*self.attempt1, self.triage_files(data, closures)]
        self.assertCode("store_review_provenance_missing", groups, Facts(closures=closures), paired=False)
        self.assertIsNone(self.admit(groups, Facts(closures=closures)))
        foreign = replace(self.provenance(data), transaction_commit=commit(self.s.id, 0))
        self.assertCode("store_review_provenance_foreign", groups, Facts(provenance=(foreign,), closures=closures),
                        paired=False)

    def test_a_future_attempt_triage_refuses_before_any_append(self):
        future = encode_triage(replace(self.triage_record(approved=False), attempt=2))
        groups = [*self.attempt1, self.triage_files(future, self.closures)]
        error = self.assertCode("candidate_history_invalid", groups,
                                Facts(provenance=(self.provenance(future),), closures=self.closures))
        self.assertEqual(error.stage, HISTORY_STAGE)

    def test_an_admitted_triage_replays_to_its_materialized_history(self):
        data = self.triage_bytes()
        groups = [*self.attempt1, self.triage_files(data, self.closures)]
        snapshot = self.replay(groups, Facts(provenance=(self.provenance(data),), closures=self.closures))
        reviewed = ReviewedTriage(record=self.triage_record(), provenance=self.provenance(data),
                                  resolutions=tuple(sorted(self.closures, key=lambda item: item.evidence_sha256)))
        self.assertEqual(snapshot.history, replace(self.prior_history, triages=(reviewed,)))

    def test_a_later_triage_resolves_closures_an_earlier_triage_stored(self):
        first, second = self.triage_bytes(approved=False), self.triage_bytes()
        groups = [*self.attempt1, self.triage_files(first, self.closures),
                  self.s.digest_file(EntryFamily.TRIAGE, second), self.s.intent(2, triage_sha256=sha(second))]
        facts = Facts(provenance=(self.provenance(first), self.provenance(second, len(self.attempt1) + 1)),
                      closures=self.closures)
        self.assertIsNone(self.admit(groups[:-1], facts))
        snapshot = self.replay(groups, facts)
        self.assertEqual(snapshot.history.triages[1].resolutions,
                         tuple(sorted(self.closures, key=lambda item: item.evidence_sha256)))
        self.assertEqual([intent.attempt for intent in snapshot.history.intents], [1, 2])

    def test_admission_accounts_prefix_and_proposed_closure_facts_once(self):
        data = self.triage_bytes()
        groups = [*self.attempt1, self.triage_files(data, (self.proof_closure,))]
        self.assertIsNone(self.admit(groups, Facts(closures=(self.proof_closure,))))
        self.assertCode("store_closure_resolution_unused", groups,
                        Facts(provenance=(self.provenance(data),), closures=self.closures))
        review = [*self.attempt1, self.s.digest_file(EntryFamily.REVIEW, self.review_bytes)]
        with self.assertRaises(VerificationError) as caught:
            self.admit(review, Facts(closures=(self.proof_closure,)))
        self.assertEqual(caught.exception.code, "store_closure_resolution_unused")


class AdmissionTests(ReplayCase):
    """Admission-only refusals: the structural proposal itself, prospective facts and the public surface."""

    def admitted(self, prefix: RawCandidateSnapshot, proposal: tuple[StoredBlob, ...], facts: Facts = Facts()) -> None:
        return admit_append(prefix, proposal, review_provenance=facts.provenance,
                            closure_resolutions=facts.closures, installer_sources=facts.sources)

    def assertRefused(self, code: str, prefix: RawCandidateSnapshot, proposal: tuple[StoredBlob, ...],
                      facts: Facts = Facts()) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.admitted(prefix, proposal, facts)
        self.assertEqual(caught.exception.code, code)

    def test_a_proposal_must_add_exactly_new_canonical_entries(self):
        proven = raw(self.s.id, [self.s.register(), self.s.intent(), self.s.claim_file(self.darwin),
                                 self.s.claim_file(self.linux), self.s.proof_file(self.linux, self.linux_zip)])
        failing = self.s.zip(self.linux, failed=True)
        npm = self.s.install(LINUX, InstallChannel.NPM)[0]
        installed = raw(self.s.id, [self.s.register(), npm])
        claimed = raw(self.s.id, [self.s.register(), self.s.intent(), self.s.claim_file(self.darwin)])
        cases = (
            ("store_candidate_tree_invalid", proven, additions(self.s.proof_file(self.linux, failing))),
            ("store_transition_invalid", proven, additions(self.s.proof_file(self.linux, self.linux_zip))),
            ("store_transition_invalid", proven, additions(self.s.register())),
            ("store_candidate_tree_invalid", proven, additions(Scenario(other_candidate()).register())),
            ("store_candidate_tree_invalid", installed,
             additions(self.s.install(LINUX, InstallChannel.BREW, named=InstallChannel.NPM)[0])),
            ("store_transition_invalid", installed, additions(npm)),
            ("store_transition_invalid", proven, ()),
            ("store_transition_invalid", claimed,
             additions(self.s.claim_file(self.darwin)) + additions(self.s.claim_file(self.linux))),
            ("store_transition_invalid", proven, additions(self.s.intent(2, triage_sha256="0" * 64)) * 2),
        )
        for code, prefix, proposal in cases:
            with self.subTest(code=code, names=[blob.entry.name for blob in proposal]):
                self.assertRefused(code, prefix, proposal)

    def test_installer_sources_are_accounted_across_prefix_and_proposal(self):
        shell = self.s.shell_source
        second_platform = self.ready[:10]
        linux_shell = name(EntryFamily.INSTALL_PROOF, platform=LINUX, channel=InstallChannel.SHELL)
        self.assertIn(linux_shell, second_platform[-1])
        self.assertIsNone(self.admit(second_platform, Facts(sources=(shell,))))
        unrelated = SourceFileBinding(source_commit=fx.COMMIT, repo_path="other.sh", sha256=sha(b"other\n"))
        self.assertCode("store_installer_source_unused", second_platform,
                        Facts(sources=(shell, (unrelated, b"other\n"))))

    def test_a_freeze_is_admitted_only_on_the_exact_verified_head(self):
        interleaved = raw(self.s.id, self.ready, head=FOREIGN_COMMIT)
        freeze = additions({name(EntryFamily.PUBLICATION): encode_publication(
            self.publication(evidence_parent=FOREIGN_COMMIT))})
        self.assertIsNone(self.admitted(interleaved, freeze, self.ready_facts()))
        self.assertRefused("store_publication_mismatch", interleaved,
                           additions({name(EntryFamily.PUBLICATION): encode_publication(self.publication())}),
                           self.ready_facts())

    def test_admission_is_the_only_new_public_surface_and_returns_no_authority(self):
        tree = ast.parse(Path(candidate_store_replay.__file__).read_text(encoding="utf-8"))
        public = {target.id for node in tree.body if isinstance(node, ast.Assign) for target in node.targets
                  if isinstance(target, ast.Name)}
        public |= {node.name for node in tree.body if isinstance(node, (ast.FunctionDef, ast.ClassDef))}
        self.assertEqual({item for item in public if not item.startswith("_")},
                         {"STAGE", "ReplayDenied", "CandidateSnapshot", "replay_candidate", "admit_append"})
        signature = inspect.signature(admit_append)
        self.assertEqual([(item.name, item.kind) for item in signature.parameters.values()],
                         [("raw", inspect.Parameter.POSITIONAL_OR_KEYWORD),
                          ("additions", inspect.Parameter.POSITIONAL_OR_KEYWORD),
                          *((key, inspect.Parameter.KEYWORD_ONLY)
                            for key in ("review_provenance", "closure_resolutions", "installer_sources"))])
        self.assertEqual(signature.return_annotation, "None")

    def test_admission_holds_the_store_history_bound(self):
        """The append that fills the bound is admitted and replays; the next one refuses like its resulting history.

        One snapshot at the bound serves both edges: the structural check of a
        history this long is the costly part of the test.
        """
        groups, provenance = [self.s.register()], []
        for index in range(1, MAX_CANDIDATE_COMMITS + 1):
            data = self.s.review(head_commit=commit("reviewed-head", index))
            groups.append(self.s.digest_file(EntryFamily.REVIEW, data))
            provenance.append(self.review_provenance(index, data=data))
        at_bound = raw(self.s.id, groups[:MAX_CANDIDATE_COMMITS])
        last = at_bound.transactions[-1]
        before = replace(at_bound, head=last.parent, transactions=at_bound.transactions[:-1])
        self.assertIsNone(self.admitted(before, last.blobs, Facts(provenance=tuple(provenance[:-2]))))
        snapshot = replay_candidate(at_bound, review_provenance=tuple(provenance[:-1]), closure_resolutions=(),
                                    installer_sources=())
        self.assertEqual((snapshot.head, len(snapshot.reviews)), (last.commit, MAX_CANDIDATE_COMMITS - 1))
        beyond = additions(groups[-1])
        with self.assertRaises(VerificationError) as refused:
            self.admitted(at_bound, beyond, Facts(provenance=tuple(provenance[:-1])))
        self.assertEqual(refusal(refused.exception), ("store_history_too_long", candidate_store_replay.STAGE, None))
        child = at_bound.entries | frozenset(blob.entry for blob in beyond)
        resulting = RawTransaction(commit=commit(self.s.id, MAX_CANDIDATE_COMMITS), parent=last.commit,
                                   parent_tree=last.tree, tree=git_tree_sha(tuple(child)),
                                   transition=classify_transition(at_bound.entries, child), blobs=beyond)
        with self.assertRaises(VerificationError) as unrepresentable:
            replace(at_bound, head=resulting.commit, transactions=at_bound.transactions + (resulting,))
        self.assertEqual(refusal(unrepresentable.exception), ("store_snapshot_invalid", STORE_STAGE, None))


class FreezeTests(ReplayCase):
    def test_the_freeze_equals_the_recomputed_authorization(self):
        snapshot = self.replay(self.frozen(), self.ready_facts())
        self.assertEqual(snapshot.history.publication, self.publication())

    def test_every_changed_freeze_field_refuses(self):
        other = ("0" * 64, "1" * 64)
        changes = (dict(candidate_id="9" * 64), dict(attempt=2, triage_sha256s=("0" * 64,)),
                   dict(evidence_parent=FOREIGN_COMMIT), dict(source_commit="9" * 40),
                   dict(source_tree="9" * 40), dict(proof_sha256s=other), dict(install_sha256s=other))
        for change in changes:
            with self.subTest(change=change):
                self.assertCode("store_publication_mismatch", self.frozen(self.publication(**change)),
                                self.ready_facts())
        self.assertCode("store_publication_review_missing",
                        self.frozen(self.publication(review_sha256="0" * 64)), self.ready_facts())

    def test_the_freeze_binds_its_actual_parent_across_interleaved_commits(self):
        freeze = len(self.ready)
        record = self.publication(evidence_parent=FOREIGN_COMMIT)
        snapshot = self.replay(self.frozen(record), self.ready_facts(), parents={freeze: FOREIGN_COMMIT})
        self.assertEqual(snapshot.history.publication.evidence_parent, FOREIGN_COMMIT)
        self.assertCode("store_publication_mismatch", self.frozen(), self.ready_facts(),
                        parents={freeze: FOREIGN_COMMIT})

    def test_the_freeze_selects_the_exact_referenced_review(self):
        unresolved = self.s.review(unresolved_findings=("finding-1",))
        groups = [*self.ready, self.s.digest_file(EntryFamily.REVIEW, unresolved)]
        record = self.publication(evidence_parent=commit(self.s.id, len(self.ready)), review_sha256=sha(unresolved))
        facts = self.ready_facts(self.review_provenance(len(self.ready), data=unresolved))
        self.assertCode("publication_not_ready", [*groups, {name(EntryFamily.PUBLICATION): encode_publication(record)}],
                        facts)
        accepted = self.publication(evidence_parent=commit(self.s.id, len(self.ready)))
        snapshot = self.replay([*groups, {name(EntryFamily.PUBLICATION): encode_publication(accepted)}], facts)
        self.assertEqual(snapshot.history.publication.review_sha256, sha(self.review_bytes))

    def test_a_freeze_before_readiness_refuses(self):
        before = self.ready[:6] + [self.ready[-1]]
        record = self.publication(evidence_parent=commit(self.s.id, 6))
        facts = Facts(provenance=(self.review_provenance(6),))
        self.assertCode("publication_not_ready",
                        [*before, {name(EntryFamily.PUBLICATION): encode_publication(record)}], facts)

    def test_no_candidate_write_follows_the_freeze(self):
        later = (self.s.digest_file(EntryFamily.REVIEW, self.s.review(unresolved_findings=("finding-1",))),
                 self.s.intent(2, triage_sha256="0" * 64), self.s.claim_file(self.s.claim(("Darwin", "amd64"))))
        for files in later:
            with self.subTest(files=sorted(files)):
                self.assertCode("store_candidate_frozen", [*self.frozen(), files], self.ready_facts())

    def test_an_unrelated_candidate_replays_separately(self):
        other = Scenario(other_candidate())
        groups = [other.register(), other.intent(), other.claim_file(other.claim(DARWIN))]
        snapshot = self.replay(groups, candidate_id=other.id,
                               parents={0: commit(self.s.id, len(self.ready)), 2: commit(self.s.id, 3)})
        self.assertEqual((snapshot.candidate, len(snapshot.history.claims), snapshot.history.publication),
                         (other.candidate, 1, None))
        self.assertCode("store_review_provenance_unused", groups, Facts(provenance=(self.review_provenance(),)),
                        candidate_id=other.id)
        self.assertCode("store_candidate_path_mismatch", [self.s.register()], candidate_id=other.id)


if __name__ == "__main__":
    unittest.main()
