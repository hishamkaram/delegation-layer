"""Hosted validation of the public release native-evidence envelope.

The shared ZIP, JSON and privacy primitives live in ``public_contract`` and
the case predicates in ``public_case_validation``. This module keeps the
release identity, tag attestation, release cleanup and its BLOCKED fallback.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib

from .contract import EvidenceError
from .public_case_validation import (
    CaseContext,
    case_privacy_keys,
    require_aggregate_verdict,
    require_case_counts,
    validate_aggregate_shape,
    validate_cases,
)
from .public_contract import (
    CASE_IDS,
    PLATFORM_TEXT,
    PUBLIC_MANIFEST_KIND,
    PUBLIC_PROOF_KIND,
    PUBLIC_SCHEMA_VERSION,
    PUBLIC_TEXT,
    REPOSITORY,
    REVISION,
    TAG,
    ProofExpectation,
    PublicProofError,
    parse_canonical,
    parse_json,
    parse_public_binaries,
    privacy_walk,
    read_public_zip,
    require_object,
    require_projected_members,
    require_sha256,
    validate_expectation,
)


_SOURCE_KEYS = {
    "stage_manifest", "aggregate_receipt", "archive_receipt", "native_receipt",
    "case_receipts", "cleanup_journals", "release_cleanup_journals",
}
@dataclass(frozen=True)
class _PublicArchive:
    manifest: dict[str, object]
    proof: dict[str, object]
    entries: dict[str, bytes]


def _validate_source_manifest(value: object) -> dict[str, object]:
    source = require_object(value, _SOURCE_KEYS, "public_source_manifest_invalid")
    for name in ("stage_manifest", "aggregate_receipt", "archive_receipt", "native_receipt"):
        require_sha256(source[name])
    cases = require_object(source["case_receipts"], set(CASE_IDS), "public_source_cases_invalid")
    cleanup = source["cleanup_journals"]
    release_cleanup = source["release_cleanup_journals"]
    if not isinstance(cleanup, dict) or not set(cleanup).issubset(CASE_IDS):
        raise PublicProofError("public_source_cleanup_invalid")
    if (not isinstance(release_cleanup, dict)
            or not set(release_cleanup).issubset({"archive", "native"})):
        raise PublicProofError("public_source_release_cleanup_invalid")
    for digest in (*cases.values(), *cleanup.values(), *release_cleanup.values()):
        require_sha256(digest)
    return source


def _validate_public_manifest(data: bytes, entries: dict[str, bytes]) -> dict[str, object]:
    value = parse_canonical(data, "public_manifest_invalid")
    manifest = require_object(value, {
        "schema_version", "kind", "source_sha256", "projected_members",
    }, "public_manifest_invalid")
    if (type(manifest.get("schema_version")) is not int
            or manifest.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or manifest.get("kind") != PUBLIC_MANIFEST_KIND):
        raise PublicProofError("public_manifest_version_invalid")
    source = _validate_source_manifest(manifest["source_sha256"])
    require_projected_members(manifest["projected_members"], entries)
    if set(source["case_receipts"]) != set(CASE_IDS):
        raise PublicProofError("public_source_cases_invalid")
    return manifest


def _fingerprint(value: object) -> dict[str, object]:
    fingerprint = require_object(value, {"sha256"}, "public_fingerprint_invalid")
    require_sha256(fingerprint.get("sha256"))
    return fingerprint


def _validate_identity_models(identity: dict[str, object]) -> None:
    opencode_model = identity.get("opencode_model")
    ccr_model = identity.get("ccr_model")
    if (not isinstance(opencode_model, str)
            or PUBLIC_TEXT.fullmatch(opencode_model) is None
            or ccr_model is not None
            and (not isinstance(ccr_model, str) or PUBLIC_TEXT.fullmatch(ccr_model) is None)):
        raise PublicProofError("public_identity_invalid")


def _validate_identity_shape(value: object) -> dict[str, object]:
    identity = require_object(value, {
        "repo", "tag", "release_revision", "harness_revision", "release_identity", "platform",
        "opencode_model", "ccr_model", "archive", "source_harness", "archive_source_harness",
        "binaries", "release_cleanup",
    }, "public_identity_invalid")
    if (not isinstance(identity.get("repo"), str)
            or REPOSITORY.fullmatch(identity["repo"]) is None
            or not isinstance(identity.get("tag"), str)
            or TAG.fullmatch(identity["tag"]) is None
            or not isinstance(identity.get("release_revision"), str)
            or REVISION.fullmatch(identity["release_revision"]) is None
            or not isinstance(identity.get("harness_revision"), str)
            or REVISION.fullmatch(identity["harness_revision"]) is None):
        raise PublicProofError("public_identity_invalid")
    _validate_identity_models(identity)
    release = require_object(identity["release_identity"], {
        "repository", "tag", "ref", "commit_sha",
    }, "public_release_identity_invalid")
    if release != {
            "repository": identity["repo"], "tag": identity["tag"],
            "ref": f"refs/tags/{identity['tag']}", "commit_sha": identity["release_revision"],
    }:
        raise PublicProofError("public_release_identity_invalid")
    platform = require_object(identity["platform"], {"os", "arch"}, "public_platform_invalid")
    for key in ("os", "arch"):
        if not isinstance(platform.get(key), str) or PLATFORM_TEXT.fullmatch(platform[key]) is None:
            raise PublicProofError("public_platform_invalid")
    archive = require_object(identity["archive"], {
        "name", "sha256", "checksums_sha256", "attestation", "attestation_binding",
    }, "public_archive_invalid")
    expected_archive = (
        f"delegation-layer_{identity['tag'][1:]}_{platform['os']}_{platform['arch']}.tar.gz"
    )
    if (not isinstance(archive.get("name"), str) or archive["name"] != expected_archive
            or len(archive["name"]) > 256 or "/" in archive["name"]
            or "\\" in archive["name"] or archive["name"] in {".", ".."}
            or archive.get("attestation") != "verified"):
        raise PublicProofError("public_archive_invalid")
    require_sha256(archive.get("sha256"))
    require_sha256(archive.get("checksums_sha256"))
    binding = require_object(archive["attestation_binding"], {
        "repository", "source_ref", "source_revision", "archive_sha256",
    }, "public_attestation_binding_invalid")
    if binding != {
            "repository": identity["repo"], "source_ref": f"refs/tags/{identity['tag']}",
            "source_revision": identity["release_revision"], "archive_sha256": archive["sha256"],
    }:
        raise PublicProofError("public_attestation_binding_invalid")
    binaries = require_object(identity["binaries"], {
        "delegate", "runner", "pueue", "pueued",
    }, "public_binaries_invalid")
    for digest in binaries.values():
        require_sha256(digest)
    cleanup = require_object(identity["release_cleanup"], {"archive", "native"},
                             "public_release_cleanup_invalid")
    for name in ("archive", "native"):
        item = require_object(cleanup[name], {
            "status", "completion_state", "final_verdict", "authorization_sha256", "journal_sha256",
            "resource_id",
        }, "public_release_cleanup_invalid")
        if item["status"] not in {
                "complete", "retained-for-native-acceptance", "retained-for-investigation",
                "incomplete", "prepared", "pending",
        }:
            raise PublicProofError("public_release_cleanup_invalid")
        if item["completion_state"] not in {None, "prepared", "complete"}:
            raise PublicProofError("public_release_cleanup_invalid")
        if item["final_verdict"] not in {None, "pass", "blocked"}:
            raise PublicProofError("public_release_cleanup_invalid")
        for digest_name in ("authorization_sha256", "journal_sha256"):
            if item[digest_name] is not None:
                require_sha256(item[digest_name])
        require_sha256(item["resource_id"])
    _fingerprint(identity["source_harness"])
    _fingerprint(identity["archive_source_harness"])
    for row in (release, archive, binding, cleanup):
        privacy_walk(row)
    return identity


def _parse_public_archive(zip_bytes: bytes) -> _PublicArchive:
    entries = read_public_zip(zip_bytes)
    manifest = _validate_public_manifest(entries["manifest.json"], entries)
    proof_value = parse_canonical(entries["proof.json"], "public_proof_invalid")
    proof = require_object(proof_value, {
        "schema_version", "kind", "identity", "aggregate",
    }, "public_proof_invalid")
    if (type(proof.get("schema_version")) is not int
            or proof.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or proof.get("kind") != PUBLIC_PROOF_KIND):
        raise PublicProofError("public_proof_version_invalid")
    _validate_identity_shape(proof["identity"])
    validate_aggregate_shape(proof["aggregate"])
    source = manifest["source_sha256"]
    cleanup = proof["identity"]["release_cleanup"]
    for name in ("archive", "native"):
        digest = cleanup[name]["journal_sha256"]
        if digest != source["release_cleanup_journals"].get(name):
            raise PublicProofError("public_release_cleanup_digest_mismatch")
    for name in CASE_IDS:
        case_data = parse_json(entries[f"cases/{name}.json"], "public_case_json_invalid")
        case_privacy_keys(case_data)
    return _PublicArchive(manifest, proof, entries)


def _provenance(zip_bytes: bytes, parsed: _PublicArchive) -> dict[str, str]:
    source = require_object(parsed.manifest.get("source_sha256"), _SOURCE_KEYS,
                           "public_source_manifest_invalid")
    return {
        "source_stage_manifest_sha256": require_sha256(source.get("stage_manifest")),
        "public_zip_sha256": hashlib.sha256(zip_bytes).hexdigest(),
    }


def inspect_public_proof(zip_bytes: bytes) -> dict[str, object]:
    """Inspect ZIP, digests, and privacy-safe envelope before trusted admission."""
    parsed = _parse_public_archive(zip_bytes)
    identity = parsed.proof["identity"]
    return {
        "status": "ENVELOPE_VALID",
        "identity": identity,
        "platform": identity["platform"],
        **_provenance(zip_bytes, parsed),
    }


def _validate_identity_binding(identity: dict[str, object], expected: ProofExpectation) -> None:
    release = identity["release_identity"]
    platform = identity["platform"]
    archive = identity["archive"]
    binding = archive["attestation_binding"]
    source = identity["source_harness"]
    if (identity["repo"] != expected.repo or identity["tag"] != expected.tag
            or identity["release_revision"] != expected.release_revision
            or identity["harness_revision"] != expected.harness_revision
            or identity["opencode_model"] != expected.opencode_model
            or identity["ccr_model"] != expected.ccr_model
            or release != {
                "repository": expected.repo, "tag": expected.tag,
                "ref": f"refs/tags/{expected.tag}", "commit_sha": expected.release_revision,
            }
            or platform != {"os": expected.platform_os, "arch": expected.platform_arch}
            or archive["sha256"] != expected.archive_sha256
            or archive["attestation"] != "verified"
            or binding != {
                "repository": expected.repo, "source_ref": f"refs/tags/{expected.tag}",
                "source_revision": expected.release_revision,
                "archive_sha256": expected.archive_sha256,
            }
            or source["sha256"] != expected.source_harness_sha256
            or identity["archive_source_harness"]["sha256"] != expected.source_harness_sha256):
        raise PublicProofError("public_identity_binding_mismatch")
    expected_binaries = {
        "delegate": expected.delegate_sha256, "runner": expected.runner_sha256,
        "pueue": expected.pueue_sha256, "pueued": expected.pueued_sha256,
    }
    if identity["binaries"] != expected_binaries:
        raise PublicProofError("public_binary_binding_mismatch")


def validate_public_identity(zip_bytes: bytes,
                             expectation: ProofExpectation) -> dict[str, object]:
    """Validate public envelope and trusted outer identities, without case policy."""
    validate_expectation(expectation)
    parsed = _parse_public_archive(zip_bytes)
    identity = parsed.proof["identity"]
    _validate_identity_binding(identity, expectation)
    return {
        "status": "IDENTITY_VALID", "identity": identity,
        "platform": identity["platform"],
        **_provenance(zip_bytes, parsed),
    }


def _require_release_cleanup(identity: dict[str, object]) -> None:
    cleanup = identity["release_cleanup"]
    archive, native = cleanup["archive"], cleanup["native"]
    if (archive["status"] != "retained-for-native-acceptance"
            or archive["completion_state"] is not None
            or archive["authorization_sha256"] is not None or archive["journal_sha256"] is not None
            or native["status"] != "complete" or native["completion_state"] != "complete"
            or native["final_verdict"] not in {"pass", "blocked"}
            or native["authorization_sha256"] is None or native["journal_sha256"] is None
            or archive["resource_id"] != native["resource_id"]):
        raise PublicProofError("public_release_cleanup_unresolved")


def _case_context(identity: dict[str, object], source: dict[str, object],
                  expected: ProofExpectation) -> CaseContext:
    return CaseContext(
        opencode_model=expected.opencode_model, ccr_model=expected.ccr_model,
        binaries=parse_public_binaries(identity["binaries"], "public_binaries_invalid"),
        case_receipts=tuple(sorted(source["case_receipts"].items())),
        cleanup_journals=tuple(sorted(source["cleanup_journals"].items())),
    )


def _validate_case_results(parsed: _PublicArchive,
                           expected: ProofExpectation) -> dict[str, object]:
    identity = parsed.proof["identity"]
    aggregate = parsed.proof["aggregate"]
    results = validate_cases(parsed.entries, _case_context(identity, parsed.manifest["source_sha256"], expected))
    require_case_counts(aggregate, results)
    if results.verdict != "FAIL":
        _require_release_cleanup(identity)
    verdict = require_aggregate_verdict(aggregate, results)
    counts = results.counts()
    unresolved_count = len(results.unresolved_case_ids)
    gate_status = "PASS" if counts["pass_count"] >= 1 and counts["failed_count"] == 0 \
        and unresolved_count == 0 else "BLOCKED"
    return {
        "status": gate_status, "case_results_verdict": verdict,
        **counts, "unresolved_case_count": unresolved_count,
        "expected_case_count": len(CASE_IDS), "cases": list(results.cases),
        "identity": identity,
    }


def validate_public_proof(zip_bytes: bytes,
                          expectation: ProofExpectation) -> dict[str, object]:
    """Validate the same envelope as inspection, then canonical case predicates."""
    validate_expectation(expectation)
    parsed = _parse_public_archive(zip_bytes)
    identity = parsed.proof["identity"]
    _validate_identity_binding(identity, expectation)
    try:
        result = _validate_case_results(parsed, expectation)
    except (PublicProofError, EvidenceError, KeyError, TypeError, ValueError):
        result = {
            "status": "BLOCKED", "case_results_verdict": "FAIL",
            "blocking_reason": "case_schema_or_predicate_invalid",
            "expected_case_count": len(CASE_IDS), "identity": identity,
        }
    return {**result, **_provenance(zip_bytes, parsed)}
