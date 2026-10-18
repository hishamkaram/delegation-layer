"""Immutable release inputs and checked receipt values."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import re


CANONICAL_REPOSITORY = "hishamkaram/delegation-layer"
WORKFLOW = "release-native-acceptance.yml"
STAGES = ("preflight", "download", "archive", "native", "finalize", "project",
          "upload", "dispatch", "watch", "cleanup")
MAX_RECEIPT = 1 << 20


class ReleaseError(Exception):
    def __init__(self, code: str, *, incomplete: bool = False):
        super().__init__(code)
        self.code = code
        self.incomplete = incomplete


def object_value(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise ReleaseError("release_record_object_invalid")
    return {key: item for key, item in value.items()}


def text_value(value: object, pattern: str | None = None) -> str:
    if (not isinstance(value, str) or not value or "\x00" in value
            or pattern is not None and re.fullmatch(pattern, value) is None):
        raise ReleaseError("release_record_text_invalid")
    return value


def integer(value: object, *, minimum: int = 0) -> int:
    if type(value) is not int or value < minimum:
        raise ReleaseError("release_record_integer_invalid")
    return value


def digest_value(value: object) -> str:
    return text_value(value, r"[0-9a-f]{64}")


def revision_value(value: object) -> str:
    return text_value(value, r"[0-9a-f]{40,64}")


@dataclass(frozen=True)
class ReleaseRequest:
    repo: str
    tag: str
    opencode_model: str
    ccr_model: str | None
    no_submit: bool

    def __post_init__(self) -> None:
        text_value(self.repo, r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")
        text_value(self.tag, r"v[0-9]+\.[0-9]+\.[0-9]+")
        for model in (self.opencode_model, self.ccr_model):
            if model is not None:
                text_value(model, r"[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}")
        if type(self.no_submit) is not bool:
            raise ReleaseError("release_submission_option_invalid")

    def document(self) -> dict[str, object]:
        return {"repo": self.repo, "tag": self.tag, "opencode_model": self.opencode_model,
                "ccr_model": self.ccr_model, "no_submit": self.no_submit}

    @classmethod
    def parse(cls, value: object) -> ReleaseRequest:
        record = object_value(value)
        ccr = record.get("ccr_model")
        no_submit = record.get("no_submit")
        if (set(record) != {"repo", "tag", "opencode_model", "ccr_model", "no_submit"}
                or ccr is not None and not isinstance(ccr, str)
                or type(no_submit) is not bool):
            raise ReleaseError("release_request_invalid")
        return cls(repo=text_value(record["repo"]), tag=text_value(record["tag"]),
                   opencode_model=text_value(record["opencode_model"]),
                   ccr_model=ccr, no_submit=no_submit)


@dataclass(frozen=True)
class Invocation:
    invocation_id: str
    request: ReleaseRequest
    checkout: Path
    root: Path
    platform_os: str
    platform_arch: str
    directory_identity: dict[str, object]

    @property
    def proof_name(self) -> str:
        return f"native-proof-{self.platform_os}-{self.platform_arch}-{self.invocation_id}.zip"

    @property
    def assets(self) -> Path:
        return self.root / "runner" / "delegation-layer-release"

    @property
    def aggregate(self) -> Path:
        return self.root / "native" / "aggregate-receipt.json"

    @property
    def archive_receipt(self) -> Path:
        return self.root / "archive-receipt.json"

    @property
    def native_receipt(self) -> Path:
        return self.root / "native-acceptance-receipt.json"

    @property
    def proof(self) -> Path:
        return self.root / self.proof_name
