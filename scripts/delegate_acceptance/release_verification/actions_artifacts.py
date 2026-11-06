"""Complete Actions run-artifact listings and exact-inventory plain ZIP reads for artifact readers.

This module owns two mechanisms only: that one run's artifact listing was read
completely and consistently, and that an archive holds exactly the caller's
members, each a plain flat unencrypted STORED or DEFLATED file read through
EOF (so its CRC is checked) within the caller's bound. Artifact selection, run
binding, the compressed size and digest, member meaning and every refusal name
stay with each caller. A refusal here is a ``VerificationError`` of ``STAGE``;
a caller translates only those through ``refusal_code`` and lets every other
error, including transport failures, propagate unchanged.
"""

from __future__ import annotations

from collections.abc import Mapping
import io
import stat
import zipfile
import zlib

from .contract import VerificationError
from typing import Protocol


class ArtifactClient(Protocol):
    """The listing consumes bounded JSON; transport ownership stays with its caller."""

    def json(self, endpoint: str, *, limit: int) -> object: ...


STAGE = "actions-artifacts"
PAGE_SIZE = 100
MAX_PAGES = 10
LISTING_INVALID = "artifact_listing_invalid"
LISTING_INCOMPLETE = "artifact_listing_incomplete"
ZIP_INVALID = "artifact_zip_invalid"
ZIP_INVENTORY_MISMATCH = "artifact_zip_inventory_mismatch"
ZIP_MEMBER_INVALID = "artifact_zip_member_invalid"
ZIP_MEMBER_TOO_LARGE = "artifact_zip_member_too_large"
_READ_CHUNK = 64 * 1024
_ENCRYPTION_FLAGS = 0x01 | 0x40
_SUPPORTED_COMPRESSION = frozenset({zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED})
_ZIP_ERRORS = (zipfile.BadZipFile, zlib.error, EOFError, OSError, UnicodeDecodeError, NotImplementedError)


def _refuse(code: str) -> VerificationError:
    return VerificationError(code, STAGE)


def refusal_code(error: VerificationError, codes: Mapping[str, str]) -> str | None:
    """The caller's own code for one of this module's refusals; None means the error must propagate unchanged."""
    return codes.get(error.code) if error.stage == STAGE else None


def _record(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise _refuse(LISTING_INVALID)
    return value


def list_run_artifacts(client: ArtifactClient, repository: str, run_id: int, *,
                       reply_limit: int) -> list[dict[str, object]]:
    """Every artifact row of the run; an inconsistent, duplicated or unfinished listing refuses."""
    rows: list[dict[str, object]] = []
    seen: set[int] = set()
    total: int | None = None
    for page in range(1, MAX_PAGES + 1):
        reply = _record(client.json(f"repos/{repository}/actions/runs/{run_id}/artifacts"
                                    f"?per_page={PAGE_SIZE}&page={page}", limit=reply_limit))
        count, listed = reply.get("total_count"), reply.get("artifacts")
        if (type(count) is not int or not 0 <= count <= PAGE_SIZE * MAX_PAGES
                or not isinstance(listed, list) or len(listed) > PAGE_SIZE
                or total is not None and count != total):
            raise _refuse(LISTING_INVALID)
        total = count
        for value in listed:
            row = _record(value)
            identity = row.get("id")
            if type(identity) is not int or identity <= 0 or identity in seen:
                raise _refuse(LISTING_INVALID)
            seen.add(identity)
            rows.append(row)
        if len(seen) == total:
            return rows
        if len(listed) < PAGE_SIZE or len(seen) > total:
            raise _refuse(LISTING_INCOMPLETE)
    raise _refuse(LISTING_INCOMPLETE)


def read_exact_zip(data: bytes, limits: Mapping[str, int]) -> dict[str, bytes]:
    """Exactly the members ``limits`` names, each read through EOF within its own bound."""
    try:
        bundle = zipfile.ZipFile(io.BytesIO(data))
    except _ZIP_ERRORS as error:
        raise _refuse(ZIP_INVALID) from error
    with bundle:
        infos = bundle.infolist()
        if len(infos) != len(limits) or {info.filename for info in infos} != set(limits):
            raise _refuse(ZIP_INVENTORY_MISMATCH)
        return {info.filename: _read_member(bundle, info, limits[info.filename]) for info in infos}


def _read_member(bundle: zipfile.ZipFile, info: zipfile.ZipInfo, limit: int) -> bytes:
    file_type = stat.S_IFMT(info.external_attr >> 16)
    if (info.filename != info.orig_filename or info.is_dir() or file_type not in (0, stat.S_IFREG)
            or info.flag_bits & _ENCRYPTION_FLAGS or info.compress_type not in _SUPPORTED_COMPRESSION):
        raise _refuse(ZIP_MEMBER_INVALID)
    if info.file_size > limit:
        raise _refuse(ZIP_MEMBER_TOO_LARGE)
    data = bytearray()
    try:
        with bundle.open(info) as member:
            while chunk := member.read(_READ_CHUNK):
                data += chunk
                if len(data) > limit:
                    raise _refuse(ZIP_MEMBER_TOO_LARGE)
    except _ZIP_ERRORS as error:
        raise _refuse(ZIP_INVALID) from error
    if len(data) != info.file_size:
        raise _refuse(ZIP_INVALID)
    return bytes(data)
