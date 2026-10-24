"""Immutable public proof inputs and bounded serialization primitives.

This module owns the one canonical public ZIP encoding and its inspection,
canonical JSON, and the privacy rule every public member obeys. Release and
candidate envelopes share them; each envelope owns its own identity.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
from io import BytesIO
import json
import re
import stat
import zipfile

from .block_reason import encode_block_reason
from .contract import BlockReason, EvidenceError, expected_cases
from ..release_verification.contract import (
    MAX_TREE_ENTRIES, BinaryDigests, VerificationError, parse_binary_digests,
)


PUBLIC_SCHEMA_VERSION = 1
# Release public cases keep the envelope's version 1. Candidate public cases
# are version 2: they add the typed block reason, non-null exactly for BLOCKED.
CANDIDATE_CASE_SCHEMA_VERSION = 2
PUBLIC_CASE_SCHEMA_VERSIONS = frozenset({PUBLIC_SCHEMA_VERSION, CANDIDATE_CASE_SCHEMA_VERSION})
PUBLIC_PROOF_KIND = "public-native-acceptance-proof"
PUBLIC_MANIFEST_KIND = "public-native-acceptance-manifest"
CANDIDATE_PROOF_KIND = "candidate-native-acceptance-proof"
CANDIDATE_MANIFEST_KIND = "candidate-native-acceptance-manifest"
MAX_PUBLIC_ZIP_BYTES = 1 << 20
MAX_PUBLIC_MEMBER_BYTES = 64 << 10
MAX_PUBLIC_TOTAL_BYTES = 1 << 20
MAX_STAGE_FILE_BYTES = 1 << 20
MAX_STAGE_TOTAL_BYTES = 4 << 20
MAX_STAGE_FILES = 128
MAX_SOURCE_FINGERPRINT_FILES = MAX_TREE_ENTRIES

HEX_DIGEST = re.compile(r"^[0-9a-f]{64}$")
REVISION = re.compile(r"^[0-9a-f]{40,64}$")
REPOSITORY = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
TAG = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+$")
PUBLIC_TEXT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}$")
OPAQUE_TEXT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
PLATFORM_TEXT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
TASK_ID = re.compile(r"^[0-9a-f]{32}$")

CASE_DEFINITIONS = expected_cases()
CASE_IDS = tuple(case.case_id for case in CASE_DEFINITIONS)
PUBLIC_MEMBER_NAMES = (
    "manifest.json",
    "proof.json",
    *(f"cases/{case_id}.json" for case_id in CASE_IDS),
)


class PublicProofError(ValueError):
    """A public proof is malformed, unbound, unsafe, or outside its bounds."""


# Public members name the bundled binaries as the release receipts do; the
# canonical digest record names delegate-run by its executable name.
PUBLIC_BINARY_NAMES = (
    ("delegate", "delegate"), ("runner", "delegate-run"), ("pueue", "pueue"), ("pueued", "pueued"),
)


def public_binaries(digests: BinaryDigests) -> dict[str, str]:
    """Return the public binary record of canonical binary digests."""
    mapping = digests.as_mapping()
    return {public: mapping[name] for public, name in PUBLIC_BINARY_NAMES}


def parse_public_binaries(value: object, code: str) -> BinaryDigests:
    """Read an exact public binary record into canonical binary digests."""
    record = require_object(value, {public for public, _ in PUBLIC_BINARY_NAMES}, code)
    try:
        return parse_binary_digests({name: record[public] for public, name in PUBLIC_BINARY_NAMES}, code)
    except VerificationError as error:
        raise PublicProofError(code) from error


@dataclass(frozen=True)
class ProofExpectation:
    """Trusted release facts supplied by the caller, never read from the ZIP."""

    repo: str
    tag: str
    release_revision: str
    harness_revision: str
    platform_os: str
    platform_arch: str
    opencode_model: str
    ccr_model: str | None
    source_harness_sha256: str
    archive_sha256: str
    delegate_sha256: str
    runner_sha256: str
    pueue_sha256: str
    pueued_sha256: str


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical_json(value: object) -> bytes:
    try:
        return (json.dumps(value, sort_keys=True, separators=(",", ":"),
                           allow_nan=False) + "\n").encode("utf-8")
    except (TypeError, ValueError, UnicodeError) as error:
        raise PublicProofError("public_json_invalid") from error


DENIED_KEYS = frozenset({
    "argv", "env", "environment", "stdout", "stderr", "output", "provider_output",
    "failure", "failure_detail", "diagnostic_text", "receipt_path", "archive_path",
    "checksums_path", "journal_path", "executable", "provider_runtime_identities",
    "creation_identity", "command", "commands",
})
_SECRET_VALUE = re.compile(
    r"(?i)(?:gh[pousr]_|github_pat_|sk_(?:live|test)_|sk-|xox[baprs]-|"
    r"AKIA[0-9A-Z]{16}|bearer\s|-----BEGIN [A-Z ]*PRIVATE KEY-----)"
)


def privacy_walk(value: object, depth: int = 0) -> None:
    """Refuse private keys, host paths, control characters and secret-shaped values."""
    if depth > 24:
        raise PublicProofError("public_json_depth_invalid")
    if isinstance(value, dict):
        for key, item in value.items():
            if not isinstance(key, str) or key.lower() in DENIED_KEYS:
                raise PublicProofError("public_privacy_field_denied")
            privacy_walk(item, depth + 1)
    elif isinstance(value, list):
        if len(value) > 1024:
            raise PublicProofError("public_json_list_too_large")
        for item in value:
            privacy_walk(item, depth + 1)
    elif isinstance(value, str):
        if (len(value) > 4096 or any(ord(char) < 32 for char in value)
                or value.startswith("/") or re.match(r"^[A-Za-z]:[\\/]", value)
                or value.startswith("\\\\") or _SECRET_VALUE.search(value) is not None):
            raise PublicProofError("public_privacy_value_denied")


def canonical_public_zip(files: dict[str, bytes]) -> bytes:
    """Encode the sole accepted public ZIP representation."""
    if set(files) != set(PUBLIC_MEMBER_NAMES):
        raise PublicProofError("public_zip_members_invalid")
    stream = BytesIO()
    total = 0
    with zipfile.ZipFile(stream, "w", compression=zipfile.ZIP_STORED) as archive:
        archive.comment = b""
        for name in PUBLIC_MEMBER_NAMES:
            data = files[name]
            if not isinstance(data, bytes) or len(data) > MAX_PUBLIC_MEMBER_BYTES:
                raise PublicProofError("public_zip_member_invalid")
            total += len(data)
            if total > MAX_PUBLIC_TOTAL_BYTES:
                raise PublicProofError("public_zip_unpacked_size_invalid")
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_STORED
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            archive.writestr(info, data)
    encoded = stream.getvalue()
    if len(encoded) > MAX_PUBLIC_ZIP_BYTES:
        raise PublicProofError("public_zip_size_invalid")
    return encoded


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise PublicProofError("public_json_duplicate_key")
        result[key] = value
    return result


def _reject_constant(_value: str) -> object:
    raise PublicProofError("public_json_nonfinite")


def parse_json(data: bytes, code: str = "public_json_invalid") -> object:
    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=_unique_object,
                          parse_constant=_reject_constant)
    except (UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise PublicProofError(code) from error
    except PublicProofError as error:
        if error.args == ("public_json_duplicate_key",):
            raise PublicProofError("public_json_duplicate_key") from error
        raise PublicProofError(code) from error


def parse_canonical(data: bytes, code: str) -> object:
    """Parse one member that must already be in its canonical JSON encoding."""
    value = parse_json(data, code)
    if canonical_json(value) != data:
        raise PublicProofError(code)
    return value


def read_public_zip(data: bytes) -> dict[str, bytes]:
    """Return the members of the sole accepted public ZIP representation."""
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


def require_projected_members(rows: object, entries: dict[str, bytes]) -> None:
    """Every member after the manifest is listed in order with its exact digest."""
    if not isinstance(rows, list) or len(rows) != len(PUBLIC_MEMBER_NAMES) - 1:
        raise PublicProofError("public_projected_members_invalid")
    for index, name in enumerate(PUBLIC_MEMBER_NAMES[1:]):
        row = require_object(rows[index], {"path", "sha256"}, "public_projected_member_invalid")
        if row.get("path") != name:
            raise PublicProofError("public_projected_members_invalid")
        expected_hash = require_sha256(row.get("sha256"))
        if hashlib.sha256(entries[name]).hexdigest() != expected_hash:
            raise PublicProofError("public_projected_digest_mismatch")


def require_sha256(value: object, code: str = "public_digest_invalid") -> str:
    if not isinstance(value, str) or HEX_DIGEST.fullmatch(value) is None:
        raise PublicProofError(code)
    return value


def validate_expectation(value: ProofExpectation) -> None:
    if not isinstance(value, ProofExpectation):
        raise PublicProofError("public_expectation_invalid")
    if REPOSITORY.fullmatch(value.repo) is None or TAG.fullmatch(value.tag) is None:
        raise PublicProofError("public_expectation_release_invalid")
    if (REVISION.fullmatch(value.release_revision) is None
            or REVISION.fullmatch(value.harness_revision) is None):
        raise PublicProofError("public_expectation_revision_invalid")
    if (PLATFORM_TEXT.fullmatch(value.platform_os) is None
            or PLATFORM_TEXT.fullmatch(value.platform_arch) is None):
        raise PublicProofError("public_expectation_platform_invalid")
    if (PUBLIC_TEXT.fullmatch(value.opencode_model) is None
            or value.ccr_model is not None
            and PUBLIC_TEXT.fullmatch(value.ccr_model) is None):
        raise PublicProofError("public_expectation_model_invalid")
    for digest in (value.source_harness_sha256, value.archive_sha256,
                   value.delegate_sha256, value.runner_sha256,
                   value.pueue_sha256, value.pueued_sha256):
        require_sha256(digest, "public_expectation_digest_invalid")


def require_object(value: object, keys: set[str], code: str) -> dict[str, object]:
    if not isinstance(value, dict) or set(value) != keys:
        raise PublicProofError(code)
    if not all(isinstance(key, str) for key in value):
        raise PublicProofError(code)
    return value


def require_public_text(value: object, *, opaque: bool = False,
                        code: str = "public_text_invalid") -> str:
    pattern = OPAQUE_TEXT if opaque else PUBLIC_TEXT
    if (not isinstance(value, str) or pattern.fullmatch(value) is None
            or value.startswith("/") or "\\" in value):
        raise PublicProofError(code)
    return value


def safe_repo_path(value: object) -> str:
    if (not isinstance(value, str) or not value or len(value) > 256
            or "\\" in value or value.startswith("/") or "\x00" in value):
        raise PublicProofError("public_source_path_invalid")
    parts = value.split("/")
    if any(part in {"", ".", ".."} for part in parts):
        raise PublicProofError("public_source_path_invalid")
    return value


def _matches(pattern: re.Pattern[str], value: object) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _optional_model(value: object) -> bool:
    return value is None or _matches(PUBLIC_TEXT, value)


def _positive(value: object) -> bool:
    return type(value) is int and value >= 1


@dataclass(frozen=True)
class NativeCompletion:
    """Sanitized facts of one native child's natural completion.

    Only the existing child-observation owner produces this, after it has read
    the started, invocation and completed records; the digests identify those
    private records without exposing their process identities or paths.
    """

    invocation_id: str
    natural_wait: bool
    exit_code: int
    started_sha256: str
    invocation_sha256: str
    completed_sha256: str

    def __post_init__(self) -> None:
        if (not _matches(OPAQUE_TEXT, self.invocation_id) or self.natural_wait is not True
                or type(self.exit_code) is not int or not 0 <= self.exit_code <= 255):
            raise PublicProofError("native_completion_invalid")
        for digest in (self.started_sha256, self.invocation_sha256, self.completed_sha256):
            require_sha256(digest, "native_completion_invalid")


@dataclass(frozen=True)
class CandidateProofExpectation:
    """Trusted candidate facts derived from the manifest, locator and claim, never the ZIP.

    ``harness_*`` is the independently verified native harness that ran the
    cases; it is distinct from the candidate's build harness.
    """

    repository: str
    candidate_id: str
    run_id: int
    run_attempt: int
    artifact_id: int
    artifact_archive_sha256: str
    source_commit: str
    source_tree: str
    platform_os: str
    platform_arch: str
    archive_name: str
    archive_sha256: str
    claim_sha256: str
    invocation_id: str
    harness_revision: str
    harness_sha256: str
    binaries: BinaryDigests
    opencode_model: str
    ccr_model: str | None
    agy_model: str | None

    def __post_init__(self) -> None:
        if (not _matches(REPOSITORY, self.repository)
                or not all(_positive(value) for value in (self.run_id, self.run_attempt, self.artifact_id))
                or not all(_matches(REVISION, value)
                           for value in (self.source_commit, self.source_tree, self.harness_revision))
                or not _matches(PLATFORM_TEXT, self.platform_os) or not _matches(PLATFORM_TEXT, self.platform_arch)
                or not _matches(PUBLIC_TEXT, self.archive_name) or "/" in self.archive_name
                or not _matches(OPAQUE_TEXT, self.invocation_id)
                or not isinstance(self.binaries, BinaryDigests) or not _matches(PUBLIC_TEXT, self.opencode_model)
                or not _optional_model(self.ccr_model) or not _optional_model(self.agy_model)):
            raise PublicProofError("candidate_expectation_invalid")
        for digest in (self.candidate_id, self.artifact_archive_sha256, self.archive_sha256,
                       self.claim_sha256, self.harness_sha256):
            require_sha256(digest, "candidate_expectation_invalid")


CASE_STATUSES = frozenset({"PASS", "BLOCKED", "FAIL"})


def require_case_schema_version(value: object) -> int:
    """Return a known public case format; any other value is refused."""
    if type(value) is not int or value not in PUBLIC_CASE_SCHEMA_VERSIONS:
        raise PublicProofError("public_case_format_invalid")
    return value


@dataclass(frozen=True)
class CaseOutcome:
    """The validated verdict of one canonical case and its typed block reason."""

    case_id: str
    status: str
    block_reason: BlockReason | None

    def __post_init__(self) -> None:
        if (not isinstance(self.case_id, str) or self.case_id not in CASE_IDS
                or not isinstance(self.status, str) or self.status not in CASE_STATUSES):
            raise PublicProofError("candidate_proof_result_invalid")
        try:
            encode_block_reason(self.status, self.block_reason)
        except EvidenceError:
            raise PublicProofError("candidate_proof_result_invalid") from None


@dataclass(frozen=True)
class ValidatedCandidateProof:
    """Digest-bound facts of one candidate proof ZIP that passed every check.

    ``cases`` holds every canonical case in order. ``unresolved_case_ids``
    are the cases whose evidence or owned lifetime closure stayed unproven;
    nothing marks a case closed by default.
    """

    public_zip_sha256: str
    aggregate_sha256: str
    candidate_id: str
    claim_sha256: str
    invocation_id: str
    platform_os: str
    platform_arch: str
    archive_sha256: str
    binaries: BinaryDigests
    completion: NativeCompletion
    status: str
    case_results_verdict: str
    cases: tuple[CaseOutcome, ...]
    unresolved_case_ids: tuple[str, ...]

    def __post_init__(self) -> None:
        for digest in (self.public_zip_sha256, self.aggregate_sha256, self.candidate_id,
                       self.claim_sha256, self.archive_sha256):
            require_sha256(digest, "candidate_proof_result_invalid")
        if (not isinstance(self.completion, NativeCompletion)
                or self.completion.invocation_id != self.invocation_id
                or not isinstance(self.binaries, BinaryDigests)
                or self.status not in CASE_STATUSES or self.case_results_verdict not in CASE_STATUSES
                or not isinstance(self.cases, tuple)
                or not all(isinstance(case, CaseOutcome) for case in self.cases)
                or tuple(case.case_id for case in self.cases) != CASE_IDS
                or not isinstance(self.unresolved_case_ids, tuple)
                or list(self.unresolved_case_ids) != sorted(set(self.unresolved_case_ids))
                or not set(self.unresolved_case_ids) <= set(CASE_IDS)):
            raise PublicProofError("candidate_proof_result_invalid")
