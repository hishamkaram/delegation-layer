"""Tagged-source observations from GitHub for published-consumer verification."""

from __future__ import annotations

from .contract import MAX_OUTPUT, VerificationError
from .published_contract import (
    SKILL_SOURCE_PREFIX, SkillManifest, parse_tree_listing, skill_file,
)
from .published_ports import ConsumerPorts


_TREE_FILTER = (
    '"truncated\\t" + (.truncated | tostring), '
    '(.tree[] | select(.path | startswith("' + SKILL_SOURCE_PREFIX + '")) '
    '| [.path, .type, .mode, .sha, ((.size // 0) | tostring)] | @tsv)'
)


def fetch_skill_manifest(
        ports: ConsumerPorts, receipt: dict, repo: str, commit: str,
        env: dict[str, str]) -> SkillManifest:
    """Derive the skill file set from the tagged commit and prove every blob's git hash."""
    listing = ports.run(receipt, "github-skill-tree", [
        "gh", "api", f"repos/{repo}/git/trees/{commit}?recursive=1", "--jq", _TREE_FILTER,
    ], env, capture=True, timeout=60)
    entries = parse_tree_listing(listing)
    files = []
    for index, entry in enumerate(entries):
        if entry.size > MAX_OUTPUT:
            raise VerificationError("skill_file_too_large", "skill-manifest")
        content = ports.run(receipt, f"github-skill-blob-{index}", [
            "gh", "api", "--header", "Accept: application/vnd.github.raw+json",
            f"repos/{repo}/git/blobs/{entry.blob_sha}",
        ], env, capture=True, timeout=60)
        files.append(skill_file(entry, content))
    return SkillManifest(commit=commit, files=tuple(files))


def github_latest_tag(
        ports: ConsumerPorts, receipt: dict, repo: str, env: dict[str, str]) -> str:
    raw = ports.run(receipt, "github-latest-release", [
        "gh", "api", f"repos/{repo}/releases/latest", "--jq", ".tag_name",
    ], env, capture=True, timeout=30)
    try:
        value = raw.decode("ascii").strip()
    except UnicodeError:
        raise VerificationError("github_latest_invalid", "github-latest") from None
    if not value.startswith("v") or len(value) > 32:
        raise VerificationError("github_latest_invalid", "github-latest")
    return value
