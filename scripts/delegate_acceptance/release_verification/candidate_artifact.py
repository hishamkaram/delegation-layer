"""Load one development candidate from its immutable hosted Actions artifact.

The caller supplies the trusted external locator, a container path that does not
exist yet, and an empty payload directory it owns; both stay exclusive to this
load. The loader reads the repository, the canonical CI workflow descriptor, the
exact artifact and run attempt, downloads the container once, and streams each
payload member into a new file. It returns only after every member matches the
manifest the locator binds. Any failure raises and leaves whatever was written
for the caller's retention policy: this module never removes files or
directories, retries, launches a provider, edits a receipt, or decides readiness.

Artifact metadata carries no run attempt. The attempt binding rests on the
trusted locator, the immutable artifact ID and digest, and the producer's
artifact name: a producer convention, not a service attestation.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
import errno
import hashlib
import os
from pathlib import Path
import stat
from typing import BinaryIO
import zipfile
import zlib

from .candidate_codec import positive_int, revision
from .candidate_manifest import (
    CANDIDATE_WORKFLOW_FILE, CANDIDATE_WORKFLOW_PATH, MANIFEST_PATH, MAX_CONTAINER_BYTES,
    MAX_MANIFEST_BYTES, PAYLOAD_MEMBER_COUNT, CandidateLocator, CandidateManifest, bind_manifest,
    candidate_artifact_name, payload_paths,
)
from .contract import MAX_OUTPUT, VerificationError
from .filesystem import create_new_file, write_all
from .github import ArtifactDownload, GitHubClient, object_value, positive_id


STAGE = "load-candidate-artifact"


# A run attempt reply embeds its head commit and both repositories; real replies
# were read within this bound.
_ATTEMPT_RESPONSE_LIMIT = 1 << 20


# The largest single read of the container. It covers the largest spec-valid read
# for this payload (PAYLOAD_MEMBER_COUNT central entries of 46 + 3 * 65,535 bytes),
# the end-of-archive tail, one local header field, and one member chunk. It bounds
# each allocation, not total memory.
_MAX_ZIP_READ_BYTES = 2 * 1024 * 1024


_MEMBER_CHUNK = 1024 * 1024


# General-purpose flag bits 0 (encrypted) and 6 (strong encryption).
_ENCRYPTION_FLAGS = 0x01 | 0x40


_SUPPORTED_COMPRESSION = frozenset({zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED})


# What a malformed container raises from the standard reader; never VerificationError.
# A UTF-8-flagged name that is not UTF-8 raises UnicodeDecodeError; an extract version
# above the reader's, or central flag bit 5 (patched data), raises NotImplementedError.
_ZIP_ERRORS = (zipfile.BadZipFile, zlib.error, EOFError, OSError, UnicodeDecodeError, NotImplementedError)


@dataclass(frozen=True)
class WorkflowObservation:
    """The canonical CI workflow descriptor as observed; its state is recorded, never gated."""

    id: int
    path: str
    state: str


@dataclass(frozen=True)
class AttemptObservation:
    """The exact run attempt; its path, status and conclusion are recorded, never gated."""

    run_id: int
    run_attempt: int
    workflow_id: int
    head_sha: str
    path: str
    status: str | None
    conclusion: str | None


@dataclass(frozen=True)
class ArtifactObservation:
    """The exact hosted artifact; ``sha256`` is its container digest."""

    id: int
    name: str
    size: int
    sha256: str
    run_id: int
    head_sha: str


@dataclass(frozen=True)
class CandidateArtifact:
    """A loaded candidate: observed facts and the verified payload directory."""

    locator: CandidateLocator
    manifest: CandidateManifest
    repository_id: int
    workflow: WorkflowObservation
    attempt: AttemptObservation
    artifact: ArtifactObservation
    container: ArtifactDownload
    payload: Path

    def observation_record(self) -> dict[str, object]:
        """Return a fresh JSON-compatible record of the observed facts."""
        return {
            "candidate_id": self.locator.candidate_id, "repository": self.locator.repository,
            "repository_id": self.repository_id, "version": self.manifest.version,
            "source_commit": self.manifest.source.commit, "workflow": asdict(self.workflow),
            "attempt": asdict(self.attempt), "artifact": asdict(self.artifact),
            "container": {"path": str(self.container.path), "size": self.container.size,
                          "sha256": self.container.sha256},
            "payload": {"path": str(self.payload), "files": list(payload_paths(self.manifest.version))},
        }


def load_candidate_artifact(client: GitHubClient, locator: CandidateLocator, *, container: Path,
                            payload: Path) -> CandidateArtifact:
    """Download and verify the candidate the locator names; raise on any mismatch."""
    _require_empty_payload(payload, container)
    repository_id = _observe_repository(client, locator)
    workflow = _observe_workflow(client, locator.repository)
    artifact = _observe_artifact(client, locator, repository_id)
    attempt = _observe_attempt(client, locator, repository_id, workflow)
    if artifact.head_sha != attempt.head_sha:
        raise VerificationError("candidate_source_mismatch", STAGE)
    download = client.download_artifact(locator.repository, locator.artifact_id, container,
                                        limit=artifact.size)
    if (download.size, download.sha256) != (artifact.size, artifact.sha256):
        raise VerificationError("candidate_container_mismatch", STAGE)
    with _open_container(container) as raw:
        _require_container(raw, download.size)
        manifest = _verify_payload(raw, locator, head_sha=attempt.head_sha, payload=payload)
    return CandidateArtifact(
        locator=locator, manifest=manifest, repository_id=repository_id, workflow=workflow,
        attempt=attempt, artifact=artifact, container=download, payload=payload)


def _require_empty_payload(payload: Path, container: Path) -> None:
    try:
        is_directory = stat.S_ISDIR(payload.lstat().st_mode)
        with os.scandir(payload) as entries:
            empty = next(entries, None) is None
    except OSError:
        raise VerificationError("candidate_payload_directory_invalid", STAGE) from None
    if not is_directory or not empty or container.absolute().parent == payload.absolute():
        raise VerificationError("candidate_payload_directory_invalid", STAGE)


def _get(client: GitHubClient, endpoint: str, *, limit: int = MAX_OUTPUT) -> dict[str, object]:
    return object_value(client.json(endpoint, limit=limit), STAGE)


def _text(record: dict[str, object], key: str) -> str:
    value = record.get(key)
    if type(value) is not str:
        raise VerificationError("candidate_field_invalid", STAGE)
    return value


def _optional_text(record: dict[str, object], key: str) -> str | None:
    return None if record.get(key) is None else _text(record, key)


def _flag(record: dict[str, object], key: str) -> bool:
    value = record.get(key)
    if type(value) is not bool:
        raise VerificationError("candidate_field_invalid", STAGE)
    return value


def _nested_id(record: dict[str, object], key: str) -> int:
    return positive_id(object_value(record.get(key), STAGE).get("id"), STAGE)


def _observe_repository(client: GitHubClient, locator: CandidateLocator) -> int:
    record = _get(client, f"repos/{locator.repository}")
    repository_id = positive_id(record.get("id"), STAGE)
    if _text(record, "full_name") != locator.repository:
        raise VerificationError("candidate_repository_mismatch", STAGE)
    return repository_id


def _observe_workflow(client: GitHubClient, repository: str) -> WorkflowObservation:
    """The current descriptor of the canonical file; a recreated workflow has a new ID."""
    record = _get(client, f"repos/{repository}/actions/workflows/{CANDIDATE_WORKFLOW_FILE}")
    workflow = WorkflowObservation(id=positive_id(record.get("id"), STAGE), path=_text(record, "path"),
                                   state=_text(record, "state"))
    if workflow.path != CANDIDATE_WORKFLOW_PATH:
        raise VerificationError("candidate_workflow_mismatch", STAGE)
    return workflow


def _observe_artifact(client: GitHubClient, locator: CandidateLocator,
                      repository_id: int) -> ArtifactObservation:
    record = _get(client, f"repos/{locator.repository}/actions/artifacts/{locator.artifact_id}")
    run = object_value(record.get("workflow_run"), STAGE)
    artifact = ArtifactObservation(
        id=positive_id(record.get("id"), STAGE), name=_text(record, "name"),
        size=positive_int(record.get("size_in_bytes"), STAGE), sha256=locator.artifact_archive_sha256,
        run_id=positive_id(run.get("id"), STAGE), head_sha=revision(run.get("head_sha"), STAGE))
    repositories = (positive_id(run.get("repository_id"), STAGE),
                    positive_id(run.get("head_repository_id"), STAGE))
    if _text(record, "digest") != "sha256:" + locator.artifact_archive_sha256:
        raise VerificationError("candidate_artifact_digest_mismatch", STAGE)
    if (artifact.id != locator.artifact_id or artifact.run_id != locator.run_id
            or artifact.name != candidate_artifact_name(locator.run_id, locator.run_attempt)
            or repositories != (repository_id, repository_id)):
        raise VerificationError("candidate_artifact_mismatch", STAGE)
    if _flag(record, "expired"):
        raise VerificationError("candidate_artifact_expired", STAGE)
    if artifact.size > MAX_CONTAINER_BYTES:
        raise VerificationError("candidate_artifact_too_large", STAGE)
    return artifact


def _observe_attempt(client: GitHubClient, locator: CandidateLocator, repository_id: int,
                     workflow: WorkflowObservation) -> AttemptObservation:
    endpoint = (f"repos/{locator.repository}/actions/runs/{locator.run_id}"
                f"/attempts/{locator.run_attempt}")
    record = _get(client, endpoint, limit=_ATTEMPT_RESPONSE_LIMIT)
    attempt = AttemptObservation(
        run_id=positive_id(record.get("id"), STAGE),
        run_attempt=positive_id(record.get("run_attempt"), STAGE),
        workflow_id=positive_id(record.get("workflow_id"), STAGE),
        head_sha=revision(record.get("head_sha"), STAGE), path=_text(record, "path"),
        status=_optional_text(record, "status"), conclusion=_optional_text(record, "conclusion"))
    repositories = (_nested_id(record, "repository"), _nested_id(record, "head_repository"))
    if ((attempt.run_id, attempt.run_attempt) != (locator.run_id, locator.run_attempt)
            or repositories != (repository_id, repository_id)):
        raise VerificationError("candidate_attempt_mismatch", STAGE)
    if attempt.workflow_id != workflow.id:
        raise VerificationError("candidate_workflow_mismatch", STAGE)
    return attempt


def _no_follow(path: str, flags: int) -> int:
    return os.open(path, flags | getattr(os, "O_NOFOLLOW", 0))


def _open_container(container: Path) -> BinaryIO:
    try:
        return open(container, "rb", opener=_no_follow)
    except OSError:
        raise VerificationError("candidate_container_unreadable", STAGE) from None


def _require_container(raw: BinaryIO, size: int) -> None:
    """The opened file is still the regular file of exactly the downloaded size."""
    try:
        metadata = os.fstat(raw.fileno())
    except OSError:
        raise VerificationError("candidate_container_unreadable", STAGE) from None
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_size != size:
        raise VerificationError("candidate_container_changed", STAGE)


class _BoundedZipInput:
    """The container as ZipFile reads it, refusing any single read over the budget.

    A request is resolved against the bytes that remain, so an unbounded or
    oversized request that would return the same bytes is forwarded as exactly
    that size, and the refusal happens before anything is read. The caller keeps
    the file immutable and exclusive for the whole load and closes it.
    """

    def __init__(self, raw: BinaryIO):
        self._raw = raw

    def read(self, size: int | None = -1) -> bytes:
        remaining = max(os.fstat(self._raw.fileno()).st_size - self._raw.tell(), 0)
        effective = remaining if size is None or size < 0 else min(size, remaining)
        if effective > _MAX_ZIP_READ_BYTES:
            raise VerificationError("candidate_zip_read_budget_exceeded", STAGE)
        return self._raw.read(effective)

    def seek(self, offset: int, whence: int = os.SEEK_SET) -> int:
        return self._raw.seek(offset, whence)

    def tell(self) -> int:
        return self._raw.tell()

    def seekable(self) -> bool:
        return self._raw.seekable()


def _verify_payload(raw: BinaryIO, locator: CandidateLocator, *, head_sha: str,
                    payload: Path) -> CandidateManifest:
    """Check every member before any write, then stream each one into a new payload file."""
    try:
        bundle = zipfile.ZipFile(_BoundedZipInput(raw))
    except _ZIP_ERRORS as error:
        raise VerificationError("candidate_zip_invalid", STAGE) from error
    with bundle:
        members = _flat_members(bundle.infolist())
        if MANIFEST_PATH not in members:
            raise VerificationError("candidate_zip_inventory_mismatch", STAGE)
        manifest_info = members[MANIFEST_PATH]
        manifest = bind_manifest(locator, _read_manifest(bundle, manifest_info))
        if manifest.source.commit != head_sha:
            raise VerificationError("candidate_source_mismatch", STAGE)
        if tuple(sorted(members)) != payload_paths(manifest.version):
            raise VerificationError("candidate_zip_inventory_mismatch", STAGE)
        _copy_member(bundle, manifest_info, payload / MANIFEST_PATH,
                     size=manifest_info.file_size, sha256=locator.manifest_sha256)
        for entry in manifest.artifacts:
            _copy_member(bundle, members[entry.path], payload / entry.path, size=entry.size,
                         sha256=entry.sha256)
    return manifest


def _flat_members(infos: list[zipfile.ZipInfo]) -> dict[str, zipfile.ZipInfo]:
    if len(infos) != PAYLOAD_MEMBER_COUNT:
        raise VerificationError("candidate_zip_inventory_mismatch", STAGE)
    members: dict[str, zipfile.ZipInfo] = {}
    for info in infos:
        if not _plain_member(info) or info.filename in members:
            raise VerificationError("candidate_zip_member_invalid", STAGE)
        members[info.filename] = info
    return members


def _plain_member(info: zipfile.ZipInfo) -> bool:
    """An unencrypted STORED or DEFLATED regular file with an unchanged flat name."""
    name = info.filename
    file_type = stat.S_IFMT(info.external_attr >> 16)
    return (name == info.orig_filename and name not in {"", ".", ".."} and name.isascii()
            and name.isprintable() and "/" not in name and "\\" not in name and not info.is_dir()
            and file_type in (0, stat.S_IFREG) and not info.flag_bits & _ENCRYPTION_FLAGS
            and info.compress_type in _SUPPORTED_COMPRESSION)


def _open_member(bundle: zipfile.ZipFile, info: zipfile.ZipInfo) -> zipfile.ZipExtFile:
    try:
        return bundle.open(info)
    except _ZIP_ERRORS as error:
        raise VerificationError("candidate_zip_invalid", STAGE) from error


def _read_chunk(member: zipfile.ZipExtFile) -> bytes:
    """Read at most one chunk; the standard reader checks the CRC when the member ends."""
    try:
        return member.read(_MEMBER_CHUNK)
    except _ZIP_ERRORS as error:
        raise VerificationError("candidate_zip_invalid", STAGE) from error


def _read_manifest(bundle: zipfile.ZipFile, info: zipfile.ZipInfo) -> bytes:
    if info.file_size > MAX_MANIFEST_BYTES:
        raise VerificationError("candidate_manifest_too_large", STAGE)
    data = bytearray()
    with _open_member(bundle, info) as member:
        while chunk := _read_chunk(member):
            data += chunk
            if len(data) > MAX_MANIFEST_BYTES:
                raise VerificationError("candidate_manifest_too_large", STAGE)
    return bytes(data)


def _copy_member(bundle: zipfile.ZipFile, info: zipfile.ZipInfo, destination: Path, *, size: int,
                 sha256: str) -> None:
    """Stream one member into a new file, never writing past ``size``; keep it on failure."""
    if info.file_size != size:
        raise VerificationError("candidate_payload_member_mismatch", STAGE)
    descriptor = create_new_file(destination, "candidate_payload_path_invalid", stage=STAGE)
    digest, written, failure = hashlib.sha256(), 0, None
    try:
        with _open_member(bundle, info) as member:
            while chunk := _read_chunk(member):
                written += len(chunk)
                if written > size:
                    raise VerificationError("candidate_payload_member_mismatch", STAGE)
                _write_payload(descriptor, chunk)
                digest.update(chunk)
        _sync(descriptor)
    except BaseException as error:
        failure = error
        raise
    finally:
        _close_payload_file(descriptor, failure)
    if written != size or digest.hexdigest() != sha256:
        raise VerificationError("candidate_payload_member_mismatch", STAGE)


def _write_payload(descriptor: int, data: bytes) -> None:
    try:
        write_all(descriptor, data)
    except OSError as error:
        raise VerificationError("candidate_payload_write_failed", STAGE) from error


def _sync(descriptor: int) -> None:
    try:
        os.fsync(descriptor)
    except OSError as error:
        raise VerificationError("candidate_payload_write_failed", STAGE) from error


def _close_payload_file(descriptor: int, failure: BaseException | None) -> None:
    """Close one payload file; a close fault never replaces a primary failure."""
    try:
        os.close(descriptor)
    except OSError as close_error:
        if failure is None:
            raise VerificationError("candidate_payload_close_failed", STAGE) from close_error
        failure.add_note("payload file close failed: " + errno.errorcode.get(
            close_error.errno or 0, "unknown"))
