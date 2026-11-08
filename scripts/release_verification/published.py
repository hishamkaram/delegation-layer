"""Published-consumer verification: npm package or Homebrew formula against one exact release."""

from __future__ import annotations

import argparse
import re
import secrets
from pathlib import Path

from scripts.release_verification import filesystem as filesystem
from scripts.acceptance.directories.journal import release_exclusive_file_lock
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.release_verification.assets import gh_environment
from scripts.release_verification.assets import platform_identity
from scripts.release_verification.assets import remember_directory
from scripts.release_verification.assets import resolve_release_identity
from scripts.release_verification.cleanup import _replay_prepared_cleanup
from scripts.release_verification.cleanup import finish_gate
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import new_receipt
from scripts.release_verification.filesystem import _acquire_receipt_lock
from scripts.release_verification.published_archive import load_archive_facts
from scripts.release_verification.published_archive import published_paths
from scripts.release_verification.published_contract import CHANNELS
from scripts.release_verification.published_contract import PublishedRequest
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES
from scripts.release_verification.published_contract import record_fact
from scripts.release_verification.published_homebrew import verify_homebrew_consumer
from scripts.release_verification.published_npm import verify_npm_consumer
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import default_ports
from scripts.release_verification.published_ports import prepare_layout
from scripts.acceptance.evidence.source import fingerprint_sources


CLAIM = "published-consumer-install"


_SEEDS = ("scripts/verify_release_archive.py",)


_LIMITS = {
    "npm": [
        "Executed package identity relies on npm's registry integrity check; the packed tarball is "
        "separately bound to registry metadata.",
    ],
    "homebrew": [
        "The Homebrew installation is retained, not uninstalled; run only on an ephemeral macOS runner.",
        "Default-path supervisor state is not observed for the Homebrew layout.",
    ],
}


def request_from_args(args: argparse.Namespace) -> PublishedRequest:
    return PublishedRequest(
        channel=args.channel, tag=args.tag, repo=args.repo, source_revision=args.source_revision,
        archive_receipt=Path(args.archive_receipt), extract_dir=Path(args.extract_dir),
        receipt=Path(args.receipt), github_auth=getattr(args, "github_auth", "environment"),
    )


def run_published(args: argparse.Namespace) -> int:
    return verify_published(request_from_args(args))


def verify_published(request: PublishedRequest, *, ports: ConsumerPorts | None = None) -> int:
    """Verify one published channel; returns the process exit code (0 only on a proven pass)."""
    receipt_path = request.receipt.absolute()
    lock_fd = _acquire_receipt_lock(receipt_path)
    try:
        return _published_locked(request, receipt_path, ports or default_ports())
    finally:
        release_exclusive_file_lock(lock_fd)


def _new_published_receipt(request: PublishedRequest, gate: str) -> dict:
    receipt = new_receipt(gate, request.tag, request.source_revision)
    receipt["claim"] = CLAIM
    receipt["published"] = {
        "channel": request.channel, "status": "partial", "verified": {},
        "retry": "reruns verify the same published artifacts; nothing is published or invoked with providers",
    }
    receipt["unresolved_limits"] = [
        "No authenticated provider acceptance or task execution; CLI checks cover versions, "
        "providers, capabilities and default-path state only.",
        "Descendant process closure of package-manager children is not proven; any failure retains "
        "the workspace for investigation.",
        *_LIMITS[request.channel],
    ]
    return receipt


def _published_locked(request: PublishedRequest, receipt_path: Path, ports: ConsumerPorts) -> int:
    gate = f"published-{request.channel}"
    replayed = _replay_prepared_cleanup(
        receipt_path, gate=gate, claim=CLAIM, tag=request.tag, revision=request.source_revision)
    if replayed is not None:
        return replayed
    receipt = _new_published_receipt(request, gate)
    filesystem.write_receipt(receipt_path, receipt)
    owner, locations = DirectoryOwner(), {}

    def action():
        try:
            _verify_channel(request, ports, receipt, owner, locations)
        except VerificationError as error:
            if error.code in UNRESOLVED_OWNERSHIP_CODES:
                receipt["published"]["unresolved_ownership"] = {
                    "stage": error.stage, "code": error.code,
                    "workspace": str(locations.get("assets", "")),
                }
            raise

    return finish_gate(receipt_path, receipt, owner, action, "remove", locations)


def _verify_channel(
        request: PublishedRequest, ports: ConsumerPorts, receipt: dict,
        owner: DirectoryOwner, locations: dict[str, Path]) -> None:
    if request.channel not in CHANNELS:
        raise VerificationError("published_channel_invalid", "validate-channel")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", request.repo):
        raise VerificationError("repository_invalid", "validate-repository")
    info = platform_identity()
    receipt["platform"] = info
    receipt["source_harness"] = fingerprint_sources(Path(__file__).resolve().parents[2], _SEEDS)
    archive = load_archive_facts(request, info)
    runner, _, _ = published_paths(request)
    receipt["release_revision"] = archive.release_commit
    receipt["release_identity"] = {
        "repository": request.repo, "tag": request.tag, "ref": f"refs/tags/{request.tag}",
        "commit_sha": archive.release_commit,
    }
    receipt["artifact"] = {"archive_sha256": archive.archive_sha256}
    receipt["published"]["archive_binaries"] = archive.binaries.to_record()
    workspace = runner / f"delegation-layer-published-{request.channel}-{secrets.token_hex(6)}"
    token = owner.create("published consumer workspace", workspace, runner)
    remember_directory(receipt, token)
    locations["assets"] = workspace
    layout = prepare_layout(workspace.resolve())
    receipt["stage"] = f"verify-{request.channel}"
    if request.channel == "npm":
        github_env = gh_environment(auth_mode=request.github_auth)
        identity = resolve_release_identity(request.repo, request.tag, receipt, github_env)
        if identity["commit_sha"] != archive.release_commit:
            raise VerificationError("release_identity_changed", "resolve-release-identity")
        verify_npm_consumer(
            ports=ports, receipt=receipt, archive=archive, layout=layout, info=info,
            repo=request.repo, github_env=github_env)
    else:
        verify_homebrew_consumer(
            ports=ports, receipt=receipt, archive=archive, layout=layout, info=info, repo=request.repo)
    receipt["published"]["status"] = "verified"
    record_fact(receipt, "complete", True)
