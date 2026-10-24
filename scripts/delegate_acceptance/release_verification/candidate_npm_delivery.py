"""npm registry delivery of one promoted candidate's exact package TGZ.

The manifest and locator own the immutable TGZ bytes. The caller owns
publication authorization, the exclusive payload directory and its lifetime,
the trusted publishing environment and its credentials, release-wide
authorization and the serialization of promotions. ``decide_npm`` owns act,
skip or refuse; ``require_latest_delivery`` owns the numeric version order;
``parse_npm_view`` owns the registry metadata shape and its ``gitHead`` rule;
the registry owns the published package and its ``latest`` tag. This module
performs only the npm reads and the single publish those decisions call for,
through the canonical command owner, and stores nothing. Its one ordering
guard is npm's own: before publishing it reads npm's current ``latest`` and
refuses unless the target is strictly newer, so a destination the caller's
GitHub-side check cannot see is never moved backwards.

Only a successful, complete version listing without the target means absence;
every failed, timed-out, oversized or malformed read stops. npm owns
authentication from the caller's environment and configuration; this module
never inspects credentials. The publish is attempted at most once: a failed or
unknown publish stops with no later read. Only after it exits 0 do bounded
read-only confirmation rounds wait for the registry to list the exact package
with ``latest`` at the target; an error or conflicting identity stops at once,
and nothing is republished or retagged. Running out of rounds is
``candidate_npm_confirmation_pending``: the receipt keeps the publish exit 0 and
a later explicit invocation observes again from the start. Without a publish, a
``latest`` tag other than the target is refused, never rewritten.
"""

from __future__ import annotations

from collections.abc import Mapping
import json
from time import sleep

from .candidate_artifact import CandidateArtifact
from .candidate_manifest import ArtifactEntry, ArtifactKind
from .candidate_publication_contract import (
    Absent, Delivery, NpmPackage, decide_npm, npm_package, require_latest_delivery,
)
from .contract import VerificationError
from .filesystem import bounded_fingerprint
from .published_contract import NPM_PACKAGE, NPM_REGISTRY, PackageIdentity, parse_npm_latest, parse_npm_view
from .published_files import read_package_bytes
from .published_ports import ConsumerPorts


STAGE = "deliver-candidate-npm"


# One publish uploads at most one package of the manifest's largest accepted size.
PUBLISH_TIMEOUT = 600


READ_TIMEOUT = 60


# Read-only rounds after a known successful publish, and the pause between pending rounds.
# Each round makes at most three reads, so the worst case is 15 reads of READ_TIMEOUT
# (900 s of read budgets) plus four pauses (40 s), excluding bounded child cleanup.
CONFIRMATION_ROUNDS = 5


CONFIRMATION_INTERVAL_SECONDS = 10


_VIEW_FIELDS = ("name", "version", "dist.integrity", "dist.shasum", "dist.tarball", "gitHead")


def deliver_npm(artifact: CandidateArtifact, *, ports: ConsumerPorts, receipt: dict,
                environment: Mapping[str, str], resume_published: bool = False) -> PackageIdentity:
    """Publish the exact candidate TGZ unless the registry holds it; return the confirmed identity.

    ``environment`` is passed to every npm child unchanged and never recorded.
    """
    if not isinstance(artifact, CandidateArtifact) or type(resume_published) is not bool:
        raise VerificationError("candidate_npm_request_invalid", STAGE)
    entry = _package_entry(artifact)
    expected = npm_package(artifact.manifest, read_package_bytes(artifact.payload / entry.path))
    npm = _Npm(ports, receipt, dict(environment), artifact)
    release_commit = artifact.manifest.source.commit
    observed = _observe(npm, "candidate-npm", expected, release_commit)
    if decide_npm(expected=expected, observed=observed, resume_published=resume_published) is Delivery.ACT:
        # decide_npm already refused ACT under resume_published, so this guard is a normal promotion.
        require_latest_delivery(version=expected.version, latest_version=_latest(npm, "candidate-npm-latest"),
                                resume_published=False)
        _require_unchanged(artifact, entry)
        npm.run("candidate-npm-publish", [
            "publish", str(artifact.payload / entry.path), "--access", "public", "--ignore-scripts",
            "--tag", "latest",
        ], timeout=PUBLISH_TIMEOUT)
        return _confirm_published(npm, expected, release_commit)
    if not isinstance(observed, PackageIdentity):
        # Checked narrowing of decide_npm's contract: it skips only a positive identity.
        raise VerificationError("candidate_npm_publish_unconfirmed", STAGE)
    if _latest(npm, "candidate-npm-latest") != expected.version:
        raise VerificationError("candidate_npm_latest_mismatch", STAGE)
    return observed


class _Npm:
    """The one npm executable, trusted environment and working directory of this delivery."""

    def __init__(self, ports: ConsumerPorts, receipt: dict, environment: dict[str, str],
                 artifact: CandidateArtifact):
        if not all(isinstance(key, str) and isinstance(value, str) for key, value in environment.items()):
            raise VerificationError("candidate_npm_environment_invalid", STAGE)
        npm = ports.which("npm")
        if npm is None:
            raise VerificationError("npm_unavailable", "locate-tools")
        self.ports, self.receipt, self.environment, self.npm = ports, receipt, environment, npm
        # The payload holds only the manifest's files, so no project npm configuration applies.
        self.cwd = artifact.payload

    def run(self, name: str, args: list[str], *, capture: bool = False, timeout: int = READ_TIMEOUT) -> bytes:
        return self.ports.run(self.receipt, name, [self.npm, *args, "--registry", NPM_REGISTRY],
                              self.environment, capture=capture, timeout=timeout, cwd=self.cwd)


def _package_entry(artifact: CandidateArtifact) -> ArtifactEntry:
    if not artifact.payload.is_absolute():
        raise VerificationError("candidate_npm_payload_invalid", STAGE)
    (entry,) = (item for item in artifact.manifest.artifacts if item.kind is ArtifactKind.NPM_PACKAGE)
    return entry


def _confirm_published(npm: _Npm, expected: NpmPackage, release_commit: str) -> PackageIdentity:
    """Read until the exact package is listed with ``latest`` at it; never publish or retag.

    Only a successful listing without the target, or a valid ``latest`` naming
    another version, is pending; every error and conflict propagates at once.
    """
    for round_index in range(CONFIRMATION_ROUNDS):
        if round_index:
            sleep(CONFIRMATION_INTERVAL_SECONDS)
        observed = _observe(npm, "candidate-npm-confirm", expected, release_commit)
        if observed is Absent.ABSENT:
            continue
        # A positive identity only skips or refuses; this is never publication authority.
        decide_npm(expected=expected, observed=observed, resume_published=False)
        if _latest(npm, "candidate-npm-confirm-latest") == expected.version:
            return observed
    raise VerificationError("candidate_npm_confirmation_pending", STAGE)


def _observe(npm: _Npm, prefix: str, expected: NpmPackage, release_commit: str) -> PackageIdentity | Absent:
    """Absence only from the complete listing; a listed version must have exact metadata."""
    versions = _versions(npm.run(f"{prefix}-versions", ["view", NPM_PACKAGE, "versions", "--json"],
                                 capture=True))
    if expected.version not in versions:
        return Absent.ABSENT
    raw = npm.run(f"{prefix}-metadata", ["view", f"{NPM_PACKAGE}@{expected.version}", *_VIEW_FIELDS, "--json"],
                  capture=True)
    return parse_npm_view(raw, expected.version, release_commit)


def _latest(npm: _Npm, name: str) -> str:
    return parse_npm_latest(npm.run(name, ["view", NPM_PACKAGE, "dist-tags.latest", "--json"], capture=True))


def _versions(raw: bytes) -> frozenset[str]:
    """The registry's complete version list; any other shape is unknown, never empty."""
    try:
        value = json.loads(raw)
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("candidate_npm_versions_invalid", STAGE) from None
    if (not isinstance(value, list) or not value
            or not all(isinstance(item, str) and item for item in value) or len(set(value)) != len(value)):
        raise VerificationError("candidate_npm_versions_invalid", STAGE)
    return frozenset(value)


def _require_unchanged(artifact: CandidateArtifact, entry: ArtifactEntry) -> None:
    """The TGZ is still the regular, non-symlink file of exactly the manifest's bytes."""
    observed = bounded_fingerprint(artifact.payload / entry.path, entry.size, "candidate_npm_package_changed")
    if (observed.size, observed.sha256) != (entry.size, entry.sha256):
        raise VerificationError("candidate_npm_package_changed", STAGE)
