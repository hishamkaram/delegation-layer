"""Construction of one install proof and its witness from one observed installation.

This module owns only assembly. The values, channel rules, codecs and the
authoritative ``read_install_proof`` stay in ``candidate_install_contract``;
every pair built here is returned only after that reader accepts its bytes.
"""

from __future__ import annotations

from ..evidence.digests import sha
from .candidate_install_contract import (
    CHANNEL_RULES, INSTALLER_REPO_PATH, PROOF_STAGE, WITNESS_ROLE_KINDS, ArtifactRef,
    CandidateInstallFacts, ChannelRule, HarnessIdentity, InstallChannel, InstallerBinding,
    InstallObservation, InstallProof, ManifestArtifactBinding, SourceFileBinding, encode_install_proof,
    encode_install_witness, read_install_proof,
)
from .candidate_manifest import (
    PLATFORMS, ArtifactKind, CandidateLocator, CandidateManifest, bind_manifest, encode_manifest,
)
from .contract import VerificationError, parse_binary_digests


def _unique_pairs(pairs: object) -> dict[str, object]:
    """Return the mapping of (name, value) pairs, refusing a repeated name that a dict would hide."""
    if not isinstance(pairs, tuple) or not all(isinstance(pair, tuple) and len(pair) == 2
                                               and isinstance(pair[0], str) for pair in pairs):
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    mapping = dict(pairs)
    if len(mapping) != len(pairs):
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    return mapping


def _sole_observation(facts: CandidateInstallFacts) -> tuple[InstallObservation, InstallChannel]:
    if not isinstance(facts.installs, tuple) or len(facts.installs) != 1:
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    observation = facts.installs[0]
    if not isinstance(observation, InstallObservation):
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    try:
        return observation, InstallChannel(observation.channel)
    except ValueError:
        raise VerificationError("install_witness_mismatch", PROOF_STAGE) from None


def _consumed(manifest: CandidateManifest, rule: ChannelRule,
              platform: tuple[str, str]) -> tuple[ArtifactRef, ...]:
    """The manifest entries of the channel's kinds; the archive is the one of ``platform``."""
    entries = [manifest.archive(*platform) if kind is ArtifactKind.ARCHIVE
               else next(entry for entry in manifest.artifacts if entry.kind is kind)
               for kind in rule.consumed]
    refs = (ArtifactRef(kind=entry.kind, path=entry.path, sha256=entry.sha256) for entry in entries)
    return tuple(sorted(refs, key=lambda ref: ref.path))


def _installer(rule: ChannelRule, manifest: CandidateManifest, consumed: tuple[ArtifactRef, ...],
               observation: InstallObservation) -> InstallerBinding:
    """Bind the installer the run executed: install.sh of the source commit, or the channel's artifact."""
    if rule.installer_kind is None:
        return SourceFileBinding(source_commit=manifest.source.commit, repo_path=INSTALLER_REPO_PATH,
                                 sha256=observation.installer_sha256)
    return ManifestArtifactBinding(artifact=next(ref for ref in consumed if ref.kind is rule.installer_kind))


def install_evidence(facts: CandidateInstallFacts, manifest: CandidateManifest, locator: CandidateLocator, *,
                     installer_source: bytes | None,
                     harness: HarnessIdentity) -> tuple[InstallProof, bytes, bytes]:
    """Return the read-back proof, its canonical bytes and the canonical witness bytes of one install.

    ``facts`` must record exactly one observation, its platform, and exactly the
    candidate file roles of that observation's channel; nothing is filtered.
    ``installer_source`` is the source-commit install.sh for the shell channel
    and None for the artifact-bound channels, as ``read_install_proof`` requires.
    """
    if not isinstance(manifest, CandidateManifest) or not isinstance(locator, CandidateLocator):
        raise VerificationError("install_proof_candidate_mismatch", PROOF_STAGE)
    # The locator owner refuses a manifest whose canonical bytes it does not bind.
    bind_manifest(locator, encode_manifest(manifest))
    if not isinstance(facts, CandidateInstallFacts) or facts.platform not in PLATFORMS:
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    observation, channel = _sole_observation(facts)
    rule = CHANNEL_RULES[channel]
    roles = {role for role, kind in WITNESS_ROLE_KINDS.items() if kind in rule.consumed}
    if set(_unique_pairs(facts.candidate_files)) != roles:
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    binaries = parse_binary_digests(_unique_pairs(observation.binaries), PROOF_STAGE)
    consumed = _consumed(manifest, rule, facts.platform)
    witness = encode_install_witness(facts)
    os_name, arch = facts.platform
    proof = encode_install_proof(InstallProof(
        candidate_id=locator.candidate_id, os=os_name, arch=arch, channel=channel, consumed=consumed,
        installer=_installer(rule, manifest, consumed, observation), harness=harness,
        binaries=binaries,
        delegate_version=observation.delegate_version, default_state=observation.default_state,
        bootstrap=observation.bootstrap, witness_sha256=sha(witness)))
    value = read_install_proof(proof, witness, manifest=manifest, installer_source=installer_source,
                               repository=locator.repository)
    return value, proof, witness
