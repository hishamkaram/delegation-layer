"""Pure validation of the native lifetime receipt and its source identities."""

from __future__ import annotations

import hashlib
import json
import re
from typing import Mapping


MAX_STAGE_LENGTH = 128


MAX_EVIDENCE_ITEMS = 8


MAX_EVIDENCE_LENGTH = 160


FINAL_CLEANUP_STATUSES = {"released", "scope-retained", "handle-retained", "not-attempted"}


def native_go_architecture(machine: str) -> str:
    return {
        "x86_64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
        "i386": "386",
        "i686": "386",
    }.get(machine.lower(), machine.lower())


def _mapping(value: object) -> Mapping[str, object] | None:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        return None
    return value


def _has_exact_keys(value: Mapping[str, object], keys: set[str]) -> bool:
    return set(value.keys()) == keys


def _text(value: Mapping[str, object], key: str) -> str | None:
    item = value.get(key)
    return item if isinstance(item, str) else None


def _digest(value: object) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None


def _primitive_digest(value: Mapping[str, object]) -> str:
    encoded = json.dumps(value, separators=(",", ":"), ensure_ascii=True).encode("ascii")
    return hashlib.sha256(encoded).hexdigest()


def _bounded_text(value: object, maximum: int) -> bool:
    return isinstance(value, str) and 0 < len(value) <= maximum


def _bounded_evidence(value: object) -> bool:
    return (
        isinstance(value, list)
        and len(value) <= MAX_EVIDENCE_ITEMS
        and all(_bounded_text(item, MAX_EVIDENCE_LENGTH) for item in value)
    )


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def _validate_envelope(envelope: Mapping[str, object]) -> list[str]:
    errors: list[str] = []
    if not _has_exact_keys(
        envelope,
        {
            "schema_version",
            "claim_class",
            "primitive_receipt",
            "assurance_receipt",
            "last_stage",
            "cleanup_status",
            "retained_evidence",
            "unresolved_evidence",
            "failure",
            "verdict",
        },
    ):
        errors.append("receipt envelope fields are incomplete or unexpected")
    if type(envelope.get("schema_version")) is not int or envelope.get("schema_version") != 1 or envelope.get("claim_class") != "native-lifetime":
        errors.append("receipt schema or claim class is invalid")
    if envelope.get("verdict") != "passed":
        errors.append("native lifetime proof verdict is not passed")

    last_stage = envelope.get("last_stage")
    if not _bounded_text(last_stage, MAX_STAGE_LENGTH):
        errors.append("receipt last stage is missing or invalid")
    cleanup_status = envelope.get("cleanup_status")
    if not isinstance(cleanup_status, str) or cleanup_status not in FINAL_CLEANUP_STATUSES:
        errors.append("receipt cleanup status is missing or unresolved")
    retained_evidence = envelope.get("retained_evidence")
    unresolved_evidence = envelope.get("unresolved_evidence")
    if not _bounded_evidence(retained_evidence):
        errors.append("receipt retained evidence is missing or unbounded")
    if not _bounded_evidence(unresolved_evidence):
        errors.append("receipt unresolved evidence is missing or unbounded")
    failure = _mapping(envelope.get("failure"))
    if failure is None or not _has_exact_keys(failure, {"code", "stage", "message"}):
        errors.append("receipt failure details are incomplete or unexpected")
    else:
        if not all(_bounded_text(failure.get(key), MAX_STAGE_LENGTH) or failure.get(key) == "" for key in ("code", "stage", "message")):
            errors.append("receipt failure details are invalid")
        if envelope.get("verdict") == "passed":
            if failure != {"code": "none", "stage": "", "message": ""}:
                errors.append("passed receipt contains failure details")
        elif envelope.get("verdict") == "failed":
            if (not isinstance(failure.get("code"), str)
                    or failure.get("code") in {"", "none", "pending"}
                    or failure.get("stage") != last_stage or not failure.get("message")):
                errors.append("failed receipt does not bind structured failure details")
    if envelope.get("verdict") == "passed":
        if cleanup_status != "released":
            errors.append("passed receipt cleanup is not released")
        if retained_evidence != [] or unresolved_evidence != []:
            errors.append("passed receipt retains unresolved resource evidence")

    return errors


def _validate_primitive(
        primitive: Mapping[str, object], assurance: Mapping[str, object],
        envelope: Mapping[str, object], expected_os: str, expected_arch: str) -> list[str]:
    errors: list[str] = []
    primitive_keys = {
        "schema_version", "host_os", "host_kernel", "architecture", "backend", "assurance",
        "test", "primitive", "observed", "plan_sha256", "scope_identity_sha256", "source",
        "binary", "verdict",
    }
    assurance_keys = {
        "schema_version", "primitive_receipt_sha256", "assurance", "owner", "lease", "proof",
        "unresolved_limits",
    }
    if not _has_exact_keys(primitive, primitive_keys):
        errors.append("primitive receipt fields are incomplete or unexpected")
    if not _has_exact_keys(assurance, assurance_keys):
        errors.append("assurance receipt fields are incomplete or unexpected")

    host_os = _text(primitive, "host_os")
    host_kernel = _text(primitive, "host_kernel")
    architecture = _text(primitive, "architecture")
    if host_os != expected_os or architecture != expected_arch:
        errors.append("receipt host does not match the native runner")
    expected_kernel = "Linux " if expected_os == "linux" else "Darwin "
    if (
        host_kernel is None
        or not host_kernel.startswith(expected_kernel)
        or len(host_kernel) > 128
        or re.fullmatch(r"[A-Za-z0-9._ -]+", host_kernel) is None
    ):
        errors.append("receipt kernel identity is missing or invalid")

    backend = _text(primitive, "backend")
    assurance_name = _text(primitive, "assurance")
    expected_pairs = {
        "linux": {("cgroup-v2", "exact"), ("process-group", "cooperative")},
        "darwin": {("process-group", "cooperative")},
    }
    if (backend, assurance_name) not in expected_pairs.get(expected_os, set()):
        errors.append("receipt backend and assurance do not match the native backend matrix")
    if (type(primitive.get("schema_version")) is not int or primitive.get("schema_version") != 1
            or not isinstance(primitive.get("verdict"), str)
            or primitive.get("verdict") not in {"passed", "failed"}):
        errors.append("primitive proof verdict is invalid")
    elif envelope.get("verdict") == "passed" and primitive.get("verdict") != "passed":
        errors.append("primitive proof verdict is not passed")
    elif envelope.get("verdict") == "failed" and primitive.get("verdict") != "failed":
        errors.append("failed receipt primitive verdict is not failed")
    if primitive.get("test") != "TestNativeBackendProof":
        errors.append("receipt does not identify the required native proof test")
    if not _digest(primitive.get("plan_sha256")) or not _digest(primitive.get("scope_identity_sha256")):
        errors.append("plan or scope identity fingerprint is missing")

    return errors


def _validate_source(
        source: Mapping[str, object] | None, expected_source: tuple[str, str, str]) -> list[str]:
    errors: list[str] = []
    if source is None or not _has_exact_keys(source, {"revision", "package_sha256", "gate_sha256"}):
        errors.append("source identity is incomplete")
    else:
        revision = _text(source, "revision")
        if revision is None or re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", revision) is None:
            errors.append("source revision is invalid")
        if not _digest(source.get("package_sha256")) or not _digest(source.get("gate_sha256")):
            errors.append("source fingerprint is invalid")
        expected_revision, expected_package_hash, expected_gate_hash = expected_source
        if revision != expected_revision:
            errors.append("receipt source revision does not match the pre-launch fingerprint")
        if source.get("package_sha256") != expected_package_hash:
            errors.append("receipt source package hash does not match the pre-launch fingerprint")
        if source.get("gate_sha256") != expected_gate_hash:
            errors.append("receipt source gate hash does not match the pre-launch fingerprint")
    return errors


def _validate_binary(binary: Mapping[str, object] | None) -> list[str]:
    errors: list[str] = []
    if binary is None or not _has_exact_keys(binary, {"name", "sha256", "size_bytes", "go_version"}):
        errors.append("test binary identity is incomplete")
    else:
        name = _text(binary, "name")
        go_version = _text(binary, "go_version")
        size = binary.get("size_bytes")
        if name is None or not name or "/" in name or "\\" in name:
            errors.append("test binary name is invalid")
        if not _digest(binary.get("sha256")) or type(size) is not int or size <= 0 or size > 256 * 1024 * 1024:
            errors.append("test binary fingerprint is invalid")
        if go_version is None or not go_version.startswith("go") or len(go_version) > 32:
            errors.append("test binary Go version is invalid")

    return errors


def _validate_assurance(
        assurance: Mapping[str, object], primitive: Mapping[str, object],
        expected_os: str) -> list[str]:
    errors: list[str] = []
    backend = _text(primitive, "backend")
    assurance_name = _text(primitive, "assurance")
    if type(assurance.get("schema_version")) is not int or assurance.get("schema_version") != 1 or assurance.get("assurance") != assurance_name:
        errors.append("assurance receipt does not match the selected backend")
    if assurance.get("primitive_receipt_sha256") != _primitive_digest(primitive):
        errors.append("assurance receipt does not bind the primitive receipt")
    if assurance.get("owner") != "native proof test process":
        errors.append("assurance receipt owner is missing")
    if assurance.get("lease") != "test invocation owns the created scope until cleanup":
        errors.append("assurance receipt lease is missing")
    if assurance.get("proof") != (
        "production Prepare selected the backend; CreatePrepared created it; the observed child was waited and the scope was proven empty before destruction"
    ):
        errors.append("assurance receipt proof description is invalid")

    expected_primitive: str | None = None
    expected_observation = "prepared child terminated; command Wait consumed once; scope empty and destroyed"
    if expected_os == "linux" and backend == "cgroup-v2":
        expected_primitive = "cgroup-v2 cgroup.kill with setsid descendant"
        expected_observation = "setsid descendant terminated; command Wait consumed once; scope empty and destroyed"
    elif expected_os == "linux" and backend == "process-group":
        expected_primitive = "Linux private session with owner-bound process termination"
    elif expected_os == "darwin" and backend == "process-group":
        expected_primitive = "Darwin private session with process identity-checked termination"
    if primitive.get("primitive") != expected_primitive or primitive.get("observed") != expected_observation:
        errors.append("receipt does not name the native primitive and observation exercised")

    expected_limits = [
        "fixture_only_no_live_provider_acceptance",
        "application_lifetime_owner_lease_not_exercised",
        "cooperative_scope_owner_loss_remains_unverifiable"
        if backend == "process-group"
        else "cgroup_proof_limited_to_recorded_kernel_and_exercised_primitive",
    ]
    limits = assurance.get("unresolved_limits")
    if limits != expected_limits:
        errors.append("assurance receipt does not enumerate the required unresolved limits")
    return errors


def validate_receipt(
    value: object,
    expected_os: str,
    expected_arch: str,
    expected_source: tuple[str, str, str],
) -> list[str]:
    errors: list[str] = []
    envelope = _mapping(value)
    if envelope is None:
        return ["receipt must be a JSON object"]
    errors.extend(_validate_envelope(envelope))
    primitive = _mapping(envelope.get("primitive_receipt"))
    assurance = _mapping(envelope.get("assurance_receipt"))
    if primitive is None or assurance is None:
        return errors + ["primitive and assurance receipts are required"]
    errors.extend(_validate_primitive(primitive, assurance, envelope, expected_os, expected_arch))
    errors.extend(_validate_source(_mapping(primitive.get("source")), expected_source))
    errors.extend(_validate_binary(_mapping(primitive.get("binary"))))
    errors.extend(_validate_assurance(assurance, primitive, expected_os))
    return errors
