"""Pure ZIP inspection and hosted validation for public native evidence."""

from __future__ import annotations

from dataclasses import dataclass
from io import BytesIO
import hashlib
import re
import stat
import zipfile

from .contract import (
    CaseDefinition,
    EvidenceError,
    MatrixRow,
    _aggregate_exit,
    _case_result_verdict,
    _validate_case_completion,
    _validate_case_retention,
    _validate_case_sessions,
    _validate_requested_options,
)
from .public_contract import (
    CASE_DEFINITIONS,
    CASE_IDS,
    MAX_PUBLIC_MEMBER_BYTES,
    MAX_PUBLIC_TOTAL_BYTES,
    MAX_PUBLIC_ZIP_BYTES,
    OPAQUE_TEXT,
    PLATFORM_TEXT,
    PUBLIC_MANIFEST_KIND,
    PUBLIC_MEMBER_NAMES,
    PUBLIC_PROOF_KIND,
    PUBLIC_SCHEMA_VERSION,
    PUBLIC_TEXT,
    REPOSITORY,
    REVISION,
    TAG,
    TASK_ID,
    ProofExpectation,
    PublicProofError,
    canonical_public_zip,
    canonical_json,
    parse_json,
    require_object,
    require_sha256,
    validate_expectation,
)


_SOURCE_KEYS = {
    "stage_manifest", "aggregate_receipt", "archive_receipt", "native_receipt",
    "case_receipts", "cleanup_journals", "release_cleanup_journals",
}
_CASE_KEYS = {
    "schema_version", "kind", "case_id", "provider", "permission", "scenario",
    "status", "exit_code", "evidence_status", "source_compact_sha256",
    "source_proof", "cleanup", "natural_daemon_shutdown",
}
_PROOF_KEYS = {
    "schema_version", "kind", "state", "final_status", "source_status", "source_receipt_sha256",
    "gate", "provider", "permission", "mode", "scenario", "requested_options_state",
    "requested_model", "requested_effort", "task_ids", "task_session_bindings",
    "diagnostic_task_ids", "session_ids", "binary_identities", "lifecycle_proof",
}
_BINARY_KEYS = {"state", "sha256", "task_ids"}
_CLEANUP_KEYS = {
    "observation", "raw_before", "raw_after", "raw_deleted", "terminal_state",
    "journal_sha256", "authorization_sha256",
}
_OBSERVATION_KEYS = {
    "status", "source_status", "supervisor_shutdown_observed", "raw_process_captures",
    "method", "cleanup_receipt_sha256",
}
_SUMMARY_KEYS = {"root_count", "file_count", "byte_count", "complete", "count", "bytes"}
_SESSION_KEYS = {"state", "values", "by_alias"}
_LIFECYCLE_KEYS = {
    "state", "cleanup_status", "supervisor_shutdown_observed",
    "natural_daemon_shutdown", "failure_diagnostics_sha256",
}
_DENIED_KEYS = {
    "argv", "env", "environment", "stdout", "stderr", "output", "provider_output",
    "failure", "failure_detail", "diagnostic_text", "receipt_path", "archive_path",
    "checksums_path", "journal_path", "executable", "provider_runtime_identities",
    "creation_identity", "command", "commands",
}
_SECRET_VALUE = re.compile(
    r"(?i)(?:gh[pousr]_|github_pat_|sk_(?:live|test)_|sk-|xox[baprs]-|"
    r"AKIA[0-9A-Z]{16}|bearer\s|-----BEGIN [A-Z ]*PRIVATE KEY-----)"
)
_SESSION_FACTS = tuple[list[dict[str, str]], list[str], list[str], dict[str, object]]


@dataclass(frozen=True)
class _PublicArchive:
    manifest: dict[str, object]
    proof: dict[str, object]
    entries: dict[str, bytes]


def _parse_canonical(data: bytes, code: str) -> object:
    value = parse_json(data, code)
    if canonical_json(value) != data:
        raise PublicProofError(code)
    return value


def _privacy_walk(value: object, depth: int = 0) -> None:
    if depth > 24:
        raise PublicProofError("public_json_depth_invalid")
    if isinstance(value, dict):
        for key, item in value.items():
            if not isinstance(key, str) or key.lower() in _DENIED_KEYS:
                raise PublicProofError("public_privacy_field_denied")
            _privacy_walk(item, depth + 1)
    elif isinstance(value, list):
        if len(value) > 1024:
            raise PublicProofError("public_json_list_too_large")
        for item in value:
            _privacy_walk(item, depth + 1)
    elif isinstance(value, str):
        if (len(value) > 4096 or any(ord(char) < 32 for char in value)
                or value.startswith("/") or re.match(r"^[A-Za-z]:[\\/]", value)
                or value.startswith("\\\\") or _SECRET_VALUE.search(value) is not None):
            raise PublicProofError("public_privacy_value_denied")


def _case_privacy_keys(value: object) -> None:
    if not isinstance(value, dict):
        _privacy_walk(value)
        return
    if not set(value).issubset(_CASE_KEYS):
        raise PublicProofError("public_privacy_field_denied")
    proof = value.get("source_proof")
    if isinstance(proof, dict):
        if not set(proof).issubset(_PROOF_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        sessions = proof.get("session_ids")
        if isinstance(sessions, dict) and not set(sessions).issubset(_SESSION_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        binaries = proof.get("binary_identities")
        if isinstance(binaries, dict) and not set(binaries).issubset(_BINARY_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        binary_hashes = binaries.get("sha256") if isinstance(binaries, dict) else None
        if isinstance(binary_hashes, dict) and not set(binary_hashes).issubset({
                "provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256",
        }):
            raise PublicProofError("public_privacy_field_denied")
        lifecycle = proof.get("lifecycle_proof")
        if isinstance(lifecycle, dict) and not set(lifecycle).issubset(_LIFECYCLE_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        bindings = proof.get("task_session_bindings")
        if isinstance(bindings, list):
            if any(isinstance(item, dict)
                   and not set(item).issubset({"alias", "task_id", "session_id"})
                   for item in bindings):
                raise PublicProofError("public_privacy_field_denied")
    cleanup = value.get("cleanup")
    if isinstance(cleanup, dict):
        if not set(cleanup).issubset(_CLEANUP_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        observation = cleanup.get("observation")
        if isinstance(observation, dict) and not set(observation).issubset(_OBSERVATION_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        for key in ("raw_before", "raw_after"):
            summary = cleanup.get(key)
            if isinstance(summary, dict) and not set(summary).issubset(_SUMMARY_KEYS):
                raise PublicProofError("public_privacy_field_denied")
    _privacy_walk(value)


def _read_zip_members(data: bytes) -> dict[str, bytes]:
    if (not isinstance(data, bytes) or not data or len(data) > MAX_PUBLIC_ZIP_BYTES
            or not data.startswith(b"PK\x03\x04") or data[-22:-18] != b"PK\x05\x06"):
        raise PublicProofError("public_zip_size_or_header_invalid")
    try:
        with zipfile.ZipFile(BytesIO(data), "r") as archive:
            info_rows = archive.infolist()
            if (archive.comment or tuple(item.filename for item in info_rows) != PUBLIC_MEMBER_NAMES
                    or len(info_rows) != len(PUBLIC_MEMBER_NAMES)):
                raise PublicProofError("public_zip_members_invalid")
            total = 0
            entries: dict[str, bytes] = {}
            for info in info_rows:
                mode = info.external_attr >> 16
                if (info.filename != info.orig_filename or info.compress_type != zipfile.ZIP_STORED
                        or info.extra or info.comment or info.flag_bits != 0 or info.volume != 0
                        or info.file_size > MAX_PUBLIC_MEMBER_BYTES
                        or info.file_size != info.compress_size
                        or stat.S_IFMT(mode) not in {0, stat.S_IFREG}):
                    raise PublicProofError("public_zip_member_invalid")
                total += info.file_size
                if total > MAX_PUBLIC_TOTAL_BYTES:
                    raise PublicProofError("public_zip_unpacked_size_invalid")
                entries[info.filename] = archive.read(info)
    except (OSError, EOFError, RuntimeError, zipfile.BadZipFile, zipfile.LargeZipFile) as error:
        raise PublicProofError("public_zip_invalid") from error
    if set(entries) != set(PUBLIC_MEMBER_NAMES):
        raise PublicProofError("public_zip_members_invalid")
    if canonical_public_zip(entries) != data:
        raise PublicProofError("public_zip_noncanonical")
    return entries


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
    value = _parse_canonical(data, "public_manifest_invalid")
    manifest = require_object(value, {
        "schema_version", "kind", "source_sha256", "projected_members",
    }, "public_manifest_invalid")
    if (type(manifest.get("schema_version")) is not int
            or manifest.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or manifest.get("kind") != PUBLIC_MANIFEST_KIND):
        raise PublicProofError("public_manifest_version_invalid")
    source = _validate_source_manifest(manifest["source_sha256"])
    rows = manifest["projected_members"]
    if not isinstance(rows, list) or len(rows) != len(PUBLIC_MEMBER_NAMES) - 1:
        raise PublicProofError("public_projected_members_invalid")
    expected_names = PUBLIC_MEMBER_NAMES[1:]
    for index, name in enumerate(expected_names):
        row = require_object(rows[index], {"path", "sha256"}, "public_projected_member_invalid")
        if row.get("path") != name:
            raise PublicProofError("public_projected_members_invalid")
        expected_hash = require_sha256(row.get("sha256"))
        if hashlib.sha256(entries[name]).hexdigest() != expected_hash:
            raise PublicProofError("public_projected_digest_mismatch")
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
        _privacy_walk(row)
    return identity


def _validate_aggregate_shape(value: object) -> dict[str, object]:
    aggregate = require_object(value, {
        "status", "exit_code", "case_results_verdict", "pass_count", "blocked_count",
        "failed_count", "unresolved_case_count", "expected_case_count",
        "require_at_least_one_pass",
    }, "public_aggregate_invalid")
    if (aggregate.get("status") not in {"PASS", "BLOCKED", "FAIL"}
            or aggregate.get("case_results_verdict") not in {"PASS", "BLOCKED", "FAIL"}
            or type(aggregate.get("exit_code")) is not int
            or not 0 <= aggregate["exit_code"] <= 255
            or type(aggregate.get("require_at_least_one_pass")) is not bool):
        raise PublicProofError("public_aggregate_invalid")
    for key in ("pass_count", "blocked_count", "failed_count", "unresolved_case_count"):
        if type(aggregate.get(key)) is not int or not 0 <= aggregate[key] <= len(CASE_IDS):
            raise PublicProofError("public_aggregate_count_invalid")
    if aggregate.get("expected_case_count") != len(CASE_IDS):
        raise PublicProofError("public_aggregate_count_invalid")
    return aggregate


def _parse_public_archive(zip_bytes: bytes) -> _PublicArchive:
    entries = _read_zip_members(zip_bytes)
    manifest = _validate_public_manifest(entries["manifest.json"], entries)
    proof_value = _parse_canonical(entries["proof.json"], "public_proof_invalid")
    proof = require_object(proof_value, {
        "schema_version", "kind", "identity", "aggregate",
    }, "public_proof_invalid")
    if (type(proof.get("schema_version")) is not int
            or proof.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or proof.get("kind") != PUBLIC_PROOF_KIND):
        raise PublicProofError("public_proof_version_invalid")
    _validate_identity_shape(proof["identity"])
    _validate_aggregate_shape(proof["aggregate"])
    source = manifest["source_sha256"]
    cleanup = proof["identity"]["release_cleanup"]
    for name in ("archive", "native"):
        digest = cleanup[name]["journal_sha256"]
        if digest != source["release_cleanup_journals"].get(name):
            raise PublicProofError("public_release_cleanup_digest_mismatch")
    for name in CASE_IDS:
        case_data = parse_json(entries[f"cases/{name}.json"], "public_case_json_invalid")
        _case_privacy_keys(case_data)
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


def _validate_case_shape(value: object, expected_case: CaseDefinition) -> dict[str, object]:
    case = require_object(value, _CASE_KEYS, "public_case_invalid")
    if (type(case.get("schema_version")) is not int
            or case.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or case.get("kind") != "public-native-acceptance-case"):
        raise PublicProofError("public_case_version_invalid")
    for key, expected in (("case_id", expected_case.case_id),
                          ("provider", expected_case.provider),
                          ("permission", expected_case.permission),
                          ("scenario", expected_case.scenario)):
        if case.get(key) != expected:
            raise PublicProofError("public_case_identity_mismatch")
    if (case.get("status") not in {"PASS", "BLOCKED", "FAIL"}
            or case.get("evidence_status") not in {"RESOLVED", "UNRESOLVED"}
            or type(case.get("exit_code")) is not int
            or not 0 <= case["exit_code"] <= 255
            or type(case.get("natural_daemon_shutdown")) not in {bool, type(None)}):
        raise PublicProofError("public_case_verdict_invalid")
    expected_exit = {"PASS": 0, "BLOCKED": 2}.get(case["status"])
    if (expected_exit is not None and case["exit_code"] != expected_exit
            or case["status"] == "FAIL" and case["exit_code"] in {0, 2}):
        raise PublicProofError("public_case_exit_mismatch")
    require_sha256(case.get("source_compact_sha256"))
    return case


def _validate_public_proof_fields(
        case: dict[str, object], expected: ProofExpectation) -> tuple[dict[str, object], _SESSION_FACTS]:
    proof = require_object(case["source_proof"], _PROOF_KEYS, "public_case_proof_invalid")
    if (type(proof.get("schema_version")) is not int
            or proof.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or proof.get("kind") != "public-native-acceptance-case-proof"):
        raise PublicProofError("public_case_proof_version_invalid")
    for key, expected_value in (("final_status", case["status"]), ("gate", "acceptance-native"),
                                ("provider", case["provider"]), ("permission", case["permission"]),
                                ("mode", case["permission"]), ("scenario", case["scenario"])):
        if proof.get(key) != expected_value:
            raise PublicProofError("public_case_proof_identity_mismatch")
    if proof.get("state") not in {"complete", "blocked-before-runtime", "unresolved"}:
        raise PublicProofError("public_case_proof_state_invalid")
    if proof.get("source_status") not in {None, "pass", "passed", "blocked", "block", "fail", "failed"}:
        raise PublicProofError("public_case_source_status_invalid")
    source_digest = proof.get("source_receipt_sha256")
    if source_digest is not None:
        require_sha256(source_digest)
    options = {key: proof[key] for key in (
        "requested_options_state", "requested_model", "requested_effort",
    )}
    _validate_requested_options(options)
    for key in ("requested_model", "requested_effort"):
        if options[key] is not None and PUBLIC_TEXT.fullmatch(str(options[key])) is None:
            raise PublicProofError("public_case_option_invalid")
    model = options["requested_model"]
    if expected_case_provider(case) == "opencode" and model != expected.opencode_model:
        raise PublicProofError("public_opencode_model_mismatch")
    if expected_case_provider(case) == "ccr":
        if expected.ccr_model is None:
            if case["status"] != "BLOCKED" or model is not None:
                raise PublicProofError("public_ccr_absence_not_blocked")
        elif model != expected.ccr_model:
            raise PublicProofError("public_ccr_model_mismatch")
    sessions = _validate_case_sessions(proof)
    ids = [*proof["task_ids"], *proof["diagnostic_task_ids"]]
    ids.extend(binding["task_id"] for binding in proof["task_session_bindings"])
    if any(not isinstance(item, str) or TASK_ID.fullmatch(item) is None for item in ids):
        raise PublicProofError("public_case_task_id_invalid")
    session_values = [*proof["session_ids"]["values"],
                      *proof["session_ids"]["by_alias"].values()]
    if any(not isinstance(item, str) or OPAQUE_TEXT.fullmatch(item) is None
           or item.startswith("/") for item in session_values):
        raise PublicProofError("public_case_session_id_invalid")
    return proof, sessions


def expected_case_provider(case: dict[str, object]) -> str:
    provider = case.get("provider")
    if not isinstance(provider, str):
        raise PublicProofError("public_case_provider_invalid")
    return provider.split(":", 1)[0]


def _validate_public_binary_facts(
        case: dict[str, object], proof: dict[str, object], identity: dict[str, object],
        sessions: _SESSION_FACTS) -> tuple[dict[str, object], list[str]]:
    binaries = require_object(proof["binary_identities"], _BINARY_KEYS,
                              "public_case_binary_invalid")
    hash_names = {"provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256"}
    if (binaries.get("state") not in {"complete", "unavailable"}
            or not isinstance(binaries.get("sha256"), dict)
            or not set(binaries["sha256"]).issubset(hash_names)):
        raise PublicProofError("public_case_binary_invalid")
    hashes = binaries["sha256"]
    for digest in hashes.values():
        require_sha256(digest)
    expected = identity["binaries"]
    bindings = {
        "delegate_sha256": expected["delegate"], "runner_sha256": expected["runner"],
        "pueue_sha256": expected["pueue"], "pueued_sha256": expected["pueued"],
    }
    if any(key in hashes and hashes[key] != digest for key, digest in bindings.items()):
        raise PublicProofError("public_case_binary_binding_mismatch")
    ids = binaries.get("task_ids")
    if (not isinstance(ids, list) or any(not isinstance(item, str) or TASK_ID.fullmatch(item) is None
                                        for item in ids)
            or ids != sorted(set(ids))):
        raise PublicProofError("public_case_binary_tasks_invalid")
    if (case["status"] == "PASS" and case["evidence_status"] == "RESOLVED"
            and (binaries["state"] != "complete" or not ids or set(hashes) != hash_names
                 or any(hashes[key] != digest for key, digest in bindings.items()))):
        raise PublicProofError("public_case_binary_incomplete")
    return binaries, ids


def _validate_lifecycle(proof: dict[str, object], case: dict[str, object]) -> dict[str, object]:
    lifecycle = require_object(proof.get("lifecycle_proof"), _LIFECYCLE_KEYS,
                               "public_case_lifecycle_invalid")
    if (lifecycle.get("state") not in {"complete", "unresolved"}
            or lifecycle.get("cleanup_status") not in {
                "verified", "unverified", "missing", "malformed", "unreadable", None,
            }
            or type(lifecycle.get("supervisor_shutdown_observed")) not in {bool, type(None)}
            or type(lifecycle.get("natural_daemon_shutdown")) not in {bool, type(None)}):
        raise PublicProofError("public_case_lifecycle_invalid")
    digest = lifecycle.get("failure_diagnostics_sha256")
    if digest is not None:
        require_sha256(digest)
    if lifecycle["natural_daemon_shutdown"] != case["natural_daemon_shutdown"]:
        raise PublicProofError("public_case_lifecycle_mismatch")
    return lifecycle


def _summary(value: object) -> dict[str, object]:
    summary = require_object(value, _SUMMARY_KEYS, "public_cleanup_summary_invalid")
    integer_keys = ("root_count", "file_count", "byte_count", "count", "bytes")
    if (any(type(summary.get(key)) is not int or summary[key] < 0 for key in integer_keys)
            or type(summary.get("complete")) is not bool
            or summary["bytes"] != summary["byte_count"]):
        raise PublicProofError("public_cleanup_summary_invalid")
    return summary


def _validate_cleanup(case: dict[str, object], source: dict[str, object]) -> dict[str, object]:
    cleanup = require_object(case.get("cleanup"), _CLEANUP_KEYS, "public_cleanup_invalid")
    observation = require_object(cleanup.get("observation"), _OBSERVATION_KEYS,
                                 "public_cleanup_observation_invalid")
    if observation.get("status") not in {
            "verified", "unverified", "missing", "malformed", "unreadable"}:
        raise PublicProofError("public_cleanup_observation_invalid")
    for key in ("supervisor_shutdown_observed",):
        if type(observation.get(key)) not in {bool, type(None)}:
            raise PublicProofError("public_cleanup_observation_invalid")
    for key, allowed in (("source_status", {None, "removed", "complete"}),
                         ("raw_process_captures", {None, "removed", "retained"}),
                         ("method", {None, "blocked-before-runtime-cleanup"})):
        if observation.get(key) not in allowed:
            raise PublicProofError("public_cleanup_observation_invalid")
    receipt_digest = observation.get("cleanup_receipt_sha256")
    if receipt_digest is not None:
        require_sha256(receipt_digest)
    _summary(cleanup.get("raw_before"))
    after = _summary(cleanup.get("raw_after"))
    journal_digest, authorization = cleanup.get("journal_sha256"), cleanup.get("authorization_sha256")
    _validate_case_retention(case["evidence_status"], cleanup["terminal_state"], cleanup["raw_deleted"],
                             journal_digest is not None or authorization is not None
                             or case["case_id"] in source["cleanup_journals"])
    if case["evidence_status"] == "RESOLVED":
        require_sha256(journal_digest)
        require_sha256(authorization)
        if (journal_digest != source["cleanup_journals"].get(case["case_id"])
                or after != {"root_count": 0, "file_count": 0, "byte_count": 0,
                             "complete": True, "count": 0, "bytes": 0}):
            raise PublicProofError("public_cleanup_binding_mismatch")
    return cleanup


def _validate_one_case(
        data: bytes, expected_case: CaseDefinition, identity: dict[str, object],
        source: dict[str, object], expected: ProofExpectation) -> tuple[dict[str, object], MatrixRow, bool]:
    value = _parse_canonical(data, "public_case_invalid")
    case = _validate_case_shape(value, expected_case)
    if case["source_compact_sha256"] != source["case_receipts"][expected_case.case_id]:
        raise PublicProofError("public_case_source_digest_mismatch")
    proof, sessions = _validate_public_proof_fields(case, expected)
    binaries, binary_ids = _validate_public_binary_facts(case, proof, identity, sessions)
    _validate_lifecycle(proof, case)
    _validate_cleanup(case, source)
    _validate_case_completion(case, proof, sessions,
                              (binaries, binary_ids))
    row = MatrixRow(expected_case, case["exit_code"], case["status"])
    unresolved = case["evidence_status"] == "UNRESOLVED"
    return case, row, unresolved


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


def _validate_case_results(parsed: _PublicArchive,
                           expected: ProofExpectation) -> dict[str, object]:
    identity = parsed.proof["identity"]
    aggregate = parsed.proof["aggregate"]
    source = parsed.manifest["source_sha256"]
    cases: list[dict[str, object]] = []
    rows: list[MatrixRow] = []
    unresolved_count = 0
    for definition in CASE_DEFINITIONS:
        case, row, unresolved = _validate_one_case(
            parsed.entries[f"cases/{definition.case_id}.json"], definition,
            identity, source, expected,
        )
        cases.append(case)
        rows.append(row)
        unresolved_count += int(unresolved)
    counts = {
        "pass_count": sum(row.status == "PASS" for row in rows),
        "blocked_count": sum(row.status == "BLOCKED" for row in rows),
        "failed_count": sum(row.status == "FAIL" for row in rows),
    }
    if (any(aggregate[key] != value for key, value in counts.items())
            or aggregate["unresolved_case_count"] != unresolved_count):
        raise PublicProofError("public_aggregate_case_counts_mismatch")
    verdict = _case_result_verdict(tuple(rows), unresolved_count > 0)
    if verdict != "FAIL":
        _require_release_cleanup(identity)
    if aggregate["case_results_verdict"] != verdict:
        raise PublicProofError("public_aggregate_verdict_mismatch")
    status = "FAIL" if verdict == "FAIL" else "PASS" if verdict == "PASS" else "BLOCKED"
    if aggregate["status"] != status or aggregate["exit_code"] != _aggregate_exit(
            status, verdict, aggregate["require_at_least_one_pass"]):
        raise PublicProofError("public_aggregate_status_mismatch")
    gate_status = "PASS" if counts["pass_count"] >= 1 and counts["failed_count"] == 0 \
        and unresolved_count == 0 else "BLOCKED"
    return {
        "status": gate_status, "case_results_verdict": verdict,
        **counts, "unresolved_case_count": unresolved_count,
        "expected_case_count": len(CASE_IDS), "cases": cases,
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
