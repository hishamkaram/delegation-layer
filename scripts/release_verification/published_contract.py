"""Immutable facts and pure rules for published-consumer verification."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
import base64
import hashlib
import json
import re
from scripts.release_verification.contract import InstalledExecutables
from scripts.acceptance.evidence.source import VerificationError


CHANNELS = ("npm", "homebrew")


NPM_PACKAGE = "delegation-layer"


NPM_REGISTRY = "https://registry.npmjs.org/"


HOMEBREW_FORMULA = "delegation-layer"


# The supported Homebrew prefixes of each host OS; no other OS has one.
_HOMEBREW_PREFIXES = (
    ("Darwin", ("/opt/homebrew", "/usr/local")),
    ("Linux", ("/home/linuxbrew/.linuxbrew",)),
)


SKILL_SOURCE_PREFIX = "skills/agent-integration/"


PACKAGE_SKILL_PREFIX = "package/" + SKILL_SOURCE_PREFIX


MAX_PACKAGE = 16 * 1024 * 1024


MAX_SKILL_FILES = 256


UNRESOLVED_OWNERSHIP_CODES = frozenset({
    "command_timeout", "command_termination_timeout", "command_termination_failed",
    "command_capture_unavailable",
})


INTEGRITY_RE = re.compile(r"^sha512-[A-Za-z0-9+/]{86}==$")


SHASUM_RE = re.compile(r"^[0-9a-f]{40}$")


SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


_TLS_PASSTHROUGH = ("SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS")


_SKILL_MODES = frozenset({"100644", "100755"})


@dataclass(frozen=True)
class PublishedRequest:
    channel: str
    tag: str
    repo: str
    source_revision: str
    archive_receipt: Path
    extract_dir: Path
    receipt: Path
    github_auth: str = "environment"


@dataclass(frozen=True)
class ArchiveFacts:
    """Verified release/archive provenance and the extracted installation selection."""

    tag: str
    package_version: str
    release_commit: str
    archive_sha256: str
    binaries: InstalledExecutables


@dataclass(frozen=True)
class PackageIdentity:
    name: str
    version: str
    integrity: str
    shasum: str
    tarball: str
    git_head: str | None


@dataclass(frozen=True)
class TreeEntry:
    path: str
    blob_sha: str
    size: int


@dataclass(frozen=True)
class SkillFile:
    path: str
    blob_sha: str
    sha256: str


@dataclass(frozen=True)
class SkillManifest:
    """Skill files of the tagged commit; the file set is derived, never counted."""

    commit: str
    files: tuple[SkillFile, ...]

    def digests(self) -> dict[str, str]:
        return {item.path: item.sha256 for item in self.files}


@dataclass(frozen=True)
class InstallFacts:
    label: str
    requested: str
    binaries: InstalledExecutables
    skill_files: int


@dataclass(frozen=True)
class NpmFacts:
    identity: PackageIdentity
    skill_commit: str
    default_pass: str
    installs: tuple[InstallFacts, ...]


@dataclass(frozen=True)
class LatestDecision:
    npm_latest: str
    github_latest: str
    run_default: bool
    reason: str


def homebrew_prefixes(os_name: str) -> tuple[str, ...]:
    """The supported Homebrew prefixes of the host OS ``os_name``; empty when it has none."""
    return dict(_HOMEBREW_PREFIXES).get(os_name, ())


def record_fact(receipt: dict, key: str, value: object) -> None:
    """Publish one verified fact into the receipt's partial-publication ledger."""
    receipt["published"]["verified"][key] = value


def git_blob_sha(data: bytes) -> str:
    return hashlib.sha1(b"blob %d\0" % len(data) + data, usedforsecurity=False).hexdigest()


def npm_integrity(data: bytes) -> str:
    return "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode("ascii")


def npm_shasum(data: bytes) -> str:
    return hashlib.sha1(data, usedforsecurity=False).hexdigest()


def _json_value(raw: bytes, code: str, stage: str) -> object:
    try:
        return json.loads(raw)
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError(code, stage) from None


def _single_object(document: object) -> object:
    """npm renders one exact match as an object or a one-element list, and pack keys it by name."""
    if isinstance(document, list) and len(document) == 1:
        return document[0]
    if isinstance(document, dict) and len(document) == 1:
        (value,) = document.values()
        if isinstance(value, dict):
            return value
    return document


def _field(document: dict, dotted: str) -> object:
    if dotted in document:
        return document[dotted]
    current: object = document
    for part in dotted.split("."):
        if not isinstance(current, dict) or part not in current:
            return None
        current = current[part]
    return current


def registry_tarball_url(version: str) -> str:
    return f"{NPM_REGISTRY}{NPM_PACKAGE}/-/{NPM_PACKAGE}-{version}.tgz"


def parse_npm_view(raw: bytes, package_version: str, release_commit: str) -> PackageIdentity:
    document = _single_object(_json_value(raw, "npm_view_invalid", "npm-view"))
    if not isinstance(document, dict):
        raise VerificationError("npm_view_invalid", "npm-view")
    values = {key: _field(document, key) for key in (
        "name", "version", "dist.integrity", "dist.shasum", "dist.tarball")}
    head = _field(document, "gitHead")
    if (values["name"] != NPM_PACKAGE or values["version"] != package_version
            or not isinstance(values["dist.integrity"], str)
            or not INTEGRITY_RE.fullmatch(values["dist.integrity"])
            or not isinstance(values["dist.shasum"], str)
            or not SHASUM_RE.fullmatch(values["dist.shasum"])
            or values["dist.tarball"] != registry_tarball_url(package_version)):
        raise VerificationError("npm_package_identity_mismatch", "npm-view")
    if head is not None and (not isinstance(head, str) or head != release_commit):
        raise VerificationError("npm_package_revision_mismatch", "npm-view")
    return PackageIdentity(
        name=NPM_PACKAGE, version=package_version, integrity=values["dist.integrity"],
        shasum=values["dist.shasum"], tarball=registry_tarball_url(package_version), git_head=head,
    )


def parse_npm_latest(raw: bytes) -> str:
    value = _json_value(raw, "npm_latest_invalid", "npm-latest")
    if isinstance(value, list) and len(value) == 1:
        value = value[0]
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value):
        raise VerificationError("npm_latest_invalid", "npm-latest")
    return value


def parse_pack_filename(raw: bytes, identity: PackageIdentity) -> str:
    entry = _single_object(_json_value(raw, "npm_pack_invalid", "npm-pack"))
    if (not isinstance(entry, dict) or entry.get("name") != identity.name
            or entry.get("version") != identity.version
            or entry.get("integrity") != identity.integrity
            or entry.get("shasum") != identity.shasum):
        raise VerificationError("npm_pack_identity_mismatch", "npm-pack")
    filename = entry.get("filename")
    if (not isinstance(filename, str) or "/" in filename or "\\" in filename
            or not filename.endswith(".tgz") or filename.startswith(".")):
        raise VerificationError("npm_pack_identity_mismatch", "npm-pack")
    return filename


def check_package_bytes(data: bytes, identity: PackageIdentity) -> None:
    if (npm_integrity(data) != identity.integrity or npm_shasum(data) != identity.shasum):
        raise VerificationError("npm_pack_integrity_mismatch", "npm-pack")


def safe_skill_path(relative: str) -> str:
    parts = relative.split("/")
    if (not relative or relative.startswith("/") or "\\" in relative or "\0" in relative
            or any(part in {"", ".", ".."} for part in parts)):
        raise VerificationError("skill_source_entry_invalid", "skill-manifest")
    return relative


def parse_tree_listing(raw: bytes) -> tuple[TreeEntry, ...]:
    try:
        lines = raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise VerificationError("skill_tree_invalid", "skill-manifest") from None
    if not lines or lines[0] != "truncated\tfalse":
        raise VerificationError("skill_tree_truncated", "skill-manifest")
    entries: list[TreeEntry] = []
    for line in lines[1:]:
        fields = line.split("\t")
        if len(fields) != 5 or not fields[0].startswith(SKILL_SOURCE_PREFIX):
            raise VerificationError("skill_tree_invalid", "skill-manifest")
        path, kind, mode, blob_sha, size = fields
        if kind == "tree":
            continue
        if (kind != "blob" or mode not in _SKILL_MODES
                or not re.fullmatch(r"[0-9a-f]{40}", blob_sha) or not size.isdigit()):
            raise VerificationError("skill_source_entry_invalid", "skill-manifest")
        entries.append(TreeEntry(
            safe_skill_path(path[len(SKILL_SOURCE_PREFIX):]), blob_sha, int(size)))
    names = [entry.path for entry in entries]
    if (not entries or len(entries) > MAX_SKILL_FILES or len(set(names)) != len(names)
            or "SKILL.md" not in names):
        raise VerificationError("skill_tree_invalid", "skill-manifest")
    return tuple(sorted(entries, key=lambda entry: entry.path))


def skill_file(entry: TreeEntry, content: bytes) -> SkillFile:
    if len(content) != entry.size or git_blob_sha(content) != entry.blob_sha:
        raise VerificationError("skill_blob_mismatch", "skill-manifest")
    return SkillFile(entry.path, entry.blob_sha, hashlib.sha256(content).hexdigest())


def parse_single_line(raw: bytes, *, code: str, stage: str) -> str:
    """Return the one non-blank UTF-8 line of a command's output; anything else fails with ``code``.

    Empty or blank output, undecodable bytes and any second line are refused, so a caller never
    takes the first line of a reply it did not fully accept.
    """
    try:
        lines = raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise VerificationError(code, stage) from None
    if len(lines) != 1 or not lines[0].strip():
        raise VerificationError(code, stage)
    return lines[0]


def compare_hashes(expected: dict[str, str], observed: dict[str, str], code: str, stage: str) -> None:
    """Require exactly the same names and digests; extra and missing names both fail."""
    if expected != observed:
        raise VerificationError(code, stage)


def decide_latest(npm_latest: str, github_tag: str, tag: str) -> LatestDecision:
    github_latest = github_tag.removeprefix("v")
    target = tag.removeprefix("v")
    if npm_latest != target:
        return LatestDecision(npm_latest, github_tag, False, "npm_latest_differs_from_target")
    if github_latest != target:
        return LatestDecision(npm_latest, github_tag, False, "github_latest_differs_from_target")
    return LatestDecision(npm_latest, github_tag, True, "both_latest_equal_target")


def _passthrough(inherited: Mapping[str, str]) -> dict[str, str]:
    return {name: inherited[name] for name in _TLS_PASSTHROUGH if inherited.get(name)}


def npm_environment(
        *, path: str, home: str, cache: str, user_npmrc: str, global_npmrc: str,
        inherited: Mapping[str, str]) -> dict[str, str]:
    """Allowlisted npm child environment: private config, explicit registry, no tokens."""
    return {
        "PATH": path, "HOME": home, "TMPDIR": home, "LANG": "C", "LC_ALL": "C",
        "npm_config_cache": cache, "npm_config_userconfig": user_npmrc,
        "npm_config_globalconfig": global_npmrc, "npm_config_registry": NPM_REGISTRY,
        "npm_config_update_notifier": "false", "npm_config_fund": "false",
        "npm_config_audit": "false", "npm_config_loglevel": "error",
        **_passthrough(inherited),
    }


def homebrew_environment(*, path: str, home: str, inherited: Mapping[str, str]) -> dict[str, str]:
    return {
        "PATH": path, "HOME": home, "TMPDIR": home, "LANG": "C", "LC_ALL": "C",
        "HOMEBREW_NO_ANALYTICS": "1", "HOMEBREW_NO_AUTO_UPDATE": "1",
        "HOMEBREW_NO_ENV_HINTS": "1", "HOMEBREW_NO_INSTALL_CLEANUP": "1",
        "NONINTERACTIVE": "1", **_passthrough(inherited),
    }
