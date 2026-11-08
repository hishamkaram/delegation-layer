"""Bounded public GitHub asset transport; callers own archive pins and installation."""

from __future__ import annotations

import argparse
from http.client import HTTPException, HTTPMessage
import json
import os
import re
import sys
from typing import IO, Literal
import urllib.error
import urllib.parse
import urllib.request


METADATA_LIMIT = 1024 * 1024
TIMEOUT_SECONDS = 30
CHUNK = 128 * 1024
TOKEN_ENV = "DELEGATE_TOOL_DOWNLOAD_TOKEN"


class DownloadError(Exception):
    """The exact release asset could not be transported safely."""


class HTTPSRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req: urllib.request.Request, fp: IO[bytes], code: int,
                         msg: str, headers: HTTPMessage, newurl: str) -> urllib.request.Request | None:
        if urllib.parse.urlsplit(newurl).scheme != "https":
            raise DownloadError("archive download redirected away from HTTPS")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def transient_error(error: BaseException) -> bool:
    if isinstance(error, urllib.error.HTTPError):
        return 500 <= error.code <= 599
    reason = error.reason if isinstance(error, urllib.error.URLError) else error
    return isinstance(reason, (TimeoutError, ConnectionError))


def validate_token(token: str | None) -> str | None:
    if token is None or token == "":
        return None
    if re.fullmatch(r"[\x21-\x7e]+", token) is None:
        raise DownloadError("invalid explicit tool-download token: require printable ASCII without whitespace")
    return token


def http_failure(error: urllib.error.HTTPError, phase: Literal["metadata", "asset"], mode: str) -> str:
    fields = {}
    for name in ("x-ratelimit-remaining", "x-ratelimit-reset", "retry-after"):
        value = error.headers.get(name) if error.headers is not None else None
        if isinstance(value, str) and re.fullmatch(r"[0-9]{1,20}", value):
            fields[name] = value
    cause = "cause not reported"
    if error.code in {403, 429}:
        if "x-ratelimit-remaining" in fields and int(fields["x-ratelimit-remaining"]) == 0:
            cause = "rate limit reported (remaining=0)"
        elif "retry-after" in fields:
            cause = "possible secondary rate limit"
    details = [f"{phase}, {mode}, HTTP {error.code}", cause]
    details.extend(f"{name}={value}" for name, value in fields.items())
    return "GitHub release-asset API failed (" + "; ".join(details) + ")"


def release_identity(url: str) -> tuple[str, str, str, str]:
    parsed = urllib.parse.urlsplit(url)
    parts = parsed.path.split("/")
    if (parsed.scheme != "https" or parsed.netloc != "github.com" or parsed.query
            or parsed.fragment or len(parts) != 7 or parts[3:5] != ["releases", "download"]):
        raise DownloadError("fallback requires an exact HTTPS GitHub release-asset URL")
    owner, repository, tag, asset = (urllib.parse.unquote(parts[i]) for i in (1, 2, 5, 6))
    for name in (owner, repository):
        if re.fullmatch(r"[A-Za-z0-9_.-]+", name) is None or name in {".", ".."}:
            raise DownloadError("invalid GitHub release repository")
    if (not tag or not asset or tag in {".", ".."} or asset in {".", ".."}
            or any(c in asset for c in "/\\") or any(ord(c) < 32 for c in tag + asset)):
        raise DownloadError("invalid GitHub release tag or asset name")
    return owner, repository, tag, asset


def copy_bounded(source: IO[bytes], target: IO[bytes], limit: int) -> int:
    total = 0
    while chunk := source.read(min(CHUNK, limit - total + 1)):
        total += len(chunk)
        if total > limit:
            raise DownloadError("GitHub asset response exceeded its byte bound")
        if target.write(chunk) != len(chunk):
            raise DownloadError("GitHub asset output write made incomplete progress")
    return total


def asset_identity(metadata: object, tag: str, name: str, limit: int) -> tuple[int, int]:
    if (not isinstance(metadata, dict) or metadata.get("tag_name") != tag
            or not isinstance(metadata.get("assets"), list)):
        raise DownloadError("GitHub release metadata does not match the requested tag and asset list")
    if any(not isinstance(row, dict) for row in metadata["assets"]):
        raise DownloadError("GitHub release metadata contains an invalid asset row")
    matches = [row for row in metadata["assets"] if isinstance(row, dict) and row.get("name") == name]
    if len(matches) != 1:
        raise DownloadError("GitHub release metadata must contain exactly one matching asset")
    row = matches[0]
    identifier, size = row.get("id"), row.get("size")
    if (type(identifier) is not int or identifier <= 0 or type(size) is not int
            or not 0 < size <= limit or row.get("state") != "uploaded"):
        raise DownloadError("GitHub release asset has invalid identity, size or upload state")
    return identifier, size


def download_asset(url: str, target: IO[bytes], limit: int, *, token: str | None = None) -> None:
    """Download one exact asset; an explicit token belongs only to direct API requests."""
    token = validate_token(token)
    mode = "authenticated" if token else "anonymous"
    owner, repository, tag, name = release_identity(url)
    if limit <= 0:
        raise DownloadError("GitHub asset byte bound must be positive")
    base = f"https://api.github.com/repos/{owner}/{repository}/releases"
    headers = {"User-Agent": "delegation-layer-archive-installer", "X-GitHub-Api-Version": "2022-11-28"}
    opener = urllib.request.build_opener(HTTPSRedirects())
    request = urllib.request.Request(f"{base}/tags/{urllib.parse.quote(tag, safe='')}",
                                     headers={**headers, "Accept": "application/vnd.github+json"})
    if token:
        request.add_unredirected_header("Authorization", f"Bearer {token}")
    phase: Literal["metadata", "asset"] = "metadata"
    try:
        with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
            if urllib.parse.urlsplit(response.url).scheme != "https":
                raise DownloadError("GitHub metadata redirected away from HTTPS")
            raw = response.read(METADATA_LIMIT + 1)
        if len(raw) > METADATA_LIMIT:
            raise DownloadError("GitHub release metadata exceeded its byte bound")
        try:
            metadata = json.loads(raw)
        except (UnicodeError, ValueError, RecursionError) as error:
            raise DownloadError("GitHub release metadata is not valid JSON") from error
        identifier, size = asset_identity(metadata, tag, name, limit)
        request = urllib.request.Request(f"{base}/assets/{identifier}",
                                         headers={**headers, "Accept": "application/octet-stream"})
        if token:
            request.add_unredirected_header("Authorization", f"Bearer {token}")
        phase = "asset"
        with opener.open(request, timeout=TIMEOUT_SECONDS) as response:
            if urllib.parse.urlsplit(response.url).scheme != "https":
                raise DownloadError("GitHub asset redirected away from HTTPS")
            if copy_bounded(response, target, limit) != size:
                raise DownloadError("GitHub asset response length differs from release metadata")
    except urllib.error.HTTPError as error:
        raise DownloadError(http_failure(error, phase, mode)) from error
    except (urllib.error.URLError, HTTPException) as error:
        raise DownloadError(f"GitHub release-asset API transport failed ({phase}, {mode})") from error


class Arguments(argparse.Namespace):
    url: str = ""
    max_bytes: int = 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True)
    parser.add_argument("--max-bytes", type=int, required=True)
    args = Arguments()
    parser.parse_args(namespace=args)
    try:
        download_asset(args.url, sys.stdout.buffer, args.max_bytes, token=os.environ.get(TOKEN_ENV))
    except (DownloadError, OSError) as error:
        print(f"GitHub asset API fallback failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
