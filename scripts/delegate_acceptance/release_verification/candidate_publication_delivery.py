"""Hosted exact-byte publication of one frozen candidate by one trusted-main run.

This application composes existing owners only. Root's dispatch names the
request, its invocation and H, the trusted-main code this run executes.
``hosted_run`` binds H to the run's own ``GITHUB_SHA``, repository, ref, event
and workflow before any I/O. Inside one ``finish_gate`` receipt the run then,
in order and stopping at the first refusal:

1. observes the executing checkout and requires it to be H, committed;
2. requires the trusted credentials its mode needs, without recording them: a
   normal run needs the main, tap and npm credentials; an explicit
   ``resume_published`` run needs only main and tap, since every canonical
   resume decision refuses an npm or release mutation, and its npm children
   get no token and empty owned configuration;
3. runs the shared publication ``preflight`` with the protection-unverified
   store reader, records that observation, and requires an exactly public
   repository. Strict store protection needs ruleset ``bypass_actors``, which
   GitHub shows only to ruleset writers, so it stays Root's own read; every
   fresh freeze, review, readiness and source binding still holds here;
4. observes the merge of the readiness pull request and every required check,
   and builds the canonical ``CandidatePromotion``;
5. loads the immutable candidate artifact by its locator and verifies its
   payload, then ``prepare_release_assets`` -> ``publish_prepared_release``
   (which creates the tag) -> ``deliver_npm`` -> ``deliver_tap``.

The npm package installs the exact version's archives from the public GitHub
release, so that release is published before npm can expose the version. Each
delivery edge owns its decision, its single attempt and its positive
postcondition; nothing here retries, rebuilds, repacks, installs or rolls back.
A failure after the publication leaves the published release, which the
receipt records, and the later destinations missing; a new explicit invocation
observes that release, skips it and may finish the rest. The
receipt's one ``publication`` report binds the dispatch and this run attempt.
It records delivery facts only after every postcondition held, and its
``first_mutation_started`` is derived from the receipt's own command records.
``finish_gate`` finalizes and removes the owned workspace; a failed run keeps
its sanitized failure and retains the workspace. The receipt grants nothing:
Root alone reads it, through ``require_published`` or ``require_failed``.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
import os
from pathlib import Path
import re

from scripts.delegate_acceptance.native_release.contract import PRODUCTION_STORE, ReleaseError
from scripts.delegate_acceptance.native_release.publication_contract import (
    PUBLICATION_RUN_ATTEMPT, PUBLICATION_WORKFLOW, REPORT_FIELD, PublicationDispatch, PublishedDelivery,
    ReportBinding, publication_report, require_public_repository,
)
from ..directories.journal import release_exclusive_file_lock
from ..directories.operations import DirectoryOwner
from ..evidence.digests import sha
from .assets import remember_directory
from .candidate_artifact import CandidateArtifact, load_candidate_artifact
from .candidate_build_source import child_environment
from .candidate_github_delivery import prepare_release_assets, publish_prepared_release, release_notes_endpoint
from .candidate_merge_observation import observe_publication_source, observe_required_checks
from .candidate_npm_delivery import deliver_npm
from .candidate_publication_contract import CandidatePromotion, encode_promotion
from .candidate_publication_preflight import preflight
from .candidate_tap_delivery import deliver_tap
from .cleanup import finish_gate
from .contract import VerificationError, new_receipt
from .filesystem import _acquire_receipt_lock, write_new_file
from .github import GitHubClient
from .published_contract import NPM_REGISTRY
from .published_ports import ConsumerPorts
from .source import observe_harness


CLAIM = "candidate-publication"


GATE = "candidate-publication"


STAGE = "deliver-candidate-publication"


# The maintained entry point; its static import closure is the executing harness H.
CLI_SEED = "scripts/publish_development_candidate.py"


RECEIPT_NAME = "receipt.json"


WORKSPACE_NAME = "workspace"


# The run's own identity; the workflow file is the shared protocol's, dispatched only on main.
RUN_VARIABLES = ("GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_REF",
                 "GITHUB_EVENT_NAME", "GITHUB_WORKFLOW_REF")


MAIN_REF = "refs/heads/main"


# The trusted job's credentials: main repository writes, the tap's writes and, outside resume, npm's publish.
GITHUB_TOKEN_VARIABLE = "GH_TOKEN"


TAP_TOKEN_VARIABLE = "HOMEBREW_TAP_TOKEN"


NPM_TOKEN_VARIABLE = "NPM_TOKEN"


# npm expands ``${NPM_TOKEN}`` from its own environment, so the owned file never holds the secret.
NPM_USER_CONFIG = f"//{NPM_REGISTRY.removeprefix('https://')}:_authToken=${{{NPM_TOKEN_VARIABLE}}}\n".encode()


# Request methods that change GitHub state; the transport records each request before it runs.
MUTATING_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})


# The one POST that writes nothing: GitHub's release-notes generation for this repository.
READ_ONLY_POST = release_notes_endpoint(PRODUCTION_STORE.repository)


_DECIMAL_RE = re.compile(r"^[1-9][0-9]{0,17}$")


@dataclass(frozen=True)
class HostedRun:
    """This run's repository, attempt and H; constructed only by ``hosted_run``."""

    repository: str
    run_id: int
    run_attempt: int
    harness: str


def hosted_run(environment: Mapping[str, str], dispatch: PublicationDispatch) -> HostedRun:
    """Bind the executing run to the dispatch before any I/O; a refusal names only fixed variables.

    H must be ``GITHUB_SHA``; the run must be this workflow's ``workflow_dispatch``
    on main of the production repository, in the attempt Root observes.
    """
    if not isinstance(dispatch, PublicationDispatch):
        raise VerificationError("publication_dispatch_invalid", STAGE)
    values = {name: environment.get(name) for name in RUN_VARIABLES}
    repository = PRODUCTION_STORE.repository
    expected = {
        "GITHUB_REPOSITORY": repository, "GITHUB_SHA": dispatch.harness, "GITHUB_REF": MAIN_REF,
        "GITHUB_EVENT_NAME": "workflow_dispatch",
        "GITHUB_WORKFLOW_REF": f"{repository}/.github/workflows/{PUBLICATION_WORKFLOW}@{MAIN_REF}",
    }
    mismatched = [name for name, value in expected.items() if values[name] != value]
    mismatched += [name for name in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT")
                   if not isinstance(values[name], str) or _DECIMAL_RE.fullmatch(values[name]) is None]
    # Root observes only the first attempt, so a rerun never reaches a destination it cannot reconcile.
    if "GITHUB_RUN_ATTEMPT" not in mismatched and int(values["GITHUB_RUN_ATTEMPT"]) != PUBLICATION_RUN_ATTEMPT:
        mismatched.append("GITHUB_RUN_ATTEMPT")
    if mismatched:
        raise VerificationError("publication_run_mismatch:" + ",".join(name.lower() for name in mismatched), STAGE)
    return HostedRun(repository=repository, run_id=int(values["GITHUB_RUN_ID"]),
                     run_attempt=int(values["GITHUB_RUN_ATTEMPT"]), harness=dispatch.harness)


@dataclass(frozen=True)
class HostedPublication:
    """One hosted publication: the dispatch, its bound run, a new output directory and the executing checkout."""

    dispatch: PublicationDispatch
    run: HostedRun
    output_dir: Path
    checkout: Path


def require_output_dir(request: HostedPublication) -> None:
    """Refuse, before anything is created, an output directory this invocation cannot own alone."""
    output, checkout = request.output_dir, request.checkout.resolve()
    if not output.is_absolute() or ".." in output.parts:
        raise VerificationError("candidate_publication_output_invalid", STAGE)
    if os.path.lexists(output):
        raise VerificationError("candidate_publication_output_exists", STAGE)
    resolved = output.parent.resolve() / output.name
    if resolved.is_relative_to(checkout) or checkout.is_relative_to(resolved):
        raise VerificationError("candidate_publication_output_overlap", STAGE)


def publish_candidate(request: HostedPublication, *, environment: Mapping[str, str], ports: ConsumerPorts) -> int:
    """Publish once; return the process exit code, 0 only for a delivered, finalized and cleaned-up run.

    ``environment`` is the job's environment; each child receives only the
    variables its own owner needs, and no credential is recorded or printed.
    """
    require_output_dir(request)
    output = request.output_dir.parent.resolve() / request.output_dir.name
    try:
        output.mkdir(mode=0o700)
    except OSError:
        raise VerificationError("candidate_publication_output_unavailable", STAGE) from None
    receipt_path = output / RECEIPT_NAME
    lock = _acquire_receipt_lock(receipt_path)
    try:
        return _publish_locked(request, output, receipt_path, environment, ports)
    finally:
        release_exclusive_file_lock(lock)


def first_mutation_started(receipt: dict) -> bool:
    """Whether any destination write was attempted, from the receipt's own command records.

    Both command owners append a record before the child runs, so a write that
    failed or never acknowledged still counts as started. Only the exact
    ``READ_ONLY_POST`` request is a read; its record keeps its real method.
    """
    for record in receipt["commands"]:
        if not isinstance(record, dict):
            raise VerificationError("receipt_commands_invalid", STAGE)
        argv = record.get("argv")
        method = record.get("method")
        if (record.get("name") == "github-api" and method in MUTATING_METHODS
                and (method, record.get("endpoint")) != ("POST", READ_ONLY_POST)):
            return True
        if isinstance(argv, list) and (argv[:3] == ["gh", "release", "upload"] or (
                len(argv) > 1 and isinstance(argv[0], str) and Path(argv[0]).name == "npm"
                and argv[1] == "publish")):
            return True
    return False


def _new_publication_receipt(run: HostedRun) -> dict:
    receipt = new_receipt(GATE, "", run.harness)
    # The tag is unknown until the candidate's manifest is observed.
    receipt.update({"claim": CLAIM, "tag": None})
    receipt["candidate_publication"] = {"status": "partial"}
    receipt["unresolved_limits"] = [
        "The receipt records one hosted attempt's observations; only Root's verification of the "
        "finalized receipt and its exact bindings can count a publication.",
        "Post-publication observations cover integrity and delivery only; no install, provider or "
        "behavior test runs after publication.",
    ]
    return receipt


def _publish_locked(request: HostedPublication, output: Path, receipt_path: Path,
                    environment: Mapping[str, str], ports: ConsumerPorts) -> int:
    receipt = _new_publication_receipt(request.run)
    binding = ReportBinding.of(request.dispatch, run_id=request.run.run_id, run_attempt=request.run.run_attempt)
    owner, locations = DirectoryOwner(), {}

    def action() -> None:
        try:
            _publish(request, output=output, environment=environment, ports=ports, receipt=receipt,
                     owner=owner, locations=locations, binding=binding)
        except ReleaseError as error:
            raise VerificationError(error.code, receipt["stage"]) from None
        finally:
            # A failed attempt still reports its binding and whether a write began; never a delivery.
            if REPORT_FIELD not in receipt:
                receipt[REPORT_FIELD] = publication_report(
                    binding, first_mutation_started=first_mutation_started(receipt))

    return finish_gate(receipt_path, receipt, owner, action, "remove", locations)


@dataclass(frozen=True)
class _Environments:
    """Each child owner's environment; the parent environment is never forwarded whole."""

    github: dict[str, str]
    tap: dict[str, str]
    npm: dict[str, str]
    # The owned npm user configuration's bytes; empty when npm may only be read.
    npm_user_config: bytes


def _environments(environment: Mapping[str, str], npm_config: Path, *, resume_published: bool) -> _Environments:
    """Require the mode's credentials before any request; a refusal names only the missing variables.

    An explicit resume publishes nothing to npm, so it neither requires nor
    forwards the npm credential, even when the job supplies one.
    """
    names = (GITHUB_TOKEN_VARIABLE, TAP_TOKEN_VARIABLE, *(() if resume_published else (NPM_TOKEN_VARIABLE,)))
    missing = [name for name in names if not environment.get(name)]
    if missing:
        raise VerificationError("publication_credentials_missing:" + ",".join(name.lower() for name in missing),
                                "credentials")
    base = child_environment(environment)
    npm_token = {} if resume_published else {NPM_TOKEN_VARIABLE: environment[NPM_TOKEN_VARIABLE]}
    return _Environments(
        github={**base, "GH_TOKEN": environment[GITHUB_TOKEN_VARIABLE], "GH_PROMPT_DISABLED": "1"},
        tap={**base, "GH_TOKEN": environment[TAP_TOKEN_VARIABLE], "GH_PROMPT_DISABLED": "1"},
        npm={**base, **npm_token, "NPM_CONFIG_USERCONFIG": str(npm_config / "user.npmrc"),
             "NPM_CONFIG_GLOBALCONFIG": str(npm_config / "global.npmrc")},
        npm_user_config=b"" if resume_published else NPM_USER_CONFIG)


def _publish(request: HostedPublication, *, output: Path, environment: Mapping[str, str], ports: ConsumerPorts,
             receipt: dict, owner: DirectoryOwner, locations: dict[str, Path], binding: ReportBinding) -> None:
    dispatch, record = request.dispatch, receipt["candidate_publication"]
    record["binding"] = binding.document()
    receipt["stage"] = "observe-harness"
    head, fingerprint, committed = observe_harness(receipt, environment, checkout=request.checkout,
                                                   seeds=(CLI_SEED,))
    receipt["source_harness"] = {"revision": head, "sha256": fingerprint}
    if not committed or head != dispatch.harness:
        raise VerificationError("publication_harness_mismatch", "observe-harness")
    receipt["stage"] = "credentials"
    workspace = output / WORKSPACE_NAME
    npm_config = workspace / "npm"
    resume = dispatch.request.resume_published
    environments = _environments(environment, npm_config, resume_published=resume)
    client = GitHubClient(receipt, environments.github)

    receipt["stage"] = "preflight"
    context = preflight(client, dispatch.request, verify_store_protection=False)
    record["preflight"] = {"pull_request": context.pull_request, "target_commit": context.target.commit,
                           "readiness_artifact_id": context.artifact.id, "store_head": context.snapshot.head,
                           "protection_unverified": context.protection_unverified}
    require_public_repository(context.repository_private)
    receipt["stage"] = "observe-merge"
    candidate = context.snapshot.candidate
    source = observe_publication_source(client, candidate=candidate, pull_request=context.pull_request,
                                        merged_commit=dispatch.request.reference.merged_commit)
    checks = observe_required_checks(client, context=context)
    record["required_checks"] = [item.context for item in checks.checks]
    freeze = context.snapshot.history.publication
    if freeze is None:
        raise VerificationError("publication_candidate_not_frozen", STAGE)
    promotion = CandidatePromotion(reference=dispatch.request.reference, candidate=candidate, freeze=freeze,
                                   source=source)
    receipt.update({"tag": candidate.manifest.build_tag, "package_version": candidate.manifest.version})
    record["promotion_sha256"] = sha(encode_promotion(promotion))

    receipt["stage"] = "load-candidate"
    remember_directory(receipt, owner.create("candidate publication workspace", workspace, output))
    locations["assets"] = workspace
    payload, requests = workspace / "payload", workspace / "requests"
    for directory in (payload, requests, npm_config):
        directory.mkdir(mode=0o700)
    for name, data in (("user.npmrc", environments.npm_user_config), ("global.npmrc", b"")):
        write_new_file(npm_config / name, data, path_code="publication_npm_config_invalid",
                       write_code="publication_npm_config_write_failed", stage="load-candidate")
    artifact = load_candidate_artifact(client, candidate.locator, container=workspace / "candidate-artifact.zip",
                                       payload=payload)
    _require_candidate(artifact, promotion)
    record["artifact"] = artifact.observation_record()

    receipt["stage"] = "prepare-release"
    prepared = prepare_release_assets(promotion, artifact, client=client, ports=ports, work_directory=requests,
                                      resume_published=resume)
    record["prepared_release_id"] = prepared.release_id
    receipt["stage"] = "publish-release"
    published = publish_prepared_release(promotion, prepared, client=client, work_directory=requests,
                                         resume_published=resume)
    record["published_release_id"] = published.release_id
    receipt["stage"] = "deliver-npm"
    npm = deliver_npm(artifact, ports=ports, receipt=receipt, environment=environments.npm,
                      resume_published=resume)
    record["npm"] = {"version": npm.version, "integrity": npm.integrity}
    receipt["stage"] = "deliver-tap"
    tap_blob = deliver_tap(artifact, client=GitHubClient(receipt, environments.tap), work_directory=requests)
    record["tap_blob_sha"] = tap_blob

    receipt["stage"] = "report"
    # deliver_npm returns only after it read ``latest`` at the target version, so that is the confirmed tag.
    delivery = PublishedDelivery(release_id=published.release_id, release=published.observed, npm=npm,
                                 npm_latest=candidate.manifest.version, tap_blob_sha=tap_blob)
    receipt[REPORT_FIELD] = publication_report(binding, first_mutation_started=first_mutation_started(receipt),
                                               promotion=promotion, delivery=delivery)
    record["status"] = "delivered"


def _require_candidate(artifact: CandidateArtifact, promotion: CandidatePromotion) -> None:
    """The loaded payload is the promoted candidate's, before any edge sees it."""
    if (not isinstance(artifact, CandidateArtifact) or artifact.locator != promotion.candidate.locator
            or artifact.manifest != promotion.candidate.manifest):
        raise VerificationError("candidate_publication_artifact_mismatch", STAGE)
