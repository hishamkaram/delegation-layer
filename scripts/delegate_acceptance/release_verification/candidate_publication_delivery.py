"""Publish an existing CI distribution without rebuilding or consulting a proof store.

The trusted main workflow supplies only an artifact ID and explicit resume
choice. GitHub owns CI provenance; the artifact loader owns byte validation.
Existing GitHub, npm and tap edges own their destination decisions. The usual
directory owner and finish_gate retain failures and clean completed children.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
import os
from pathlib import Path
import re

from ..directories.journal import release_exclusive_file_lock
from ..directories.operations import DirectoryOwner
from .assets import remember_directory
from .candidate_artifact import load_candidate_artifact
from .candidate_build_source import child_environment
from .candidate_github_delivery import prepare_release_assets, publish_prepared_release, release_notes_endpoint
from .candidate_npm_delivery import deliver_npm
from .candidate_publication_contract import ReleaseSubject
from .candidate_publication_preflight import require_publishable
from .candidate_tap_delivery import deliver_tap
from .cleanup import finish_gate
from .contract import VerificationError, new_receipt
from .filesystem import _acquire_receipt_lock, write_new_file
from .github import GitHubClient
from .published_contract import NPM_REGISTRY
from .published_ports import ConsumerPorts


STAGE = "publish-distribution"
REPOSITORY = "hishamkaram/delegation-layer"
RECEIPT_NAME = "receipt.json"
WORKSPACE_NAME = "workspace"
MUTATING_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})
READ_ONLY_POST = release_notes_endpoint(REPOSITORY)
NPM_USER_CONFIG = f"//{NPM_REGISTRY.removeprefix('https://')}:_authToken=${{NPM_TOKEN}}\n".encode()


@dataclass(frozen=True)
class HostedRun:
    repository: str
    run_id: int
    run_attempt: int
    commit: str


def hosted_run(environment: Mapping[str, str]) -> HostedRun:
    """Only the existing trusted release workflow on main may publish."""
    expected = {
        "GITHUB_REPOSITORY": REPOSITORY, "GITHUB_REF": "refs/heads/main",
        "GITHUB_EVENT_NAME": "workflow_dispatch",
        "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/release.yml@refs/heads/main",
    }
    if any(environment.get(name) != value for name, value in expected.items()):
        raise VerificationError("publication_run_untrusted", STAGE)
    for name in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"):
        if re.fullmatch(r"[1-9][0-9]{0,17}", environment.get(name, "")) is None:
            raise VerificationError("publication_run_untrusted", STAGE)
    commit = environment.get("GITHUB_SHA", "")
    if re.fullmatch(r"[0-9a-f]{40,64}", commit) is None:
        raise VerificationError("publication_run_untrusted", STAGE)
    return HostedRun(REPOSITORY, int(environment["GITHUB_RUN_ID"]),
                     int(environment["GITHUB_RUN_ATTEMPT"]), commit)


@dataclass(frozen=True)
class HostedPublication:
    artifact_id: int
    resume_published: bool
    run: HostedRun
    output_dir: Path
    checkout: Path

    def __post_init__(self) -> None:
        if type(self.artifact_id) is not int or self.artifact_id <= 0 or type(self.resume_published) is not bool:
            raise VerificationError("publication_input_invalid", STAGE)


def require_output_dir(request: HostedPublication) -> None:
    output, checkout = request.output_dir, request.checkout.resolve()
    if not output.is_absolute() or ".." in output.parts:
        raise VerificationError("candidate_publication_output_invalid", STAGE)
    if os.path.lexists(output):
        raise VerificationError("candidate_publication_output_exists", STAGE)
    resolved = output.parent.resolve() / output.name
    if resolved.is_relative_to(checkout) or checkout.is_relative_to(resolved):
        raise VerificationError("candidate_publication_output_overlap", STAGE)


def first_mutation_started(receipt: dict) -> bool:
    """Diagnostic only: report actual destination write attempts, including uncertain failures."""
    for record in receipt["commands"]:
        if not isinstance(record, dict):
            raise VerificationError("receipt_commands_invalid", STAGE)
        argv, method = record.get("argv"), record.get("method")
        if (record.get("name") == "github-api" and method in MUTATING_METHODS
                and (method, record.get("endpoint")) != ("POST", READ_ONLY_POST)):
            return True
        if isinstance(argv, list) and (argv[:3] == ["gh", "release", "upload"] or (
                len(argv) > 1 and isinstance(argv[0], str) and Path(argv[0]).name == "npm"
                and argv[1] == "publish")):
            return True
    return False


@dataclass(frozen=True)
class _Environments:
    github: dict[str, str]
    tap: dict[str, str]
    npm: dict[str, str]
    npm_user_config: bytes


def _environments(environment: Mapping[str, str], npm_config: Path, *, resume_published: bool) -> _Environments:
    names = ("GH_TOKEN", "HOMEBREW_TAP_TOKEN", *(() if resume_published else ("NPM_TOKEN",)))
    missing = [name for name in names if not environment.get(name)]
    if missing:
        raise VerificationError("publication_credentials_missing:" + ",".join(name.lower() for name in missing),
                                "credentials")
    base = child_environment(environment)
    token = {} if resume_published else {"NPM_TOKEN": environment["NPM_TOKEN"]}
    return _Environments(
        github={**base, "GH_TOKEN": environment["GH_TOKEN"], "GH_PROMPT_DISABLED": "1"},
        tap={**base, "GH_TOKEN": environment["HOMEBREW_TAP_TOKEN"], "GH_PROMPT_DISABLED": "1"},
        npm={**base, **token, "NPM_CONFIG_USERCONFIG": str(npm_config / "user.npmrc"),
             "NPM_CONFIG_GLOBALCONFIG": str(npm_config / "global.npmrc")},
        npm_user_config=b"" if resume_published else NPM_USER_CONFIG)


def publish_candidate(request: HostedPublication, *, environment: Mapping[str, str], ports: ConsumerPorts) -> int:
    """One explicit invocation; matching completed destinations skip, conflicts refuse."""
    require_output_dir(request)
    output = request.output_dir.parent.resolve() / request.output_dir.name
    try:
        output.mkdir(mode=0o700)
    except OSError:
        raise VerificationError("candidate_publication_output_unavailable", STAGE) from None
    receipt_path = output / RECEIPT_NAME
    lock = _acquire_receipt_lock(receipt_path)
    try:
        receipt = new_receipt(STAGE, "", request.run.commit)
        receipt["claim"] = "distribution-publication"
        receipt["publication"] = {"artifact_id": request.artifact_id, "status": "partial"}
        owner, locations = DirectoryOwner(), {}

        def action() -> None:
            try:
                _publish(request, output, environment=environment, ports=ports, receipt=receipt,
                         owner=owner, locations=locations)
            finally:
                receipt["publication"]["first_mutation_started"] = first_mutation_started(receipt)

        return finish_gate(receipt_path, receipt, owner, action, "remove", locations)
    finally:
        release_exclusive_file_lock(lock)


def _publish(request: HostedPublication, output: Path, *, environment: Mapping[str, str],
             ports: ConsumerPorts, receipt: dict, owner: DirectoryOwner, locations: dict[str, Path]) -> None:
    workspace = output / WORKSPACE_NAME
    npm_config = workspace / "npm"
    environments = _environments(environment, npm_config, resume_published=request.resume_published)
    client = GitHubClient(receipt, environments.github)
    receipt["stage"] = "load-distribution"
    remember_directory(receipt, owner.create("distribution publication workspace", workspace, output))
    locations["assets"] = workspace
    payload, requests = workspace / "payload", workspace / "requests"
    for directory in (payload, requests, npm_config):
        directory.mkdir(mode=0o700)
    for name, data in (("user.npmrc", environments.npm_user_config), ("global.npmrc", b"")):
        write_new_file(npm_config / name, data, path_code="publication_npm_config_invalid",
                       write_code="publication_npm_config_write_failed", stage=receipt["stage"])
    artifact = load_candidate_artifact(client, request.artifact_id, repository=request.run.repository,
                                       container=workspace / "distribution.zip", payload=payload)
    receipt["stage"] = "validate-ci"
    require_publishable(client, artifact)
    subject = ReleaseSubject(locator=artifact.locator, manifest=artifact.manifest)
    receipt.update({"tag": artifact.manifest.build_tag, "package_version": artifact.manifest.version})
    record = receipt["publication"]
    record["artifact"] = artifact.observation_record()
    receipt["stage"] = "prepare-release"
    prepared = prepare_release_assets(subject, artifact, client=client, ports=ports, work_directory=requests,
                                      resume_published=request.resume_published)
    receipt["stage"] = "publish-release"
    published = publish_prepared_release(subject, prepared, client=client, work_directory=requests,
                                         resume_published=request.resume_published)
    record["release_id"] = published.release_id
    receipt["stage"] = "deliver-npm"
    npm = deliver_npm(artifact, ports=ports, receipt=receipt, environment=environments.npm,
                      resume_published=request.resume_published)
    record["npm"] = {"version": npm.version, "integrity": npm.integrity}
    receipt["stage"] = "deliver-tap"
    record["tap_blob_sha"] = deliver_tap(
        artifact, client=GitHubClient(receipt, environments.tap), work_directory=requests)
    record["status"] = "delivered"
