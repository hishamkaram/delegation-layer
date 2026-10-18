"""Immutable public proof inputs and bounded serialization primitives."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
from io import BytesIO
import json
import re
import stat
import zipfile

from .contract import expected_cases
from ..release_verification.contract import MAX_TREE_ENTRIES


PUBLIC_SCHEMA_VERSION = 1
PUBLIC_PROOF_KIND = "public-native-acceptance-proof"
PUBLIC_MANIFEST_KIND = "public-native-acceptance-manifest"
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
