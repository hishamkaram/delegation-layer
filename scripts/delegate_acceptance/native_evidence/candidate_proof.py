"""Closed candidate native-proof envelope: its codec and its only validator.

A candidate proof ZIP has the canonical public members of a release proof but
its own manifest and proof kinds, so neither validator accepts the other's
ZIP. Its identity is the caller's trusted expectation: the candidate, its
immutable Actions locator, the source, the claimed platform invocation, the
expected archive, native harness and bundled binaries, and every selected
model. The proof binds those expected facts; it does not itself observe the
archive or harness. Its completion is the sanitized natural Wait of the native
child, and the manifest binds that record's digest. Its cases use the
candidate public case format, which carries the typed block reason. Every case
passes the shared case predicates; nothing falls back to BLOCKED, so any
invalid shape, binding, reason or predicate refuses. A case whose evidence or
owned lifetime closure stayed unproven is reported unresolved, never closed by
default.
"""

from __future__ import annotations

from .contract import EvidenceError
from .lifetime_closure import case_lifetime_closed
from .public_case_validation import (
    CaseContext,
    agy_case_model_bound,
    case_privacy_keys,
    require_aggregate_verdict,
    require_case_counts,
    validate_aggregate_shape,
    validate_cases,
)
from .public_contract import (
    CANDIDATE_CASE_SCHEMA_VERSION,
    CANDIDATE_MANIFEST_KIND,
    CANDIDATE_PROOF_KIND,
    CASE_IDS,
    PUBLIC_MEMBER_NAMES,
    PUBLIC_SCHEMA_VERSION,
    CandidateProofExpectation,
    CaseOutcome,
    NativeCompletion,
    PublicProofError,
    ValidatedCandidateProof,
    canonical_json,
    parse_canonical,
    parse_json,
    privacy_walk,
    public_binaries,
    read_public_zip,
    require_object,
    require_projected_members,
    require_sha256,
    sha256,
)


# The candidate stage's sources: its manifest, aggregate and sanitized native
# completion, and the case receipts and cleanup journals the cases bind.
CANDIDATE_SOURCE_KEYS = frozenset({
    "stage_manifest", "aggregate_receipt", "native_completion", "case_receipts", "cleanup_journals",
})


_PROOF_KEYS = {"schema_version", "kind", "identity", "completion", "aggregate"}


_MANIFEST_KEYS = {"schema_version", "kind", "source_sha256", "projected_members"}


_COMPLETION_KEYS = {
    "invocation_id", "natural_wait", "exit_code", "started_sha256", "invocation_sha256",
    "completed_sha256",
}


def candidate_identity_record(expectation: CandidateProofExpectation) -> dict[str, object]:
    """Return the only identity record a proof for ``expectation`` may carry."""
    return {
        "repository": expectation.repository, "candidate_id": expectation.candidate_id,
        "actions": {
            "run_id": expectation.run_id, "run_attempt": expectation.run_attempt,
            "artifact_id": expectation.artifact_id,
            "artifact_archive_sha256": expectation.artifact_archive_sha256,
        },
        "source": {"commit": expectation.source_commit, "tree": expectation.source_tree},
        "platform": {"os": expectation.platform_os, "arch": expectation.platform_arch},
        "archive": {"name": expectation.archive_name, "sha256": expectation.archive_sha256},
        "claim": {"sha256": expectation.claim_sha256, "invocation_id": expectation.invocation_id},
        "harness": {"revision": expectation.harness_revision, "sha256": expectation.harness_sha256},
        "binaries": public_binaries(expectation.binaries),
        "models": {
            "opencode": expectation.opencode_model, "ccr": expectation.ccr_model,
            "agy": expectation.agy_model,
        },
    }


def completion_record(completion: NativeCompletion) -> dict[str, object]:
    return {
        "invocation_id": completion.invocation_id, "natural_wait": completion.natural_wait,
        "exit_code": completion.exit_code, "started_sha256": completion.started_sha256,
        "invocation_sha256": completion.invocation_sha256,
        "completed_sha256": completion.completed_sha256,
    }


def parse_completion(value: object) -> NativeCompletion:
    record = require_object(value, _COMPLETION_KEYS, "candidate_completion_invalid")
    return NativeCompletion(
        invocation_id=record["invocation_id"], natural_wait=record["natural_wait"],
        exit_code=record["exit_code"], started_sha256=record["started_sha256"],
        invocation_sha256=record["invocation_sha256"], completed_sha256=record["completed_sha256"],
    )


def require_completion_binding(completion: NativeCompletion, expectation: CandidateProofExpectation, *,
                               exit_code: object) -> None:
    """The completion is the expected invocation's, and its exit code is the aggregate's."""
    if completion.invocation_id != expectation.invocation_id:
        raise PublicProofError("candidate_completion_invocation_mismatch")
    if type(exit_code) is not int or completion.exit_code != exit_code:
        raise PublicProofError("candidate_completion_exit_mismatch")


def require_harness_revision(aggregate: dict[str, object], expectation: CandidateProofExpectation) -> None:
    """The private aggregate ran from the expected native harness revision."""
    if (aggregate.get("source_revision") != expectation.harness_revision
            or aggregate.get("harness_revision") != expectation.harness_revision):
        raise PublicProofError("candidate_harness_revision_mismatch")


def candidate_proof_document(expectation: CandidateProofExpectation, completion: NativeCompletion,
                             aggregate: dict[str, object]) -> bytes:
    """Encode ``proof.json`` of a candidate proof."""
    return canonical_json({
        "schema_version": PUBLIC_SCHEMA_VERSION, "kind": CANDIDATE_PROOF_KIND,
        "identity": candidate_identity_record(expectation), "completion": completion_record(completion),
        "aggregate": aggregate,
    })


def candidate_manifest_document(source_sha256: dict[str, object], entries: dict[str, bytes]) -> bytes:
    """Encode ``manifest.json`` over every other member of a candidate proof."""
    return canonical_json({
        "schema_version": PUBLIC_SCHEMA_VERSION, "kind": CANDIDATE_MANIFEST_KIND,
        "source_sha256": source_sha256,
        "projected_members": [{"path": name, "sha256": sha256(entries[name])}
                              for name in PUBLIC_MEMBER_NAMES[1:]],
    })


def _require_header(record: dict[str, object], kind: str, code: str) -> None:
    if (type(record.get("schema_version")) is not int
            or record.get("schema_version") != PUBLIC_SCHEMA_VERSION or record.get("kind") != kind):
        raise PublicProofError(code)


def _candidate_source(entries: dict[str, bytes]) -> dict[str, object]:
    manifest = require_object(parse_canonical(entries["manifest.json"], "candidate_manifest_invalid"),
                              _MANIFEST_KEYS, "candidate_manifest_invalid")
    _require_header(manifest, CANDIDATE_MANIFEST_KIND, "candidate_manifest_kind_invalid")
    source = require_object(manifest["source_sha256"], set(CANDIDATE_SOURCE_KEYS),
                            "candidate_source_manifest_invalid")
    cases = require_object(source["case_receipts"], set(CASE_IDS), "candidate_source_cases_invalid")
    cleanup = source["cleanup_journals"]
    if not isinstance(cleanup, dict) or not set(cleanup) <= set(CASE_IDS):
        raise PublicProofError("candidate_source_cleanup_invalid")
    for name in ("stage_manifest", "aggregate_receipt", "native_completion"):
        require_sha256(source[name])
    for digest in (*cases.values(), *cleanup.values()):
        require_sha256(digest)
    require_projected_members(manifest["projected_members"], entries)
    return source


def _record(value: object, code: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise PublicProofError(code)
    return value


def _case_closed(case: dict[str, object]) -> bool:
    """Apply the canonical lifetime predicate to one already validated case."""
    proof = _record(case.get("source_proof"), "public_case_proof_invalid")
    lifecycle = _record(proof.get("lifecycle_proof"), "public_case_lifecycle_invalid")
    cleanup = _record(case.get("cleanup"), "public_cleanup_invalid")
    observation = _record(cleanup.get("observation"), "public_cleanup_observation_invalid")
    return case_lifetime_closed(
        status=case["status"], cleanup_method=observation.get("method"), proof_state=proof.get("state"),
        lifecycle_state=lifecycle.get("state"),
        supervisor_shutdown_observed=lifecycle.get("supervisor_shutdown_observed"))


def _validated_cases(entries: dict[str, bytes], source: dict[str, object],
                     expectation: CandidateProofExpectation,
                     aggregate: dict[str, object]) -> tuple[tuple[CaseOutcome, ...], tuple[str, ...]]:
    for name in CASE_IDS:
        case_privacy_keys(parse_json(entries[f"cases/{name}.json"], "public_case_json_invalid"),
                          case_schema_version=CANDIDATE_CASE_SCHEMA_VERSION)
    context = CaseContext(
        opencode_model=expectation.opencode_model, ccr_model=expectation.ccr_model,
        binaries=expectation.binaries, case_receipts=tuple(sorted(source["case_receipts"].items())),
        cleanup_journals=tuple(sorted(source["cleanup_journals"].items())),
        case_schema_version=CANDIDATE_CASE_SCHEMA_VERSION,
    )
    try:
        results = validate_cases(entries, context)
    except PublicProofError:
        raise
    except (EvidenceError, KeyError, TypeError, ValueError) as error:
        raise PublicProofError("candidate_case_predicate_invalid") from error
    require_case_counts(aggregate, results)
    require_aggregate_verdict(aggregate, results)
    # Readiness counts passing cells, so every PASS AGY case binds its model,
    # even inside a FAIL aggregate.
    if not all(agy_case_model_bound(case, expectation.agy_model) for case in results.cases):
        raise PublicProofError("candidate_agy_model_mismatch")
    unclosed = {str(case["case_id"]) for case in results.cases if not _case_closed(case)}
    if results.outcomes is None:
        raise PublicProofError("candidate_case_predicate_invalid")
    return results.outcomes, tuple(sorted(set(results.unresolved_case_ids) | unclosed))


def validate_candidate_proof(zip_bytes: bytes,
                             expectation: CandidateProofExpectation) -> ValidatedCandidateProof:
    """Return the digest-bound facts of a candidate proof ZIP, or refuse it."""
    if not isinstance(expectation, CandidateProofExpectation):
        raise PublicProofError("candidate_expectation_invalid")
    entries = read_public_zip(zip_bytes)
    source = _candidate_source(entries)
    proof = require_object(parse_canonical(entries["proof.json"], "candidate_proof_invalid"),
                           _PROOF_KEYS, "candidate_proof_invalid")
    _require_header(proof, CANDIDATE_PROOF_KIND, "candidate_proof_kind_invalid")
    # Canonical bytes compare JSON types too: true is not 1.
    if canonical_json(proof["identity"]) != canonical_json(candidate_identity_record(expectation)):
        raise PublicProofError("candidate_identity_mismatch")
    completion = parse_completion(proof["completion"])
    if sha256(canonical_json(completion_record(completion))) != source["native_completion"]:
        raise PublicProofError("candidate_completion_source_mismatch")
    aggregate = validate_aggregate_shape(proof["aggregate"])
    require_completion_binding(completion, expectation, exit_code=aggregate["exit_code"])
    privacy_walk(proof)
    cases, unresolved = _validated_cases(entries, source, expectation, aggregate)
    return ValidatedCandidateProof(
        public_zip_sha256=sha256(zip_bytes), aggregate_sha256=str(source["aggregate_receipt"]),
        candidate_id=expectation.candidate_id, claim_sha256=expectation.claim_sha256,
        invocation_id=expectation.invocation_id, platform_os=expectation.platform_os,
        platform_arch=expectation.platform_arch, archive_sha256=expectation.archive_sha256,
        binaries=expectation.binaries, completion=completion, status=str(aggregate["status"]),
        case_results_verdict=str(aggregate["case_results_verdict"]), cases=cases,
        unresolved_case_ids=unresolved,
    )
