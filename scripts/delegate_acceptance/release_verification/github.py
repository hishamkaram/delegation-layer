"""Bounded GitHub transport; callers own policy and ambiguous mutation recovery."""

from __future__ import annotations

import errno
import hashlib
import json
import os
import re
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure, OutputSink,
)
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from .contract import MAX_OUTPUT, REVISION_RE, VerificationError
from .filesystem import create_new_file, write_all, write_new_file


API_VERSION = "2026-03-10"
RELEASE_PAGE_SIZE = 30
MAX_RELEASE_PAGES = 100
MAX_RELEASE_PAGE_BYTES = 1 << 20
ASSET_PAGE_SIZE = 100
MAX_ASSET_PAGES = 100
ASSETS_STAGE = "github-release-assets"
# Encoded request bodies live in memory. The cap is generous for a base64 Git blob
# (about 4/3 of its payload plus the JSON envelope) of the largest accepted proof.
MAX_REQUEST_BODY = 4 << 20
REQUEST_TIMEOUT = 120
DOWNLOAD_TIMEOUT = 600
_MAX_BODY_DEPTH = 32
# Neither segment may be a dot path that would leave the repository's endpoint.
_REPO_RE = re.compile(r"^(?!\.{1,2}/)[A-Za-z0-9_.-]{1,100}/(?!\.{1,2}$)[A-Za-z0-9_.-]{1,100}$")
# Local-file header, or the end-of-central-directory record of an empty archive.
_ZIP_SIGNATURES = (b"PK\x03\x04", b"PK\x05\x06")


@dataclass(frozen=True)
class ArtifactDownload:
    """A complete download: ``size`` bytes at ``path`` hash to ``sha256``."""

    path: Path
    size: int
    sha256: str


class _ArchiveWriter:
    """Own one new file and its running digest; the first refusal is kept in ``failure``.

    The bounded command owner delivers chunks here and never retains them.
    """

    def __init__(self, descriptor: int):
        self.descriptor = descriptor
        self.digest = hashlib.sha256()
        self.size = 0
        self.failure: str | None = None
        self._prefix = b""

    def __call__(self, chunk: bytes) -> None:
        if len(self._prefix) < 4:
            self._prefix += chunk[:4 - len(self._prefix)]
            if len(self._prefix) == 4 and self._prefix not in _ZIP_SIGNATURES:
                self.failure = "github_artifact_header_invalid"
                raise OSError("artifact header invalid")
        try:
            write_all(self.descriptor, chunk)
        except OSError:
            self.failure = "github_download_write_failed"
            raise
        self.digest.update(chunk)
        self.size += len(chunk)

    def finish(self) -> None:
        """Require a recognized header and a durable file of exactly the streamed size."""
        if len(self._prefix) < 4:
            raise VerificationError("github_artifact_header_invalid", "github-api")
        try:
            os.fsync(self.descriptor)
            stored = os.fstat(self.descriptor).st_size
        except OSError:
            raise VerificationError("github_download_write_failed", "github-api") from None
        if stored != self.size:
            raise VerificationError("github_download_write_failed", "github-api")


class GitHubClient:
    def __init__(self, receipt: dict, environment: dict[str, str]):
        self.receipt = receipt
        self.environment = environment

    def _begin(self, endpoint: str, method: str) -> dict[str, object]:
        """Validate the endpoint and append this request's single receipt record."""
        _require_endpoint(endpoint)
        commands = self.receipt.get("commands")
        if not isinstance(commands, list):
            raise VerificationError("receipt_commands_invalid", "github-api")
        record: dict[str, object] = {"name": "github-api", "endpoint": endpoint,
                                     "method": method, "exit_code": None}
        commands.append(record)
        return record

    def _execute(self, argv: list[str], record: dict[str, object], *, limit: int,
                 timeout: float, sink: OutputSink | None = None) -> BoundedCommandResult:
        """Run through the canonical owner; its typed failure and exit-code fact pass through."""
        try:
            result = run_bounded_command(
                argv, self.environment, max_output=limit, capture=True, timeout=timeout,
                sink=sink,
            )
        except BoundedCommandError as error:
            record["exit_code"] = error.exit_code
            raise
        record["exit_code"] = result.exit_code
        return result

    def _run(self, argv: list[str], record: dict[str, object], *, limit: int,
             timeout: float) -> bytes:
        try:
            result = self._execute(argv, record, limit=limit, timeout=timeout)
        except BoundedCommandError as error:
            raise VerificationError(error.reason.value, "github-api") from error
        if result.exit_code != 0:
            raise VerificationError("github_request_failed", "github-api")
        return result.output

    def _request(self, endpoint: str, *, method: str, accept: str,
                 fields: dict[str, str] | None, limit: int) -> bytes:
        record = self._begin(endpoint, method)
        argv = _api_argv(endpoint, method, accept)
        for name, value in (fields or {}).items():
            argv.extend(["-f", f"{name}={value}"])
        return self._run(argv, record, limit=limit, timeout=REQUEST_TIMEOUT)

    def json(self, endpoint: str, *, method: str = "GET",
             fields: dict[str, str] | None = None,
             limit: int = MAX_OUTPUT) -> dict[str, object] | list[object]:
        raw = self._request(endpoint, method=method, accept="application/vnd.github+json",
                            fields=fields, limit=limit)
        return _json_response(raw)

    def json_body(self, endpoint: str, body: dict[str, object], *, method: str,
                  body_path: Path, limit: int = MAX_OUTPUT) -> dict[str, object] | list[object]:
        """Send a nested JSON object as the request body and return the JSON reply.

        ``body_path`` must name a file that does not exist yet in a caller-owned
        directory; it is created owner-only, holds the exact encoded body, and is
        left for that owner to retain or remove. Fields are deliberately absent:
        with an input file ``gh`` would turn them into query parameters.
        """
        if method not in {"POST", "PUT", "PATCH"}:
            raise VerificationError("github_request_method_invalid", "github-api")
        data = _encode_body(body)
        record = self._begin(endpoint, method)
        record.update({"body_bytes": len(data), "body_sha256": hashlib.sha256(data).hexdigest()})
        write_new_file(body_path, data, path_code="github_request_body_path_invalid",
                       write_code="github_request_body_write_failed", stage="github-api")
        argv = _api_argv(endpoint, method, "application/vnd.github+json")
        argv.extend(["-H", "Content-Type: application/json", "--input", str(body_path)])
        return _json_response(self._run(argv, record, limit=limit, timeout=REQUEST_TIMEOUT))

    def bytes(self, endpoint: str, *, limit: int) -> bytes:
        return self._request(endpoint, method="GET", accept="application/octet-stream",
                             fields=None, limit=limit)

    def artifact_archive(self, repo: str, artifact_id: int, *, limit: int) -> bytes:
        # Actions archive redirects require the GitHub API media type, unlike release assets.
        return self._request(f"repos/{repo}/actions/artifacts/{artifact_id}/zip",
                             method="GET", accept="application/vnd.github+json",
                             fields=None, limit=limit)

    def raw_blob(self, repo: str, blob_sha: str, *, limit: int) -> bytes:
        """Return the exact bytes of one Git blob; the caller decides whether they hash to ``blob_sha``.

        The blob endpoint answers raw content only to its raw media type and refuses
        the generic octet-stream one, so the media type is fixed here.
        """
        if (_REPO_RE.fullmatch(repo) is None or type(blob_sha) is not str or len(blob_sha) != 40
                or REVISION_RE.fullmatch(blob_sha) is None or type(limit) is not int or limit <= 0):
            raise VerificationError("github_raw_blob_invalid", "github-api")
        return self._request(f"repos/{repo}/git/blobs/{blob_sha}", method="GET",
                             accept="application/vnd.github.raw+json", fields=None, limit=limit)

    def download_artifact(self, repo: str, artifact_id: int, destination: Path, *,
                          limit: int, timeout: float = DOWNLOAD_TIMEOUT) -> ArtifactDownload:
        """Stream one Actions artifact archive into a new file and hash it as it arrives.

        ``destination`` must not exist; it is created owner-only in a caller-owned
        directory. Any failure raises and leaves whatever arrived at ``destination``
        as partial evidence, never as a result: the caller owns its retention.
        The receipt record keeps ``bytes`` (the stored file size, read from the file),
        ``writer_failure`` (the writer's own refusal, if any), ``close_failed``, and
        ``sha256`` only after the file was fully written and closed.
        A refusal by the writer is reported under its own code only when the child's
        Wait is proven; otherwise the canonical closure failure code is kept and the
        writer's refusal stays in the receipt and the exception chain.
        """
        if (_REPO_RE.fullmatch(repo) is None or type(limit) is not int or limit <= 0):
            raise VerificationError("github_download_invalid", "github-api")
        positive_id(artifact_id, "github-api")
        endpoint = f"repos/{repo}/actions/artifacts/{artifact_id}/zip"
        record = self._begin(endpoint, "GET")
        record.update({"limit": limit, "bytes": 0, "sha256": None,
                       "writer_failure": None, "close_failed": False})
        descriptor = create_new_file(destination, "github_download_destination_invalid", stage="github-api")
        writer = _ArchiveWriter(descriptor)
        failure: BaseException | None = None
        try:
            self._stream_archive(endpoint, record, writer, limit=limit, timeout=timeout)
        except BaseException as error:
            failure = error
            raise
        finally:
            _close_download(record, writer, failure)
        digest = writer.digest.hexdigest()
        record["sha256"] = digest
        return ArtifactDownload(destination, writer.size, digest)

    def _stream_archive(self, endpoint: str, record: dict[str, object], writer: _ArchiveWriter,
                        *, limit: int, timeout: float) -> None:
        argv = _api_argv(endpoint, "GET", "application/vnd.github+json")
        try:
            result = self._execute(argv, record, limit=limit, timeout=timeout, sink=writer)
        except BoundedCommandError as error:
            raise _download_failure(error, writer) from error
        if result.exit_code != 0:
            raise VerificationError("github_request_failed", "github-api")
        writer.finish()

    def release(self, repo: str, tag: str) -> dict[str, object]:
        """Find one exact release, including drafts visible to this caller.

        GitHub's release-by-tag endpoint returns published releases only.
        Complete the bounded listing before accepting an unambiguous match.
        """
        found: dict[str, object] | None = None
        for page in range(1, MAX_RELEASE_PAGES + 1):
            rows = self.json(f"repos/{repo}/releases?per_page={RELEASE_PAGE_SIZE}&page={page}",
                             limit=MAX_RELEASE_PAGE_BYTES)
            if not isinstance(rows, list) or len(rows) > RELEASE_PAGE_SIZE:
                raise VerificationError("github_releases_invalid", "github-release")
            for value in rows:
                row = object_value(value, "github-release")
                if row.get("tag_name") == tag:
                    positive_id(row.get("id"), "github-release")
                    if found is not None:
                        raise VerificationError("github_release_ambiguous", "github-release")
                    found = row
            if len(rows) < RELEASE_PAGE_SIZE:
                if found is None:
                    raise VerificationError("github_release_not_found", "github-release")
                return found
        raise VerificationError("github_releases_pagination_incomplete", "github-release")


def iter_release_assets(client: GitHubClient, repo: str, release_id: int) -> Iterator[object]:
    """Yield every listed asset row of one release, one validated page at a time.

    This owns only page shape and exhaustion: a short page ends the listing, and a
    full final page leaves it incomplete. Callers own row policy and must consume
    the whole iterator before acting on what it lists.
    """
    for page in range(1, MAX_ASSET_PAGES + 1):
        rows = client.json(f"repos/{repo}/releases/{release_id}/assets"
                           f"?per_page={ASSET_PAGE_SIZE}&page={page}", limit=MAX_RELEASE_PAGE_BYTES)
        if not isinstance(rows, list) or len(rows) > ASSET_PAGE_SIZE:
            raise VerificationError("github_release_assets_invalid", ASSETS_STAGE)
        yield from rows
        if len(rows) < ASSET_PAGE_SIZE:
            return
    raise VerificationError("github_release_assets_pagination_incomplete", ASSETS_STAGE)


def object_value(value: object, stage: str) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise VerificationError("github_object_invalid", stage)
    return value


def positive_id(value: object, stage: str) -> int:
    if type(value) is not int or value <= 0:
        raise VerificationError("github_id_invalid", stage)
    return value


def _require_endpoint(endpoint: str) -> None:
    if (endpoint != "user" and not endpoint.startswith("repos/")
            and not endpoint.startswith("users/")):
        raise VerificationError("github_endpoint_invalid", "github-api")


def _api_argv(endpoint: str, method: str, accept: str) -> list[str]:
    """The fixed ``gh api`` command; callers append their own options after the endpoint."""
    return ["gh", "api", "--method", method, "-H", f"Accept: {accept}",
            "-H", f"X-GitHub-Api-Version: {API_VERSION}", endpoint]


def _json_response(raw: bytes) -> dict[str, object] | list[object]:
    try:
        value = json.loads(raw)
    except (UnicodeError, ValueError, RecursionError) as error:
        raise VerificationError("github_response_invalid", "github-api") from error
    if not isinstance(value, (dict, list)):
        raise VerificationError("github_response_invalid", "github-api")
    return value


def _require_json_value(value: object, depth: int = 0) -> None:
    """Accept only exact JSON types with string object keys within a finite depth."""
    if depth > _MAX_BODY_DEPTH:
        raise ValueError("body nesting exceeds the bound")
    if type(value) in (type(None), bool, int, float, str):
        return
    if type(value) is list:
        for item in value:
            _require_json_value(item, depth + 1)
    elif type(value) is dict:
        for key, item in value.items():
            if type(key) is not str:
                raise ValueError("body object key is not a string")
            _require_json_value(item, depth + 1)
    else:
        raise TypeError("body value is not JSON")


def _encode_body(body: dict[str, object]) -> bytes:
    try:
        if type(body) is not dict:
            raise TypeError("body is not a JSON object")
        _require_json_value(body)
        data = json.dumps(body, allow_nan=False, sort_keys=True,
                          separators=(",", ":")).encode("ascii")
    except (TypeError, ValueError, RecursionError):
        raise VerificationError("github_request_body_invalid", "github-api") from None
    if len(data) > MAX_REQUEST_BODY:
        raise VerificationError("github_request_body_invalid", "github-api")
    return data


def _download_failure(error: BoundedCommandError, writer: _ArchiveWriter) -> VerificationError:
    """Name the writer's refusal only when the owner proved the child's Wait.

    ``exit_code`` is the owner's proof of closure. Without it (unknown Wait) the
    owner's own failure reason is the authoritative fact and is never rewritten.
    """
    if (error.reason is CommandFailure.CAPTURE_UNAVAILABLE and error.exit_code is not None
            and writer.failure is not None):
        return VerificationError(writer.failure, "github-api")
    return VerificationError(error.reason.value, "github-api")


def _close_download(record: dict[str, object], writer: _ArchiveWriter,
                    failure: BaseException | None) -> None:
    """Record the stored size, then close; a close fault never replaces a primary failure."""
    record["writer_failure"] = writer.failure
    try:
        record["bytes"] = os.fstat(writer.descriptor).st_size
    except OSError:
        record["bytes"] = None
    try:
        os.close(writer.descriptor)
    except OSError as close_error:
        record["close_failed"] = True
        if failure is None:
            raise VerificationError("github_download_close_failed", "github-api") from close_error
        failure.add_note("download file close failed: " + errno.errorcode.get(
            close_error.errno or 0, "unknown"))
