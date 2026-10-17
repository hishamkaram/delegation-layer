"""Contract for exact release verification."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import hashlib
import json
import re


MAX_RECEIPT = 32 * 1024


MAX_OUTPUT = 64 * 1024


MAX_ARCHIVE = 256 * 1024 * 1024


MAX_BINARY = 128 * 1024 * 1024


MAX_SOURCE_FILE = 8 * 1024 * 1024


MAX_TREE_ENTRIES = 4096


BINARIES = ("delegate", "delegate-run", "pueue", "pueued")


TAG_RE = re.compile(r"^v([0-9]+\.[0-9]+\.[0-9]+)$")


CHECKSUM_RE = re.compile(r"^([0-9a-fA-F]{64})  ([^/\\\r\n]+)$")


REVISION_RE = re.compile(r"^[0-9a-f]{40,64}$")


SOURCE_FILES = {
    "archive-smoke": (".github/workflows/release.yml",),
    "native-acceptance": (
        ".github/workflows/release.yml",
        ".github/workflows/release-native-acceptance.yml",
        "scripts/acceptance_native_all.sh",
        "scripts/acceptance_native.py",
        "scripts/acceptance_provider_common.py",
        "scripts/native_acceptance_evidence.py",
    ),
}


class VerificationError(Exception):
    def __init__(self, code: str, stage: str):
        super().__init__(code)
        self.code, self.stage = code, stage


def expected_archive(info: dict[str, str], tag: str) -> str:
    match = TAG_RE.fullmatch(tag)
    if match is None:
        raise VerificationError("invalid_release_tag", "validate-tag")
    return f"delegation-layer_{match.group(1)}_{info['os']}_{info['arch']}.tar.gz"


def binary_version(output: bytes, name: str) -> str:
    try:
        line = output.decode("utf-8").splitlines()[0].strip()
        value = line[len(name) + 1:].split(maxsplit=1)[0] if line.startswith(name + " ") else ""
    except (UnicodeError, IndexError):
        value = ""
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}", value):
        raise VerificationError("version_output_invalid", f"version-{name}")
    return value


def new_receipt(kind: str, tag: str, revision: str) -> dict:
    match = TAG_RE.fullmatch(tag)
    return {
        "schema_version": 1, "claim": "packaged-default-path", "gate": kind,
        "verdict": "in_progress", "stage": "initialize", "tag": tag,
        "package_version": match.group(1) if match else None,
        # source_revision is retained as a compatibility label for older
        # consumers; the distinct fields below are authoritative.
        "source_revision": revision, "harness_revision": revision, "release_revision": None,
        "release_identity": {},
        "platform": {}, "artifact": {}, "binaries": {}, "delegate_build_identity": {},
        "default_path_observation": {}, "commands": [], "source_harness": {},
        "cleanup": {"status": "pending", "removed_paths": [], "retained_paths": [],
                    "directory_identities": {}, "errors": []},
        "failure": None,
        "unresolved_limits": [
            "No authenticated provider acceptance or task execution." if kind == "archive-smoke"
            else "Native evidence covers only the providers and scenarios recorded by acceptance_native_all.sh."
        ],
    }


def set_failure(receipt: dict, code: str, stage: str) -> None:
    receipt.update({"verdict": "fail", "stage": stage, "failure": {"code": code, "stage": stage}})


def _release_cleanup_authorization(
        receipt: dict, final_verdict: str, target_identity: dict) -> str:
    immutable = {
        key: value for key, value in receipt.items()
        if key not in {"cleanup", "completion", "proof", "verdict", "stage", "failure"}
    }
    immutable["final_verdict"] = final_verdict
    immutable["cleanup_target_identity"] = target_identity
    data = json.dumps(
        immutable, sort_keys=True, separators=(",", ":"), allow_nan=False,
    ).encode()
    return hashlib.sha256(data).hexdigest()


def _release_cleanup_transaction(receipt: dict) -> tuple[dict, dict, str]:
    cleanup = receipt.get("cleanup")
    proof = receipt.get("proof")
    transaction = cleanup.get("transaction") if isinstance(cleanup, dict) else None
    if (not isinstance(cleanup, dict) or not isinstance(proof, dict)
            or proof.get("state") != "committed"
            or proof.get("final_verdict") not in {"pass", "blocked"}
            or not isinstance(proof.get("authorization_sha256"), str)
            or not isinstance(transaction, dict)
            or transaction.get("authorization_sha256") != proof["authorization_sha256"]
            or not isinstance(transaction.get("journal_path"), str)
            or not isinstance(transaction.get("target_identity"), dict)):
        raise VerificationError("release_cleanup_journal_invalid", "cleanup-replay")
    authorization = _release_cleanup_authorization(
        receipt, proof["final_verdict"], transaction["target_identity"],
    )
    if authorization != proof["authorization_sha256"]:
        raise VerificationError("release_cleanup_authorization_changed", "cleanup-replay")
    return cleanup, transaction, authorization


@dataclass(frozen=True)
class _ArchiveEvidence:
    aggregate_path: Path
    extracted: Path
    platform: dict[str, str]
    release_identity: dict
    artifact: dict
    observation: dict
    build: dict
    receipt_sha256: str


def _validate_archive_header(
        archived: dict, tag: str, source_revision: str, repo: str | None,
        info: dict[str, str]) -> tuple[dict, str]:
    match = TAG_RE.fullmatch(tag)
    if (match is None or archived.get("claim") != "packaged-default-path"
            or archived.get("gate") != "native-acceptance" or archived.get("verdict") != "pass"
            or archived.get("tag") != tag or archived.get("package_version") != match.group(1)
            or archived.get("platform") != info
            or archived.get("harness_revision") != source_revision
            or archived.get("source_revision") != source_revision):
        raise VerificationError("archive_receipt_not_passing", "validate-archive-receipt")
    release_identity = archived.get("release_identity")
    if (not isinstance(release_identity, dict)
            or release_identity.get("tag") != tag
            or release_identity.get("ref") != f"refs/tags/{tag}"
            or (repo is not None
                and release_identity.get("repository") != repo)
            or not REVISION_RE.fullmatch(str(release_identity.get("commit_sha", "")))
            or archived.get("release_revision") != release_identity.get("commit_sha")):
        raise VerificationError("release_identity_unbound", "validate-archive-receipt")
    return release_identity, match.group(1)


def _validate_archived_default_path(archived: dict) -> dict:
    observation = archived.get("default_path_observation")
    if (not isinstance(observation, dict) or observation.get("path_source") != "PATH"
            or observation.get("command_name") != "delegate-default-supervisor-status"
            or observation.get("explicit_path_overrides") != []
            or (observation.get("state_before"), observation.get("observed_state"), observation.get("state_after"))
            != ("absent", "not_initialized", "absent")
            or observation.get("delegate_pueue_config_override") != "unset"):
        raise VerificationError("default_path_observation_invalid", "validate-archive-receipt")
    commands = archived.get("commands")
    required_commands = {
        "github-attestation-verify", "delegate-version", "pueue-version", "pueued-version",
        "delegate-providers", "delegate-capabilities", "delegate-default-supervisor-status",
    }
    if not isinstance(commands, list):
        raise VerificationError("archive_receipt_inconclusive", "validate-archive-receipt")
    command_map = {row.get("name"): row for row in commands if isinstance(row, dict)}
    if (len(command_map) != len(commands) or not required_commands.issubset(command_map)
            or any(row.get("exit_code") != 0 for row in command_map.values())
            or command_map["delegate-default-supervisor-status"].get("argv")
            != ["delegate", "supervisor", "status", "--json"]):
        raise VerificationError("archive_receipt_inconclusive", "validate-archive-receipt")
    return observation


def _native_case_hashes(aggregate: dict, receipt: dict) -> list[dict]:
    aggregate_cases = aggregate.get("cases")
    if not isinstance(aggregate_cases, list):
        raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
    expected_case_binaries = {
        "delegate_sha256": receipt["binaries"]["delegate"]["sha256"],
        "runner_sha256": receipt["binaries"]["delegate-run"]["sha256"],
        "pueue_sha256": receipt["binaries"]["pueue"]["sha256"],
        "pueued_sha256": receipt["binaries"]["pueued"]["sha256"],
    }
    for case in aggregate_cases:
        if (not isinstance(case, dict) or case.get("status") != "PASS"
                or case.get("evidence_status") != "RESOLVED"):
            continue
        identities = case.get("binary_identities")
        hashes = identities.get("sha256") if isinstance(identities, dict) else None
        if (not isinstance(hashes, dict) or identities.get("state") != "complete"
                or any(hashes.get(key) != digest
                       for key, digest in expected_case_binaries.items())):
            raise VerificationError(
                "native_case_binary_identity_mismatch", "validate-binary-identity",
            )
    case_hashes = [
        {
            "case_id": case["case_id"], "provider": case["provider"],
            "permission": case["permission"], "scenario": case["scenario"],
            "status": case["status"], "exit_code": case["exit_code"],
            "evidence_status": case["evidence_status"],
            "receipt_path": case["receipt_path"], "receipt_sha256": case["receipt_sha256"],
        }
        for case in aggregate_cases if isinstance(case, dict)
    ]
    if len(case_hashes) != len(aggregate_cases):
        raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
    return case_hashes


def _record_native_verdict(aggregate: dict, receipt: dict) -> None:
    aggregate_status = aggregate["status"]
    if aggregate_status == "FAIL":
        failure_code = (
            "native_acceptance_failed"
            if aggregate.get("case_results_verdict") == "FAIL"
            else "native_acceptance_evidence_unresolved"
        )
        raise VerificationError(failure_code, "native-acceptance")
    if aggregate_status == "BLOCKED":
        policy = aggregate.get("policy")
        require_pass = isinstance(policy, dict) and policy.get("require_at_least_one_pass") is True
        if require_pass:
            raise VerificationError("native_acceptance_pass_required", "native-acceptance-policy")
        receipt["verdict"] = "blocked"
