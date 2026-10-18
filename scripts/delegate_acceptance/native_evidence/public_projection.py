"""Build a bounded, privacy-filtered public proof from staged evidence."""

from __future__ import annotations

import json
from pathlib import Path

from ..evidence.files import write_bytes

from ..release_verification.contract import (
    VerificationError,
    _release_cleanup_transaction,
    _validate_archived_default_path,
    expected_archive,
)
from .contract import (
    AGGREGATE_RECEIPT_NAME,
)
from .contract import (
    CaseDefinition,
    EvidenceError,
    MatrixRow,
    _case_cleanup_authorization,
    _case_cleanup_journal_present,
    _case_result_verdict,
    _validate_aggregate_result,
    _validate_case_binaries,
    _validate_case_completion,
    _validate_case_retention,
    _validate_case_fields,
    _validate_case_proof_identity,
    _validate_case_sessions,
    expected_cases,
)
from .public_contract import (
    CASE_IDS,
    MAX_SOURCE_FINGERPRINT_FILES,
    PUBLIC_MANIFEST_KIND,
    PUBLIC_PROOF_KIND,
    PUBLIC_SCHEMA_VERSION,
    OPAQUE_TEXT,
    TASK_ID,
    ProofExpectation,
    PublicProofError,
    canonical_public_zip,
    canonical_json,
    parse_json,
    require_object,
    require_public_text,
    require_sha256,
    safe_repo_path,
    sha256,
    validate_expectation,
)
from .public_stage import StageSnapshot, load_stage
from .public_validation import inspect_public_proof, validate_public_proof


_CASE_BINARY_HASH_KEYS = (
    "provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256",
)


def _object(data: bytes, code: str) -> dict[str, object]:
    value = parse_json(data, code)
    if not isinstance(value, dict):
        raise PublicProofError(code)
    return value


def _fingerprint(value: object, expected_digest: str | None = None) -> dict[str, object]:
    fingerprint = require_object(value, {"sha256", "files"}, "public_fingerprint_invalid")
    digest = require_sha256(fingerprint.get("sha256"))
    rows = fingerprint.get("files")
    if (not isinstance(rows, list) or not rows
            or len(rows) > MAX_SOURCE_FINGERPRINT_FILES):
        raise PublicProofError("public_fingerprint_files_invalid")
    files: list[dict[str, str]] = []
    for row in rows:
        item = require_object(row, {"path", "sha256"}, "public_fingerprint_file_invalid")
        path = safe_repo_path(item.get("path"))
        files.append({"path": path, "sha256": require_sha256(item.get("sha256"))})
    names = [item["path"] for item in files]
    if names != sorted(set(names)):
        raise PublicProofError("public_fingerprint_order_invalid")
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    if sha256(encoded) != digest or expected_digest is not None and digest != expected_digest:
        raise PublicProofError("public_fingerprint_digest_mismatch")
    return {"sha256": digest, "files": files}


def _case_cleanup_source(
        compact: dict[str, object], case_id: str, snapshot: StageSnapshot) -> tuple[str | None, str | None]:
    cleanup = compact.get("cleanup")
    deletion = compact.get("raw_deletion")
    if not isinstance(cleanup, dict) or not isinstance(deletion, dict):
        raise PublicProofError("public_case_cleanup_invalid")
    _validate_case_retention(compact.get("evidence_status"), compact.get("terminal_state"),
                             cleanup.get("raw_deleted"),
                             _case_cleanup_journal_present(deletion) or case_id in snapshot.cleanup)
    if compact.get("evidence_status") != "RESOLVED":
        return None, None
    entry = snapshot.cleanup.get(case_id)
    identity = compact.get("creation_identity")
    journal_sha = deletion.get("journal_sha256")
    authorization = deletion.get("authorization_sha256")
    if (entry is None or not isinstance(identity, dict)
            or entry["sha256"] != journal_sha
            or entry["authorization_sha256"] != authorization
            or not isinstance(journal_sha, str)
            or not isinstance(authorization, str)
            or _case_cleanup_authorization(compact) != authorization):
        raise PublicProofError("public_case_cleanup_binding_invalid")
    journal = _object(snapshot.data[entry["path"]], "public_case_cleanup_journal_invalid")
    if (journal.get("schema_version") != 1 or journal.get("kind") != "owned-directory-cleanup"
            or journal.get("state") != "complete" or journal.get("target") != identity
            or journal.get("authorization_sha256") != authorization):
        raise PublicProofError("public_case_cleanup_journal_invalid")
    return journal_sha, authorization


def _source_status(value: object, choices: set[str]) -> str | None:
    return value if isinstance(value, str) and value in choices else None


def _public_option(value: object) -> str | None:
    if value is None:
        return None
    try:
        return require_public_text(value, code="public_case_option_invalid")
    except PublicProofError:
        return None


def _public_sha(value: object) -> str | None:
    if value is None:
        return None
    try:
        return require_sha256(value)
    except PublicProofError:
        return None


def _public_summary(value: object) -> dict[str, object]:
    keys = {"root_count", "file_count", "byte_count", "complete", "count", "bytes"}
    summary = require_object(value, keys, "public_cleanup_summary_invalid")
    if (any(type(summary[key]) is not int or summary[key] < 0
            for key in ("root_count", "file_count", "byte_count", "count", "bytes"))
            or type(summary["complete"]) is not bool or summary["bytes"] != summary["byte_count"]):
        raise PublicProofError("public_cleanup_summary_invalid")
    return summary


def _project_case(
        definition: CaseDefinition, reference: dict[str, object], compact_bytes: bytes,
        snapshot: StageSnapshot) -> dict[str, object]:
    compact = _object(compact_bytes, "public_case_receipt_invalid")
    compact_digest = snapshot.digests[f"case-receipts/{definition.case_id}.json"]
    if reference.get("receipt_sha256") != compact_digest:
        raise PublicProofError("public_case_receipt_digest_mismatch")
    try:
        _validate_case_fields(reference)
        proof = _validate_case_proof_identity(reference, compact)
        sessions = _validate_case_sessions(proof)
        binaries, binary_ids = _validate_case_binaries(reference, proof)
        _validate_case_completion(reference, proof, sessions, (binaries, binary_ids))
        journal_digest, authorization = _case_cleanup_source(compact, definition.case_id, snapshot)
    except EvidenceError as error:
        raise PublicProofError("public_case_source_invalid") from error
    original_cleanup = compact["cleanup"]
    observation = original_cleanup.get("observation")
    if not isinstance(observation, dict):
        raise PublicProofError("public_case_cleanup_invalid")
    lifecycle = proof["lifecycle_proof"]
    lifecycle = lifecycle if isinstance(lifecycle, dict) else {}
    source_binary_hashes = binaries.get("sha256")
    source_binary_hashes = source_binary_hashes if isinstance(source_binary_hashes, dict) else {}
    safe_hashes = {
        name: source_binary_hashes[name] for name in _CASE_BINARY_HASH_KEYS
        if name in source_binary_hashes
    }
    public_proof = {
        "schema_version": PUBLIC_SCHEMA_VERSION, "kind": "public-native-acceptance-case",
        "case_id": definition.case_id, "provider": definition.provider,
        "permission": definition.permission, "scenario": definition.scenario,
        "status": reference["status"], "exit_code": reference["exit_code"],
        "evidence_status": reference["evidence_status"], "source_compact_sha256": compact_digest,
        "source_proof": _project_case_proof(proof, binaries, safe_hashes, lifecycle),
        "cleanup": _project_case_cleanup(compact, observation, journal_digest, authorization),
        "natural_daemon_shutdown": compact.get("natural_daemon_shutdown")
        if type(compact.get("natural_daemon_shutdown")) is bool else None,
    }
    return public_proof


def _project_case_proof(
        proof: dict[str, object], binaries: dict[str, object], hashes: dict[str, object],
        lifecycle: dict[str, object]) -> dict[str, object]:
    model = _public_option(proof.get("requested_model"))
    effort = _public_option(proof.get("requested_effort"))
    options_valid = ((proof.get("requested_model") is None or model is not None)
                     and (proof.get("requested_effort") is None or effort is not None))
    source_digest = _public_sha(proof.get("source_receipt_sha256"))
    sessions = proof.get("session_ids")
    bindings = proof.get("task_session_bindings")
    task_ids = proof.get("task_ids")
    diagnostics = proof.get("diagnostic_task_ids")
    sessions_valid = (
        isinstance(sessions, dict) and isinstance(sessions.get("values"), list)
        and isinstance(sessions.get("by_alias"), dict) and isinstance(bindings, list)
        and isinstance(task_ids, list) and isinstance(diagnostics, list)
    )
    if sessions_valid:
        aliases = sessions["by_alias"]
        session_values = sessions["values"]
        sessions_valid = (
            all(isinstance(value, str) and OPAQUE_TEXT.fullmatch(value) is not None
                for value in session_values)
            and all(isinstance(alias, str) and require_public_text(alias, opaque=True)
                    and isinstance(value, str) and OPAQUE_TEXT.fullmatch(value) is not None
                    for alias, value in aliases.items())
            and all(
                isinstance(item, dict) and set(item) == {"alias", "task_id", "session_id"}
                and isinstance(item.get("alias"), str)
                and require_public_text(item["alias"], opaque=True)
                and isinstance(item.get("task_id"), str)
                and TASK_ID.fullmatch(item["task_id"]) is not None
                and isinstance(item.get("session_id"), str)
                and OPAQUE_TEXT.fullmatch(item["session_id"]) is not None
                for item in bindings
            )
            and all(isinstance(item, str) and TASK_ID.fullmatch(item) is not None
                    for item in [*task_ids, *diagnostics])
        )
    if not sessions_valid:
        sessions = {"state": "not-recorded", "values": [], "by_alias": {}}
        bindings, task_ids, diagnostics = [], [], []
    safe_lifecycle = {
        "state": _source_status(lifecycle.get("state"), {"complete", "unresolved"}),
        "cleanup_status": _source_status(lifecycle.get("cleanup_status"), {
            "verified", "unverified", "missing", "malformed", "unreadable",
        }),
        "supervisor_shutdown_observed": lifecycle.get("supervisor_shutdown_observed")
        if type(lifecycle.get("supervisor_shutdown_observed")) is bool else None,
        "natural_daemon_shutdown": lifecycle.get("natural_daemon_shutdown")
        if type(lifecycle.get("natural_daemon_shutdown")) is bool else None,
        "failure_diagnostics_sha256": _public_sha(lifecycle.get("failure_diagnostics_sha256")),
    }
    return {
        "schema_version": PUBLIC_SCHEMA_VERSION,
        "kind": "public-native-acceptance-case-proof",
        "state": proof.get("state"), "final_status": proof.get("final_status"),
        "source_status": _source_status(proof.get("source_status"),
                                        {"pass", "passed", "blocked", "block", "fail", "failed"}),
        "source_receipt_sha256": source_digest,
        "gate": proof.get("gate"), "provider": proof.get("provider"),
        "permission": proof.get("permission"), "mode": proof.get("mode"),
        "scenario": proof.get("scenario"),
        "requested_options_state": proof.get("requested_options_state") if options_valid else "invalid",
        "requested_model": model, "requested_effort": effort,
        "task_ids": task_ids, "task_session_bindings": bindings,
        "diagnostic_task_ids": diagnostics, "session_ids": sessions,
        "binary_identities": {
            "state": binaries.get("state"), "sha256": hashes,
            "task_ids": binaries.get("task_ids"),
        },
        "lifecycle_proof": safe_lifecycle,
    }


def _project_case_cleanup(
        compact: dict[str, object], observation: dict[str, object],
        journal_digest: str | None, authorization: str | None) -> dict[str, object]:
    cleanup = compact.get("cleanup")
    if not isinstance(cleanup, dict):
        raise PublicProofError("public_case_cleanup_invalid")
    return {
        "observation": {
            "status": _source_status(observation.get("status"), {
                "verified", "unverified", "missing", "malformed", "unreadable",
            }),
            "source_status": _source_status(observation.get("source_status"), {"removed", "complete"}),
            "supervisor_shutdown_observed": observation.get("supervisor_shutdown_observed")
            if type(observation.get("supervisor_shutdown_observed")) is bool else None,
            "raw_process_captures": _source_status(observation.get("raw_process_captures"),
                                                    {"removed", "retained"}),
            "method": _source_status(observation.get("method"), {"blocked-before-runtime-cleanup"}),
            "cleanup_receipt_sha256": _public_sha(observation.get("cleanup_receipt_sha256")),
        },
        "raw_before": _public_summary(cleanup.get("raw_before")),
        "raw_after": _public_summary(cleanup.get("raw_after")),
        "raw_deleted": cleanup.get("raw_deleted") if type(cleanup.get("raw_deleted")) is bool else None,
        "terminal_state": compact["terminal_state"],
        "journal_sha256": journal_digest, "authorization_sha256": authorization,
    }


def _validate_archive_attestation(archive: dict[str, object], expected: ProofExpectation) -> None:
    artifact = archive.get("artifact")
    if not isinstance(artifact, dict):
        raise PublicProofError("public_archive_artifact_invalid")
    name = expected_archive({"os": expected.platform_os, "arch": expected.platform_arch}, expected.tag)
    if (archive.get("claim") != "packaged-default-path" or archive.get("verdict") != "pass"
            or archive.get("tag") != expected.tag or archive.get("harness_revision") != expected.harness_revision
            or archive.get("source_revision") != expected.harness_revision
            or archive.get("release_revision") != expected.release_revision
            or artifact.get("archive_name") != name or artifact.get("archive_sha256") != expected.archive_sha256
            or artifact.get("attestation") != "verified"):
        raise PublicProofError("public_archive_identity_mismatch")
    commands = archive.get("commands")
    if not isinstance(commands, list) or len(commands) > 64:
        raise PublicProofError("public_attestation_command_missing")
    selected: dict[str, dict[str, object]] = {}
    for row in commands:
        if (not isinstance(row, dict) or not isinstance(row.get("name"), str)
                or row.get("name") in selected or type(row.get("exit_code")) is not int
                or row["exit_code"] != 0):
            raise PublicProofError("public_archive_command_invalid")
        selected[row["name"]] = row
    attestation = selected.get("github-attestation-verify")
    argv = attestation.get("argv") if attestation is not None else None
    archive_path = artifact.get("archive_path")
    if (not isinstance(archive_path, str) or not archive_path.startswith("/")
            or not isinstance(argv, list) or len(argv) != 12
            or argv != [
                "gh", "attestation", "verify", archive_path, "--repo", expected.repo,
                "--signer-workflow", f"{expected.repo}/.github/workflows/release.yml",
                "--source-ref", f"refs/tags/{expected.tag}",
                "--source-digest", expected.release_revision,
            ]):
        raise PublicProofError("public_attestation_binding_invalid")
    try:
        _validate_archived_default_path(archive)
    except (KeyError, TypeError, ValueError, VerificationError) as error:
        raise PublicProofError("public_archive_observation_invalid") from error


def _public_binary_map(value: object) -> dict[str, str]:
    if not isinstance(value, dict) or set(value) != {"delegate", "delegate-run", "pueue", "pueued"}:
        raise PublicProofError("public_release_binary_map_invalid")
    result: dict[str, str] = {}
    for source, public in (("delegate", "delegate"), ("delegate-run", "runner"),
                           ("pueue", "pueue"), ("pueued", "pueued")):
        item = value[source]
        if not isinstance(item, dict):
            raise PublicProofError("public_release_binary_map_invalid")
        result[public] = require_sha256(item.get("sha256"))
    return result


def _project_release_cleanup(
        receipt: dict[str, object], alias: str, snapshot: StageSnapshot, *,
        resource_id: str) -> dict[str, object]:
    cleanup = receipt.get("cleanup")
    proof = receipt.get("proof")
    transaction = cleanup.get("transaction") if isinstance(cleanup, dict) else None
    completion = receipt.get("completion")
    transaction = transaction if isinstance(transaction, dict) else {}
    completion = completion if isinstance(completion, dict) else {}
    entry = snapshot.release_cleanup.get(alias)
    journal_digest = transaction.get("journal_sha256")
    authorization = transaction.get("authorization_sha256")
    if entry is None:
        if journal_digest is not None or authorization is not None:
            raise PublicProofError("public_release_cleanup_missing")
        journal_digest, authorization = None, None
    else:
        journal = _object(snapshot.data[entry["path"]], "public_release_cleanup_journal_invalid")
        try:
            _cleanup, expected_transaction, expected_authorization = _release_cleanup_transaction(receipt)
        except VerificationError as error:
            raise PublicProofError("public_release_cleanup_authorization_invalid") from error
        if (journal.get("state") != "complete" or journal.get("authorization_sha256") != authorization
                or entry["sha256"] != journal_digest or entry["authorization_sha256"] != authorization
                or authorization != expected_authorization
                or journal.get("target") != expected_transaction.get("target_identity")):
            raise PublicProofError("public_release_cleanup_journal_invalid")
    final_verdict = proof.get("final_verdict") if isinstance(proof, dict) else None
    return {
        "status": cleanup.get("status") if isinstance(cleanup, dict) else "pending",
        "completion_state": completion.get("state"),
        "final_verdict": final_verdict,
        "authorization_sha256": authorization,
        "journal_sha256": journal_digest,
        "resource_id": resource_id,
    }


def _release_resource_id(archive: dict[str, object], native: dict[str, object]) -> str:
    artifact = archive["artifact"]
    cleanup, native_cleanup = archive.get("cleanup"), native.get("cleanup")
    if (not isinstance(artifact, dict) or not isinstance(cleanup, dict)
            or not isinstance(native_cleanup, dict)
            or cleanup.get("status") != "retained-for-native-acceptance"
            or cleanup.get("transaction") is not None or archive.get("completion") is not None):
        raise PublicProofError("public_archive_retention_invalid")
    assets = str(Path(str(artifact["archive_path"])).parent)
    identities, native_identities = cleanup.get("directory_identities"), native_cleanup.get("directory_identities")
    if not isinstance(identities, dict) or not isinstance(native_identities, dict):
        raise PublicProofError("public_release_resource_unbound")
    identity = require_object(identities.get(assets), {
        "path", "parent", "device", "inode", "parent_device", "parent_inode",
    }, "public_release_resource_invalid")
    if (identity.get("path") != assets or identity.get("parent") != str(Path(assets).parent)
            or any(type(identity.get(name)) is not int or identity[name] < 0
                   for name in ("device", "inode", "parent_device", "parent_inode"))
            or native_identities.get(assets) != identity):
        raise PublicProofError("public_release_resource_unbound")
    transaction = native_cleanup.get("transaction")
    if native_cleanup.get("status") == "complete" and (
            not isinstance(transaction, dict) or transaction.get("target_identity") != identity):
        raise PublicProofError("public_release_resource_unbound")
    return sha256(canonical_json(identity))


def _validate_release_identity_bindings(
        archive: dict[str, object], native: dict[str, object],
        expected: ProofExpectation) -> tuple[dict[str, object], dict[str, str]]:
    platform = {"os": expected.platform_os, "arch": expected.platform_arch}
    release = archive.get("release_identity")
    if (archive.get("gate") != "native-acceptance"
            or native.get("gate") != "native-acceptance"
            or not isinstance(release, dict) or release != native.get("release_identity")):
        raise PublicProofError("public_release_identity_mismatch")
    if release != {
            "repository": expected.repo, "tag": expected.tag,
            "ref": f"refs/tags/{expected.tag}", "commit_sha": expected.release_revision,
    }:
        raise PublicProofError("public_release_identity_mismatch")
    for receipt in (archive, native):
        receipt_platform = receipt.get("platform")
        if (not isinstance(receipt_platform, dict)
                or receipt_platform.get("os") != platform["os"]
                or receipt_platform.get("arch") != platform["arch"]
                or receipt.get("tag") != expected.tag
                or receipt.get("harness_revision") != expected.harness_revision
                or receipt.get("source_revision") != expected.harness_revision
                or receipt.get("release_revision") != expected.release_revision):
            raise PublicProofError("public_release_platform_or_revision_mismatch")
    return release, platform


def _release_identity(
        snapshot: StageSnapshot, expected: ProofExpectation,
        aggregate: dict[str, object]) -> dict[str, object]:
    archive = _object(snapshot.data["archive-receipt.json"], "public_archive_receipt_invalid")
    native = _object(snapshot.data["native-acceptance-receipt.json"], "public_native_receipt_invalid")
    release, platform = _validate_release_identity_bindings(archive, native, expected)
    archive_source = _fingerprint(archive.get("source_harness"), expected.source_harness_sha256)
    native_source = _fingerprint(native.get("source_harness"), expected.source_harness_sha256)
    artifact = archive.get("artifact")
    native_artifact = native.get("artifact")
    if not isinstance(artifact, dict) or not isinstance(native_artifact, dict):
        raise PublicProofError("public_archive_artifact_invalid")
    _validate_archive_attestation(archive, expected)
    archive_binaries = _public_binary_map(archive.get("binaries"))
    native_binaries = _public_binary_map(native.get("binaries"))
    expected_binaries = {
        "delegate": expected.delegate_sha256, "runner": expected.runner_sha256,
        "pueue": expected.pueue_sha256, "pueued": expected.pueued_sha256,
    }
    if archive_binaries != expected_binaries or native_binaries != expected_binaries:
        raise PublicProofError("public_release_binary_mismatch")
    if any(native_artifact.get(key) != artifact.get(key) for key in (
            "archive_name", "archive_sha256", "checksums_sha256")):
        raise PublicProofError("public_native_archive_identity_mismatch")
    native_acceptance = native.get("native_acceptance")
    if (native.get("archive_receipt_sha256") != snapshot.digests["archive-receipt.json"]
            or not isinstance(native_acceptance, dict)
            or native.get("aggregate_receipt_sha256") != snapshot.digests[AGGREGATE_RECEIPT_NAME]
            or native_acceptance.get("aggregate_receipt_sha256")
            != snapshot.digests[AGGREGATE_RECEIPT_NAME]):
        raise PublicProofError("public_native_receipt_binding_mismatch")
    if (aggregate.get("tag") != expected.tag
            or aggregate.get("source_revision") != expected.harness_revision
            or aggregate.get("harness_revision") != expected.harness_revision):
        raise PublicProofError("public_aggregate_identity_mismatch")
    _validate_native_case_hashes(native, aggregate, snapshot)
    resource_id = _release_resource_id(archive, native)
    archive_cleanup = _project_release_cleanup(archive, "archive", snapshot, resource_id=resource_id)
    native_cleanup = _project_release_cleanup(native, "native", snapshot, resource_id=resource_id)
    return {
        "repo": expected.repo, "tag": expected.tag,
        "release_revision": expected.release_revision,
        "harness_revision": expected.harness_revision,
        "opencode_model": expected.opencode_model,
        "ccr_model": expected.ccr_model,
        "release_identity": release, "platform": platform,
        "archive": {
            "name": artifact.get("archive_name"),
            "sha256": artifact.get("archive_sha256"),
            "checksums_sha256": artifact.get("checksums_sha256"),
            "attestation": "verified",
            "attestation_binding": {
                "repository": expected.repo, "source_ref": f"refs/tags/{expected.tag}",
                "source_revision": expected.release_revision,
                "archive_sha256": expected.archive_sha256,
            },
        },
        "source_harness": {"sha256": native_source["sha256"]},
        "archive_source_harness": {"sha256": archive_source["sha256"]},
        "binaries": expected_binaries,
        "release_cleanup": {"archive": archive_cleanup, "native": native_cleanup},
    }


def _validate_native_case_hashes(
        native: dict[str, object], aggregate: dict[str, object], snapshot: StageSnapshot) -> None:
    receipt = native.get("native_acceptance")
    cases = aggregate.get("cases")
    receipt_rows = receipt.get("case_receipt_hashes") if isinstance(receipt, dict) else None
    if (not isinstance(cases, list) or not isinstance(receipt_rows, list)
            or len(cases) != len(CASE_IDS) or len(receipt_rows) != len(CASE_IDS)):
        raise PublicProofError("public_native_case_hashes_invalid")
    case_hashes = {case["case_id"]: case["receipt_sha256"] for case in cases
                   if isinstance(case, dict) and isinstance(case.get("case_id"), str)}
    receipt_hashes: dict[str, str] = {}
    for row in receipt_rows:
        item = require_object(row, {
            "case_id", "provider", "permission", "scenario", "status", "exit_code",
            "evidence_status", "receipt_path", "receipt_sha256",
        }, "public_native_case_hashes_invalid")
        case_id = item.get("case_id")
        digest = require_sha256(item.get("receipt_sha256"))
        if not isinstance(case_id, str) or case_id in receipt_hashes:
            raise PublicProofError("public_native_case_hashes_invalid")
        receipt_hashes[case_id] = digest
    staged = {case_id: item["sha256"] for case_id, item in snapshot.cases.items()}
    if case_hashes != staged or receipt_hashes != staged:
        raise PublicProofError("public_native_case_hashes_mismatch")


def _public_aggregate(value: dict[str, object], cases: list[dict[str, object]]) -> dict[str, object]:
    policy = value.get("policy")
    if not isinstance(policy, dict) or type(policy.get("require_at_least_one_pass")) is not bool:
        raise PublicProofError("public_aggregate_policy_invalid")
    unresolved = value.get("unresolved_case_count")
    rows = tuple(MatrixRow(
        CaseDefinition(str(case["case_id"]), str(case["provider"]), str(case["permission"]),
                       str(case["scenario"]), ""), int(case["exit_code"]), str(case["status"]),
    ) for case in cases)
    verdict = _case_result_verdict(rows, type(unresolved) is int and unresolved > 0)
    try:
        _validate_aggregate_result(value, cases)
    except EvidenceError as error:
        raise PublicProofError("public_aggregate_source_invalid") from error
    if not isinstance(unresolved, int):
        raise PublicProofError("public_aggregate_unresolved_invalid")
    return {
        "status": value.get("status"), "exit_code": value.get("exit_code"),
        "case_results_verdict": verdict,
        "pass_count": value.get("pass_count"), "blocked_count": value.get("blocked_count"),
        "failed_count": value.get("failed_count"), "unresolved_case_count": unresolved,
        "expected_case_count": len(CASE_IDS),
        "require_at_least_one_pass": policy["require_at_least_one_pass"],
    }


def _public_zip(snapshot: StageSnapshot, expected: ProofExpectation) -> bytes:
    aggregate = _object(snapshot.data[AGGREGATE_RECEIPT_NAME], "public_aggregate_receipt_invalid")
    cases_value = aggregate.get("cases")
    if not isinstance(cases_value, list) or len(cases_value) != len(CASE_IDS):
        raise PublicProofError("public_aggregate_cases_invalid")
    references: dict[str, dict[str, object]] = {}
    for reference in cases_value:
        if not isinstance(reference, dict) or not isinstance(reference.get("case_id"), str):
            raise PublicProofError("public_aggregate_cases_invalid")
        case_id = reference["case_id"]
        if case_id in references:
            raise PublicProofError("public_aggregate_case_duplicate")
        references[case_id] = reference
    definitions = expected_cases()
    if set(references) != {item.case_id for item in definitions}:
        raise PublicProofError("public_aggregate_case_matrix_invalid")
    public_cases = {
        f"cases/{definition.case_id}.json": canonical_json(_project_case(
            definition, references[definition.case_id],
            snapshot.data[f"case-receipts/{definition.case_id}.json"], snapshot,
        )) for definition in definitions
    }
    try:
        _validate_aggregate_result(aggregate, cases_value)
    except EvidenceError as error:
        raise PublicProofError("public_aggregate_source_invalid") from error
    identity = _release_identity(snapshot, expected, aggregate)
    proof = {
        "schema_version": PUBLIC_SCHEMA_VERSION, "kind": PUBLIC_PROOF_KIND,
        "identity": identity, "aggregate": _public_aggregate(aggregate, cases_value),
    }
    proof_bytes = canonical_json(proof)
    source_sha = {
        "stage_manifest": sha256(snapshot.manifest_bytes),
        "aggregate_receipt": snapshot.digests[AGGREGATE_RECEIPT_NAME],
        "archive_receipt": snapshot.digests["archive-receipt.json"],
        "native_receipt": snapshot.digests["native-acceptance-receipt.json"],
        "case_receipts": {case_id: snapshot.cases[case_id]["sha256"] for case_id in CASE_IDS},
        "cleanup_journals": {case_id: item["sha256"] for case_id, item in snapshot.cleanup.items()},
        "release_cleanup_journals": {alias: item["sha256"]
                                     for alias, item in snapshot.release_cleanup.items()},
    }
    payloads = {"proof.json": proof_bytes, **public_cases}
    manifest = {
        "schema_version": PUBLIC_SCHEMA_VERSION, "kind": PUBLIC_MANIFEST_KIND,
        "source_sha256": source_sha,
        "projected_members": [{"path": name, "sha256": sha256(data)}
                              for name, data in payloads.items()],
    }
    return canonical_public_zip({"manifest.json": canonical_json(manifest), **payloads})


def build_public_proof(
        stage_dir: Path, output_zip: Path, expectation: ProofExpectation) -> dict[str, object]:
    """Project local staged evidence without modifying its private originals."""
    validate_expectation(expectation)
    snapshot = load_stage(stage_dir)
    destination = output_zip.absolute()
    if destination.resolve(strict=False).is_relative_to(snapshot.root):
        raise PublicProofError("public_output_inside_private_stage")
    zip_data = _public_zip(snapshot, expectation)
    inspect_public_proof(zip_data)
    result = validate_public_proof(zip_data, expectation)
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
        write_bytes(destination, zip_data)
    except FileExistsError as error:
        raise PublicProofError("public_output_already_exists") from error
    except OSError as error:
        raise PublicProofError("public_output_write_failed") from error
    return result
