"""Prove one unpublished candidate's install on this host through exactly one channel.

The application composes existing owners only. The locator owner binds the
hosted candidate, ``load_candidate_artifact`` verifies its payload, the archive
owner extracts and hashes the host binaries, ``read_regular_blobs`` reads the
shell installer from the manifest source commit, ``verify_candidate_install``
observes the one installation with this application's ``InstalledBootstrapProbe``
as its required bootstrap port, ``install_evidence`` assembles the proof, and
``read_install_proof`` reads the exported bytes back. ``observe_harness`` binds
the executing checkout before and after. The invocation receipt, its cleanup
layout and ``finish_gate`` own every created resource; the proof grants no
readiness, store or publication authority.

Output directory layout, created new for one invocation: ``receipt.json`` (and
the cleanup owner's lock and journal beside it), ``install-proof.json``,
``install-witness.json`` and the ``workspace`` directory that cleanup removes. The
bootstrap probe's short private root lies below the request's existing bootstrap parent
(the operator's HOME unless one is selected), because the deep workspace cannot hold a
private supervisor endpoint; the probe removes it after a verified turn and otherwise
retains it as a location the gate reports.
"""

from __future__ import annotations

from dataclasses import dataclass, replace
import os
from pathlib import Path

from ..directories.journal import release_exclusive_file_lock
from ..directories.operations import DirectoryOwner
from ..evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from ..evidence.digests import sha
from .assets import extract_archive, gh_environment, inspect_binaries, platform_identity, remember_directory
from .candidate_artifact import load_candidate_artifact
from .candidate_build_source import child_environment
from .candidate_install import open_workspace, verify_candidate_install
from .candidate_install_bootstrap import InstalledBootstrapProbe
from .candidate_install_contract import (
    CHANNEL_RULES, INSTALLER_REPO_PATH, WITNESS_ROLE_KINDS, CandidateFiles, CandidateInstallRequest,
    HarnessIdentity, InstallChannel, read_install_proof,
)
from .candidate_install_evidence import install_evidence
from .candidate_manifest import MAX_LOCATOR_BYTES, ArtifactKind, CandidateManifest, parse_locator
from .candidate_source_observation import read_regular_blobs
from .cleanup import finish_gate
from .contract import BINARIES, VerificationError, new_receipt, parse_binary_digests
from .filesystem import _acquire_receipt_lock, write_new_file
from .github import GitHubClient
from .published_contract import UNRESOLVED_OWNERSHIP_CODES
from .published_ports import ConsumerPorts, default_ports
from .published_source import fetch_skill_manifest
from .source import observe_harness


CLAIM = "candidate-install-proof"


STAGE = "prove-candidate-install"


EXPORT_STAGE = "export-install-proof"


# The maintained entry point; its static import closure is the executing harness.
CLI_SEED = "scripts/prove_candidate_install.py"


RECEIPT_NAME = "receipt.json"


PROOF_NAME = "install-proof.json"


WITNESS_NAME = "install-witness.json"


WORKSPACE_NAME = "workspace"


# The operator-facing channel names; Homebrew is the contract's ``brew`` channel.
CHANNELS = {"shell": InstallChannel.SHELL, "npm": InstallChannel.NPM, "homebrew": InstallChannel.BREW}


_LIMITS = {
    InstallChannel.SHELL: [],
    InstallChannel.NPM: [],
    InstallChannel.BREW: [
        "The Homebrew installation is retained, not uninstalled; run only on a fresh disposable "
        "supported Homebrew host.",
    ],
}


@dataclass(frozen=True)
class ProofRequest:
    """One invocation: the locator file, one channel, a new output directory and the executing checkout.

    ``bootstrap_parent`` is an existing directory short enough to hold the bootstrap probe's
    isolated HOME and its private supervisor endpoint; None selects the operator's HOME.
    """

    locator: Path
    channel: InstallChannel
    output_dir: Path
    checkout: Path
    github_auth: str = "environment"
    bootstrap_parent: Path | None = None


def require_output_dir(request: ProofRequest) -> None:
    """Refuse, before anything is created, an output directory this invocation cannot own alone."""
    output, checkout = request.output_dir, request.checkout.resolve()
    if not output.is_absolute() or ".." in output.parts:
        raise VerificationError("candidate_install_output_invalid", STAGE)
    if os.path.lexists(output):
        raise VerificationError("candidate_install_output_exists", STAGE)
    resolved = output.parent.resolve() / output.name
    locator = request.locator.absolute()
    if (resolved.is_relative_to(checkout) or checkout.is_relative_to(resolved)
            or locator.is_relative_to(resolved)):
        raise VerificationError("candidate_install_output_overlap", STAGE)


def require_bootstrap_parent(request: ProofRequest) -> Path:
    """Resolve, once and before anything is created, the existing parent of the bootstrap root.

    The directory owner that creates the root below it repeats its own identity checks.
    """
    selected = Path.home() if request.bootstrap_parent is None else request.bootstrap_parent
    if not selected.is_absolute() or ".." in selected.parts:
        raise VerificationError("candidate_install_bootstrap_parent_invalid", STAGE)
    try:
        parent = selected.resolve(strict=True)
    except (OSError, RuntimeError):
        raise VerificationError("candidate_install_bootstrap_parent_invalid", STAGE) from None
    if not parent.is_dir():
        raise VerificationError("candidate_install_bootstrap_parent_invalid", STAGE)
    return parent


def prove_candidate_install(request: ProofRequest, *, ports: ConsumerPorts | None = None) -> int:
    """Prove one channel; returns the process exit code (0 only on a proven, cleaned-up pass)."""
    require_output_dir(request)
    request = replace(request, bootstrap_parent=require_bootstrap_parent(request))
    try:
        request.output_dir.mkdir(mode=0o700)
    except OSError:
        raise VerificationError("candidate_install_output_unavailable", STAGE) from None
    receipt_path = request.output_dir / RECEIPT_NAME
    lock = _acquire_receipt_lock(receipt_path)
    try:
        return _prove_locked(request, receipt_path, ports or default_ports())
    finally:
        release_exclusive_file_lock(lock)


def _new_proof_receipt(request: ProofRequest, gate: str) -> dict:
    receipt = new_receipt(gate, "", "")
    # Tag and revisions are unknown until the candidate and harness are observed.
    receipt.update({"claim": CLAIM, "tag": None, "source_revision": None, "harness_revision": None})
    receipt["candidate_install"] = {"channel": request.channel.value, "status": "partial"}
    receipt["unresolved_limits"] = [
        "No authenticated provider acceptance; the one executed task is a local shell fixture "
        "turn through the installed delegate, runner and supervisor pair.",
        "Binary digests cannot distinguish byte-identical copies from the installed files.",
        "Descendant process closure of installer children is not proven; any failure retains "
        "the workspace for investigation.",
        "The proof records an observation only; it grants no readiness, store or publication authority.",
        *_LIMITS[request.channel],
    ]
    return receipt


def _prove_locked(request: ProofRequest, receipt_path: Path, ports: ConsumerPorts) -> int:
    receipt = _new_proof_receipt(request, f"candidate-install-{request.channel.value}")
    owner, locations = DirectoryOwner(), {}

    def action() -> None:
        try:
            _prove(request, ports, receipt, owner, locations)
        except VerificationError as error:
            if error.code in UNRESOLVED_OWNERSHIP_CODES:
                receipt["candidate_install"]["unresolved_ownership"] = {
                    "stage": error.stage, "code": error.code,
                    "workspace": str(locations.get("assets", "")),
                }
            raise

    return finish_gate(receipt_path, receipt, owner, action, "remove", locations)


def executing_harness(receipt: dict, checkout: Path) -> HarnessIdentity:
    """The committed identity of the harness this checkout executes; never the manifest's build harness."""
    head, fingerprint, committed = observe_harness(receipt, os.environ, checkout=checkout, seeds=(CLI_SEED,))
    if not committed:
        raise VerificationError("candidate_install_harness_not_committed", STAGE)
    return HarnessIdentity(commit=head, sha256=fingerprint)


def installer_source(receipt: dict, *, checkout: Path, manifest: CandidateManifest) -> bytes:
    """The exact install.sh blob of the manifest source commit; a missing object is a refusal."""
    return read_regular_blobs(receipt, child_environment(os.environ), repository=checkout,
                              tree=manifest.source.commit, paths=(INSTALLER_REPO_PATH,))[INSTALLER_REPO_PATH]


def channel_files(manifest: CandidateManifest, payload: Path, channel: InstallChannel,
                  platform: tuple[str, str]) -> CandidateFiles:
    """Exactly the payload files the channel consumes; the archive is the host platform's."""
    def path(kind: ArtifactKind) -> Path:
        entry = (manifest.archive(*platform) if kind is ArtifactKind.ARCHIVE
                 else next(item for item in manifest.artifacts if item.kind is kind))
        return payload / entry.path
    consumed = CHANNEL_RULES[channel].consumed
    return CandidateFiles(**{role: path(kind) for role, kind in WITNESS_ROLE_KINDS.items() if kind in consumed})


def export_file(path: Path, data: bytes) -> bytes:
    """Write ``data`` to a new file and return the bytes read back from it; a failed write reads nothing."""
    write_new_file(path, data, path_code="candidate_install_evidence_unwritable",
                   write_code="candidate_install_evidence_write_failed", stage=EXPORT_STAGE)
    try:
        return read_bounded_regular_file(path, len(data))
    except BoundedReadError:
        raise VerificationError("candidate_install_evidence_readback_failed", EXPORT_STAGE) from None


def _prove(request: ProofRequest, ports: ConsumerPorts, receipt: dict, owner: DirectoryOwner,
           locations: dict[str, Path]) -> None:
    receipt["stage"] = "observe-harness"
    harness = executing_harness(receipt, request.checkout)
    receipt.update({"source_revision": harness.commit, "harness_revision": harness.commit,
                    "source_harness": {"revision": harness.commit, "sha256": harness.sha256}})
    info = platform_identity()
    receipt["platform"] = info
    platform = (info["os"], info["arch"])
    receipt["stage"] = "load-candidate"
    try:
        locator = parse_locator(read_bounded_regular_file(request.locator, MAX_LOCATOR_BYTES))
    except BoundedReadError:
        raise VerificationError("candidate_locator_unreadable", STAGE) from None
    github_env = gh_environment(auth_mode=request.github_auth)
    workspace = request.output_dir / WORKSPACE_NAME
    remember_directory(receipt, owner.create("candidate install proof workspace", workspace, request.output_dir))
    locations["assets"] = workspace
    payload = workspace / "payload"
    payload.mkdir(mode=0o700)
    artifact = load_candidate_artifact(GitHubClient(receipt, github_env), locator,
                                       container=workspace / "candidate-artifact.zip", payload=payload)
    manifest = artifact.manifest
    receipt.update({"tag": manifest.build_tag, "package_version": manifest.version})
    receipt["candidate_install"]["artifact"] = artifact.observation_record()
    receipt["stage"] = "inspect-candidate-archive"
    files = channel_files(manifest, payload, request.channel, platform)
    extracted = workspace / "extracted"
    extract_archive(files.archive, extracted, owner, receipt)
    inspect_binaries(extracted, receipt)
    binaries = parse_binary_digests({name: receipt["binaries"][name]["sha256"] for name in BINARIES}, STAGE)
    rule = CHANNEL_RULES[request.channel]
    source = None
    if rule.installer_kind is None:
        receipt["stage"] = "read-source-installer"
        source = installer_source(receipt, checkout=request.checkout, manifest=manifest)
        installer = workspace / INSTALLER_REPO_PATH
        if export_file(installer, source) != source:
            raise VerificationError("candidate_install_evidence_readback_failed", EXPORT_STAGE)
    else:
        installer = dict(files.roles())[next(role for role, kind in WITNESS_ROLE_KINDS.items()
                                             if kind is rule.installer_kind)]
    skill: tuple[tuple[str, str], ...] = ()
    if request.channel is InstallChannel.NPM:
        receipt["stage"] = "fetch-skill-manifest"
        skill = tuple(sorted(fetch_skill_manifest(
            ports, receipt, locator.repository, manifest.source.commit, github_env).digests().items()))
    install_request = CandidateInstallRequest(
        tag=manifest.build_tag, repo=locator.repository, installer=installer, files=files,
        binaries=tuple(sorted(binaries.as_mapping().items())), skill=skill)
    layout = open_workspace(owner=owner, parent=workspace, receipt=receipt)
    receipt["candidate_install"]["bootstrap_parent"] = str(request.bootstrap_parent)
    probe = InstalledBootstrapProbe(receipt=receipt, locations=locations, parent=request.bootstrap_parent)
    facts = verify_candidate_install(install_request, ports=ports, probe=probe, receipt=receipt, layout=layout,
                                     info=info, channels=(request.channel.value,))
    if tuple(observation.channel for observation in facts.installs) != (request.channel.value,):
        raise VerificationError("candidate_install_channel_mismatch", STAGE)
    # The harness is matched again before the pair exists, so no consumable proof names a changed harness.
    receipt["stage"] = "reobserve-harness"
    if executing_harness(receipt, request.checkout) != harness:
        raise VerificationError("candidate_install_harness_changed", STAGE)
    receipt["stage"] = EXPORT_STAGE
    value, proof, witness = install_evidence(facts, manifest, locator, installer_source=source, harness=harness)
    exported = (export_file(request.output_dir / PROOF_NAME, proof),
                export_file(request.output_dir / WITNESS_NAME, witness))
    if exported != (proof, witness) or read_install_proof(
            exported[0], exported[1], manifest=manifest, installer_source=source,
            repository=locator.repository) != value:
        raise VerificationError("candidate_install_evidence_readback_failed", EXPORT_STAGE)
    receipt["candidate_install"].update({
        "status": "verified", "candidate_id": locator.candidate_id,
        "proof": {"path": str(request.output_dir / PROOF_NAME), "sha256": sha(proof)},
        "witness": {"path": str(request.output_dir / WITNESS_NAME), "sha256": sha(witness)},
    })
