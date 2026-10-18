"""Bounded GitHub transport; callers own policy and ambiguous mutation recovery."""

from __future__ import annotations

import json

from scripts.delegate_acceptance.processes.contracts import BoundedCommandError
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from .contract import MAX_OUTPUT, VerificationError


API_VERSION = "2026-03-10"
RELEASE_PAGE_SIZE = 30
MAX_RELEASE_PAGES = 100
MAX_RELEASE_PAGE_BYTES = 1 << 20


class GitHubClient:
    def __init__(self, receipt: dict, environment: dict[str, str]):
        self.receipt = receipt
        self.environment = environment

    def _request(self, endpoint: str, *, method: str, accept: str,
                 fields: dict[str, str] | None, limit: int) -> bytes:
        if (endpoint != "user" and not endpoint.startswith("repos/")
                and not endpoint.startswith("users/")):
            raise VerificationError("github_endpoint_invalid", "github-api")
        argv = ["gh", "api", "--method", method, "-H", f"Accept: {accept}",
                "-H", f"X-GitHub-Api-Version: {API_VERSION}", endpoint]
        for name, value in (fields or {}).items():
            argv.extend(["-f", f"{name}={value}"])
        record: dict[str, object] = {"name": "github-api", "endpoint": endpoint,
                                     "method": method, "exit_code": None}
        commands = self.receipt.get("commands")
        if not isinstance(commands, list):
            raise VerificationError("receipt_commands_invalid", "github-api")
        commands.append(record)
        try:
            result = run_bounded_command(
                argv, self.environment, max_output=limit, capture=True, timeout=120,
            )
        except BoundedCommandError as error:
            record["exit_code"] = error.exit_code
            raise VerificationError(error.reason.value, "github-api") from error
        record["exit_code"] = result.exit_code
        if result.exit_code != 0:
            raise VerificationError("github_request_failed", "github-api")
        return result.output

    def json(self, endpoint: str, *, method: str = "GET",
             fields: dict[str, str] | None = None,
             limit: int = MAX_OUTPUT) -> dict[str, object] | list[object]:
        raw = self._request(endpoint, method=method, accept="application/vnd.github+json",
                            fields=fields, limit=limit)
        try:
            value = json.loads(raw)
        except (UnicodeError, ValueError, RecursionError) as error:
            raise VerificationError("github_response_invalid", "github-api") from error
        if not isinstance(value, (dict, list)):
            raise VerificationError("github_response_invalid", "github-api")
        return value

    def bytes(self, endpoint: str, *, limit: int) -> bytes:
        return self._request(endpoint, method="GET", accept="application/octet-stream",
                             fields=None, limit=limit)

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


def object_value(value: object, stage: str) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise VerificationError("github_object_invalid", stage)
    return value


def positive_id(value: object, stage: str) -> int:
    if type(value) is not int or value <= 0:
        raise VerificationError("github_id_invalid", stage)
    return value
