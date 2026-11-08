"""Test one CI artifact's installation through one channel on this host.

The loader verifies the ZIP and manifest; the archive reader verifies its safe executable inventory.
Shell installs execute install.sh from the exact manifest source commit.
Existing installation and bootstrap owners check the installed default path
and one closed fixture turn. finish_gate owns workspace cleanup and retains
unknown child lifetimes. The invocation receipt records actual test results.
"""

from __future__ import annotations

from dataclasses import dataclass, replace
import os
from pathlib import Path

from scripts.acceptance.directories.journal import release_exclusive_file_lock
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.bounded_files import BoundedReadError
from scripts.acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.release_verification.assets import extract_archive
from scripts.release_verification.assets import gh_environment
from scripts.release_verification.assets import inspect_binaries
from scripts.release_verification.assets import platform_identity
from scripts.release_verification.assets import remember_directory
from scripts.release_verification.candidate_artifact import load_candidate_artifact
from scripts.acceptance.evidence.source import child_environment
from scripts.release_verification.candidate_install import open_workspace
from scripts.release_verification.candidate_install import verify_candidate_install
from scripts.release_verification.candidate_install_bootstrap import InstalledBootstrapProbe
from scripts.release_verification.candidate_install_contract import CHANNEL_RULES
from scripts.release_verification.candidate_install_contract import INSTALLER_REPO_PATH
from scripts.release_verification.candidate_install_contract import WITNESS_ROLE_KINDS
from scripts.release_verification.candidate_install_contract import CandidateFiles
from scripts.release_verification.candidate_install_contract import CandidateInstallRequest
from scripts.release_verification.candidate_install_contract import InstallChannel
from scripts.release_verification.candidate_manifest import ArtifactKind
from scripts.release_verification.candidate_manifest import CandidateManifest
from scripts.acceptance.evidence.source import read_regular_blobs
from scripts.release_verification.cleanup import finish_gate
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import new_receipt
from scripts.release_verification.filesystem import _acquire_receipt_lock
from scripts.release_verification.filesystem import write_new_file
from scripts.release_verification.github import GitHubClient
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import default_ports
from scripts.release_verification.published_source import fetch_skill_manifest


CLAIM = "candidate-install-proof"


STAGE = "prove-candidate-install"


EXPORT_STAGE = "export-install-proof"


RECEIPT_NAME = "receipt.json"


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
    """One invocation: an artifact ID, channel, output directory and source checkout.

    ``bootstrap_parent`` is an existing directory short enough to hold the bootstrap probe's
    isolated HOME and its private supervisor endpoint; None selects the operator's HOME.
    """

    artifact_id: int
    channel: InstallChannel
    output_dir: Path
    checkout: Path
    repository: str = "hishamkaram/delegation-layer"
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
    if resolved.is_relative_to(checkout) or checkout.is_relative_to(resolved):
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
    # Tag and source revision are unknown until the artifact is loaded.
    receipt.update({"claim": CLAIM, "tag": None, "source_revision": None, "harness_revision": None})
    receipt["candidate_install"] = {"channel": request.channel.value, "status": "partial"}
    receipt["unresolved_limits"] = [
        "No authenticated provider acceptance; the one executed task is a local shell fixture "
        "turn through the installed delegate, runner and supervisor pair.",
        "Installed paths record selection; required native behavior determines compatibility.",
        "Descendant process closure of installer children is not proven; any failure retains "
        "the workspace for investigation.",
        "This receipt records the installation test result.",
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
    bootstrap_parent = request.bootstrap_parent
    if bootstrap_parent is None:
        raise VerificationError("candidate_install_bootstrap_parent_invalid", STAGE)
    info = platform_identity()
    receipt["platform"] = info
    platform = (info["os"], info["arch"])
    receipt["stage"] = "load-candidate"
    github_env = gh_environment(auth_mode=request.github_auth)
    workspace = request.output_dir / WORKSPACE_NAME
    remember_directory(receipt, owner.create("candidate install workspace", workspace, request.output_dir))
    locations["assets"] = workspace
    payload = workspace / "payload"
    payload.mkdir(mode=0o700)
    artifact = load_candidate_artifact(GitHubClient(receipt, github_env), request.artifact_id,
                                       repository=request.repository,
                                       container=workspace / "candidate-artifact.zip", payload=payload)
    locator = artifact.locator
    manifest = artifact.manifest
    receipt.update({"tag": manifest.build_tag, "package_version": manifest.version})
    receipt["candidate_install"]["artifact"] = artifact.observation_record()
    receipt["stage"] = "inspect-candidate-archive"
    files = channel_files(manifest, payload, request.channel, platform)
    extracted = workspace / "extracted"
    extract_archive(files.archive, extracted, owner, receipt)
    inspect_binaries(extracted, receipt)
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
        skill=skill)
    layout = open_workspace(owner=owner, parent=workspace, receipt=receipt)
    receipt["candidate_install"]["bootstrap_parent"] = str(bootstrap_parent)
    probe = InstalledBootstrapProbe(receipt=receipt, locations=locations, parent=bootstrap_parent)
    facts = verify_candidate_install(install_request, ports=ports, probe=probe, receipt=receipt, layout=layout,
                                     info=info, channels=(request.channel.value,))
    if tuple(observation.channel for observation in facts.installs) != (request.channel.value,):
        raise VerificationError("candidate_install_channel_mismatch", STAGE)
    receipt["candidate_install"].update({
        "status": "verified", "artifact_id": request.artifact_id,
        "installation": facts.to_record(),
    })
