"""Candidate proofs produced from the canonical release fixture and one real native child.

The release fixture owns producing real compact cases through the archive
verifier, native finalizer, staging and projection. Inside its private
lifetime, the canonical child-observation fixture runs one real finite child
whose natural Wait ``observe_child_completion`` observes. The candidate
producer then stages that completion with the same aggregate and builds the
candidate proof. Every changed member, compact receipt or completion is a test
mutation, never producer output. The candidate archive, harness and binary
facts are trusted expectations only; nothing here observes them.
"""

from __future__ import annotations

from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, replace
from functools import cache
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_evidence.candidate_proof import (
    candidate_manifest_document, candidate_proof_document, completion_record,
)
from scripts.delegate_acceptance.native_evidence.contract import AGGREGATE_RECEIPT_NAME, EvidenceError
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CANDIDATE_CASE_SCHEMA_VERSION, CASE_DEFINITIONS, PUBLIC_MEMBER_NAMES, CandidateProofExpectation,
    NativeCompletion, ProofExpectation, ValidatedCandidateProof, canonical_json, canonical_public_zip,
    read_public_zip, sha256,
)
from scripts.delegate_acceptance.native_evidence.public_projection import (
    _project_case, build_candidate_proof, build_public_proof,
)
from scripts.delegate_acceptance.native_evidence.public_stage import StageSnapshot, load_stage
from scripts.delegate_acceptance.native_evidence.staging import stage_candidate_evidence
from scripts.delegate_acceptance.native_release import execution
from scripts.delegate_acceptance.release_verification.contract import BinaryDigests
from scripts.test_native_release_execution import ChildObservationFixture
from scripts.test_public_native_proof import _PublicNativeFixture


Change = Callable[[dict[str, object]], None]
AGY_CASE = next(name for name in PUBLIC_MEMBER_NAMES if "-antigravity-" in name)


@dataclass(frozen=True)
class CandidateSource:
    """A live canonical release-fixture source and one observed real native completion."""

    root: Path
    aggregate: Path
    legacy_stage: Path
    legacy_expectation: ProofExpectation
    completion: NativeCompletion


def observed_completion(exit_code: int) -> NativeCompletion:
    """Run one real finite child through the canonical owner and observe its natural Wait.

    The child-observation fixture owns the invocation tree, the child and their cleanup.
    Its registered cleanups run after setup, the child or the observation fail;
    that failure propagates unchanged, with a note if cleanup also failed. A
    completion is returned only after every cleanup succeeded. Outside a test
    run, unittest reports a cleanup failure only through ``doCleanups()``
    returning False, not the cleanup's own exception.
    """
    owner = ChildObservationFixture()
    try:
        owner.setUp()
        owner.child(code=exit_code)
        completion = execution.observe_child_completion(owner.receipt, "native")
    except BaseException as failure:
        if not owner.doCleanups():
            failure.add_note("candidate fixture cleanup also failed")
        raise
    if not owner.doCleanups():
        raise AssertionError("candidate fixture cleanup failed; its observed completion is not returned")
    return completion


@contextmanager
def candidate_source(*, failed: bool) -> Iterator[CandidateSource]:
    """Yield a live release-fixture aggregate and a completion that exited as it did.

    ``failed`` makes the fixture's second case fail. Everything is removed on exit.
    """
    with tempfile.TemporaryDirectory(prefix="candidate-proof-fixture-") as directory:
        root = Path(directory).resolve(strict=True)
        producer = _PublicNativeFixture(root, failed_index=1 if failed else None)
        stage = producer.build_stage()
        if producer.expectation is None:
            raise AssertionError("release fixture did not derive its expectation")
        aggregate = root / "aggregate" / AGGREGATE_RECEIPT_NAME
        exit_code = json.loads(aggregate.read_bytes())["exit_code"]
        yield CandidateSource(root, aggregate, stage, producer.expectation, observed_completion(exit_code))


@dataclass(frozen=True)
class ReleaseProof:
    """The release proof and the produced candidate proof of one canonical fixture run.

    ``facts`` is the trusted candidate expectation the producer was given.
    """

    data: bytes
    expectation: ProofExpectation
    snapshot: StageSnapshot
    facts: CandidateProofExpectation
    completion: NativeCompletion
    candidate: bytes
    validated: ValidatedCandidateProof

    def members(self) -> dict[str, bytes]:
        return read_public_zip(self.data)

    def candidate_members(self) -> dict[str, bytes]:
        return read_public_zip(self.candidate)

    @property
    def candidate_cases(self) -> tuple[tuple[str, bytes], ...]:
        return tuple(sorted((name, data) for name, data in self.candidate_members().items()
                            if name.startswith("cases/")))

    def aggregate_cases(self) -> list[object]:
        cases = json.loads(self.snapshot.data[AGGREGATE_RECEIPT_NAME])["cases"]
        if not isinstance(cases, list):
            raise AssertionError("fixture aggregate cases missing")
        return cases

    def record(self, name: str) -> dict[str, object]:
        value = json.loads(self.members()[name])
        if not isinstance(value, dict):
            raise AssertionError("fixture member is not an object")
        return value


@cache
def release_proof(*, failed: bool) -> ReleaseProof:
    """Produce both proofs once; ``failed`` makes the fixture's second case fail."""
    with candidate_source(failed=failed) as source:
        output = source.root / "release-proof.zip"
        build_public_proof(source.legacy_stage, output, source.legacy_expectation)
        data = output.read_bytes()
        facts = candidate_facts(data, source.legacy_expectation, source.completion.invocation_id)
        candidate_stage = source.root / "candidate-stage"
        stage_candidate_evidence(source.aggregate, candidate_stage, completion=source.completion,
                                 expectation=facts)
        candidate_output = source.root / "candidate-proof.zip"
        validated = build_candidate_proof(candidate_stage, candidate_output, facts)
        return ReleaseProof(data, source.legacy_expectation, load_stage(source.legacy_stage), facts,
                            source.completion, candidate_output.read_bytes(), validated)


def _tree(root: Path) -> dict[str, str]:
    return {path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(root.rglob("*")) if path.is_file()}


class LiveSourceCase(unittest.TestCase):
    """One live release-fixture source per class; tests never change its bytes."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.source = cls.enterClassContext(candidate_source(failed=False))
        release = cls.source.root / "release-proof-for-facts.zip"
        build_public_proof(cls.source.legacy_stage, release, cls.source.legacy_expectation)
        cls.facts = candidate_facts(release.read_bytes(), cls.source.legacy_expectation,
                                    cls.source.completion.invocation_id)
        release.unlink()
        cls.original = _tree(cls.source.aggregate.parent)

    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="candidate-stage-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)

    def tearDown(self) -> None:
        self.assertEqual(_tree(self.source.aggregate.parent), self.original)

    def stage(self, output: Path | None = None, **changes: object) -> Path:
        output = output or self.root / "stage"
        options = {"completion": self.source.completion, "expectation": self.facts, **changes}
        stage_candidate_evidence(self.source.aggregate, output, **options)
        return output

    def owned_output(self) -> tuple[Path, dict[str, object]]:
        parent = self.root / "owned-parent"
        parent.mkdir()
        output = parent / "stage"
        owner = DirectoryOwner()
        try:
            return output, directory_identity_record(owner.create("candidate stage", output, parent))
        finally:
            owner.release()

    def assertEvidence(self, code: str, output: Path, **changes: object) -> None:
        with self.assertRaises(EvidenceError) as caught:
            self.stage(output, **changes)
        self.assertEqual(caught.exception.args, (code,))
        self.assertFalse(output.exists())

    def copy(self, stage: Path) -> Path:
        target = self.root / f"copy-{len(list(self.root.iterdir()))}"
        shutil.copytree(stage, target, symlinks=True)
        return target


def project_compact(release: ReleaseProof, index: int, *, case_schema_version: int,
                    change: Change | None = None) -> dict[str, object]:
    """Project one staged compact receipt through the shared projector.

    ``change`` mutates the compact receipt first and rebinds its digest; the
    result is a projection of a test mutation, not of producer output.
    """
    definition = CASE_DEFINITIONS[index]
    path = f"case-receipts/{definition.case_id}.json"
    compact = json.loads(release.snapshot.data[path])
    if change is not None:
        change(compact)
    encoded = canonical_json(compact)
    reference = next(case for case in release.aggregate_cases()
                     if isinstance(case, dict) and case.get("case_id") == definition.case_id)
    reference = {**reference, "status": compact["status"], "exit_code": compact["exit_code"],
                 "receipt_sha256": sha256(encoded)}
    snapshot = replace(release.snapshot, digests={**release.snapshot.digests, path: sha256(encoded)})
    return _project_case(definition, reference, encoded, snapshot, case_schema_version=case_schema_version)


def blocked_before_runtime(version: int, reason: object) -> Change:
    """Rewrite a FAIL compact receipt as a BLOCKED-before-runtime one of ``version``."""
    def change(compact: dict[str, object]) -> None:
        compact.update(status="BLOCKED", exit_code=2, schema_version=version)
        if version == CANDIDATE_CASE_SCHEMA_VERSION:
            compact["block_reason"] = reason
        else:
            compact.pop("block_reason", None)
        proof = compact["source_proof"]
        if not isinstance(proof, dict) or not isinstance(proof.get("binary_identities"), dict):
            raise AssertionError("fixture compact proof missing")
        proof.update({
            "state": "blocked-before-runtime", "final_status": "BLOCKED", "source_status": "blocked",
            "task_ids": [], "task_session_bindings": [], "diagnostic_task_ids": [],
            "session_ids": {"state": "not-recorded", "values": [], "by_alias": {}},
        })
        proof["binary_identities"].update(state="unavailable", sha256={}, task_ids=[],
                                          provider_runtime_identities={})
    return change


def agy_case_name(release: ReleaseProof) -> str:
    return AGY_CASE


def candidate_facts(release_zip: bytes, source: ProofExpectation, invocation_id: str) -> CandidateProofExpectation:
    """Trusted candidate facts whose models and binaries are the ones the fixture cases recorded."""
    members = read_public_zip(release_zip)
    agy_proof = json.loads(members[AGY_CASE])["source_proof"]
    archive = json.loads(members["proof.json"])["identity"]["archive"]
    if not isinstance(agy_proof, dict) or not isinstance(archive, dict):
        raise AssertionError("fixture release proof missing")
    return CandidateProofExpectation(
        repository=source.repo, candidate_id="c" * 64, run_id=901, run_attempt=1, artifact_id=77,
        artifact_archive_sha256="f" * 64, source_commit=source.release_revision, source_tree="b" * 40,
        platform_os=source.platform_os, platform_arch=source.platform_arch,
        archive_name=str(archive["name"]), archive_sha256=source.archive_sha256,
        claim_sha256="e" * 64, invocation_id=invocation_id,
        harness_revision=source.harness_revision, harness_sha256=source.source_harness_sha256,
        binaries=BinaryDigests(delegate=source.delegate_sha256, delegate_run=source.runner_sha256,
                               pueue=source.pueue_sha256, pueued=source.pueued_sha256),
        opencode_model=source.opencode_model, ccr_model=source.ccr_model,
        agy_model=None if agy_proof["requested_model"] is None else str(agy_proof["requested_model"]),
    )


def expectation(release: ReleaseProof, **changes: object) -> CandidateProofExpectation:
    """Return the producer's trusted facts, optionally changed."""
    return replace(release.facts, **changes)


def completion(release: ReleaseProof, facts: CandidateProofExpectation) -> NativeCompletion:
    """The observed completion, attributed to ``facts``' invocation (a mutation when it differs)."""
    return replace(release.completion, invocation_id=facts.invocation_id)


def candidate_zip(release: ReleaseProof, facts: CandidateProofExpectation, *,
                  native: NativeCompletion | None = None, proof_change: Change | None = None,
                  case_changes: dict[str, Change] | None = None) -> bytes:
    """Re-encode the produced candidate proof for ``facts``, optionally changing members first.

    With the producer's facts and no change, this reproduces the produced bytes.
    """
    entries = _changed_cases(release.candidate_members(), case_changes or {})
    aggregate = json.loads(entries["proof.json"])["aggregate"]
    native = native or completion(release, facts)
    document = json.loads(candidate_proof_document(facts, native, aggregate))
    if proof_change is not None:
        proof_change(document)
    entries["proof.json"] = canonical_json(document)
    return repack(release, entries, native=native)


def _changed_cases(entries: dict[str, bytes], case_changes: dict[str, Change]) -> dict[str, bytes]:
    for name, change in case_changes.items():
        case = json.loads(entries[name])
        change(case)
        entries[name] = canonical_json(case)
    return entries


def release_with(release: ReleaseProof, case_changes: dict[str, Change], *,
                 proof_change: Change | None = None) -> bytes:
    """Return the release proof with changed members under a re-digested release manifest."""
    entries = _changed_cases(release.members(), case_changes)
    if proof_change is not None:
        proof = release.record("proof.json")
        proof_change(proof)
        entries["proof.json"] = canonical_json(proof)
    manifest = release.record("manifest.json")
    manifest["projected_members"] = [{"path": name, "sha256": sha256(entries[name])}
                                     for name in PUBLIC_MEMBER_NAMES[1:]]
    return canonical_public_zip({**entries, "manifest.json": canonical_json(manifest)})


def repack(release: ReleaseProof, entries: dict[str, bytes], *,
           native: NativeCompletion | None = None) -> bytes:
    """Re-encode members under the produced candidate manifest's source digests.

    A supplied completion rebinds the completion digest to that record.
    """
    source = json.loads(release.candidate_members()["manifest.json"])["source_sha256"]
    if native is not None:
        source["native_completion"] = sha256(canonical_json(completion_record(native)))
    return canonical_public_zip({**entries, "manifest.json": candidate_manifest_document(source, entries)})
